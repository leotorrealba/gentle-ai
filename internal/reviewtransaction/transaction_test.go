package reviewtransaction

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOrdinaryTransactionIsOneBoundedNonIterativeFlow(t *testing.T) {
	tx := newTestTransaction(t, ModeOrdinary4R)
	if err := tx.StartReview(); err != nil {
		t.Fatalf("StartReview() error = %v", err)
	}
	if err := tx.FreezeFindings([]Finding{{ID: "R1-DET", Severity: "CRITICAL"}, {ID: "R2-INF", Severity: "CRITICAL"}}, hash("1")); err != nil {
		t.Fatalf("FreezeFindings() error = %v", err)
	}

	route, err := tx.ClassifyEvidence([]FindingEvidence{
		{FindingID: "R1-DET", Class: EvidenceDeterministic, Proof: "go test exited 1"},
		{FindingID: "R2-INF", Class: EvidenceInferential, Proof: "race requires interpretation"},
	})
	if err != nil {
		t.Fatalf("ClassifyEvidence() error = %v", err)
	}
	if len(route.RefuterClaims) != 1 || route.RefuterClaims[0].FindingID != "R2-INF" {
		t.Fatalf("RefuterClaims = %#v, want only inferential finding", route.RefuterClaims)
	}
	if got := tx.Outcomes["R1-DET"]; got != OutcomeCorroborated {
		t.Fatalf("deterministic outcome = %q, want corroborated", got)
	}

	if err := tx.ApplyRefuterOutcomes([]EvidenceResult{{FindingID: "R2-INF", Outcome: OutcomeRefuted, Proof: "locked counterexample"}}); err != nil {
		t.Fatalf("ApplyRefuterOutcomes() error = %v", err)
	}
	if tx.Counters.FullReviews != 1 || tx.Counters.RefuterBatches != 1 {
		t.Fatalf("review counters = %#v", tx.Counters)
	}
	if err := tx.BeginFix(hash("2")); err != nil {
		t.Fatalf("BeginFix() error = %v", err)
	}
	fixSnapshot := tx.Snapshot
	fixSnapshot.Kind = TargetFixDiff
	fixSnapshot.BaseTree = tx.InitialReviewTree
	fixSnapshot.CandidateTree = tree("c")
	fixSnapshot.LedgerIDs = []string{"R1-DET"}
	fixSnapshot.Identity = hash("3")
	if err := tx.CompleteFix(fixSnapshot, hash("4"), []string{"R1-DET"}); err != nil {
		t.Fatalf("CompleteFix() error = %v", err)
	}
	if err := tx.ValidateFixDelta([]string{"R1-DET"}, true); err != nil {
		t.Fatalf("ValidateFixDelta() error = %v", err)
	}
	if err := tx.ValidateFixDelta([]string{"R1-DET"}, true); err == nil {
		t.Fatal("ordinary transaction allowed a second scoped fix validation")
	}
	if err := tx.BeginFinalVerification(); err != nil {
		t.Fatalf("BeginFinalVerification() error = %v", err)
	}
	if err := tx.CompleteFinalVerification(hash("5"), true); err != nil {
		t.Fatalf("CompleteFinalVerification() error = %v", err)
	}
	if tx.State != StateApproved {
		t.Fatalf("State = %q, want approved", tx.State)
	}
	want := Counters{FullReviews: 1, RefuterBatches: 1, FixBatches: 1, ScopedFixValidations: 1, FinalVerifications: 1}
	if tx.Counters != want {
		t.Fatalf("Counters = %#v, want %#v", tx.Counters, want)
	}
	if err := tx.BeginFix(hash("6")); err == nil {
		t.Fatal("approved transaction reopened a fix batch")
	}
}

func TestOrdinaryScopedValidatorCanOnlyApproveOrEscalate(t *testing.T) {
	tx := ordinaryAtFixValidation(t)
	if err := tx.ValidateFixDelta([]string{"R1-DET"}, false); err != nil {
		t.Fatalf("ValidateFixDelta() error = %v", err)
	}
	if tx.State != StateEscalated {
		t.Fatalf("State = %q, want escalated", tx.State)
	}
	if err := tx.BeginFix(hash("9")); err == nil {
		t.Fatal("failed scoped validation triggered another ordinary fix")
	}
}

