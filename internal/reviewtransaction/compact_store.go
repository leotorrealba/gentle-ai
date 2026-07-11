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
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
)

const compactRecordSchema = "gentle-ai.review-state-record/v2"
const CompactTransportSchema = "gentle-ai.review-transport/v2"

var ErrLegacyReadOnly = errors.New("legacy v1 review lineage is read-only")

type CompactRecord struct {
	Schema   string       `json:"schema"`
	Revision string       `json:"revision"`
	State    CompactState `json:"state"`
}

type CompactStore struct {
	Dir       string
	lineageID string
	repo      string
	lockPath  string
	TracePath string
}

type CompactTraceEntry struct {
	Operation        string `json:"operation"`
	PreviousRevision string `json:"previous_revision,omitempty"`
	Revision         string `json:"revision"`
	State            State  `json:"state"`
	RecordedAt       string `json:"recorded_at"`
}

type CompactTransport struct {
	Schema       string          `json:"schema"`
	Record       CompactRecord   `json:"record"`
	Receipt      *CompactReceipt `json:"receipt,omitempty"`
	BundleDigest string          `json:"bundle_digest"`
}

func CompactAuthoritativeStore(ctx context.Context, repo, lineageID string) (CompactStore, error) {
	if err := validateLineageID(lineageID); err != nil {
		return CompactStore{}, err
	}
	base, root, err := reviewAuthorityRoot(ctx, repo)
	if err != nil {
		return CompactStore{}, err
	}
	versionRoot := filepath.Join(base, "v2")
	dir := filepath.Join(versionRoot, lineageID)
	return CompactStore{Dir: dir, lineageID: lineageID, repo: root, lockPath: filepath.Join(versionRoot, "LOCK")}, nil
}

func DiscoverCompactStores(ctx context.Context, repo string) ([]CompactStore, error) {
	base, root, err := reviewAuthorityRoot(ctx, repo)
	if err != nil {
		return nil, err
	}
	versionRoot := filepath.Join(base, "v2")
	entries, err := os.ReadDir(versionRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return []CompactStore{}, nil
		}
		return nil, err
	}
	stores := make([]CompactStore, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || validateLineageID(entry.Name()) != nil {
			continue
		}
		stores = append(stores, CompactStore{
			Dir: filepath.Join(versionRoot, entry.Name()), lineageID: entry.Name(), repo: root,
			lockPath: filepath.Join(versionRoot, "LOCK"),
		})
	}
	sort.Slice(stores, func(i, j int) bool { return stores[i].lineageID < stores[j].lineageID })
	return stores, nil
}

func (store CompactStore) StatePath() string { return filepath.Join(store.Dir, "review-state.json") }

func (store CompactStore) ReceiptPath() string {
	return filepath.Join(store.Dir, "review-receipt.json")
}

func (store CompactStore) Load() (CompactRecord, error) {
	payload, err := os.ReadFile(store.StatePath())
	if err != nil {
		return CompactRecord{}, err
	}
	return parseCompactRecord(payload, store.lineageID)
}

func (store CompactStore) Replace(expectedRevision, operation string, next CompactState) (string, error) {
	if strings.TrimSpace(operation) == "" {
		return "", errors.New("compact review operation is required")
	}
	if err := next.Validate(); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidSuccessor, err)
	}
	if store.lineageID != "" && next.LineageID != store.lineageID {
		return "", fmt.Errorf("%w: compact lineage does not match store", ErrInvalidSuccessor)
	}
	lock, err := acquireStoreLock(store.lockPath)
	if err != nil {
		return "", err
	}
	defer lock.release()

	var current *CompactRecord
	payload, err := os.ReadFile(store.StatePath())
	if err == nil {
		loaded, parseErr := parseCompactRecord(payload, store.lineageID)
		if parseErr != nil {
			return "", parseErr
		}
		current = &loaded
	} else if !os.IsNotExist(err) {
		return "", err
	}
	record, payload, err := makeCompactRecord(next)
	if err != nil {
		return "", err
	}
	if current != nil && current.Revision == record.Revision && compactStateEqual(current.State, next) {
		return record.Revision, nil
	}
	currentRevision := ""
	if current != nil {
		currentRevision = current.Revision
	}
	if currentRevision != expectedRevision {
		return "", fmt.Errorf("%w: expected compact revision %q, current %q", ErrConcurrentUpdate, expectedRevision, currentRevision)
	}
	if current == nil {
		if operation != "review/start" || next.State != StateReviewing {
			return "", fmt.Errorf("%w: compact authority must start in reviewing state", ErrInvalidSuccessor)
		}
	} else if err := validateCompactSuccessor(current.State, next, operation); err != nil {
		return "", err
	}
	if store.repo != "" {
		if err := validateCompactRepositoryEvidence(context.Background(), store.repo, current, next, operation); err != nil {
			return "", fmt.Errorf("%w: %v", ErrInvalidSuccessor, err)
		}
	}
	if err := writeAtomic(store.StatePath(), payload, 0o644); err != nil {
		return "", err
	}
	if store.TracePath != "" {
		_ = appendCompactTrace(store.TracePath, CompactTraceEntry{
			Operation: operation, PreviousRevision: currentRevision, Revision: record.Revision,
			State: next.State, RecordedAt: time.Now().UTC().Format(time.RFC3339Nano),
		})
	}
	return record.Revision, nil
}

