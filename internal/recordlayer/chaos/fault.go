package chaos

import (
	"context"
	"math/rand/v2"
	"sync"

	"fdb.dev/pkg/fdbgo/fdb"
)

// FaultType identifies a specific fault that can be injected.
type FaultType int

const (
	// FaultCommitUnknown simulates FDB error 1021 (commit_unknown_result).
	// The transaction commits successfully, but the ChaosTransactor re-executes
	// the function in a new transaction (simulating a client retry after
	// ambiguous commit). This tests idempotency — does a retry corrupt state?
	//
	// Critical for: COUNT/SUM indexes (atomic ADD is not idempotent),
	// record counting, any mutation that isn't naturally idempotent.
	FaultCommitUnknown FaultType = iota

	// FaultConflict simulates FDB error 1020 (not_committed / transaction conflict).
	// Implemented identically to FaultCommitUnknown at the Transactor level:
	// both commit, then re-execute. In real FDB, the first attempt's writes
	// would be rolled back, but we can't simulate true rollback at this
	// abstraction level. The double-commit is a superset test: if the code
	// is correct under double-commit, it's correct under rollback+retry too.
	FaultConflict

	// FaultTransactionTooOld simulates FDB error 1007 (transaction_too_old).
	// Same implementation as FaultConflict — see comment above.
	FaultTransactionTooOld

	// FaultReadError simulates FDB error 1510 (io_error) surfacing from a READ
	// inside the transaction, rather than from the commit at its boundary. The
	// other fault types all commit and re-execute, so none of them can reach a
	// caller that only reads — and read paths are exactly where "an error means
	// the data is absent" is an easy and silent mistake to make.
	//
	// Scoped to a key prefix (see InjectReadErrorOnce) so a test can fail the one
	// read it is reasoning about and leave the surrounding open/scan reads alone;
	// a blanket failure would be satisfied by whichever read happens to come
	// first, which is not a property worth pinning.
	//
	// 1510 is deliberately NOT retryable (fdb.IsRetryable), so the fault surfaces
	// to the caller instead of spinning the transactor's retry loop against a
	// wrapper that would re-arm on every attempt.
	FaultReadError
)

// injectedReadError is the error FaultReadError surfaces from a failed read.
var injectedReadError = fdb.Error{Code: 1510}

// FaultConfig controls fault injection rates.
type FaultConfig struct {
	// Rates maps each fault type to its injection probability (0.0–1.0).
	Rates map[FaultType]float64
}

// Preset fault profiles.
var (
	// FaultsNone disables all fault injection (pure stress test).
	FaultsNone = &FaultConfig{}

	// FaultsRetryHeavy injects commit-unknown at 5% rate.
	FaultsRetryHeavy = &FaultConfig{Rates: map[FaultType]float64{
		FaultCommitUnknown: 0.05,
	}}

	// FaultsRetryVeryHeavy injects commit-unknown at 20% rate.
	FaultsRetryVeryHeavy = &FaultConfig{Rates: map[FaultType]float64{
		FaultCommitUnknown: 0.20,
	}}

	// FaultsAll injects all fault types at moderate rates.
	FaultsAll = &FaultConfig{Rates: map[FaultType]float64{
		FaultCommitUnknown:     0.03,
		FaultConflict:          0.03,
		FaultTransactionTooOld: 0.02,
	}}
)

// FaultLogEntry records a single injected fault for reproducibility.
type FaultLogEntry struct {
	OpIndex int
	Fault   FaultType
}

// ChaosTransactor wraps an fdb.Transactor to inject faults at the
// transaction boundary. It implements fdb.Transactor so it can be
// used with NewFDBDatabaseWithTransactor.
type ChaosTransactor struct {
	inner  fdb.Transactor
	faults *FaultConfig
	rng    *rand.Rand
	mu     sync.Mutex

	// pendingFault is set by InjectOnce — fires on the next Transact, then clears.
	pendingFault *FaultType

	// readErrorPrefix scopes FaultReadError to keys under this prefix. Nil means
	// every read fails.
	readErrorPrefix []byte

	// opIndex tracks the transaction sequence number for logging.
	opIndex int

	// Log records all injected faults for post-mortem analysis.
	Log []FaultLogEntry
}

// NewChaosTransactor wraps an existing transactor with fault injection.
func NewChaosTransactor(inner fdb.Transactor, faults *FaultConfig, seed uint64) *ChaosTransactor {
	return &ChaosTransactor{
		inner:  inner,
		faults: faults,
		rng:    rand.New(rand.NewPCG(seed, 0)),
	}
}