func TestOrdinaryScopedValidatorRecordsFixCausedDefectsAndEscalates(t *testing.T) {
	tx := ordinaryAtFixValidation(t)
	result := ScopedValidationResult{
		LedgerIDs: []string{"R1-DET"},
		Approved:  false,
		FixCausedFindings: []Finding{{
			ID:        "FIX-001",
			Lens:      "scoped-fix-validator",
			Location:  "internal/example.go:12",
			Severity:  "CRITICAL",
			Claim:     "the correction introduced a nil dereference",
			ProofRefs: []string{"go test ./internal/example: exit 1"},
		}},
	}

	if err := tx.ValidateFixDeltaResult(result); err != nil {
		t.Fatalf("ValidateFixDeltaResult() error = %v", err)
	}
	if tx.State != StateEscalated {
		t.Fatalf("State = %q, want escalated", tx.State)
	}
	if len(tx.FixCausedFindings) != 1 || tx.FixCausedFindings[0].ID != "FIX-001" {
		t.Fatalf("FixCausedFindings = %#v", tx.FixCausedFindings)
	}
	if err := tx.BeginFix(hash("9")); err == nil {
		t.Fatal("fix-caused defect started another ordinary correction")
	}
}

func TestInformationalFixCausedFindingCannotEscalate(t *testing.T) {
	tx := ordinaryAtFixValidation(t)
	result := ScopedValidationResult{
		LedgerIDs: []string{"R1-DET"},
		Approved:  false,
		FixCausedFindings: []Finding{{
			ID: "FIX-I01", Lens: "scoped-fix-validator", Location: "internal/example.go:12",
			Severity: "WARNING", Claim: "optional naming improvement",
			ProofRefs: []string{"internal/example.go:12 remains functionally correct"},
		}},
	}
	if err := tx.ValidateFixDeltaResult(result); err != nil {
		t.Fatalf("ValidateFixDeltaResult() error = %v", err)
	}
	if tx.State != StateReadyFinalVerification {
		t.Fatalf("State = %q, want ready_final_verification", tx.State)
	}
}

func TestJudgmentDayCarriesEarlierSevereFixCausedFindingIntoNextCorrection(t *testing.T) {
	tx := newTestTransaction(t, ModeJudgmentDay)
	_ = tx.StartReview()
	_ = tx.RecordJudgeProofs([]JudgeProof{{JudgeID: "a", ExecutionHash: hash("1"), ResultHash: hash("2"), Blind: true, Confirmed: true}, {JudgeID: "b", ExecutionHash: hash("3"), ResultHash: hash("4"), Blind: true, Confirmed: true}}, hash("5"))
	_ = tx.FreezeFindings([]Finding{{ID: "JD-001", Severity: "CRITICAL"}}, hash("6"))
	_, _ = tx.ClassifyEvidence([]FindingEvidence{{FindingID: "JD-001", Class: EvidenceDeterministic, Proof: "reproduced"}})
	_ = tx.BeginFix(hash("7"))
	fix := tx.Snapshot
	fix.Kind, fix.BaseTree, fix.CandidateTree, fix.LedgerIDs, fix.Identity = TargetFixDiff, tx.FinalCandidateTree, tree("c"), []string{"JD-001"}, hash("8")
	_ = tx.CompleteFix(fix, hash("9"), []string{"JD-001"})
	if err := tx.ValidateFixDeltaResult(ScopedValidationResult{LedgerIDs: []string{"JD-001"}, FixCausedFindings: []Finding{{ID: "FIX-001", Lens: "scoped", Location: "x.go:1", Severity: "CRITICAL", Claim: "new defect", ProofRefs: []string{"test failure"}}}}); err != nil {
		t.Fatal(err)
	}
	if tx.State != StateFixRequired || !equalStrings(tx.FixFindingIDs, []string{"FIX-001", "JD-001"}) {
		t.Fatalf("severe earlier fix-caused finding was not correction-bound: state=%q ids=%v", tx.State, tx.FixFindingIDs)
	}
}

