package reviewtransaction

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompactReleaseGateUsesIndependentCompleteCurrentEvidence(t *testing.T) {
	repo := initSnapshotRepo(t)
	writeSnapshotFile(t, repo, "tracked.txt", "candidate\n")
	gitSnapshot(t, repo, "add", "tracked.txt")
	gitSnapshot(t, repo, "commit", "-m", "candidate")
	state, store, receipt := approvedCompactRevisionFixture(t, repo, "compact-release")
	dir := t.TempDir()
	paths := map[string]string{}
	for name, content := range map[string]string{
		"configuration": "release configuration\n", "generated": "generated manifest\n",
		"provenance": "release provenance\n", "boundary": "sealed publication boundary\n",
		"freshness": "current release evidence\n",
	} {
		paths[name] = filepath.Join(dir, name)
		if err := os.WriteFile(paths[name], []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	input := NativeGateRequestInput{
		Gate: GateRelease, LineageID: state.LineageID,
		ReleaseConfiguration: paths["configuration"], ReleaseGenerated: paths["generated"],
		ReleaseProvenance: paths["provenance"], ReleasePublicationBoundary: paths["boundary"],
		ReleaseEvidenceFreshness: paths["freshness"],
	}
	if got := EvaluateCompactGate(context.Background(), repo, receipt, input); got.Result != GateAllow || got.Context.Release == nil {
		t.Fatalf("independent compact release evidence = %#v", got)
	}
	if _, err := store.Load(); err != nil {
		t.Fatal(err)
	}

	missing := input
	missing.ReleaseProvenance = ""
	if got := EvaluateCompactGate(context.Background(), repo, receipt, missing); got.Result != GateInvalidated {
		t.Fatalf("missing compact release evidence = %#v", got)
	}
	originalHook := finalGateAuthorizationHook
	finalGateAuthorizationHook = func() {
		finalGateAuthorizationHook = originalHook
		if err := os.WriteFile(paths["freshness"], []byte("tampered after derivation\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { finalGateAuthorizationHook = originalHook })
	if got := EvaluateCompactGate(context.Background(), repo, receipt, input); got.Result != GateInvalidated || !strings.Contains(got.Reason, "release evidence changed") {
		t.Fatalf("tampered compact release evidence = %#v", got)
	}
}

func TestCompactGateRejectsCallerLineageMismatch(t *testing.T) {
	repo := initSnapshotRepo(t)
	writeSnapshotFile(t, repo, "tracked.txt", "candidate\n")
	gitSnapshot(t, repo, "add", "tracked.txt")
	gitSnapshot(t, repo, "commit", "-m", "candidate")
	state, _, receipt := approvedCompactRevisionFixture(t, repo, "compact-lineage-match")
	result := EvaluateCompactGate(context.Background(), repo, receipt, NativeGateRequestInput{
		Gate: GatePrePush, LineageID: "different-lineage",
	})
	if result.Result != GateInvalidated || !strings.Contains(result.Reason, "lineage") {
		t.Fatalf("mismatched compact lineage = %#v for %s", result, state.LineageID)
	}
}

func TestCompactGateFinalRecheckRejectsConcurrentAuthorityAndGitChanges(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(t *testing.T, repo string, store CompactStore, state CompactState, revision string)
	}{
		{name: "Git target", mutate: func(t *testing.T, repo string, _ CompactStore, _ CompactState, _ string) {
			writeSnapshotFile(t, repo, "tracked.txt", "changed during gate\n")
		}},
		{name: "authority", mutate: func(t *testing.T, _ string, store CompactStore, state CompactState, revision string) {
			payload, err := os.ReadFile(store.StatePath())
			if err != nil {
				t.Fatal(err)
			}
			var record map[string]any
			if err := json.Unmarshal(payload, &record); err != nil {
				t.Fatal(err)
			}
			record["revision"] = hash("f")
			payload, _ = json.Marshal(record)
			if err := os.WriteFile(store.StatePath(), payload, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			repo := initSnapshotRepo(t)
			writeSnapshotFile(t, repo, "tracked.txt", "candidate\n")
			state := newCompactTestState(t, repo, "compact-final-recheck")
			results := make([]LensResult, len(state.SelectedLenses))
			for index, lens := range state.SelectedLenses {
				results[index] = LensResult{Lens: lens, Findings: []Finding{}, Evidence: []string{"review completed"}}
			}
			store, _ := CompactAuthoritativeStore(context.Background(), repo, state.LineageID)
			revision, _ := store.Replace("", "review/start", state)
			_ = state.CompleteReview(CompactReviewInput{LensResults: results, Classifications: []FindingEvidence{}, RefuterOutcomes: []EvidenceResult{}})
			revision, _ = store.Replace(revision, "review/complete-review", state)
			_ = state.CompleteVerification([]byte("tests pass\n"), true)
			revision, _ = store.Replace(revision, "review/complete-verification", state)
			receipt, _ := state.Receipt()
			_ = WriteCompactReceiptAtomic(store.ReceiptPath(), receipt)
			originalHook := finalGateAuthorizationHook
			finalGateAuthorizationHook = func() {
				finalGateAuthorizationHook = originalHook
				tt.mutate(t, repo, store, state, revision)
			}
			t.Cleanup(func() { finalGateAuthorizationHook = originalHook })
			got := EvaluateCompactGate(context.Background(), repo, receipt, NativeGateRequestInput{Gate: GatePreCommit, LineageID: state.LineageID})
			if got.Result != GateInvalidated || !strings.Contains(got.Reason, "changed during final authorization") {
				t.Fatalf("compact final recheck = %#v", got)
			}
		})
	}
}

func approvedCompactRevisionFixture(t *testing.T, repo, lineage string) (CompactState, CompactStore, CompactReceipt) {
	t.Helper()
	state := newCompactRevisionState(t, repo, lineage)
	store, _ := CompactAuthoritativeStore(context.Background(), repo, lineage)
	revision, err := store.Replace("", "review/start", state)
	if err != nil {
		t.Fatal(err)
	}
	results := make([]LensResult, len(state.SelectedLenses))
	for index, lens := range state.SelectedLenses {
		results[index] = LensResult{Lens: lens, Findings: []Finding{}, Evidence: []string{"review completed"}}
	}
	if err := state.CompleteReview(CompactReviewInput{LensResults: results, Classifications: []FindingEvidence{}, RefuterOutcomes: []EvidenceResult{}}); err != nil {
		t.Fatal(err)
	}
	revision, err = store.Replace(revision, "review/complete-review", state)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.CompleteVerification([]byte("independent verification passed\n"), true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Replace(revision, "review/complete-verification", state); err != nil {
		t.Fatal(err)
	}
	receipt, err := state.Receipt()
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteCompactReceiptAtomic(store.ReceiptPath(), receipt); err != nil {
		t.Fatal(err)
	}
	return state, store, receipt
}
