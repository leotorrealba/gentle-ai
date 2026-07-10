package reviewtransaction

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
)

const GateRequestSchema = "gentle-ai.review-gate-request/v1"

type GateRequest struct {
	Schema           string                      `json:"schema"`
	Gate             GateKind                    `json:"gate"`
	Target           Target                      `json:"target"`
	StoreDir         string                      `json:"store_dir,omitempty"`
	StoreRevision    string                      `json:"store_revision"`
	GenesisRevision  string                      `json:"genesis_revision"`
	ChainIdentity    string                      `json:"chain_identity"`
	BundleDigest     string                      `json:"bundle_digest"`
	PolicyArtifact   string                      `json:"policy_artifact"`
	PolicyContent    string                      `json:"policy_content,omitempty"`
	LedgerArtifact   string                      `json:"ledger_artifact"`
	LedgerContent    string                      `json:"ledger_content,omitempty"`
	FixDeltaArtifact string                      `json:"fix_delta_artifact,omitempty"`
	EvidenceArtifact string                      `json:"evidence_artifact"`
	EvidenceContent  string                      `json:"evidence_content,omitempty"`
	ExternalEvidence ExternalEvidenceDisposition `json:"external_evidence,omitempty"`
	Release          *ReleaseRequest             `json:"release,omitempty"`
}

type ReleaseRequest struct {
	Revision                    string                 `json:"revision"`
	ConfigurationArtifact       string                 `json:"configuration_artifact"`
	GeneratedArtifact           string                 `json:"generated_artifact"`
	ProvenanceArtifact          string                 `json:"provenance_artifact"`
	PublicationBoundaryArtifact string                 `json:"publication_boundary_artifact"`
	PublicationState            PublicationState       `json:"publication_state"`
	EvidenceFreshnessArtifact   string                 `json:"evidence_freshness_artifact"`
	EvidenceFreshnessState      EvidenceFreshnessState `json:"evidence_freshness_state"`
}

type NativeGateEvaluation struct {
	Result  GateResult
	Reason  string
	Context GateContext
}