func TestFrozenLedgerFindingsHashDetectsTamperedFrozenFindings(t *testing.T) {
	tx := newTestTransaction(t, ModeOrdinary4R)
	_ = tx.StartReview()
	_ = tx.FreezeFindings([]Finding{{ID: "R1-001", Severity: "CRITICAL"}}, hash("1"))
	tx.Findings[0].Severity = "WARNING"
	if _, err := ParseTransaction(mustMarshalTransaction(t, *tx)); err == nil {
		t.Fatal("ParseTransaction() accepted frozen findings that no longer match their ledger binding")
	}
}

func TestCompleteFixDerivesDeltaIdentityInsteadOfTrustingCallerArtifact(t *testing.T) {
	tx := ordinaryAtFixValidation(t)
	if tx.FixDeltaHash != FixDeltaHashForSnapshot(tx.Snapshot) {
		t.Fatalf("FixDeltaHash = %q, want authoritative snapshot-derived identity %q", tx.FixDeltaHash, FixDeltaHashForSnapshot(tx.Snapshot))
	}
	if tx.FixDeltaHash == hash("4") {
		t.Fatal("arbitrary caller artifact hash satisfied fix-delta binding")
	}
}

func mustMarshalTransaction(t *testing.T, transaction Transaction) []byte {
	t.Helper()
	payload, err := json.Marshal(transaction)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestInsufficientEvidenceIsInconclusiveAndNeverAutoFixed(t *testing.T) {
	tx := newTestTransaction(t, ModeOrdinary4R)
	_ = tx.StartReview()
	_ = tx.FreezeFindings([]Finding{{ID: "R3-INS", Severity: "CRITICAL"}}, hash("1"))
	route, err := tx.ClassifyEvidence([]FindingEvidence{{FindingID: "R3-INS", Class: EvidenceInsufficient, Proof: "no observable behavior"}})
	if err != nil {
		t.Fatalf("ClassifyEvidence() error = %v", err)
	}
	if len(route.AutoFixFindingIDs) != 0 || tx.Outcomes["R3-INS"] != OutcomeInconclusive {
		t.Fatalf("route/outcomes = %#v / %#v", route, tx.Outcomes)
	}
	if tx.State != StateEscalated {
		t.Fatalf("State = %q, want escalated", tx.State)
	}
}

func TestMalformedRefuterBatchIsConsumedAndTerminal(t *testing.T) {
	tests := []struct {
		name    string
		results []EvidenceResult
	}{
		{name: "missing output", results: nil},
		{name: "incomplete output", results: []EvidenceResult{{FindingID: "R2-INF", Outcome: OutcomeCorroborated, Proof: "independent trace"}}},
		{name: "malformed output", results: []EvidenceResult{
			{FindingID: "R2-INF", Outcome: OutcomeCorroborated, Proof: "independent trace"},
			{FindingID: "R3-INF", Outcome: "unknown", Proof: "independent trace"},
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tx := newTestTransaction(t, ModeOrdinary4R)
			_ = tx.StartReview()
			_ = tx.FreezeFindings([]Finding{
				{ID: "R2-INF", Severity: "CRITICAL"},
				{ID: "R3-INF", Severity: "BLOCKER"},
			}, hash("1"))
			_, _ = tx.ClassifyEvidence([]FindingEvidence{
				{FindingID: "R2-INF", Class: EvidenceInferential, Proof: "race requires interpretation"},
				{FindingID: "R3-INF", Class: EvidenceInferential, Proof: "ordering requires interpretation"},
			})

			if err := tx.ApplyRefuterOutcomes(tt.results); err == nil {
				t.Fatal("ApplyRefuterOutcomes() accepted malformed terminal output")
			}
			if tx.Counters.RefuterBatches != 1 || tx.State != StateEscalated || len(tx.PendingRefuterIDs) != 0 {
				t.Fatalf("terminal state = %q counters=%#v pending=%v", tx.State, tx.Counters, tx.PendingRefuterIDs)
			}
			for _, id := range []string{"R2-INF", "R3-INF"} {
				if tx.Outcomes[id] != OutcomeInconclusive {
					t.Fatalf("Outcomes[%s] = %q, want inconclusive", id, tx.Outcomes[id])
				}
			}
			if err := tx.ApplyRefuterOutcomes([]EvidenceResult{
				{FindingID: "R2-INF", Outcome: OutcomeCorroborated, Proof: "late retry"},
				{FindingID: "R3-INF", Outcome: OutcomeCorroborated, Proof: "late retry"},
			}); err == nil {
				t.Fatal("terminal malformed batch remained retryable")
			}
		})
	}
}

func TestOnlySevereFindingsEnterEvidenceRouting(t *testing.T) {
	tx := newTestTransaction(t, ModeOrdinary4R)
	_ = tx.StartReview()
	findings := []Finding{
		{ID: "R1-BLOCKER", Severity: "BLOCKER"},
		{ID: "R2-CRITICAL", Severity: "CRITICAL"},
		{ID: "R3-WARNING", Severity: "WARNING"},
		{ID: "R4-SUGGESTION", Severity: "SUGGESTION"},
	}
	if err := tx.FreezeFindings(findings, hash("1")); err != nil {
		t.Fatalf("FreezeFindings() error = %v", err)
	}
	route, err := tx.ClassifyEvidence([]FindingEvidence{
		{FindingID: "R1-BLOCKER", Class: EvidenceDeterministic, Proof: "failing security test"},
		{FindingID: "R2-CRITICAL", Class: EvidenceInferential, Proof: "concurrency trace"},
	})
	if err != nil {
		t.Fatalf("ClassifyEvidence() error = %v", err)
	}
	if len(route.AutoFixFindingIDs) != 1 || route.AutoFixFindingIDs[0] != "R1-BLOCKER" || len(route.RefuterClaims) != 1 || route.RefuterClaims[0].FindingID != "R2-CRITICAL" {
		t.Fatalf("route = %#v", route)
	}
	for _, id := range []string{"R3-WARNING", "R4-SUGGESTION"} {
		if tx.Outcomes[id] != OutcomeInfo {
			t.Fatalf("Outcomes[%s] = %q, want info", id, tx.Outcomes[id])
		}
	}
}

func TestJudgmentDayRequiresTwoDistinctBlindJudgeProofs(t *testing.T) {
	proofA := JudgeProof{JudgeID: "judge-a", ExecutionHash: hash("1"), ResultHash: hash("2"), Blind: true, Confirmed: true}
	proofB := JudgeProof{JudgeID: "judge-b", ExecutionHash: hash("3"), ResultHash: hash("4"), Blind: true, Confirmed: true}

	tests := []struct {
		name   string
		proofs []JudgeProof
	}{
		{name: "zero judges", proofs: nil},
		{name: "one judge", proofs: []JudgeProof{proofA}},
		{name: "duplicate execution", proofs: []JudgeProof{proofA, {JudgeID: "judge-b", ExecutionHash: proofA.ExecutionHash, ResultHash: hash("4"), Blind: true, Confirmed: true}}},
		{name: "duplicate result", proofs: []JudgeProof{proofA, {JudgeID: "judge-b", ExecutionHash: hash("3"), ResultHash: proofA.ResultHash, Blind: true, Confirmed: true}}},
		{name: "not blind", proofs: []JudgeProof{proofA, {JudgeID: "judge-b", ExecutionHash: hash("3"), ResultHash: hash("4"), Confirmed: true}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tx := newTestTransaction(t, ModeJudgmentDay)
			_ = tx.StartReview()
			if len(tt.proofs) > 0 {
				if err := tx.RecordJudgeProofs(tt.proofs, hash("5")); err == nil {
					t.Fatal("RecordJudgeProofs() accepted incomplete or duplicate judge proof")
				}
			}
			if err := tx.FreezeFindings([]Finding{{ID: "JD-001", Severity: "CRITICAL"}}, hash("6")); err == nil {
				t.Fatal("FreezeFindings() accepted Judgment Day without two confirmed judges")
			}
		})
	}

	tx := newTestTransaction(t, ModeJudgmentDay)
	_ = tx.StartReview()
	if err := tx.RecordJudgeProofs([]JudgeProof{proofA, proofB}, hash("5")); err != nil {
		t.Fatalf("RecordJudgeProofs(valid) error = %v", err)
	}
	if err := tx.FreezeFindings([]Finding{{ID: "JD-001", Severity: "CRITICAL"}}, hash("6")); err != nil {
		t.Fatalf("FreezeFindings(valid proof) error = %v", err)
	}
	if tx.Counters.JudgeExecutions != 2 || tx.JudgeProofHash == "" {
		t.Fatalf("judge proof = %q counters=%#v", tx.JudgeProofHash, tx.Counters)
	}
}

func TestJudgmentDayHasExactlyTwoFixAndScopedRejudgmentRounds(t *testing.T) {
	tx := newTestTransaction(t, ModeJudgmentDay)
	_ = tx.StartReview()
	if err := tx.RecordJudgeProofs([]JudgeProof{
		{JudgeID: "judge-a", ExecutionHash: hash("a"), ResultHash: hash("b"), Blind: true, Confirmed: true},
		{JudgeID: "judge-b", ExecutionHash: hash("c"), ResultHash: hash("d"), Blind: true, Confirmed: true},
	}, hash("e")); err != nil {
		t.Fatal(err)
	}
	_ = tx.FreezeFindings([]Finding{{ID: "JD-001", Severity: "CRITICAL"}}, hash("1"))
	if _, err := tx.ClassifyEvidence([]FindingEvidence{{FindingID: "JD-001", Class: EvidenceDeterministic, Proof: "confirmed by both judges"}}); err != nil {
		t.Fatalf("ClassifyEvidence() error = %v", err)
	}
	for round := 1; round <= 2; round++ {
		if err := tx.BeginFix(hash(string(rune('1' + round)))); err != nil {
			t.Fatalf("BeginFix(round %d) error = %v", round, err)
		}
		fix := tx.Snapshot
		fix.Kind = TargetFixDiff
		fix.BaseTree = tx.FinalCandidateTree
		fix.CandidateTree = tree(string(rune('c' + round)))
		fix.LedgerIDs = []string{"JD-001"}
		fix.Identity = hash(string(rune('5' + round)))
		if err := tx.CompleteFix(fix, hash(string(rune('7'+round))), []string{"JD-001"}); err != nil {
			t.Fatalf("CompleteFix(round %d) error = %v", round, err)
		}
		if err := tx.ValidateFixDelta([]string{"JD-001"}, false); err != nil {
			t.Fatalf("ValidateFixDelta(round %d) error = %v", round, err)
		}
	}
	if tx.Counters.FixRounds != 2 || tx.Counters.ScopedRejudgments != 2 || tx.State != StateEscalated {
		t.Fatalf("final judgment-day state = %q counters=%#v", tx.State, tx.Counters)
	}
	if err := tx.BeginFix(hash("f")); err == nil {
		t.Fatal("Judgment Day allowed a third fix round")
	}
}

func ordinaryAtFixValidation(t *testing.T) *Transaction {
	t.Helper()
	tx := newTestTransaction(t, ModeOrdinary4R)
	_ = tx.StartReview()
	_ = tx.FreezeFindings([]Finding{{ID: "R1-DET", Severity: "CRITICAL"}}, hash("1"))
	_, _ = tx.ClassifyEvidence([]FindingEvidence{{FindingID: "R1-DET", Class: EvidenceDeterministic, Proof: "failing test"}})
	_ = tx.BeginFix(hash("2"))
	fix := tx.Snapshot
	fix.Kind = TargetFixDiff
	fix.BaseTree = tx.InitialReviewTree
	fix.CandidateTree = tree("c")
	fix.LedgerIDs = []string{"R1-DET"}
	fix.Identity = hash("3")
	if err := tx.CompleteFix(fix, hash("4"), []string{"R1-DET"}); err != nil {
		t.Fatalf("CompleteFix() error = %v", err)
	}
	return tx
}

func newTestTransaction(t *testing.T, mode Mode) *Transaction {
	t.Helper()
	tx, err := NewTransaction(Start{
		LineageID: "lineage-1", Mode: mode, Generation: 1,
		Snapshot: Snapshot{
			Kind: TargetCurrentChanges, BaseTree: tree("a"), CandidateTree: tree("b"),
			PathsDigest: hash("a"), IntendedUntracked: []string{},
			IntendedUntrackedProof: hash("b"), Paths: []string{"internal/example.go"}, Identity: hash("c"),
		},
		PolicyHash: hash("d"),
	})
	if err != nil {
		t.Fatalf("NewTransaction() error = %v", err)
	}
	return tx
}

func hash(char string) string {
	return "sha256:" + strings.Repeat(char, 64)
}

func tree(char string) string {
	return strings.Repeat(char, 40)
}
