package sddstatus

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/gentleman-programming/gentle-ai/internal/reviewtransaction"
)

func readSpecCounts(paths []string) (SpecCounts, error) {
	contents := make([]string, 0, len(paths))
	for _, path := range paths {
		content, err := os.ReadFile(path)
		if err != nil {
			return SpecCounts{}, err
		}
		contents = append(contents, string(content))
	}
	return countSpecRequirementsAndScenarios(contents), nil
}

func readVerifyResult(path string, counts SpecCounts) (verifyResultEvaluation, error) {
	if path == "" {
		return verifyResultEvaluation{Reason: "verify result is missing"}, nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return verifyResultEvaluation{}, err
	}
	return parseVerifyResult(string(content), counts), nil
}

func readText(path string) string {
	if path == "" {
		return ""
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(content)
}

func readReviewTransaction(path, content string) (*reviewtransaction.Transaction, string) {
	if path == "" && strings.TrimSpace(content) == "" {
		return nil, "bounded review transaction is missing"
	}
	payload := []byte(content)
	if path != "" {
		read, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Sprintf("bounded review transaction cannot be read: %v", err)
		}
		payload = read
	}
	transaction, err := reviewtransaction.ParseTransaction(payload)
	if err != nil {
		return nil, fmt.Sprintf("bounded review transaction is invalid: %v", err)
	}
	return &transaction, ""
}

func resolveBoundedRemediation(required bool, verify verifyResultEvaluation, transaction *reviewtransaction.Transaction, transactionReason, applyProgress string) RemediationState {
	if !required {
		return RemediationState{}
	}
	if transaction == nil {
		return RemediationState{Reason: fmt.Sprintf("verify evidence cannot enter remediation: %s; %s", verify.Reason, transactionReason)}
	}
	if transaction.State == reviewtransaction.StateEscalated {
		return RemediationState{Reason: "review transaction is escalated; remediation cannot reopen an exhausted lineage"}
	}
	if verify.EvidenceRevision == "" || transaction.FailedEvidenceRevision != verify.EvidenceRevision {
		return RemediationState{Reason: fmt.Sprintf("transaction failed evidence revision %q does not match failed evidence revision %q", transaction.FailedEvidenceRevision, verify.EvidenceRevision)}
	}

	fixBatch := transaction.Counters.FixBatches
	switch transaction.Mode {
	case reviewtransaction.ModeOrdinary4R:
		if fixBatch != 1 {
			return RemediationState{Reason: "ordinary remediation requires its single persisted fix batch"}
		}
	case reviewtransaction.ModeJudgmentDay:
		fixBatch = transaction.Counters.FixRounds
		if fixBatch < 1 || fixBatch > 2 {
			return RemediationState{Reason: "Judgment Day remediation requires a persisted fix round within its two-round budget"}
		}
	default:
		return RemediationState{Reason: "unsupported remediation transaction mode"}
	}

	state := RemediationState{
		FailedEvidenceRevision: verify.EvidenceRevision,
		LineageID:              transaction.LineageID,
		Generation:             transaction.Generation,
		FixBatch:               fixBatch,
	}
	binding := RemediationBinding{LineageID: state.LineageID, Generation: state.Generation, FixBatch: state.FixBatch}
	evaluation := parseRemediationResult(applyProgress, verify.EvidenceRevision, binding)
	switch transaction.State {
	case reviewtransaction.StateFixing:
		state.Required = true
		state.Reason = fmt.Sprintf("verify evidence requires bounded remediation for %s: %s", verify.EvidenceRevision, verify.Reason)
	case reviewtransaction.StateFixValidating:
		state.Reason = "fix evidence exists but scoped fix-delta validation is still pending"
	case reviewtransaction.StateReadyFinalVerification:
		state.Complete = evaluation.Complete
		state.Required = !evaluation.Complete
		if !evaluation.Complete {
			state.Reason = "scoped fix validation passed but concrete remediation evidence is missing, stale, or not transaction-bound"
		}
	default:
		state.Reason = fmt.Sprintf("transaction state %q does not permit remediation", transaction.State)
	}
	return state
}

