package sddstatus

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gentleman-programming/gentle-ai/internal/reviewtransaction"
)

func TestResolveArchiveRequiresApprovedExactReviewReceipt(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(t *testing.T, changeRoot string, receipt reviewtransaction.Receipt, request reviewtransaction.GateRequest)
		wantGate    reviewtransaction.GateResult
		wantArchive DependencyState
		wantNext    string
		wantReason  string
	}{
		{
			name: "missing receipt invalidates archive",
			mutate: func(t *testing.T, changeRoot string, _ reviewtransaction.Receipt, _ reviewtransaction.GateRequest) {
				if err := os.Remove(filepath.Join(changeRoot, "reviews", "receipt.json")); err != nil {
					t.Fatal(err)
				}
			},
			wantGate: reviewtransaction.GateInvalidated, wantArchive: DependencyBlocked,
			wantNext: "resolve-review", wantReason: "approved review receipt is missing",
		},
		{
			name:     "exact authoritative artifacts allow archive",
			mutate:   func(_ *testing.T, _ string, _ reviewtransaction.Receipt, _ reviewtransaction.GateRequest) {},
			wantGate: reviewtransaction.GateAllow, wantArchive: DependencyReady,
			wantNext: "archive",
		},
		{
			name: "missing transaction invalidates archive",
			mutate: func(t *testing.T, changeRoot string, _ reviewtransaction.Receipt, _ reviewtransaction.GateRequest) {
				if err := os.Remove(filepath.Join(changeRoot, "reviews", "transaction.json")); err != nil {
					t.Fatal(err)
				}
			},
			wantGate: reviewtransaction.GateInvalidated, wantArchive: DependencyBlocked,
			wantNext: "resolve-review", wantReason: "transaction",
		},
		{
			name: "missing portable chain bundle invalidates archive",
			mutate: func(t *testing.T, changeRoot string, _ reviewtransaction.Receipt, _ reviewtransaction.GateRequest) {
				if err := os.Remove(filepath.Join(changeRoot, "reviews", "chain-bundle.json")); err != nil {
					t.Fatal(err)
				}
			},
			wantGate: reviewtransaction.GateInvalidated, wantArchive: DependencyBlocked,
			wantNext: "resolve-review", wantReason: "portable review chain bundle",
		},
		{
			name: "missing ledger invalidates archive",
			mutate: func(t *testing.T, changeRoot string, _ reviewtransaction.Receipt, _ reviewtransaction.GateRequest) {
				if err := os.Remove(filepath.Join(changeRoot, "reviews", "ledger.json")); err != nil {
					t.Fatal(err)
				}
			},
			wantGate: reviewtransaction.GateInvalidated, wantArchive: DependencyBlocked,
			wantNext: "resolve-review", wantReason: "ledger",
		},
		{
			name: "stale ledger invalidates archive",
			mutate: func(t *testing.T, changeRoot string, _ reviewtransaction.Receipt, _ reviewtransaction.GateRequest) {
				write(t, filepath.Join(changeRoot, "reviews", "ledger.json"), "{\"schema\":\"gentle-ai.review-ledger/v1\",\"findings\":[{\"id\":\"stale\"}]}")
			},
			wantGate: reviewtransaction.GateInvalidated, wantArchive: DependencyBlocked,
			wantNext: "resolve-review", wantReason: "invalidated",
		},
		{
			name: "mismatched transaction invalidates archive",
			mutate: func(t *testing.T, changeRoot string, _ reviewtransaction.Receipt, _ reviewtransaction.GateRequest) {
				var transaction reviewtransaction.Transaction
				path := filepath.Join(changeRoot, "reviews", "transaction.json")
				readJSON(t, path, &transaction)
				transaction.LineageID = "different-lineage"
				writeJSON(t, path, transaction)
			},
			wantGate: reviewtransaction.GateInvalidated, wantArchive: DependencyBlocked,
			wantNext: "resolve-review", wantReason: "persisted",
		},
		{
			name: "pending receipt invalidates archive",
			mutate: func(t *testing.T, changeRoot string, receipt reviewtransaction.Receipt, _ reviewtransaction.GateRequest) {
				payload, err := json.Marshal(receipt)
				if err != nil {
					t.Fatalf("Marshal(receipt): %v", err)
				}
				pending := strings.Replace(string(payload), `"terminal_state":"approved"`, `"terminal_state":"pending"`, 1)
				write(t, filepath.Join(changeRoot, "reviews", "receipt.json"), pending)
			},
			wantGate: reviewtransaction.GateInvalidated, wantArchive: DependencyBlocked,
			wantNext: "resolve-review", wantReason: "invalid or non-terminal",
		},
		{
			name: "unrelated candidate requires new lineage",
			mutate: func(t *testing.T, changeRoot string, _ reviewtransaction.Receipt, _ reviewtransaction.GateRequest) {
				write(t, filepath.Join(changeRoot, "tasks.md"), "- [x] 1.1 Work\n- [x] scope changed\n")
			},
			wantGate: reviewtransaction.GateScopeChanged, wantArchive: DependencyBlocked,
			wantNext: "resolve-review", wantReason: "explicit new lineage",
		},
		{
			name: "new failing external evidence escalates without review",
			mutate: func(t *testing.T, changeRoot string, _ reviewtransaction.Receipt, request reviewtransaction.GateRequest) {
				request.ExternalEvidence = reviewtransaction.ExternalEvidenceEscalating
				writeJSON(t, filepath.Join(changeRoot, "reviews", "gate-context.json"), request)
			},
			wantGate: reviewtransaction.GateEscalated, wantArchive: DependencyBlocked,
			wantNext: "resolve-review", wantReason: "external evidence",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			changeRoot := seedBoundedReadyChange(t, root)
			writeApprovedReviewArtifacts(t, changeRoot)
			var receipt reviewtransaction.Receipt
			readJSON(t, filepath.Join(changeRoot, "reviews", "receipt.json"), &receipt)
			var request reviewtransaction.GateRequest
			readJSON(t, filepath.Join(changeRoot, "reviews", "gate-context.json"), &request)
			tt.mutate(t, changeRoot, receipt, request)

			status, err := Resolve(ResolveOptions{CWD: root, ChangeName: "thin"})
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if status.ReviewGate == nil {
				t.Fatal("ReviewGate is nil for final archive decision")
			}
			if status.ReviewGate.Result != tt.wantGate {
				t.Fatalf("ReviewGate.Result = %q, want %q (%s)", status.ReviewGate.Result, tt.wantGate, status.ReviewGate.Reason)
			}
			if status.Dependencies.Archive != tt.wantArchive || status.NextRecommended != tt.wantNext {
				t.Fatalf("archive=%q next=%q, want %q/%q", status.Dependencies.Archive, status.NextRecommended, tt.wantArchive, tt.wantNext)
			}
			if tt.wantReason != "" && !strings.Contains(strings.Join(status.BlockedReasons, "\n"), tt.wantReason) {
				t.Fatalf("BlockedReasons = %v, want containing %q", status.BlockedReasons, tt.wantReason)
			}
		})
	}
}