func validateCompactRepositoryEvidence(ctx context.Context, repo string, current *CompactRecord, next CompactState, operation string) error {
	builder := SnapshotBuilder{Repo: repo}
	if current == nil {
		if err := builder.ValidateEvidence(ctx, next.InitialSnapshot); err != nil {
			return errors.New("initial compact snapshot is not repository-derived")
		}
		risk, lines, err := builder.ClassifySnapshotRisk(ctx, next.InitialSnapshot)
		if err != nil || risk != next.RiskLevel || lines != next.OriginalChangedLines {
			return errors.New("compact risk inputs do not match repository evidence")
		}
	}
	if operation == "review/complete-fix" {
		if err := builder.ValidateEvidence(ctx, next.CurrentSnapshot); err != nil {
			return errors.New("compact correction snapshot is not repository-derived")
		}
		lines, err := builder.ChangedLines(ctx, next.CurrentSnapshot)
		if err != nil || next.ActualCorrectionLines == nil || lines != *next.ActualCorrectionLines {
			return errors.New("compact correction size does not match repository evidence")
		}
	}
	return nil
}

func validateCompactSuccessor(previous, next CompactState, operation string) error {
	if previous.LineageID != next.LineageID || previous.Generation != next.Generation ||
		!snapshotsEqual(previous.InitialSnapshot, next.InitialSnapshot) || !equalStrings(previous.GenesisPaths, next.GenesisPaths) ||
		previous.PolicyHash != next.PolicyHash || previous.RiskLevel != next.RiskLevel ||
		!equalStrings(previous.SelectedLenses, next.SelectedLenses) || previous.OriginalChangedLines != next.OriginalChangedLines ||
		previous.CorrectionBudget != next.CorrectionBudget {
		return fmt.Errorf("%w: compact review scope, tier, policy, and budget are immutable", ErrInvalidSuccessor)
	}
	switch operation {
	case "review/complete-review":
		if previous.State != StateReviewing || next.State != StateCorrectionRequired && next.State != StateValidating && next.State != StateEscalated {
			return fmt.Errorf("%w: invalid compact review completion", ErrInvalidSuccessor)
		}
		if !snapshotsEqual(previous.CurrentSnapshot, next.CurrentSnapshot) || next.ProposedCorrectionLines != nil || next.ActualCorrectionLines != nil || next.FixDeltaHash != EmptyFixDeltaHash || next.OriginalCriteria != nil || next.EvidenceHash != "" {
			return fmt.Errorf("%w: compact review completion changed correction or delivery state", ErrInvalidSuccessor)
		}
	case "review/begin-fix":
		if previous.State != StateCorrectionRequired || next.State != StateCorrectionRequired && next.State != StateEscalated || previous.ProposedCorrectionLines != nil || next.ProposedCorrectionLines == nil {
			return fmt.Errorf("%w: invalid compact correction start", ErrInvalidSuccessor)
		}
		expected := previous
		expected.State = next.State
		expected.ProposedCorrectionLines = next.ProposedCorrectionLines
		if !compactStateEqual(expected, next) {
			return fmt.Errorf("%w: compact correction start changed unrelated state", ErrInvalidSuccessor)
		}
	case "review/complete-fix":
		if previous.State != StateCorrectionRequired || previous.ProposedCorrectionLines == nil || next.State != StateValidating && next.State != StateEscalated || next.ActualCorrectionLines == nil {
			return fmt.Errorf("%w: invalid compact correction completion", ErrInvalidSuccessor)
		}
		if !reflectCompactReviewData(previous, next) || previous.EvidenceHash != next.EvidenceHash {
			return fmt.Errorf("%w: compact correction changed frozen review evidence", ErrInvalidSuccessor)
		}
	case "review/complete-verification":
		if previous.State != StateValidating || next.State != StateApproved && next.State != StateEscalated || !validSHA256(next.EvidenceHash) {
			return fmt.Errorf("%w: invalid compact verification completion", ErrInvalidSuccessor)
		}
		expected := previous
		expected.State = next.State
		expected.EvidenceHash = next.EvidenceHash
		if !compactStateEqual(expected, next) {
			return fmt.Errorf("%w: compact verification changed unrelated state", ErrInvalidSuccessor)
		}
	default:
		return fmt.Errorf("%w: unsupported compact operation %q", ErrInvalidSuccessor, operation)
	}
	return nil
}

