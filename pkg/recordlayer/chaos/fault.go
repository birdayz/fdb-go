package chaos

import (
	"math/rand/v2"
	"sync"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
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
func (c *ChaosTransactor) Transact(fn func(fdb.Transaction) (interface{}, error)) (interface{}, error) {
	c.mu.Lock()
	pending := c.pendingFault
	if pending != nil {
		c.pendingFault = nil
	}
	opIdx := c.opIndex
	c.opIndex++
	c.mu.Unlock()

	// Determine if we should inject commit-unknown
	injectCommitUnknown := (pending != nil && *pending == FaultCommitUnknown) ||
		c.shouldInject(FaultCommitUnknown)

	// Execute the real transaction
	result, err := c.inner.Transact(fn)
	if err != nil {
		return result, err
	}

	// Post-commit fault: commit-unknown simulation
	if injectCommitUnknown {
		c.logFault(opIdx, FaultCommitUnknown)
		// Commit succeeded, but simulate client getting error 1021.
		// Re-execute fn in a new transaction. The second execution
		// sees the effects of the first commit. If fn is idempotent,
		// this is a no-op. If not (e.g., COUNT atomic ADD), state corrupts.
		return c.inner.Transact(fn)
	}

	return result, nil
}

// ReadTransact implements fdb.ReadTransactor. No fault injection on reads.
func (c *ChaosTransactor) ReadTransact(fn func(fdb.ReadTransaction) (interface{}, error)) (interface{}, error) {
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