func TestReviewArtifactPathsUseExactOpenSpecAndEngramReferences(t *testing.T) {
	changeRoot := t.TempDir()
	for _, name := range []string{"transaction.json", "policy.md", "ledger.json", "receipt.json", "chain-bundle.json", "gate-context.json"} {
		write(t, filepath.Join(changeRoot, "reviews", name), "{}\n")
	}
	paths, err := resolveArtifactPaths(changeRoot)
	if err != nil {
		t.Fatal(err)
	}
	if got := firstPath(paths.ReviewState); got != filepath.Join(changeRoot, "reviews", "transaction.json") {
		t.Fatalf("OpenSpec transaction path = %q", got)
	}
	if got := firstPath(paths.ReviewLedger); got != filepath.Join(changeRoot, "reviews", "ledger.json") {
		t.Fatalf("OpenSpec ledger path = %q", got)
	}
	if got := firstPath(paths.ReviewPolicy); got != filepath.Join(changeRoot, "reviews", "policy.md") {
		t.Fatalf("OpenSpec policy path = %q", got)
	}

	engram := engramArtifactPaths("bounded", map[string]engramObservation{
		"review/transaction":  {Title: "sdd/bounded/review/transaction", Content: "{}"},
		"review/policy":       {Title: "sdd/bounded/review/policy", Content: "policy"},
		"review/ledger":       {Title: "sdd/bounded/review/ledger", Content: "{}"},
		"review/receipt":      {Title: "sdd/bounded/review/receipt", Content: "{}"},
		"review/chain-bundle": {Title: "sdd/bounded/review/chain-bundle", Content: "{}"},
		"review/gate-context": {Title: "sdd/bounded/review/gate-context", Content: "{}"},
	})
	for label, got := range map[string]string{
		"transaction":  firstPath(engram.ReviewState),
		"ledger":       firstPath(engram.ReviewLedger),
		"policy":       firstPath(engram.ReviewPolicy),
		"receipt":      firstPath(engram.ReviewReceipt),
		"chain-bundle": firstPath(engram.ReviewBundle),
		"context":      firstPath(engram.ReviewContext),
	} {
		want := "sdd/bounded/review/" + label
		if label == "context" {
			want = "sdd/bounded/review/gate-context"
		}
		if got != want {
			t.Errorf("Engram %s path = %q, want %q", label, got, want)
		}
	}
}

