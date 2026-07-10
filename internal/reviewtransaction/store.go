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
	"regexp"
	"strings"
)

const RecordSchema = "gentle-ai.review-record/v1"

var ErrConcurrentUpdate = errors.New("review transaction changed concurrently")
var ErrInvalidSuccessor = errors.New("review transaction successor is invalid")

type Record struct {
	Schema           string      `json:"schema"`
	Operation        string      `json:"operation"`
	PreviousRevision string      `json:"previous_revision"`
	Transaction      Transaction `json:"transaction"`
}

type Store struct {
	Dir       string
	lineageID string
}

type ValidatedChain struct {
	Records         []Record `json:"records"`
	Revisions       []string `json:"revisions"`
	GenesisRevision string   `json:"genesis_revision"`
	HeadRevision    string   `json:"head_revision"`
	Identity        string   `json:"identity"`
}

var lineageIDPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

func AuthoritativeStore(ctx context.Context, repo, lineageID string) (Store, error) {
	if err := validateLineageID(lineageID); err != nil {
		return Store{}, err
	}
	root, err := (SnapshotBuilder{Repo: repo}).repositoryRoot(ctx)
	if err != nil {
		return Store{}, fmt.Errorf("resolve authoritative review repository: %w", err)
	}
	output, err := runGit(ctx, root, nil, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return Store{}, fmt.Errorf("resolve repository Git common directory: %w", err)
	}
	commonDir := strings.TrimSpace(string(output))
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(root, commonDir)
	}
	commonDir, err = filepath.Abs(commonDir)
	if err != nil {
		return Store{}, err
	}
	authorityRoot := filepath.Join(filepath.Clean(commonDir), "gentle-ai", "review-transactions", "v1")
	dir := filepath.Join(authorityRoot, lineageID)
	relative, err := filepath.Rel(authorityRoot, dir)
	if err != nil || relative != lineageID || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return Store{}, errors.New("lineage_id escapes the repository review store")
	}
	return Store{Dir: dir, lineageID: lineageID}, nil
}

func validateLineageID(lineageID string) error {
	if len(lineageID) == 0 || len(lineageID) > 128 || !lineageIDPattern.MatchString(lineageID) {
		return errors.New("lineage_id must be a canonical lowercase kebab-case identifier of at most 128 bytes")
	}
	return nil
}

func (store Store) Append(expectedRevision string, record Record) (string, error) {
	if strings.TrimSpace(store.Dir) == "" {
		return "", errors.New("review store directory is required")
	}
	if strings.TrimSpace(record.Operation) == "" {
		return "", errors.New("record operation is required")
	}
	if err := record.Transaction.validate(); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidSuccessor, err)
	}
	if store.lineageID != "" && record.Transaction.LineageID != store.lineageID {
		return "", fmt.Errorf("%w: transaction lineage does not match authoritative store lineage", ErrInvalidSuccessor)
	}
	if err := os.MkdirAll(filepath.Join(store.Dir, "events"), 0o755); err != nil {
		return "", err
	}
	lockPath := filepath.Join(store.Dir, "LOCK")
	lock, err := acquireStoreLock(lockPath)
	if err != nil {
		return "", err
	}
	defer lock.release()
	record.Schema = RecordSchema
	record.PreviousRevision = expectedRevision
	payload, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return "", err
	}
	payload = append(payload, '\n')
	sum := sha256.Sum256(payload)
	revision := "sha256:" + hex.EncodeToString(sum[:])

	current, err := readRevision(filepath.Join(store.Dir, "HEAD"))
	if err != nil {
		return "", err
	}
	if current == revision {
		if _, err := store.loadChain(current); err != nil {
			return "", err
		}
		existing, err := os.ReadFile(filepath.Join(store.Dir, "events", strings.TrimPrefix(revision, "sha256:")+".json"))
		if err != nil {
			return "", err
		}
		if !bytes.Equal(existing, payload) {
			return "", ErrConcurrentUpdate
		}
		return revision, nil
	}
	if current != expectedRevision {
		return "", fmt.Errorf("%w: expected predecessor %q, current HEAD %q, candidate revision %q", ErrConcurrentUpdate, expectedRevision, current, revision)
	}
	if current == "" {
		if !validInitialStoreRecord(record) {
			return "", fmt.Errorf("%w: first event must be review/start in reviewing state", ErrInvalidSuccessor)
		}
	} else {
		chain, err := store.loadChain(current)
		if err != nil {
			return "", err
		}
		previous := chain.Records[len(chain.Records)-1]
		if err := validateSuccessor(previous.Transaction, record.Transaction, record.Operation); err != nil {
			return "", err
		}
	}
	temp, err := os.CreateTemp(filepath.Join(store.Dir, "events"), ".event-*")
	if err != nil {
		return "", err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if _, err := temp.Write(payload); err != nil {
		_ = temp.Close()
		return "", err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return "", err
	}
	if err := temp.Close(); err != nil {
		return "", err
	}
	eventPath := filepath.Join(store.Dir, "events", strings.TrimPrefix(revision, "sha256:")+".json")
	if err := os.Link(tempPath, eventPath); err != nil {
		if os.IsExist(err) {
			existing, readErr := os.ReadFile(eventPath)
			if readErr != nil {
				return "", readErr
			}
			if !reflect.DeepEqual(existing, payload) {
				return "", errors.New("existing content-addressed review event does not match its revision")
			}
		} else {
			return "", err
		}
	}
	if err := writeAtomic(filepath.Join(store.Dir, "HEAD"), []byte(revision+"\n"), 0o644); err != nil {
		return "", err
	}
	return revision, nil
}