func reflectCompactReviewData(previous, next CompactState) bool {
	return reflect.DeepEqual(previous.LensResults, next.LensResults) &&
		reflect.DeepEqual(previous.Findings, next.Findings) &&
		reflect.DeepEqual(previous.Classifications, next.Classifications) &&
		reflect.DeepEqual(previous.Outcomes, next.Outcomes) &&
		equalStrings(previous.FixFindingIDs, next.FixFindingIDs) &&
		len(previous.FollowUps) <= len(next.FollowUps) && reflect.DeepEqual(previous.FollowUps, next.FollowUps[:len(previous.FollowUps)])
}

func makeCompactRecord(state CompactState) (CompactRecord, []byte, error) {
	statePayload, err := json.Marshal(state)
	if err != nil {
		return CompactRecord{}, nil, err
	}
	sum := sha256.Sum256(append([]byte("gentle-ai.review-state/v2\x00"), statePayload...))
	record := CompactRecord{Schema: compactRecordSchema, Revision: "sha256:" + hex.EncodeToString(sum[:]), State: state}
	payload, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return CompactRecord{}, nil, err
	}
	return record, append(payload, '\n'), nil
}

func parseCompactRecord(payload []byte, lineageID string) (CompactRecord, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var record CompactRecord
	if err := decoder.Decode(&record); err != nil {
		return CompactRecord{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return CompactRecord{}, errors.New("multiple JSON values in compact review state")
	}
	if record.Schema != compactRecordSchema || !validSHA256(record.Revision) {
		return CompactRecord{}, errors.New("invalid compact review state record")
	}
	if err := record.State.Validate(); err != nil {
		return CompactRecord{}, err
	}
	if lineageID != "" && record.State.LineageID != lineageID {
		return CompactRecord{}, errors.New("compact state lineage does not match its directory")
	}
	want, _, err := makeCompactRecord(record.State)
	if err != nil || want.Revision != record.Revision {
		return CompactRecord{}, errors.New("compact review state checksum mismatch")
	}
	return record, nil
}

func appendCompactTrace(path string, entry CompactTraceEntry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	payload, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(payload, '\n')); err != nil {
		return err
	}
	return file.Sync()
}

func (store CompactStore) ExportTransport() (CompactTransport, error) {
	record, err := store.Load()
	if err != nil {
		return CompactTransport{}, err
	}
	transport := CompactTransport{Schema: CompactTransportSchema, Record: record}
	if payload, readErr := os.ReadFile(store.ReceiptPath()); readErr == nil {
		receipt, parseErr := ParseCompactReceipt(payload)
		authoritative, authorityErr := record.State.Receipt()
		if parseErr != nil || authorityErr != nil || !compactReceiptEqual(receipt, authoritative) {
			return CompactTransport{}, errors.New("compact receipt does not match authority")
		}
		transport.Receipt = &receipt
	} else if !os.IsNotExist(readErr) {
		return CompactTransport{}, readErr
	}
	transport.BundleDigest = compactTransportDigest(transport)
	return transport, nil
}