func TestEngramReviewArtifactDiscoveryRetainsOnlyExactNames(t *testing.T) {
	observations := []engramObservation{
		{Title: "sdd/bounded/review/chain-bundle", Content: "exact bundle", Project: "gentle-ai", Scope: "project"},
		{Title: "sdd/bounded/review/chain-bundles", Content: "plural", Project: "gentle-ai", Scope: "project"},
		{Title: "sdd/bounded/review/chain-bundle/extra", Content: "nested", Project: "gentle-ai", Scope: "project"},
		{Title: "sdd/bounded/review/arbitrary", Content: "arbitrary", Project: "gentle-ai", Scope: "project"},
	}

	changes := collectEngramChanges(observations, "gentle-ai")
	if !reflect.DeepEqual(changes, []string{"bounded"}) {
		t.Fatalf("collectEngramChanges() = %v, want exact chain-bundle change", changes)
	}
	artifacts := engramArtifactsForChange(observations, "gentle-ai", "bounded")
	if len(artifacts) != 1 || artifacts["review/chain-bundle"].Content != "exact bundle" {
		t.Fatalf("engramArtifactsForChange() = %#v, want only exact review/chain-bundle", artifacts)
	}
}

func TestResolveEngramArchiveRecoversRetainedPolicyWithoutSourceArtifacts(t *testing.T) {
	root := t.TempDir()
	changeRoot := seedBoundedReadyChange(t, root)
	writeApprovedReviewArtifacts(t, changeRoot)
	mkdir(t, filepath.Join(root, ".engram"))
	project := strings.ToLower(filepath.Base(root))
	observations := boundedEngramObservations(t, project, changeRoot)
	if err := os.RemoveAll(filepath.Join(changeRoot, "reviews")); err != nil {
		t.Fatal(err)
	}
	restore := stubEngramExport(t, observations)
	defer restore()

	status, ok, err := resolveEngramStatus(root, "thin", false)
	if err != nil {
		t.Fatalf("resolveEngramStatus() error = %v", err)
	}
	if !ok {
		t.Fatal("resolveEngramStatus() did not retain the Engram change")
	}
	if status.Artifacts["reviewBundle"] != ArtifactDone || firstPath(status.ArtifactPaths.ReviewBundle) != "sdd/thin/review/chain-bundle" {
		t.Fatalf("Engram bundle artifact = %q paths=%v", status.Artifacts["reviewBundle"], status.ArtifactPaths.ReviewBundle)
	}
	if status.ReviewGate == nil || status.ReviewGate.Result != reviewtransaction.GateAllow || status.Dependencies.Archive != DependencyReady || status.NextRecommended != "archive" {
		t.Fatalf("Engram archive status = gate=%#v archive=%q next=%q", status.ReviewGate, status.Dependencies.Archive, status.NextRecommended)
	}
}