// InjectOnce schedules a specific fault to fire on the next Transact call.
// The fault fires exactly once and then clears. Use for targeted tests.
func (c *ChaosTransactor) InjectOnce(fault FaultType) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pendingFault = &fault
}

// InjectReadErrorOnce schedules FaultReadError for the next Transact call: every
// read of a key starting with keyPrefix fails with a non-retryable FDB error. A nil
// prefix fails every read. The fault fires exactly once and then clears.
func (c *ChaosTransactor) InjectReadErrorOnce(keyPrefix []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fault := FaultReadError
	c.pendingFault = &fault
	c.readErrorPrefix = keyPrefix
}

// Transact implements fdb.Transactor. Wraps the inner Transact with fault injection.
func (c *ChaosTransactor) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	ctx, cancel := chaosOpContext()
	defer cancel()
	return c.TransactCtx(ctx, fn)
}

// TransactCtx implements fdb.CtxTransactor — same fault injection, threading ctx to the
// inner transactor's ctx-aware path when present (RFC-090).
func (c *ChaosTransactor) TransactCtx(ctx context.Context, fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	runInner := func(wrap func(fdb.WritableTransaction) fdb.WritableTransaction) (any, error) {
		call := fn
		if wrap != nil {
			call = func(tr fdb.WritableTransaction) (any, error) { return fn(wrap(tr)) }
		}
		if ct, ok := c.inner.(fdb.CtxTransactor); ok {
			return ct.TransactCtx(ctx, call)
		}
		return c.inner.Transact(call)
	}
	c.mu.Lock()
	pending := c.pendingFault
	readErrorPrefix := c.readErrorPrefix
	if pending != nil {
		// The scope belongs to the scheduled fault, so it clears with it — a stale
		// prefix would silently narrow a later randomly-injected read failure.
		c.pendingFault = nil
		c.readErrorPrefix = nil
	}
	opIdx := c.opIndex
	c.opIndex++
	c.mu.Unlock()

	// Determine which fault to inject (at most one per transaction).
	var injectFault *FaultType
	if pending != nil {
		injectFault = pending
	} else {
		for _, ft := range []FaultType{FaultCommitUnknown, FaultConflict, FaultTransactionTooOld, FaultReadError} {
			if c.shouldInject(ft) {
				f := ft
				injectFault = &f
				break
			}
		}
	}

	// FaultReadError is the one fault that fires DURING the transaction rather than
	// at its boundary: the transaction handed to fn fails the reads it issues, so
	// there is nothing to commit and nothing to re-execute.
	if injectFault != nil && *injectFault == FaultReadError {
		c.logFault(opIdx, FaultReadError)
		return runInner(func(tr fdb.WritableTransaction) fdb.WritableTransaction {
			return &readErrorTransaction{WritableTransaction: tr, prefix: readErrorPrefix}
		})
	}

	// Execute the real transaction.
	result, err := runInner(nil)
	if err != nil {
		return result, err
	}

	// Remaining fault types: commit succeeded, then re-execute fn in a new
	// transaction. The second execution sees effects of the first.
	// This tests idempotency — the hardest property to get right.
	if injectFault != nil {
		c.logFault(opIdx, *injectFault)
		return runInner(nil)
	}

	return result, nil
}

// ReadTransact implements fdb.ReadTransactor. No fault injection on reads.
func (c *ChaosTransactor) ReadTransact(fn func(fdb.ReadTransaction) (any, error)) (any, error) {
	ctx, cancel := chaosOpContext()
	defer cancel()
	return c.ReadTransactCtx(ctx, fn)
}

// ReadTransactCtx implements fdb.CtxReadTransactor (threads ctx to the inner read path).
func (c *ChaosTransactor) ReadTransactCtx(ctx context.Context, fn func(fdb.ReadTransaction) (any, error)) (any, error) {
	if ct, ok := c.inner.(fdb.CtxReadTransactor); ok {
		return ct.ReadTransactCtx(ctx, fn)
	}
	return c.inner.ReadTransact(fn)
}

// shouldInject checks if a fault should be randomly injected based on configured rates.
func (c *ChaosTransactor) shouldInject(fault FaultType) bool {
	if c.faults == nil || c.faults.Rates == nil {
		return false
	}
	rate, ok := c.faults.Rates[fault]
	if !ok || rate <= 0 {
		return false
	}
	c.mu.Lock()
	r := c.rng.Float64()
	c.mu.Unlock()
	return r < rate
}

func (c *ChaosTransactor) logFault(opIdx int, fault FaultType) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Log = append(c.Log, FaultLogEntry{OpIndex: opIdx, Fault: fault})
}
