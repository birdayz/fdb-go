package client

import (
	"context"
	"fmt"
	"math/rand"
	"time"
)

// FDB error codes for retry logic.
const (
	ErrNotCommitted              = 1020
	ErrCommitUnknownResult       = 1021
	ErrTransactionTooOld         = 1007
	ErrFutureVersion             = 1009
	ErrDatabaseLocked            = 1039
	ErrProxyMemoryLimitExceeded  = 1042
	ErrBatchTransactionThrottled = 1051
	ErrTagThrottled              = 1213
	ErrProcessBehind             = 1037
)

type txState int

const (
	txStateActive txState = iota
	txStateCommitted
	txStateErrored
)

// Mutation represents a key-value mutation in a transaction.
type Mutation struct {
	Type  MutationType
	Key   []byte
	Value []byte
}

// MutationType is the type of mutation.
type MutationType uint8

const (
	MutSetValue               MutationType = 0
	MutClearRange             MutationType = 1
	MutAddValue               MutationType = 2
	MutAnd                    MutationType = 3
	MutOr                     MutationType = 4
	MutXor                    MutationType = 5
	MutAppendIfFits           MutationType = 6
	MutMax                    MutationType = 7
	MutMin                    MutationType = 8
	MutSetVersionstampedKey   MutationType = 9
	MutSetVersionstampedValue MutationType = 10
	MutByteMin                MutationType = 11
	MutByteMax                MutationType = 12
	MutMinV2                  MutationType = 13
	MutAndV2                  MutationType = 14
	MutCompareAndClear        MutationType = 15
)

// KeyRange represents a range [Begin, End).
type KeyRange struct {
	Begin []byte
	End   []byte
}

// Transaction represents an FDB transaction.
// Mutations are buffered locally and sent on Commit().
type Transaction struct {
	db    *Database
	state txState

	readVersion      int64
	hasReadVersion   bool
	committedVersion int64

	mutations      []Mutation
	readConflicts  []KeyRange
	writeConflicts []KeyRange

	retryCount int
	backoff    time.Duration
}

// Get reads a single key. Returns nil if the key doesn't exist.
func (tx *Transaction) Get(ctx context.Context, key []byte) ([]byte, error) {
	if tx.state != txStateActive {
		return nil, fmt.Errorf("transaction not active")
	}

	if !tx.hasReadVersion {
		rv, err := tx.getReadVersion(ctx)
		if err != nil {
			return nil, err
		}
		tx.readVersion = rv
		tx.hasReadVersion = true
	}

	// Add read conflict range.
	tx.readConflicts = append(tx.readConflicts, KeyRange{Begin: key, End: append(key, 0)})

	return tx.getValue(ctx, key)
}

// GetRange reads a range of keys [begin, end). Returns the key-value pairs,
// a boolean indicating if more results exist beyond the limit, and any error.
func (tx *Transaction) GetRange(ctx context.Context, begin, end []byte, limit int) ([]KeyValue, bool, error) {
	if tx.state != txStateActive {
		return nil, false, fmt.Errorf("transaction not active")
	}

	if !tx.hasReadVersion {
		rv, err := tx.getReadVersion(ctx)
		if err != nil {
			return nil, false, err
		}
		tx.readVersion = rv
		tx.hasReadVersion = true
	}

	// Add read conflict range for [begin, end).
	tx.readConflicts = append(tx.readConflicts, KeyRange{Begin: begin, End: end})

	return tx.getRange(ctx, begin, end, limit)
}

// Set writes a key-value pair.
func (tx *Transaction) Set(key, value []byte) {
	tx.mutations = append(tx.mutations, Mutation{
		Type:  MutSetValue,
		Key:   key,
		Value: value,
	})
	tx.addWriteConflict(key, append(key, 0))
}

// Clear deletes a key.
func (tx *Transaction) Clear(key []byte) {
	end := make([]byte, len(key)+1)
	copy(end, key)
	end[len(key)] = 0
	tx.mutations = append(tx.mutations, Mutation{
		Type:  MutClearRange,
		Key:   key,
		Value: end,
	})
	tx.addWriteConflict(key, end)
}