func TestResolveEngramArchiveRejectsTamperedRetainedPolicy(t *testing.T) {
	root := t.TempDir()
	changeRoot := seedBoundedReadyChange(t, root)
	writeApprovedReviewArtifacts(t, changeRoot)
	mkdir(t, filepath.Join(root, ".engram"))
	observations := boundedEngramObservations(t, strings.ToLower(filepath.Base(root)), changeRoot)
	for index := range observations {
		if observations[index].Title == "sdd/thin/review/policy" {
			observations[index].Content = "tampered policy\n"
		}
	}
	if err := os.RemoveAll(filepath.Join(changeRoot, "reviews")); err != nil {
		t.Fatal(err)
	}
	restore := stubEngramExport(t, observations)
	defer restore()

	status, ok, err := resolveEngramStatus(root, "thin", false)
	if err != nil {
		t.Fatalf("resolveEngramStatus() error = %v", err)
	}
	if !ok {
		t.Fatal("resolveEngramStatus() did not retain the Engram change")
	}
	if status.ReviewGate == nil || status.ReviewGate.Result != reviewtransaction.GateInvalidated || status.Dependencies.Archive != DependencyBlocked {
		t.Fatalf("tampered Engram policy status = gate=%#v archive=%q", status.ReviewGate, status.Dependencies.Archive)
	}
}

func boundedEngramObservations(t *testing.T, project, changeRoot string) []engramObservation {
	t.Helper()
	observation := func(title, path string) engramObservation {
		t.Helper()
		payload, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return engramObservation{Title: title, Content: string(payload), Project: project, Scope: "project"}
	}
	return []engramObservation{
		observation("sdd/thin/proposal", filepath.Join(changeRoot, "proposal.md")),
		observation("sdd/thin/spec", filepath.Join(changeRoot, "specs", "auth", "spec.md")),
		observation("sdd/thin/design", filepath.Join(changeRoot, "design.md")),
		observation("sdd/thin/tasks", filepath.Join(changeRoot, "tasks.md")),
		{Title: "sdd/thin/apply-progress", Content: "all work units complete", Project: project, Scope: "project"},
		observation("sdd/thin/verify-report", filepath.Join(changeRoot, "verify-report.md")),
		observation("sdd/thin/review/transaction", filepath.Join(changeRoot, "reviews", "transaction.json")),
		observation("sdd/thin/review/policy", filepath.Join(changeRoot, "reviews", "policy.md")),
		observation("sdd/thin/review/ledger", filepath.Join(changeRoot, "reviews", "ledger.json")),
		observation("sdd/thin/review/receipt", filepath.Join(changeRoot, "reviews", "receipt.json")),
		observation("sdd/thin/review/chain-bundle", filepath.Join(changeRoot, "reviews", "chain-bundle.json")),
		observation("sdd/thin/review/gate-context", filepath.Join(changeRoot, "reviews", "gate-context.json")),
	}
}

func TestResolveFinalVerifyWaitsForAllTasks(t *testing.T) {
	root := t.TempDir()
	changeRoot := seedReadyChange(t, root, "thin", "- [x] 1.1 Done\n- [ ] 1.2 Pending\n")
	write(t, filepath.Join(changeRoot, "apply-progress.md"), "focused work-unit checks passed\n")
	status, err := Resolve(ResolveOptions{CWD: root, ChangeName: "thin"})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if status.Dependencies.Verify != DependencyBlocked || status.NextRecommended != "apply" {
		t.Fatalf("verify=%q next=%q, want blocked/apply", status.Dependencies.Verify, status.NextRecommended)
	}
}