func (store Store) Load() (Record, string, error) {
	chain, err := store.LoadChain()
	if err != nil {
		return Record{}, "", err
	}
	return chain.Records[len(chain.Records)-1], chain.HeadRevision, nil
}

func (store Store) LoadChain() (ValidatedChain, error) {
	revision, err := readRevision(filepath.Join(store.Dir, "HEAD"))
	if err != nil {
		return ValidatedChain{}, err
	}
	if revision == "" {
		return ValidatedChain{}, os.ErrNotExist
	}
	return store.loadChain(revision)
}

func (store Store) loadChain(headRevision string) (ValidatedChain, error) {
	if !validSHA256(headRevision) {
		return ValidatedChain{}, errors.New("review chain HEAD revision is invalid")
	}
	visited := map[string]struct{}{}
	reverseRecords := []Record{}
	reverseRevisions := []string{}
	for revision := headRevision; revision != ""; {
		if _, seen := visited[revision]; seen {
			return ValidatedChain{}, errors.New("review record predecessor cycle detected")
		}
		visited[revision] = struct{}{}
		record, loadedRevision, err := store.loadRevision(revision)
		if err != nil {
			return ValidatedChain{}, fmt.Errorf("load review chain revision %s: %w", revision, err)
		}
		reverseRecords = append(reverseRecords, record)
		reverseRevisions = append(reverseRevisions, loadedRevision)
		if record.PreviousRevision != "" && !validSHA256(record.PreviousRevision) {
			return ValidatedChain{}, errors.New("review record predecessor revision is invalid")
		}
		revision = record.PreviousRevision
	}

	records := make([]Record, len(reverseRecords))
	revisions := make([]string, len(reverseRevisions))
	for index := range reverseRecords {
		reverseIndex := len(reverseRecords) - 1 - index
		records[index] = reverseRecords[reverseIndex]
		revisions[index] = reverseRevisions[reverseIndex]
	}
	if len(records) == 0 || !validInitialStoreRecord(records[0]) {
		return ValidatedChain{}, errors.New("review chain must have exactly one valid review/start genesis")
	}
	if store.lineageID != "" && records[0].Transaction.LineageID != store.lineageID {
		return ValidatedChain{}, errors.New("review chain genesis lineage does not match authoritative store lineage")
	}
	for index := 1; index < len(records); index++ {
		if records[index].PreviousRevision != revisions[index-1] {
			return ValidatedChain{}, errors.New("review chain predecessor revision is discontinuous")
		}
		if err := validateSuccessor(records[index-1].Transaction, records[index].Transaction, records[index].Operation); err != nil {
			return ValidatedChain{}, fmt.Errorf("review chain successor %s: %w", revisions[index], err)
		}
	}

	chain := ValidatedChain{
		Records: records, Revisions: revisions,
		GenesisRevision: revisions[0], HeadRevision: headRevision,
	}
	chain.Identity = chainIdentity(revisions)
	return chain, nil
}