// ClearRange deletes all keys in [begin, end).
func (tx *Transaction) ClearRange(begin, end []byte) {
	tx.mutations = append(tx.mutations, Mutation{
		Type:  MutClearRange,
		Key:   begin,
		Value: end,
	})
	tx.addWriteConflict(begin, end)
}

// Atomic performs an atomic mutation.
func (tx *Transaction) Atomic(op MutationType, key, operand []byte) {
	tx.mutations = append(tx.mutations, Mutation{
		Type:  op,
		Key:   key,
		Value: operand,
	})
	// Atomic ops add write conflict but NOT read conflict.
	tx.addWriteConflict(key, append(key, 0))
}

// Commit sends mutations to a commit proxy.
func (tx *Transaction) Commit(ctx context.Context) error {
	if tx.state != txStateActive {
		return fmt.Errorf("transaction not active")
	}

	if len(tx.mutations) == 0 && len(tx.writeConflicts) == 0 {
		// Read-only transaction — no commit needed.
		tx.state = txStateCommitted
		return nil
	}

	if err := tx.commit(ctx); err != nil {
		return err
	}
	tx.state = txStateCommitted
	return nil
}

// GetCommittedVersion returns the version at which this transaction committed.
func (tx *Transaction) GetCommittedVersion() (int64, error) {
	if tx.state != txStateCommitted {
		return 0, fmt.Errorf("transaction not committed")
	}
	return tx.committedVersion, nil
}

// OnError handles a transaction error. Returns nil if the error is retryable
// (the transaction has been reset for retry). Returns the error if non-retryable.
func (tx *Transaction) OnError(err error) error {
	code := extractErrorCode(err)

	switch code {
	case ErrNotCommitted,
		ErrCommitUnknownResult,
		ErrDatabaseLocked,
		ErrProxyMemoryLimitExceeded,
		ErrBatchTransactionThrottled,
		ErrTagThrottled,
		ErrProcessBehind:
		// Retryable with exponential backoff.
		tx.retryCount++
		tx.backoff = tx.nextBackoff()
		time.Sleep(tx.backoff)
		tx.reset()
		return nil

	case ErrTransactionTooOld, ErrFutureVersion:
		// Retryable with shorter delay.
		tx.retryCount++
		tx.backoff = 10 * time.Millisecond
		time.Sleep(tx.backoff)
		tx.reset()
		return nil

	default:
		// Non-retryable.
		tx.state = txStateErrored
		return err
	}
}

// SetReadVersion sets the read version manually.
func (tx *Transaction) SetReadVersion(version int64) {
	tx.readVersion = version
	tx.hasReadVersion = true
}

func (tx *Transaction) getReadVersion(ctx context.Context) (int64, error) {
	return tx.db.grvBatcher.GetReadVersion(ctx)
}

func (tx *Transaction) addWriteConflict(begin, end []byte) {
	tx.writeConflicts = append(tx.writeConflicts, KeyRange{Begin: begin, End: end})
}

func (tx *Transaction) reset() {
	tx.state = txStateActive
	tx.hasReadVersion = false
	tx.readVersion = 0
	tx.committedVersion = 0
	tx.mutations = tx.mutations[:0]
	tx.readConflicts = tx.readConflicts[:0]
	tx.writeConflicts = tx.writeConflicts[:0]
}

func (tx *Transaction) nextBackoff() time.Duration {
	base := 100 * time.Millisecond
	for i := 0; i < tx.retryCount && base < 5*time.Second; i++ {
		base *= 2
	}
	if base > 5*time.Second {
		base = 5 * time.Second
	}
	// Add jitter: multiply by random [0.0, 1.0).
	jitter := time.Duration(float64(base) * rand.Float64())
	return jitter
}

// FDBError represents an FDB error with a code.
type FDBError struct {
	Code    int
	Message string
}

func (e *FDBError) Error() string {
	return fmt.Sprintf("fdb error %d: %s", e.Code, e.Message)
}

func extractErrorCode(err error) int {
	if fdbErr, ok := err.(*FDBError); ok {
		return fdbErr.Code
	}
	return -1
}