func TestResolveStartsBoundedReviewBeforeFinalVerification(t *testing.T) {
	root := t.TempDir()
	changeRoot := seedReadyChange(t, root, "thin", "- [x] 1.1 Done\n")
	status, err := Resolve(ResolveOptions{CWD: root, ChangeName: "thin"})
	if err != nil {
		t.Fatal(err)
	}
	if status.Dependencies.Verify != DependencyBlocked || status.NextRecommended != "review" {
		t.Fatalf("without transaction verify=%q next=%q, want blocked/review", status.Dependencies.Verify, status.NextRecommended)
	}
	dispatcher := RenderDispatcherMarkdown(status)
	for _, want := range []string{"### Next Review Operation", "gentle-ai review-start", "--intended-untracked-manifest", "reconcile-terminal-mirrors", "review-validate"} {
		if !strings.Contains(dispatcher, want) {
			t.Fatalf("dispatcher missing %q:\n%s", want, dispatcher)
		}
	}
	if strings.Contains(dispatcher, "--machine-transaction-out") {
		t.Fatalf("dispatcher routes a mirror write before the authoritative append:\n%s", dispatcher)
	}

	tx, err := reviewtransaction.NewTransaction(reviewtransaction.Start{
		LineageID: "thin-lineage", Mode: reviewtransaction.ModeOrdinary4R, Generation: 1,
		Snapshot: reviewtransaction.Snapshot{
			Kind: reviewtransaction.TargetCurrentChanges, BaseTree: strings.Repeat("a", 40), CandidateTree: strings.Repeat("b", 40),
			PathsDigest: shaID("2"), IntendedUntracked: []string{}, IntendedUntrackedProof: shaID("3"), Paths: []string{"internal/example.go"}, Identity: shaID("4"),
		},
		PolicyHash: shaID("5"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.StartReview(); err != nil {
		t.Fatal(err)
	}
	if err := freezeStatusFindings(tx, []reviewtransaction.Finding{}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ClassifyEvidence([]reviewtransaction.FindingEvidence{}); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(changeRoot, "reviews", "transaction.json"), tx)

	status, err = Resolve(ResolveOptions{CWD: root, ChangeName: "thin"})
	if err != nil {
		t.Fatal(err)
	}
	if status.Dependencies.Verify != DependencyReady || status.NextRecommended != "verify" {
		t.Fatalf("ready transaction verify=%q next=%q, want ready/verify", status.Dependencies.Verify, status.NextRecommended)
	}
}

func TestResolveRemediationIsBoundToBudgetAndFailedEvidenceRevision(t *testing.T) {
	root := t.TempDir()
	changeRoot := seedReadyChange(t, root, "thin", "- [x] 1.1 Done\n")
	write(t, filepath.Join(changeRoot, "specs", "auth", "spec.md"), "### Requirement: Auth\n#### Scenario: Valid login\n")
	revision := shaID("d")
	write(t, filepath.Join(changeRoot, "verify-report.md"), boundedVerifyEnvelope(revision, "fail"))

	tx := remediationTransaction(t, revision, false)
	writeJSON(t, filepath.Join(changeRoot, "reviews", "transaction.json"), tx)
	status, err := Resolve(ResolveOptions{CWD: root, ChangeName: "thin"})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if status.NextRecommended != "remediate" || !status.RemediationState.Required {
		t.Fatalf("next=%q remediation=%#v, want bounded remediation", status.NextRecommended, status.RemediationState)
	}

	stale := remediationTransaction(t, shaID("e"), false)
	writeJSON(t, filepath.Join(changeRoot, "reviews", "transaction.json"), stale)
	status, err = Resolve(ResolveOptions{CWD: root, ChangeName: "thin"})
	if err != nil {
		t.Fatalf("Resolve(stale) error = %v", err)
	}
	if status.NextRecommended != "resolve-review" || status.RemediationState.Required {
		t.Fatalf("stale next=%q remediation=%#v", status.NextRecommended, status.RemediationState)
	}
	if !strings.Contains(strings.Join(status.BlockedReasons, "\n"), "does not match failed evidence revision") {
		t.Fatalf("BlockedReasons = %v", status.BlockedReasons)
	}
}

func seedBoundedReadyChange(t *testing.T, root string) string {
	t.Helper()
	changeRoot := seedReadyChange(t, root, "thin", "- [x] 1.1 Work\n")
	write(t, filepath.Join(changeRoot, "specs", "auth", "spec.md"), "### Requirement: Auth\n#### Scenario: Valid login\n")
	write(t, filepath.Join(changeRoot, "verify-report.md"), boundedVerifyEnvelope(shaID("1"), "pass"))
	return changeRoot
}

func writeApprovedReviewArtifacts(t *testing.T, changeRoot string) {
	t.Helper()
	repo := filepath.Dir(filepath.Dir(filepath.Dir(changeRoot)))
	runSDDStatusGit(t, repo, "init", "-q")
	runSDDStatusGit(t, repo, "config", "user.email", "status@example.com")
	runSDDStatusGit(t, repo, "config", "user.name", "Status Test")
	runSDDStatusGit(t, repo, "add", ".")
	runSDDStatusGit(t, repo, "commit", "-qm", "base")
	tasksPath := filepath.Join(changeRoot, "tasks.md")
	tasks, err := os.ReadFile(tasksPath)
	if err != nil {
		t.Fatal(err)
	}
	write(t, tasksPath, string(tasks)+"\n# Reviewed candidate\n")

	snapshot, err := (reviewtransaction.SnapshotBuilder{Repo: repo}).Build(context.Background(), reviewtransaction.Target{
		Kind: reviewtransaction.TargetCurrentChanges, IntendedUntracked: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	reviewsDir := filepath.Join(changeRoot, "reviews")
	policyPath := filepath.Join(reviewsDir, "policy.md")
	ledgerPath := filepath.Join(reviewsDir, "ledger.json")
	verifyPath := filepath.Join(changeRoot, "verify-report.md")
	write(t, policyPath, "bounded archive policy\n")
	write(t, ledgerPath, reviewtransaction.CanonicalEmptyLedger)
	policyHash, err := reviewtransaction.HashArtifact(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	ledgerHash, err := reviewtransaction.HashArtifact(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	evidenceHash, err := reviewtransaction.HashArtifact(verifyPath)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := reviewtransaction.NewTransaction(reviewtransaction.Start{
		LineageID: "thin-lineage", Mode: reviewtransaction.ModeOrdinary4R, Generation: 1,
		Snapshot: snapshot, PolicyHash: policyHash,
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := reviewtransaction.AuthoritativeStore(context.Background(), repo, "thin-lineage")
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.StartReview(); err != nil {
		t.Fatal(err)
	}
	revision, err := store.Append("", reviewtransaction.Record{Operation: "review/start", Transaction: *tx})
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := reviewtransaction.CanonicalLedger([]reviewtransaction.Finding{})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.FreezeFindings([]reviewtransaction.Finding{}, ledger, ledgerHash); err != nil {
		t.Fatal(err)
	}
	revision, err = store.Append(revision, reviewtransaction.Record{Operation: "review/freeze-findings", Transaction: *tx})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ClassifyEvidence([]reviewtransaction.FindingEvidence{}); err != nil {
		t.Fatal(err)
	}
	revision, err = store.Append(revision, reviewtransaction.Record{Operation: "review/classify", Transaction: *tx})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.BeginFinalVerification(); err != nil {
		t.Fatal(err)
	}
	revision, err = store.Append(revision, reviewtransaction.Record{Operation: "review/begin-final-verification", Transaction: *tx})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.CompleteFinalVerification(evidenceHash, true); err != nil {
		t.Fatal(err)
	}
	revision, err = store.Append(revision, reviewtransaction.Record{Operation: "review/complete-final-verification", Transaction: *tx})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := tx.Receipt()
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := store.ExportBundle()
	if err != nil {
		t.Fatal(err)
	}
	request := reviewtransaction.GateRequest{
		Schema: reviewtransaction.GateRequestSchema, Gate: reviewtransaction.GatePostApply,
		Target:          reviewtransaction.Target{Kind: reviewtransaction.TargetCurrentChanges, IntendedUntracked: []string{}},
		StoreRevision:   revision,
		GenesisRevision: bundle.GenesisRevision, ChainIdentity: bundle.ChainIdentity, BundleDigest: bundle.BundleDigest,
		PolicyArtifact: policyPath, LedgerArtifact: ledgerPath, EvidenceArtifact: verifyPath,
	}
	writeJSON(t, filepath.Join(reviewsDir, "transaction.json"), tx)
	writeJSON(t, filepath.Join(reviewsDir, "receipt.json"), receipt)
	if err := reviewtransaction.WriteChainBundleAtomic(filepath.Join(reviewsDir, "chain-bundle.json"), bundle); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(reviewsDir, "gate-context.json"), request)
}

func boundedVerifyEnvelope(revision, verdict string) string {
	return strings.Join([]string{
		"```yaml",
		"schema: gentle-ai.verify-result/v1",
		"evidence_revision: " + revision,
		"verdict: " + verdict,
		"blockers: 0",
		"critical_findings: 0",
		"requirements: 1/1",
		"scenarios: 1/1",
		"test_command: go test ./internal/example",
		"test_exit_code: 0",
		"test_output_hash: " + shaID("2"),
		"build_command: go test ./cmd/gentle-ai",
		"build_exit_code: 0",
		"build_output_hash: " + shaID("3"),
		"```",
	}, "\n")
}

func remediationTransaction(t *testing.T, revision string, ready bool) reviewtransaction.Transaction {
	t.Helper()
	tx, err := reviewtransaction.NewTransaction(reviewtransaction.Start{
		LineageID: "thin-lineage", Mode: reviewtransaction.ModeOrdinary4R, Generation: 1,
		Snapshot: reviewtransaction.Snapshot{
			Kind: reviewtransaction.TargetCurrentChanges, BaseTree: strings.Repeat("a", 40), CandidateTree: strings.Repeat("b", 40),
			PathsDigest: shaID("4"), IntendedUntracked: []string{}, IntendedUntrackedProof: shaID("8"),
			Paths: []string{"internal/example.go"}, Identity: shaID("9"),
		},
		PolicyHash: shaID("5"),
	})
	if err != nil {
		t.Fatalf("NewTransaction() error = %v", err)
	}
	_ = tx.StartReview()
	_ = freezeStatusFindings(tx, []reviewtransaction.Finding{{ID: "R1-001", Severity: "CRITICAL"}})
	_, _ = tx.ClassifyEvidence([]reviewtransaction.FindingEvidence{{FindingID: "R1-001", Class: reviewtransaction.EvidenceDeterministic, Proof: "failing focused test"}})
	if err := tx.BeginFix(revision); err != nil {
		t.Fatalf("BeginFix() error = %v", err)
	}
	if ready {
		fix := tx.Snapshot
		fix.Kind = reviewtransaction.TargetFixDiff
		fix.BaseTree = tx.FinalCandidateTree
		fix.CandidateTree = strings.Repeat("c", 40)
		fix.LedgerIDs = []string{"R1-001"}
		fix.Identity = shaID("a")
		if err := tx.CompleteFix(fix, shaID("b"), []string{"R1-001"}); err != nil {
			t.Fatalf("CompleteFix() error = %v", err)
		}
		if err := tx.ValidateFixDelta([]string{"R1-001"}, true); err != nil {
			t.Fatalf("ValidateFixDelta() error = %v", err)
		}
	}
	return *tx
}

func readJSON(t *testing.T, path string, destination any) {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(payload, destination); err != nil {
		t.Fatal(err)
	}
}

func runSDDStatusGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", repo}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("Marshal(%T): %v", value, err)
	}
	write(t, path, string(payload)+"\n")
}

func shaID(char string) string {
	return fmt.Sprintf("sha256:%s", strings.Repeat(char, 64))
}

func freezeStatusFindings(tx *reviewtransaction.Transaction, findings []reviewtransaction.Finding) error {
	ledger, err := reviewtransaction.CanonicalLedger(findings)
	if err != nil {
		return err
	}
	return tx.FreezeFindings(findings, ledger, "")
}
