package fdb

import (
	"fmt"
	"math"
	"time"
)

// Priority selects the scheduling priority of a FoundationDB transaction.
type Priority uint8

const (
	// PriorityDefault uses the default transaction scheduling priority.
	PriorityDefault Priority = iota
	// PriorityBatch selects batch priority for background workloads.
	PriorityBatch
	// PrioritySystemImmediate selects system-immediate priority for urgent operations.
	PrioritySystemImmediate
)

// String returns the human-readable name of p.
func (p Priority) String() string {
	switch p {
	case PriorityDefault:
		return "default"
	case PriorityBatch:
		return "batch"
	case PrioritySystemImmediate:
		return "system_immediate"
	default:
		return fmt.Sprintf("Priority(%d)", uint8(p))
	}
}

// AccessMode selects whether a transaction may read or write system keys.
type AccessMode uint8

const (
	// AccessNormal restricts the transaction to normal user keyspaces.
	AccessNormal AccessMode = iota
	// AccessReadSystemKeys grants read-only access to the \xff system keyspace.
	AccessReadSystemKeys
	// AccessSystemKeys grants read and write access to the \xff system keyspace.
	AccessSystemKeys
)

// String returns the human-readable name of m.
func (m AccessMode) String() string {
	switch m {
	case AccessNormal:
		return "normal"
	case AccessReadSystemKeys:
		return "read_system_keys"
	case AccessSystemKeys:
		return "access_system_keys"
	default:
		return fmt.Sprintf("AccessMode(%d)", uint8(m))
	}
}

// LockMode selects whether a transaction may proceed against a locked database.
type LockMode uint8

const (
	// LockNormal respects database locks (default).
	LockNormal LockMode = iota
	// LockAware allows reads and commits on a locked database.
	LockAware
	// LockReadAware allows reads on a locked database while keeping writes disallowed.
	LockReadAware
)

// String returns the human-readable name of m.
func (m LockMode) String() string {
	switch m {
	case LockNormal:
		return "normal"
	case LockAware:
		return "lock_aware"
	case LockReadAware:
		return "read_lock_aware"
	default:
		return fmt.Sprintf("LockMode(%d)", uint8(m))
	}
}

// GRVCacheMode selects whether a transaction uses the database GRV cache.
type GRVCacheMode uint8

const (
	// GRVCacheDefault uses the transaction's standard GRV acquisition path.
	GRVCacheDefault GRVCacheMode = iota
	// GRVCacheUse opts in to serving the read version from the database GRV cache.
	GRVCacheUse
	// GRVCacheSkip forces bypassing the database GRV cache.
	GRVCacheSkip
)

// String returns the human-readable name of m.
func (m GRVCacheMode) String() string {
	switch m {
	case GRVCacheDefault:
		return "default"
	case GRVCacheUse:
		return "use_grv_cache"
	case GRVCacheSkip:
		return "skip_grv_cache"
	default:
		return fmt.Sprintf("GRVCacheMode(%d)", uint8(m))
	}
}

// TxConfig groups typed per-transaction options applied by [ApplyTxConfig].
type TxConfig struct {
	// Timeout sets the transaction timeout. Zero leaves the default unchanged;
	// negative values or durations exceeding math.MaxInt32 milliseconds fail
	// with invalid_option_value (2006).
	Timeout time.Duration

	// RetryLimit sets the maximum number of transaction retries. Zero leaves
	// the transaction's default retry policy unchanged; -1 sets unlimited
	// retries; values less than -1 fail with invalid_option_value (2006).
	RetryLimit int64

	// Priority configures transaction scheduling priority.
	Priority Priority

	// Access configures system-keyspace access permissions.
	Access AccessMode

	// Lock configures database-lock awareness.
	Lock LockMode

	// GRVCache configures database read-version cache usage.
	GRVCache GRVCacheMode

	// CausalReadRisky allows a slightly stale read version that may miss recent commits.
	CausalReadRisky bool

	// DisableReadYourWrites disables read-your-writes buffering for this transaction.
	DisableReadYourWrites bool

	// DisableWriteConflicts disables automatic write conflict range generation.
	DisableWriteConflicts bool

	// DebugID sets the debug transaction identifier when non-empty.
	DebugID string

	// LogTransaction enables server-side transaction logging when true.
	LogTransaction bool
}