func applyReviewGate(
	status *Status,
	repo string,
	receiptPath, bundlePath, requestPath, transactionPath, policyPath, ledgerPath, verifyPath string,
	receiptContent, bundleContent, requestContent, transactionContent, policyContent, ledgerContent, verifyContent string,
) {
	if status.Dependencies.Verify != DependencyAllDone || !status.TaskProgress.AllComplete {
		return
	}
	receiptPayload, ok := readReviewArtifact(receiptPath, receiptContent)
	if !ok {
		blockReviewGate(status, reviewtransaction.GateInvalidated, "approved review receipt is missing")
		return
	}
	receipt, err := reviewtransaction.ParseReceipt(receiptPayload)
	if err != nil {
		blockReviewGate(status, reviewtransaction.GateInvalidated, fmt.Sprintf("review receipt is invalid or non-terminal: %v", err))
		return
	}
	bundlePayload, ok := readReviewArtifact(bundlePath, bundleContent)
	if !ok {
		blockReviewGate(status, reviewtransaction.GateInvalidated, "portable review chain bundle is missing")
		return
	}
	bundle, err := reviewtransaction.ParseChainBundle(bundlePayload)
	if err != nil || bundle.TerminalReceipt == nil || !reflect.DeepEqual(*bundle.TerminalReceipt, receipt) {
		blockReviewGate(status, reviewtransaction.GateInvalidated, "portable review chain bundle is invalid or does not bind the terminal receipt")
		return
	}
	transactionPayload, ok := readReviewArtifact(transactionPath, transactionContent)
	if !ok {
		blockReviewGate(status, reviewtransaction.GateInvalidated, "non-authoritative bounded review transaction mirror is missing")
		return
	}
	transaction, err := reviewtransaction.ParseTransaction(transactionPayload)
	if err != nil {
		blockReviewGate(status, reviewtransaction.GateInvalidated, fmt.Sprintf("bounded review transaction is invalid: %v", err))
		return
	}
	transactionReceipt, err := transaction.Receipt()
	if err != nil || !reflect.DeepEqual(transactionReceipt, receipt) {
		blockReviewGate(status, reviewtransaction.GateInvalidated, "receipt does not match the persisted terminal review transaction")
		return
	}
	if _, ok := readReviewArtifact(ledgerPath, ledgerContent); !ok {
		blockReviewGate(status, reviewtransaction.GateInvalidated, "persisted frozen review ledger is missing")
		return
	}
	if _, ok := readReviewArtifact(policyPath, policyContent); !ok {
		blockReviewGate(status, reviewtransaction.GateInvalidated, "persisted review policy is missing")
		return
	}
	if _, ok := readReviewArtifact(verifyPath, verifyContent); !ok {
		blockReviewGate(status, reviewtransaction.GateInvalidated, "current structured verify evidence is missing")
		return
	}
	requestPayload, ok := readReviewArtifact(requestPath, requestContent)
	if !ok {
		blockReviewGate(status, reviewtransaction.GateInvalidated, "review gate request is missing")
		return
	}
	request, err := reviewtransaction.ParseGateRequest(requestPayload)
	if err != nil {
		blockReviewGate(status, reviewtransaction.GateInvalidated, fmt.Sprintf("review gate request is invalid: %v", err))
		return
	}
	// Engram retains artifact bytes even after an OpenSpec workspace has been
	// cleaned. Feed those bytes to the native gate instead of treating virtual
	// topic names as filesystem paths.
	if ledgerPath == "" {
		request.LedgerContent = ledgerContent
	}
	if policyPath == "" {
		request.PolicyContent = policyContent
	}
	if verifyPath == "" {
		request.EvidenceContent = verifyContent
	}
	if request.StoreRevision != bundle.HeadRevision || request.GenesisRevision != bundle.GenesisRevision || request.ChainIdentity != bundle.ChainIdentity || request.BundleDigest != bundle.BundleDigest {
		blockReviewGate(status, reviewtransaction.GateInvalidated, "gate request does not bind the portable review chain bundle identity")
		return
	}
	if request.Gate != reviewtransaction.GatePostApply {
		blockReviewGate(status, reviewtransaction.GateInvalidated, "archive readiness requires a post-apply gate request; explicit maintainer action is required")
		return
	}
	request.Target.Kind = reviewtransaction.TargetCurrentChanges
	request.Target.BaseRef = ""
	request.Target.Revision = ""
	if transaction.Snapshot.IntendedUntracked == nil {
		blockReviewGate(status, reviewtransaction.GateInvalidated, "persisted transaction lacks an explicit intended-untracked target")
		return
	}
	request.Target.IntendedUntracked = append([]string{}, transaction.Snapshot.IntendedUntracked...)
	request.Target.LedgerIDs = nil
	if transactionPath != "" {
		reviewsDir := filepath.Dir(transactionPath)
		request.LedgerArtifact = ledgerPath
		request.EvidenceArtifact = verifyPath
		if request.FixDeltaArtifact != "" && filepath.Clean(request.FixDeltaArtifact) != filepath.Join(reviewsDir, "fix-delta.json") {
			blockReviewGate(status, reviewtransaction.GateInvalidated, "gate request fix delta is outside the persisted SDD review artifacts")
			return
		}
	}
	store, err := reviewtransaction.AuthoritativeStore(context.Background(), repo, receipt.LineageID)
	if err != nil {
		blockReviewGate(status, reviewtransaction.GateInvalidated, fmt.Sprintf("authoritative repository review store cannot be derived: %v", err))
		return
	}
	chain, err := store.LoadChain()
	if err != nil || chain.HeadRevision != request.StoreRevision || !reflect.DeepEqual(chain.Records[len(chain.Records)-1].Transaction, transaction) {
		blockReviewGate(status, reviewtransaction.GateInvalidated, "persisted transaction is stale or does not match the authoritative CAS revision")
		return
	}
	evaluation := reviewtransaction.EvaluateNativeGate(context.Background(), repo, receipt, request)
	result := evaluation.Result
	switch result {
	case reviewtransaction.GateAllow:
		status.ReviewGate = &ReviewGateState{Result: result, Reason: "approved receipt exactly matches authoritative transaction, current repository, policy, ledger, fix delta, and verify evidence"}
	case reviewtransaction.GateScopeChanged:
		blockReviewGate(status, result, "review scope changed; maintainer must create an explicit new lineage without reusing this budget")
	case reviewtransaction.GateEscalated:
		blockReviewGate(status, result, "new external evidence or terminal transaction state escalated the receipt without reopening review")
	default:
		blockReviewGate(status, reviewtransaction.GateInvalidated, "review receipt was invalidated by content relationship, policy, ledger, evidence, or publication state; explicit maintainer action is required and no budget resets")
	}
}

func readReviewArtifact(path, content string) ([]byte, bool) {
	if path != "" {
		payload, err := os.ReadFile(path)
		return payload, err == nil && len(strings.TrimSpace(string(payload))) > 0
	}
	if strings.TrimSpace(content) == "" {
		return nil, false
	}
	return []byte(content), true
}

func blockReviewGate(status *Status, result reviewtransaction.GateResult, reason string) {
	status.ReviewGate = &ReviewGateState{Result: result, Reason: reason}
	status.Dependencies.Archive = DependencyBlocked
	status.NextRecommended = "resolve-review"
	status.BlockedReasons = append(status.BlockedReasons, reason)
}