func (store Store) loadRevision(revision string) (Record, string, error) {
	if !validSHA256(revision) {
		return Record{}, "", errors.New("review record revision is invalid")
	}
	path := filepath.Join(store.Dir, "events", strings.TrimPrefix(revision, "sha256:")+".json")
	payload, err := os.ReadFile(path)
	if err != nil {
		return Record{}, "", err
	}
	sum := sha256.Sum256(payload)
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != revision {
		return Record{}, "", errors.New("review record hash mismatch")
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	var record Record
	if err := decoder.Decode(&record); err != nil {
		return Record{}, "", err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return Record{}, "", errors.New("multiple JSON values in review record")
	}
	if record.Schema != RecordSchema || strings.TrimSpace(record.Operation) == "" {
		return Record{}, "", errors.New("invalid review record")
	}
	if err := record.Transaction.validate(); err != nil {
		return Record{}, "", err
	}
	return record, revision, nil
}

func validateSuccessor(previous, next Transaction, operation string) error {
	if previous.LineageID != next.LineageID || previous.Generation != next.Generation || previous.Mode != next.Mode {
		return fmt.Errorf("%w: lineage, generation, and mode are immutable", ErrInvalidSuccessor)
	}
	if previous.BaseTree != next.BaseTree || previous.InitialReviewTree != next.InitialReviewTree || previous.PathsDigest != next.PathsDigest || previous.PolicyHash != next.PolicyHash {
		return fmt.Errorf("%w: initial target and policy are immutable", ErrInvalidSuccessor)
	}
	fixCompletion := previous.State == StateFixing && next.State == StateFixValidating
	if !fixCompletion && !snapshotsEqual(previous.Snapshot, next.Snapshot) {
		return fmt.Errorf("%w: immutable review snapshot changed", ErrInvalidSuccessor)
	}
	if fixCompletion && (next.Snapshot.Kind != TargetFixDiff || next.Snapshot.BaseTree != previous.FinalCandidateTree || next.Snapshot.CandidateTree != next.FinalCandidateTree || !equalStrings(next.Snapshot.LedgerIDs, next.FixFindingIDs)) {
		return fmt.Errorf("%w: fix completion is not bound to the prior candidate and frozen ledger IDs", ErrInvalidSuccessor)
	}
	releaseBinding := previous.Release == nil && next.Release != nil
	if previous.Release != nil && !reflect.DeepEqual(previous.Release, next.Release) {
		return fmt.Errorf("%w: bound release evidence is immutable", ErrInvalidSuccessor)
	}
	if releaseBinding {
		withoutRelease := next
		withoutRelease.Release = nil
		if operation != "review/bind-release-evidence" || previous.State != StateReadyFinalVerification || next.State != StateReadyFinalVerification || !transactionsEqual(previous, withoutRelease) {
			return fmt.Errorf("%w: release evidence must be bound as the only change while ready for final verification", ErrInvalidSuccessor)
		}
	}
	if !releaseBinding && !legalStateTransition(previous.State, next.State) {
		return fmt.Errorf("%w: illegal state transition %q -> %q", ErrInvalidSuccessor, previous.State, next.State)
	}
	if !countersMonotonic(previous.Counters, next.Counters) {
		return fmt.Errorf("%w: counters cannot decrease", ErrInvalidSuccessor)
	}
	if err := validateSuccessorCounters(previous, next); err != nil {
		return err
	}
	if previous.LedgerHash != "" && (previous.LedgerHash != next.LedgerHash || previous.LedgerFindingsHash != next.LedgerFindingsHash) {
		return fmt.Errorf("%w: frozen ledger hash changed", ErrInvalidSuccessor)
	}
	if previous.JudgeProofHash != "" && (previous.JudgeProofHash != next.JudgeProofHash || previous.JudgeAgreementHash != next.JudgeAgreementHash || !reflect.DeepEqual(previous.JudgeProofs, next.JudgeProofs)) {
		return fmt.Errorf("%w: Judgment Day proof changed", ErrInvalidSuccessor)
	}
	if previous.State != StateReviewing && previous.State != StateJudgesConfirmed && !reflect.DeepEqual(previous.Findings, next.Findings) {
		return fmt.Errorf("%w: frozen findings changed", ErrInvalidSuccessor)
	}
	if !mapIsMonotonic(previous.Classifications, next.Classifications) || !mapIsMonotonic(previous.Outcomes, next.Outcomes) {
		return fmt.Errorf("%w: evidence classifications or outcomes regressed", ErrInvalidSuccessor)
	}
	if !sliceIsSubset(previous.FixFindingIDs, next.FixFindingIDs) || !findingSliceIsPrefix(previous.FixCausedFindings, next.FixCausedFindings) {
		return fmt.Errorf("%w: correction findings regressed", ErrInvalidSuccessor)
	}
	if !equalStrings(previous.PendingRefuterIDs, next.PendingRefuterIDs) {
		classifiedPending := previous.State == StateFindingsFrozen && next.State == StateEvidenceClassified && len(previous.PendingRefuterIDs) == 0 && len(next.PendingRefuterIDs) > 0 && next.Counters.RefuterBatches == previous.Counters.RefuterBatches
		completedBatch := previous.State == StateEvidenceClassified && len(previous.PendingRefuterIDs) > 0 && len(next.PendingRefuterIDs) == 0 && next.Counters.RefuterBatches == previous.Counters.RefuterBatches+1
		if !classifiedPending && !completedBatch {
			return fmt.Errorf("%w: pending refuter IDs changed without one complete consumed batch", ErrInvalidSuccessor)
		}
	}
	if previous.FinalCandidateTree != next.FinalCandidateTree && !fixCompletion {
		return fmt.Errorf("%w: final candidate changed outside fix completion", ErrInvalidSuccessor)
	}
	if previous.FixDeltaHash != next.FixDeltaHash && !fixCompletion {
		return fmt.Errorf("%w: fix delta changed outside fix completion", ErrInvalidSuccessor)
	}
	return nil
}

// transactionsEqual compares persisted transaction state. JSON omits empty
// optional arrays, so a local empty slice and its nil decoded form represent
// the same immutable release-binding state.
func transactionsEqual(left, right Transaction) bool {
	normalize := func(transaction *Transaction) {
		if len(transaction.Snapshot.IntendedUntracked) == 0 {
			transaction.Snapshot.IntendedUntracked = nil
		}
		if len(transaction.Snapshot.LedgerIDs) == 0 {
			transaction.Snapshot.LedgerIDs = nil
		}
		if len(transaction.Snapshot.Paths) == 0 {
			transaction.Snapshot.Paths = nil
		}
		if len(transaction.Findings) == 0 {
			transaction.Findings = nil
		}
		if len(transaction.FixFindingIDs) == 0 {
			transaction.FixFindingIDs = nil
		}
		if len(transaction.PendingRefuterIDs) == 0 {
			transaction.PendingRefuterIDs = nil
		}
		if len(transaction.FixCausedFindings) == 0 {
			transaction.FixCausedFindings = nil
		}
		if len(transaction.JudgeProofs) == 0 {
			transaction.JudgeProofs = nil
		}
		for index := range transaction.Findings {
			if len(transaction.Findings[index].ProofRefs) == 0 {
				transaction.Findings[index].ProofRefs = nil
			}
		}
		for index := range transaction.FixCausedFindings {
			if len(transaction.FixCausedFindings[index].ProofRefs) == 0 {
				transaction.FixCausedFindings[index].ProofRefs = nil
			}
		}
	}
	normalize(&left)
	normalize(&right)
	return reflect.DeepEqual(left, right)
}

func validateSuccessorCounters(previous, next Transaction) error {
	expected := previous.Counters
	switch {
	case previous.State == StateReviewing && next.State == StateJudgesConfirmed:
		expected.JudgeExecutions = 2
	case previous.State == StateEvidenceClassified && next.State != StateEvidenceClassified:
		expected.RefuterBatches++
	case previous.State == StateFixRequired && next.State == StateFixing:
		if previous.Mode == ModeOrdinary4R {
			expected.FixBatches++
		} else {
			expected.FixRounds++
		}
	case previous.State == StateFixValidating && (next.State == StateReadyFinalVerification || next.State == StateFixRequired || next.State == StateEscalated):
		if previous.Mode == ModeOrdinary4R {
			expected.ScopedFixValidations++
		} else {
			expected.ScopedRejudgments++
		}
	case previous.State == StateReadyFinalVerification && next.State == StateFinalVerifying:
		expected.FinalVerifications++
	}
	if expected != next.Counters {
		return fmt.Errorf("%w: counters do not match the semantic state transition", ErrInvalidSuccessor)
	}
	return nil
}

func snapshotsEqual(previous, next Snapshot) bool {
	return previous.Kind == next.Kind &&
		previous.BaseTree == next.BaseTree &&
		previous.CandidateTree == next.CandidateTree &&
		previous.PathsDigest == next.PathsDigest &&
		previous.IntendedUntrackedProof == next.IntendedUntrackedProof &&
		previous.Identity == next.Identity &&
		equalStrings(previous.IntendedUntracked, next.IntendedUntracked) &&
		equalStrings(previous.LedgerIDs, next.LedgerIDs) &&
		equalStrings(previous.Paths, next.Paths)
}

func validInitialStoreRecord(record Record) bool {
	if record.Operation != "review/start" || record.PreviousRevision != "" {
		return false
	}
	transaction := record.Transaction
	if transaction.State != StateReviewing ||
		transaction.BaseTree != transaction.Snapshot.BaseTree ||
		transaction.PathsDigest != transaction.Snapshot.PathsDigest ||
		transaction.InitialReviewTree != transaction.Snapshot.CandidateTree ||
		transaction.FinalCandidateTree != transaction.InitialReviewTree ||
		transaction.FixDeltaHash != EmptyFixDeltaHash ||
		transaction.LedgerHash != "" || transaction.EvidenceHash != "" ||
		transaction.JudgeProofHash != "" || transaction.JudgeAgreementHash != "" ||
		transaction.Release != nil || transaction.FailedEvidenceRevision != "" ||
		len(transaction.Findings) != 0 || len(transaction.Classifications) != 0 ||
		len(transaction.Outcomes) != 0 || len(transaction.FixFindingIDs) != 0 ||
		len(transaction.PendingRefuterIDs) != 0 || len(transaction.FixCausedFindings) != 0 ||
		len(transaction.JudgeProofs) != 0 {
		return false
	}
	switch transaction.Mode {
	case ModeOrdinary4R:
		return transaction.Counters == (Counters{FullReviews: 1})
	case ModeJudgmentDay:
		return transaction.Counters == (Counters{})
	default:
		return false
	}
}

func chainIdentity(revisions []string) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte("gentle-ai.review-chain/v1\x00"))
	for _, revision := range revisions {
		writeLengthPrefixed(hash, []byte(revision))
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func legalStateTransition(previous, next State) bool {
	allowed := map[State][]State{
		StateReviewing:              {StateJudgesConfirmed, StateFindingsFrozen, StateEscalated},
		StateJudgesConfirmed:        {StateFindingsFrozen, StateEscalated},
		StateFindingsFrozen:         {StateEvidenceClassified, StateFixRequired, StateReadyFinalVerification, StateEscalated},
		StateEvidenceClassified:     {StateFixRequired, StateReadyFinalVerification, StateEscalated},
		StateFixRequired:            {StateFixing, StateEscalated},
		StateFixing:                 {StateFixValidating, StateEscalated},
		StateFixValidating:          {StateReadyFinalVerification, StateFixRequired, StateEscalated},
		StateReadyFinalVerification: {StateFinalVerifying, StateEscalated},
		StateFinalVerifying:         {StateApproved, StateEscalated},
	}
	for _, candidate := range allowed[previous] {
		if candidate == next {
			return true
		}
	}
	return false
}

func countersMonotonic(previous, next Counters) bool {
	return next.FullReviews >= previous.FullReviews &&
		next.RefuterBatches >= previous.RefuterBatches &&
		next.FixBatches >= previous.FixBatches &&
		next.ScopedFixValidations >= previous.ScopedFixValidations &&
		next.FinalVerifications >= previous.FinalVerifications &&
		next.FixRounds >= previous.FixRounds &&
		next.ScopedRejudgments >= previous.ScopedRejudgments &&
		next.JudgeExecutions >= previous.JudgeExecutions
}

func mapIsMonotonic[K comparable, V comparable](previous, next map[K]V) bool {
	for key, value := range previous {
		if candidate, ok := next[key]; !ok || candidate != value {
			return false
		}
	}
	return true
}

func sliceIsSubset(previous, next []string) bool {
	set := make(map[string]struct{}, len(next))
	for _, value := range next {
		set[value] = struct{}{}
	}
	for _, value := range previous {
		if _, ok := set[value]; !ok {
			return false
		}
	}
	return true
}

func findingSliceIsPrefix(previous, next []Finding) bool {
	return len(previous) <= len(next) && reflect.DeepEqual(previous, next[:len(previous)])
}

func WriteReceiptAtomic(path string, receipt Receipt) error {
	if err := validateReceiptStructure(receipt); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, append(payload, '\n'), 0o644)
}

func WriteTransactionAtomic(path string, transaction Transaction) error {
	if err := transaction.validate(); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(transaction, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, append(payload, '\n'), 0o644)
}

func readRevision(path string) (string, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	revision := strings.TrimSpace(string(payload))
	if revision != "" && !validSHA256(revision) {
		return "", fmt.Errorf("invalid review store HEAD %q", revision)
	}
	return revision, nil
}

func writeAtomic(path string, payload []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".atomic-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(payload); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}