func ParseCompactTransport(payload []byte) (CompactTransport, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var transport CompactTransport
	if err := decoder.Decode(&transport); err != nil {
		return CompactTransport{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return CompactTransport{}, errors.New("multiple JSON values in compact review transport")
	}
	if transport.Schema != CompactTransportSchema || transport.BundleDigest != compactTransportDigest(transport) {
		return CompactTransport{}, errors.New("compact review transport checksum mismatch")
	}
	recordPayload, _ := json.Marshal(transport.Record)
	if _, err := parseCompactRecord(recordPayload, transport.Record.State.LineageID); err != nil {
		return CompactTransport{}, err
	}
	if transport.Receipt != nil {
		authoritative, err := transport.Record.State.Receipt()
		if err != nil || transport.Receipt.Validate() != nil || !compactReceiptEqual(*transport.Receipt, authoritative) {
			return CompactTransport{}, errors.New("compact transport receipt does not match state")
		}
	}
	return transport, nil
}

func WriteCompactTransportAtomic(path string, transport CompactTransport) error {
	transport.BundleDigest = compactTransportDigest(transport)
	payload, err := json.MarshalIndent(transport, "", "  ")
	if err != nil {
		return err
	}
	validated, err := ParseCompactTransport(append(payload, '\n'))
	if err != nil || validated.BundleDigest != transport.BundleDigest {
		return errors.New("invalid compact review transport")
	}
	return writeAtomic(path, append(payload, '\n'), 0o644)
}

func ImportCompactTransport(ctx context.Context, repo string, transport CompactTransport) (CompactRecord, error) {
	payload, _ := json.Marshal(transport)
	validated, err := ParseCompactTransport(payload)
	if err != nil {
		return CompactRecord{}, err
	}
	store, err := CompactAuthoritativeStore(ctx, repo, validated.Record.State.LineageID)
	if err != nil {
		return CompactRecord{}, err
	}
	if legacy, legacyErr := AuthoritativeStore(ctx, repo, validated.Record.State.LineageID); legacyErr == nil {
		if _, loadErr := legacy.LoadChain(); loadErr == nil {
			return CompactRecord{}, errors.New("cannot import compact authority over an existing legacy v1 lineage")
		}
	}
	if err := store.installTransportRecord(ctx, validated.Record); err != nil {
		return CompactRecord{}, err
	}
	if validated.Receipt != nil {
		if err := WriteCompactReceiptAtomic(store.ReceiptPath(), *validated.Receipt); err != nil {
			return CompactRecord{}, err
		}
	}
	return store.Load()
}

func (store CompactStore) installTransportRecord(ctx context.Context, record CompactRecord) error {
	lock, err := acquireStoreLock(store.lockPath)
	if err != nil {
		return err
	}
	defer lock.release()
	if existing, loadErr := store.Load(); loadErr == nil {
		if existing.Revision == record.Revision && compactStateEqual(existing.State, record.State) {
			return nil
		}
		return ErrConcurrentUpdate
	} else if !os.IsNotExist(loadErr) {
		return loadErr
	}
	if err := validateCompactTransportDelivery(ctx, store.repo, record.State); err != nil {
		return err
	}
	want, payload, err := makeCompactRecord(record.State)
	if err != nil || want.Revision != record.Revision {
		return errors.New("imported compact record checksum changed")
	}
	return writeAtomic(store.StatePath(), payload, 0o644)
}

func validateCompactTransportDelivery(ctx context.Context, repo string, state CompactState) error {
	builder := SnapshotBuilder{Repo: repo}
	headTree, err := builder.resolveTree(ctx, "HEAD")
	if err != nil || headTree != state.CurrentSnapshot.CandidateTree {
		return errors.New("imported compact authority does not match the current delivered tree")
	}
	paths, err := builder.changedPaths(ctx, state.InitialSnapshot.BaseTree, state.CurrentSnapshot.CandidateTree)
	if err != nil {
		return fmt.Errorf("derive imported compact delivered scope: %w", err)
	}
	if !equalStrings(paths, state.GenesisPaths) || digestPaths(paths) != state.InitialSnapshot.PathsDigest {
		return errors.New("imported compact authority does not match the original base-to-final path scope")
	}
	proof, err := builder.untrackedProof(ctx, state.CurrentSnapshot.CandidateTree, state.CurrentSnapshot.IntendedUntracked)
	if err != nil || proof != state.CurrentSnapshot.IntendedUntrackedProof {
		return errors.New("imported compact authority does not match delivered intended-untracked content")
	}
	return nil
}

func compactTransportDigest(transport CompactTransport) string {
	copy := transport
	copy.BundleDigest = ""
	payload, _ := json.Marshal(copy)
	sum := sha256.Sum256(append([]byte("gentle-ai.review-transport/v2\x00"), payload...))
	return "sha256:" + hex.EncodeToString(sum[:])
}
