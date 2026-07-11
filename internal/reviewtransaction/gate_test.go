package reviewtransaction

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestNativeReleaseGateDerivesCompleteImmutableBoundary(t *testing.T) {
	repo := initSnapshotRepo(t)
	writeSnapshotFile(t, repo, "tracked.txt", "release\n")
	gitSnapshot(t, repo, "add", "tracked.txt")
	gitSnapshot(t, repo, "commit", "-m", "release")
	releaseCommit := trimGit(gitSnapshot(t, repo, "rev-parse", "HEAD"))
	snapshot, err := (SnapshotBuilder{Repo: repo}).Build(context.Background(), Target{Kind: TargetExactRevision, Revision: releaseCommit})
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	artifacts := map[string]string{
		"policy":        "bounded release policy\n",
		"ledger":        CanonicalEmptyLedger,
		"evidence":      "fresh verification evidence\n",
		"configuration": "release configuration\n",
		"generated":     "generated artifact manifest\n",
		"provenance":    "signed provenance\n",
		"boundary":      "publication boundary\n",
		"freshness":     "current evidence marker\n",
	}
	paths := make(map[string]string, len(artifacts))
	hashes := make(map[string]string, len(artifacts))
	for name, content := range artifacts {
		path := filepath.Join(dir, name+".txt")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		paths[name] = path
		hashes[name], err = HashArtifact(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	release := ReleaseEvidence{
		ReleaseTree: snapshot.CandidateTree, ConfigurationHash: hashes["configuration"],
		GeneratedArtifactHash: hashes["generated"], ProvenanceHash: hashes["provenance"],
		PublicationBoundaryHash: hashes["boundary"], PublicationState: PublicationStateSealed,
		EvidenceFreshnessHash: hashes["freshness"], EvidenceFreshnessState: EvidenceFreshnessCurrent,
	}
	tx, err := NewTransaction(Start{
		LineageID: "release-lineage", Mode: ModeOrdinary4R, Generation: 1,
		Snapshot: snapshot, PolicyHash: hashes["policy"],
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := AuthoritativeStore(context.Background(), repo, "release-lineage")
	if err != nil {
		t.Fatal(err)
	}
	_ = tx.StartReview()
	revision, err := store.Append("", Record{Operation: "review/start", Transaction: *tx})
	if err != nil {
		t.Fatal(err)
	}
	ledger, _ := CanonicalLedger([]Finding{})
	_ = tx.FreezeFindings([]Finding{}, ledger, hashes["ledger"])
	revision, err = store.Append(revision, Record{Operation: "review/freeze-findings", Transaction: *tx})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = tx.ClassifyEvidence([]FindingEvidence{})
	revision, err = store.Append(revision, Record{Operation: "review/classify", Transaction: *tx})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.BindReleaseEvidence(release); err != nil {
		t.Fatal(err)
	}
	revision, err = store.Append(revision, Record{Operation: "review/bind-release-evidence", Transaction: *tx})
	if err != nil {
		t.Fatal(err)
	}
	_ = tx.BeginFinalVerification()
	revision, err = store.Append(revision, Record{Operation: "review/begin-final-verification", Transaction: *tx})
	if err != nil {
		t.Fatal(err)
	}
	_ = tx.CompleteFinalVerification(hashes["evidence"], true)
	revision, err = store.Append(revision, Record{Operation: "review/complete-final-verification", Transaction: *tx})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := tx.Receipt()
	if err != nil {
		t.Fatal(err)
	}
	request := GateRequest{
		Schema: GateRequestSchema, Gate: GateRelease,
		Target:         Target{Kind: TargetExactRevision, Revision: releaseCommit},
		StoreRevision:  revision,
		PolicyArtifact: paths["policy"], LedgerArtifact: paths["ledger"], EvidenceArtifact: paths["evidence"],
		Release: &ReleaseRequest{
			Revision: releaseCommit, ConfigurationArtifact: paths["configuration"],
			GeneratedArtifact: paths["generated"], ProvenanceArtifact: paths["provenance"],
			PublicationBoundaryArtifact: paths["boundary"], PublicationState: PublicationStateSealed,
			EvidenceFreshnessArtifact: paths["freshness"], EvidenceFreshnessState: EvidenceFreshnessCurrent,
		},
	}
	bindGateRequestToStore(t, &request, store)
	if got := EvaluateNativeGate(context.Background(), repo, receipt, request); got.Result != GateAllow {
		t.Fatalf("EvaluateNativeGate(exact release) = %#v", got)
	}

	request.Release = nil
	if got := EvaluateNativeGate(context.Background(), repo, receipt, request); got.Result != GateInvalidated {
		t.Fatalf("EvaluateNativeGate(generic release) = %#v", got)
	}
	request.Release = &ReleaseRequest{
		Revision: releaseCommit, ConfigurationArtifact: paths["configuration"],
		GeneratedArtifact: paths["generated"], ProvenanceArtifact: filepath.Join(dir, "missing-provenance"),
		PublicationBoundaryArtifact: paths["boundary"], PublicationState: PublicationStateSealed,
		EvidenceFreshnessArtifact: paths["freshness"], EvidenceFreshnessState: EvidenceFreshnessCurrent,
	}
	if got := EvaluateNativeGate(context.Background(), repo, receipt, request); got.Result != GateInvalidated {
		t.Fatalf("EvaluateNativeGate(missing provenance) = %#v", got)
	}
}

func TestNativeGateRejectsHistoricalTargetAfterHeadAdvances(t *testing.T) {
	repo := initSnapshotRepo(t)
	transaction, receipt, request := nativeGateFixture(t, repo, "lifecycle-head")
	store, err := AuthoritativeStore(context.Background(), repo, transaction.LineageID)
	if err != nil {
		t.Fatal(err)
	}
	appendApprovedStoreChain(t, store, transaction)
	bundle, err := store.ExportBundle()
	if err != nil {
		t.Fatal(err)
	}
	request.StoreRevision, request.GenesisRevision, request.ChainIdentity, request.BundleDigest = bundle.HeadRevision, bundle.GenesisRevision, bundle.ChainIdentity, bundle.BundleDigest
	request.Target = Target{Kind: TargetExactRevision, Revision: trimGit(gitSnapshot(t, repo, "rev-parse", "HEAD"))}
	writeSnapshotFile(t, repo, "tracked.txt", "newer candidate\n")
	gitSnapshot(t, repo, "add", "tracked.txt")
	gitSnapshot(t, repo, "commit", "-m", "newer")
	if got := EvaluateNativeGate(context.Background(), repo, receipt, request); got.Result == GateAllow {
		t.Fatal("historical caller-selected target authorized a newer lifecycle candidate")
	}
}

func TestNativePrePushGateAcceptsCommittedCurrentChangesReceipt(t *testing.T) {
	repo, receipt, request := approvedCurrentChangesGateFixture(t, "pre-push-current-changes")
	if got := EvaluateNativeGate(context.Background(), repo, receipt, request); got.Result != GateAllow {
		t.Fatalf("EvaluateNativeGate(pre-commit current changes) = %#v", got)
	}

	gitSnapshot(t, repo, "add", "tracked.txt")
	gitSnapshot(t, repo, "commit", "-m", "deliver reviewed current changes")
	request.Gate = GatePrePush
	if got := EvaluateNativeGate(context.Background(), repo, receipt, request); got.Result != GateAllow {
		t.Fatalf("EvaluateNativeGate(pre-push committed current changes) = %#v", got)
	}
}

func TestNativePrePushGateRejectsAdvancedEmptyCurrentChangesReceipt(t *testing.T) {
	repo, receipt, request := approvedEmptyCurrentChangesGateFixture(t, "pre-push-empty-current-changes")
	gitSnapshot(t, repo, "commit", "--allow-empty", "-m", "first empty delivery")
	gitSnapshot(t, repo, "commit", "--allow-empty", "-m", "advance empty delivery")
	request.Gate = GatePrePush

	if got := EvaluateNativeGate(context.Background(), repo, receipt, request); got.Result == GateAllow {
		t.Fatalf("EvaluateNativeGate(advanced empty current changes) = %#v, want rejection", got)
	}
}

func TestNativePrePushGateRejectsChangedOrAdvancedHead(t *testing.T) {
	tests := []struct {
		name    string
		lineage string
		advance func(t *testing.T, repo string)
		want    GateResult
	}{
		{
			name:    "changed head",
			lineage: "pre-push-changed",
			advance: func(t *testing.T, repo string) {
				t.Helper()
				writeSnapshotFile(t, repo, "tracked.txt", "altered delivery\n")
				gitSnapshot(t, repo, "add", "tracked.txt")
				gitSnapshot(t, repo, "commit", "-m", "alter reviewed delivery")
			},
			want: GateScopeChanged,
		},
		{
			name:    "advanced head",
			lineage: "pre-push-advanced",
			advance: func(t *testing.T, repo string) {
				t.Helper()
				gitSnapshot(t, repo, "commit", "--allow-empty", "-m", "advance reviewed delivery")
			},
			want: GateInvalidated,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, receipt, request := approvedCurrentChangesGateFixture(t, tt.lineage)
			gitSnapshot(t, repo, "add", "tracked.txt")
			gitSnapshot(t, repo, "commit", "-m", "deliver reviewed current changes")
			tt.advance(t, repo)
			request.Gate = GatePrePush

			if got := EvaluateNativeGate(context.Background(), repo, receipt, request); got.Result != tt.want {
				t.Fatalf("EvaluateNativeGate(%s) = %#v, want %q", tt.name, got, tt.want)
			}
		})
	}
}

func TestNativeGateUsesRetainedArtifactContentAndRejectsMismatch(t *testing.T) {
	repo := initSnapshotRepo(t)
	tx, receipt, request := nativeGateFixture(t, repo, "content-gate")
	store, err := AuthoritativeStore(context.Background(), repo, tx.LineageID)
	if err != nil {
		t.Fatal(err)
	}
	appendApprovedStoreChain(t, store, tx)
	bundle, err := store.ExportBundle()
	if err != nil {
		t.Fatal(err)
	}
	request.StoreRevision, request.GenesisRevision, request.ChainIdentity, request.BundleDigest = bundle.HeadRevision, bundle.GenesisRevision, bundle.ChainIdentity, bundle.BundleDigest
	policy, _ := os.ReadFile(request.PolicyArtifact)
	ledger, _ := os.ReadFile(request.LedgerArtifact)
	evidence, _ := os.ReadFile(request.EvidenceArtifact)
	request.PolicyArtifact, request.LedgerArtifact, request.EvidenceArtifact = "missing", "missing", "missing"
	request.PolicyContent, request.LedgerContent, request.EvidenceContent = string(policy), string(ledger), string(evidence)
	if got := EvaluateNativeGate(context.Background(), repo, receipt, request); got.Result != GateAllow {
		t.Fatalf("retained content gate = %#v", got)
	}
	request.LedgerContent = `{"schema":"gentle-ai.review-ledger/v1","findings":[{"id":"mismatch"}]}`
	if got := EvaluateNativeGate(context.Background(), repo, receipt, request); got.Result == GateAllow {
		t.Fatal("mismatched retained ledger content was accepted")
	}
}

func TestNativeGateRejectsForgedStandaloneTerminalHead(t *testing.T) {
	repo := initSnapshotRepo(t)
	tx, receipt, request := nativeGateFixture(t, repo, "forged-terminal-lineage")
	forged := Store{Dir: filepath.Join(t.TempDir(), "forged-store")}
	revision := writeStoreEvent(t, forged, Record{
		Operation:        "review/complete-final-verification",
		PreviousRevision: hash("f"),
		Transaction:      tx,
	})
	request.StoreDir = forged.Dir
	request.StoreRevision = revision
	request.GenesisRevision = hash("a")
	request.ChainIdentity = hash("b")
	request.BundleDigest = hash("c")

	evaluation := EvaluateNativeGate(context.Background(), repo, receipt, request)
	if evaluation.Result == GateAllow {
		t.Fatalf("forged standalone terminal HEAD authorized the gate: %#v", evaluation)
	}
}

func TestNativeGateCannotBeInfluencedByAlternateStore(t *testing.T) {
	repo := initSnapshotRepo(t)
	tx, receipt, request := nativeGateFixture(t, repo, "trusted-chain-lineage")
	authoritative := Store{Dir: repositoryLineageStoreDir(t, repo, tx.LineageID)}
	revision := appendApprovedStoreChain(t, authoritative, tx)

	alternateTx := approvedStoreTransaction(t, "alternate-lineage")
	alternate := Store{Dir: filepath.Join(t.TempDir(), "alternate-store")}
	writeStoreEvent(t, alternate, Record{
		Operation:        "review/complete-final-verification",
		PreviousRevision: hash("e"),
		Transaction:      alternateTx,
	})
	request.StoreDir = alternate.Dir
	request.StoreRevision = revision
	bindGateRequestToStore(t, &request, authoritative)

	evaluation := EvaluateNativeGate(context.Background(), repo, receipt, request)
	if evaluation.Result != GateAllow {
		t.Fatalf("alternate store influenced trusted repository validation: %#v", evaluation)
	}
}

func nativeGateFixture(t *testing.T, repo, lineage string) (Transaction, Receipt, GateRequest) {
	t.Helper()
	dir := t.TempDir()
	policyPath := filepath.Join(dir, "policy.md")
	ledgerPath := filepath.Join(dir, "ledger.json")
	evidencePath := filepath.Join(dir, "evidence.md")
	for path, content := range map[string]string{
		policyPath:   "bounded policy\n",
		ledgerPath:   CanonicalEmptyLedger,
		evidencePath: "verified\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := (SnapshotBuilder{Repo: repo}).Build(context.Background(), Target{Kind: TargetCurrentChanges, IntendedUntracked: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	policyHash, _ := HashArtifact(policyPath)
	ledgerHash, _ := HashArtifact(ledgerPath)
	evidenceHash, _ := HashArtifact(evidencePath)
	tx, err := NewTransaction(Start{LineageID: lineage, Mode: ModeOrdinary4R, Generation: 1, Snapshot: snapshot, PolicyHash: policyHash})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.StartReview(); err != nil {
		t.Fatal(err)
	}
	ledger, err := CanonicalLedger([]Finding{})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.FreezeFindings([]Finding{}, ledger, ledgerHash); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ClassifyEvidence([]FindingEvidence{}); err != nil {
		t.Fatal(err)
	}
	if err := tx.BeginFinalVerification(); err != nil {
		t.Fatal(err)
	}
	if err := tx.CompleteFinalVerification(evidenceHash, true); err != nil {
		t.Fatal(err)
	}
	receipt, err := tx.Receipt()
	if err != nil {
		t.Fatal(err)
	}
	return *tx, receipt, GateRequest{
		Schema:           GateRequestSchema,
		Gate:             GatePostApply,
		Target:           Target{Kind: TargetCurrentChanges, IntendedUntracked: []string{}},
		PolicyArtifact:   policyPath,
		LedgerArtifact:   ledgerPath,
		EvidenceArtifact: evidencePath,
	}
}

func approvedCurrentChangesGateFixture(t *testing.T, lineage string) (string, Receipt, GateRequest) {
	t.Helper()
	repo := initSnapshotRepo(t)
	writeSnapshotFile(t, repo, "tracked.txt", "reviewed delivery\n")
	transaction, receipt, request := nativeGateFixture(t, repo, lineage)
	store, err := AuthoritativeStore(context.Background(), repo, transaction.LineageID)
	if err != nil {
		t.Fatal(err)
	}
	appendApprovedStoreChain(t, store, transaction)
	bindGateRequestToStore(t, &request, store)
	request.Gate = GatePreCommit
	return repo, receipt, request
}

func approvedEmptyCurrentChangesGateFixture(t *testing.T, lineage string) (string, Receipt, GateRequest) {
	t.Helper()
	repo := initSnapshotRepo(t)
	transaction, receipt, request := nativeGateFixture(t, repo, lineage)
	store, err := AuthoritativeStore(context.Background(), repo, transaction.LineageID)
	if err != nil {
		t.Fatal(err)
	}
	appendApprovedStoreChain(t, store, transaction)
	bindGateRequestToStore(t, &request, store)
	request.Gate = GatePreCommit
	return repo, receipt, request
}

func appendApprovedStoreChain(t *testing.T, store Store, approved Transaction) string {
	t.Helper()
	reviewing := approved
	reviewing.State = StateReviewing
	reviewing.LedgerHash = ""
	reviewing.EvidenceHash = ""
	reviewing.Release = nil
	reviewing.Counters.FinalVerifications = 0
	revision, err := store.Append("", Record{Operation: "review/start", Transaction: reviewing})
	if err != nil {
		t.Fatal(err)
	}
	frozen := reviewing
	ledger, err := CanonicalLedger([]Finding{})
	if err != nil {
		t.Fatal(err)
	}
	if err := frozen.FreezeFindings([]Finding{}, ledger, approved.LedgerHash); err != nil {
		t.Fatal(err)
	}
	revision, err = store.Append(revision, Record{Operation: "review/freeze-findings", Transaction: frozen})
	if err != nil {
		t.Fatal(err)
	}
	classified := frozen
	if _, err := classified.ClassifyEvidence([]FindingEvidence{}); err != nil {
		t.Fatal(err)
	}
	revision, err = store.Append(revision, Record{Operation: "review/classify", Transaction: classified})
	if err != nil {
		t.Fatal(err)
	}
	verifying := classified
	if approved.Release != nil {
		bound := classified
		bound.Release = cloneReleaseEvidence(approved.Release)
		revision, err = store.Append(revision, Record{Operation: "review/bind-release-evidence", Transaction: bound})
		if err != nil {
			t.Fatal(err)
		}
		verifying = bound
	}
	if err := verifying.BeginFinalVerification(); err != nil {
		t.Fatal(err)
	}
	revision, err = store.Append(revision, Record{Operation: "review/begin-final-verification", Transaction: verifying})
	if err != nil {
		t.Fatal(err)
	}
	revision, err = store.Append(revision, Record{Operation: "review/complete-final-verification", Transaction: approved})
	if err != nil {
		t.Fatal(err)
	}
	return revision
}

func repositoryLineageStoreDir(t *testing.T, repo, lineage string) string {
	t.Helper()
	commonDir := trimGit(gitSnapshot(t, repo, "rev-parse", "--path-format=absolute", "--git-common-dir"))
	return filepath.Join(commonDir, "gentle-ai", "review-transactions", "v1", lineage)
}

func trimGit(value string) string {
	for len(value) > 0 && (value[len(value)-1] == '\n' || value[len(value)-1] == '\r') {
		value = value[:len(value)-1]
	}
	return value
}

func bindGateRequestToStore(t *testing.T, request *GateRequest, store Store) {
	t.Helper()
	bundle, err := store.ExportBundle()
	if err != nil {
		t.Fatalf("ExportBundle() error = %v", err)
	}
	request.StoreRevision = bundle.HeadRevision
	request.GenesisRevision = bundle.GenesisRevision
	request.ChainIdentity = bundle.ChainIdentity
	request.BundleDigest = bundle.BundleDigest
}