func ParseGateRequest(payload []byte) (GateRequest, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var request GateRequest
	if err := decoder.Decode(&request); err != nil {
		return GateRequest{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return GateRequest{}, errors.New("multiple JSON values in review gate request")
	}
	if err := validateGateRequest(request); err != nil {
		return GateRequest{}, err
	}
	return request, nil
}

func HashArtifact(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("artifact path is required")
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func EvaluateNativeGate(ctx context.Context, repo string, receipt Receipt, request GateRequest) NativeGateEvaluation {
	invalid := func(reason string) NativeGateEvaluation {
		return NativeGateEvaluation{Result: GateInvalidated, Reason: reason}
	}
	if err := validateReceiptStructure(receipt); err != nil {
		return invalid("review receipt is invalid: " + err.Error())
	}
	if err := validateGateRequest(request); err != nil {
		return invalid("review gate request is invalid: " + err.Error())
	}
	store, err := AuthoritativeStore(ctx, repo, receipt.LineageID)
	if err != nil {
		return invalid("authoritative review store cannot be derived: " + err.Error())
	}
	chain, err := store.LoadChain()
	if err != nil {
		return invalid("authoritative review transaction cannot be loaded: " + err.Error())
	}
	record := chain.Records[len(chain.Records)-1]
	revision := chain.HeadRevision
	bundle, err := store.ExportBundle()
	if err != nil {
		return invalid("authoritative review chain bundle identity cannot be derived: " + err.Error())
	}
	if revision != request.StoreRevision || chain.GenesisRevision != request.GenesisRevision || chain.Identity != request.ChainIdentity || bundle.BundleDigest != request.BundleDigest {
		return invalid("authoritative review transaction chain identity is stale")
	}
	authoritativeReceipt, err := record.Transaction.Receipt()
	if err != nil {
		return invalid("authoritative review transaction is non-terminal: " + err.Error())
	}
	if !reflect.DeepEqual(authoritativeReceipt, receipt) {
		return invalid("receipt does not match the authoritative transaction revision")
	}

	lifecycleTarget, err := lifecycleTargetForGate(ctx, repo, request)
	if err != nil {
		return invalid("current lifecycle target cannot be derived: " + err.Error())
	}
	snapshot, err := (SnapshotBuilder{Repo: repo}).Build(ctx, lifecycleTarget)
	if err != nil {
		return invalid("current repository target cannot be derived: " + err.Error())
	}
	policyHash, err := hashArtifactSource(request.PolicyArtifact, request.PolicyContent)
	if err != nil {
		return invalid("policy artifact cannot be hashed: " + err.Error())
	}
	ledgerHash, ledgerFindingsHash, err := hashLedgerArtifactSource(request.LedgerArtifact, request.LedgerContent)
	if err != nil {
		return invalid("frozen ledger cannot be validated: " + err.Error())
	}
	if ledgerFindingsHash != record.Transaction.LedgerFindingsHash {
		return invalid("frozen ledger findings do not match the authoritative transaction")
	}
	evidenceHash, err := hashArtifactSource(request.EvidenceArtifact, request.EvidenceContent)
	if err != nil {
		return invalid("verify evidence cannot be hashed: " + err.Error())
	}
	fixDeltaHash := record.Transaction.FixDeltaHash
	if record.Transaction.Snapshot.Kind == TargetFixDiff {
		fixDeltaHash = FixDeltaHashForSnapshot(record.Transaction.Snapshot)
	}

	gateContext := GateContext{
		Gate: request.Gate, LineageID: record.Transaction.LineageID, Generation: record.Transaction.Generation,
		StoreRevision: revision, GenesisRevision: chain.GenesisRevision, ChainIdentity: chain.Identity,
		BundleDigest: bundle.BundleDigest,
		BaseTree:     snapshot.BaseTree, CandidateTree: snapshot.CandidateTree, PathsDigest: snapshot.PathsDigest,
		FixDeltaHash: fixDeltaHash, PolicyHash: policyHash, LedgerHash: ledgerHash, EvidenceHash: evidenceHash,
		BaseRelationshipValid: snapshot.BaseTree == receipt.BaseTree,
		ExternalEvidence:      request.ExternalEvidence,
	}
	if request.Gate == GateRelease {
		release, err := deriveReleaseEvidence(ctx, repo, request.Release)
		if err != nil {
			return invalid("release boundary cannot be derived: " + err.Error())
		}
		if release.ReleaseTree != snapshot.CandidateTree {
			return invalid("immutable release tree does not match the current candidate tree")
		}
		gateContext.Release = &release
	}
	result := validateDerivedGate(receipt, gateContext)
	return NativeGateEvaluation{Result: result, Reason: nativeGateReason(result), Context: gateContext}
}

// lifecycleTargetForGate deliberately derives the candidate from the event's
// live repository context. A caller-selected historical commit/range may
// describe review evidence, but it must never authorize a newer HEAD.
func lifecycleTargetForGate(ctx context.Context, repo string, request GateRequest) (Target, error) {
	switch request.Gate {
	case GatePostApply, GatePreCommit, GatePrePush:
		intended := request.Target.IntendedUntracked
		if intended == nil {
			intended = []string{}
		}
		return Target{Kind: TargetCurrentChanges, IntendedUntracked: intended}, nil
	case GatePrePR:
		if request.Target.Kind != TargetBaseDiff || strings.TrimSpace(request.Target.BaseRef) == "" {
			return Target{}, errors.New("pre-PR validation requires an explicit base-diff target")
		}
		return Target{Kind: TargetBaseDiff, BaseRef: request.Target.BaseRef}, nil
	case GateRelease:
		if request.Target.Kind != TargetExactRevision || request.Release == nil {
			return Target{}, errors.New("release validation requires an exact current release revision")
		}
		head, err := runGit(ctx, repo, nil, nil, "rev-parse", "HEAD")
		if err != nil {
			return Target{}, err
		}
		if strings.TrimSpace(request.Release.Revision) != strings.TrimSpace(string(head)) || request.Target.Revision != request.Release.Revision {
			return Target{}, errors.New("release revision is not the current HEAD")
		}
		return Target{Kind: TargetExactRevision, Revision: request.Release.Revision}, nil
	default:
		return Target{}, errors.New("unsupported lifecycle gate")
	}
}

func validateGateRequest(request GateRequest) error {
	if request.Schema != GateRequestSchema {
		return errors.New("unsupported review gate request schema")
	}
	switch request.Gate {
	case GatePostApply, GatePreCommit, GatePrePush, GatePrePR, GateRelease:
	default:
		return fmt.Errorf("unsupported review gate %q", request.Gate)
	}
	if !validSHA256(request.StoreRevision) || !validSHA256(request.GenesisRevision) || !validSHA256(request.ChainIdentity) || !validSHA256(request.BundleDigest) {
		return errors.New("gate request requires the exact authoritative store revision, genesis, chain identity, and bundle digest")
	}
	for label, source := range map[string][2]string{
		"policy": {request.PolicyArtifact, request.PolicyContent}, "ledger": {request.LedgerArtifact, request.LedgerContent}, "evidence": {request.EvidenceArtifact, request.EvidenceContent},
	} {
		if strings.TrimSpace(source[0]) == "" && strings.TrimSpace(source[1]) == "" {
			return fmt.Errorf("gate request requires %s artifact", label)
		}
	}
	if request.Gate == GateRelease && request.Release == nil {
		return errors.New("release gate requires an immutable release request")
	}
	if request.Gate != GateRelease && request.Release != nil {
		return errors.New("release request is only valid at the release gate")
	}
	switch request.ExternalEvidence {
	case ExternalEvidenceNone, ExternalEvidenceInvalidating, ExternalEvidenceEscalating:
	default:
		return fmt.Errorf("invalid external evidence disposition %q", request.ExternalEvidence)
	}
	return nil
}

func deriveReleaseEvidence(ctx context.Context, repo string, request *ReleaseRequest) (ReleaseEvidence, error) {
	if request == nil {
		return ReleaseEvidence{}, errors.New("release request is missing")
	}
	revision, err := (SnapshotBuilder{Repo: repo}).Build(ctx, Target{Kind: TargetExactRevision, Revision: request.Revision})
	if err != nil {
		return ReleaseEvidence{}, err
	}
	hashArtifact := func(label, path string) (string, error) {
		value, err := HashArtifact(path)
		if err != nil {
			return "", fmt.Errorf("%s artifact: %w", label, err)
		}
		return value, nil
	}
	configurationHash, err := hashArtifact("configuration", request.ConfigurationArtifact)
	if err != nil {
		return ReleaseEvidence{}, err
	}
	generatedHash, err := hashArtifact("generated", request.GeneratedArtifact)
	if err != nil {
		return ReleaseEvidence{}, err
	}
	provenanceHash, err := hashArtifact("provenance", request.ProvenanceArtifact)
	if err != nil {
		return ReleaseEvidence{}, err
	}
	boundaryHash, err := hashArtifact("publication boundary", request.PublicationBoundaryArtifact)
	if err != nil {
		return ReleaseEvidence{}, err
	}
	freshnessHash, err := hashArtifact("evidence freshness", request.EvidenceFreshnessArtifact)
	if err != nil {
		return ReleaseEvidence{}, err
	}
	release := ReleaseEvidence{
		ReleaseTree: revision.CandidateTree, ConfigurationHash: configurationHash,
		GeneratedArtifactHash: generatedHash, ProvenanceHash: provenanceHash,
		PublicationBoundaryHash: boundaryHash, PublicationState: request.PublicationState,
		EvidenceFreshnessHash: freshnessHash, EvidenceFreshnessState: request.EvidenceFreshnessState,
	}
	if err := validateReleaseEvidence(release); err != nil {
		return ReleaseEvidence{}, err
	}
	return release, nil
}

func hashLedgerArtifact(path string) (string, error) {
	hash, _, err := hashLedgerArtifactBinding(path)
	return hash, err
}

func hashArtifactSource(path, content string) (string, error) {
	if strings.TrimSpace(content) != "" {
		sum := sha256.Sum256([]byte(content))
		return "sha256:" + hex.EncodeToString(sum[:]), nil
	}
	return HashArtifact(path)
}

func hashLedgerArtifactSource(path, content string) (string, string, error) {
	if strings.TrimSpace(content) == "" {
		return hashLedgerArtifactBinding(path)
	}
	return hashLedgerPayload([]byte(content))
}

func hashLedgerArtifactBinding(path string) (string, string, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	return hashLedgerPayload(payload)
}

func hashLedgerPayload(payload []byte) (string, string, error) {
	var envelope struct {
		Schema   string    `json:"schema"`
		Findings []Finding `json:"findings"`
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if err := decoder.Decode(&envelope); err != nil {
		return "", "", err
	}
	if envelope.Schema != "gentle-ai.review-ledger/v1" || envelope.Findings == nil {
		return "", "", errors.New("ledger requires gentle-ai.review-ledger/v1 and an explicit findings array")
	}
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:]), findingsHash(envelope.Findings), nil
}

func HashLedgerArtifact(path string) (string, error) {
	return hashLedgerArtifact(path)
}

func nativeGateReason(result GateResult) string {
	switch result {
	case GateAllow:
		return "authoritative transaction, current repository target, and content-bound artifacts match"
	case GateScopeChanged:
		return "current repository target no longer matches the reviewed scope"
	case GateEscalated:
		return "transaction or external evidence is terminally escalated"
	default:
		return "content-bound policy, ledger, fix delta, verify evidence, base, or release evidence does not match"
	}
}