// ApplyTxConfig validates and applies cfg to tx via [ReadTransaction.Options].
// Because it accepts [ReadTransaction], it works inside both [Transact] and
// [ReadTransact] callbacks.
//
// FoundationDB resets most transaction options (priority, system-key access,
// lock awareness, read-your-writes disable, debug ID, and logging) when a
// transaction retries via OnError, so callers should invoke ApplyTxConfig at
// the start of every attempt inside the retry closure.
func ApplyTxConfig(tx ReadTransaction, cfg TxConfig) error {
	if tx == nil {
		return Error{Code: 2000}
	}
	if cfg.Timeout < 0 {
		return Error{Code: 2006}
	}
	var timeoutMS int64
	if cfg.Timeout > 0 {
		timeoutMS = cfg.Timeout.Milliseconds()
		if timeoutMS == 0 {
			timeoutMS = 1
		}
		if timeoutMS > math.MaxInt32 {
			return Error{Code: 2006}
		}
	}
	if cfg.RetryLimit < -1 {
		return Error{Code: 2006}
	}
	if cfg.Priority > PrioritySystemImmediate {
		return Error{Code: 2006}
	}
	if cfg.Access > AccessSystemKeys {
		return Error{Code: 2006}
	}
	if cfg.Lock > LockReadAware {
		return Error{Code: 2006}
	}
	if cfg.GRVCache > GRVCacheSkip {
		return Error{Code: 2006}
	}

	opts := tx.Options()
	if timeoutMS > 0 {
		if err := opts.SetTimeout(timeoutMS); err != nil {
			return err
		}
	}
	if cfg.RetryLimit != 0 {
		if err := opts.SetRetryLimit(cfg.RetryLimit); err != nil {
			return err
		}
	}
	switch cfg.Priority {
	case PriorityBatch:
		if err := opts.SetPriorityBatch(); err != nil {
			return err
		}
	case PrioritySystemImmediate:
		if err := opts.SetPrioritySystemImmediate(); err != nil {
			return err
		}
	}
	switch cfg.Access {
	case AccessReadSystemKeys:
		if err := opts.SetReadSystemKeys(); err != nil {
			return err
		}
	case AccessSystemKeys:
		if err := opts.SetAccessSystemKeys(); err != nil {
			return err
		}
	}
	switch cfg.Lock {
	case LockAware:
		if err := opts.SetLockAware(); err != nil {
			return err
		}
	case LockReadAware:
		if err := opts.SetReadLockAware(); err != nil {
			return err
		}
	}
	switch cfg.GRVCache {
	case GRVCacheUse:
		if err := opts.SetUseGrvCache(); err != nil {
			return err
		}
	case GRVCacheSkip:
		if err := opts.SetSkipGrvCache(); err != nil {
			return err
		}
	}
	if cfg.CausalReadRisky {
		if err := opts.SetCausalReadRisky(); err != nil {
			return err
		}
	}
	if cfg.DisableReadYourWrites {
		if err := opts.SetReadYourWritesDisable(); err != nil {
			return err
		}
	}
	if cfg.DisableWriteConflicts {
		opts.SetWriteConflictsDisabled()
	}
	if cfg.DebugID != "" {
		if err := opts.SetDebugTransactionIdentifier(cfg.DebugID); err != nil {
			return err
		}
	}
	if cfg.LogTransaction {
		if err := opts.SetLogTransaction(); err != nil {
			return err
		}
	}
	return nil
}
