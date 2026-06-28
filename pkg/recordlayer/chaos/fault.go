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
)

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

// Transact implements fdb.Transactor. Wraps the inner Transact with fault injection.
func (c *ChaosTransactor) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return c.TransactCtx(context.Background(), fn)
}

// TransactCtx implements fdb.CtxTransactor — same fault injection, threading ctx to the
// inner transactor's ctx-aware path when present (RFC-090).
func (c *ChaosTransactor) TransactCtx(ctx context.Context, fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	runInner := func() (any, error) {
		if ct, ok := c.inner.(fdb.CtxTransactor); ok {
			return ct.TransactCtx(ctx, fn)
		}
		return c.inner.Transact(fn)
	}
	c.mu.Lock()
	pending := c.pendingFault
	if pending != nil {
		c.pendingFault = nil
	}
	opIdx := c.opIndex
	c.opIndex++
	c.mu.Unlock()

	// Determine which fault to inject (at most one per transaction).
	var injectFault *FaultType
	if pending != nil {
		injectFault = pending
	} else {
		for _, ft := range []FaultType{FaultCommitUnknown, FaultConflict, FaultTransactionTooOld} {
			if c.shouldInject(ft) {
				f := ft
				injectFault = &f
				break
			}
		}
	}

	// Execute the real transaction.
	result, err := runInner()
	if err != nil {
		return result, err
	}

	// All fault types: commit succeeded, then re-execute fn in a new
	// transaction. The second execution sees effects of the first.
	// This tests idempotency — the hardest property to get right.
	if injectFault != nil {
		c.logFault(opIdx, *injectFault)
		return runInner()
	}

	return result, nil
}

// ReadTransact implements fdb.ReadTransactor. No fault injection on reads.
func (c *ChaosTransactor) ReadTransact(fn func(fdb.ReadTransaction) (any, error)) (any, error) {
	return c.ReadTransactCtx(context.Background(), fn)
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
