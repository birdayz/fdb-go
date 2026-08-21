package chaos

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"

	"fdb.dev/pkg/recordlayer"
)

// Scenario is the primary chaos testing primitive. It wraps a real FDB store
// with a model and optional fault injection, providing operations that
// update both and verification that they agree.
type Scenario struct {
	t        testing.TB
	chaosDB  *recordlayer.FDBDatabase // operations go through here (with faults)
	cleanDB  *recordlayer.FDBDatabase // verification goes through here (no faults)
	metadata *recordlayer.RecordMetaData
	sub      subspace.Subspace
	model    *StoreModel
	chaos    *ChaosTransactor
	opIndex  int
	Rng      *rand.Rand
	seed     uint64
}

// Option configures a Scenario.
type Option func(*scenarioConfig)

type scenarioConfig struct {
	seed   uint64
	faults *FaultConfig
}

// WithSeed sets the random seed for deterministic replay.
func WithSeed(seed uint64) Option {
	return func(c *scenarioConfig) {
		c.seed = seed
	}
}

// WithFaults sets the fault injection configuration.
func WithFaults(faults *FaultConfig) Option {
	return func(c *scenarioConfig) {
		c.faults = faults
	}
}

// NewScenario creates a new chaos testing scenario.
// Each scenario gets its own FDB subspace for isolation.
// By default, no faults are injected — use WithFaults() or InjectOnce().
func NewScenario(t testing.TB, realDB fdb.Database, metadata *recordlayer.RecordMetaData, opts ...Option) *Scenario {
	cfg := scenarioConfig{
		seed:   42,
		faults: FaultsNone,
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	chaosT := NewChaosTransactor(realDB, cfg.faults, cfg.seed)

	return &Scenario{
		t:        t,
		chaosDB:  recordlayer.NewFDBDatabaseWithTransactor(chaosT, realDB),
		cleanDB:  recordlayer.NewFDBDatabase(realDB),
		metadata: metadata,
		sub:      subspace.FromBytes(tuple.Tuple{t.Name()}.Pack()),
		model:    NewStoreModel(metadata),
		chaos:    chaosT,
		Rng:      rand.New(rand.NewPCG(cfg.seed, 0)),
		seed:     cfg.seed,
	}
}

// InjectOnce schedules a fault for the next operation's transaction.
// The fault fires exactly once, then clears.
func (s *Scenario) InjectOnce(fault FaultType) {
	s.chaos.InjectOnce(fault)
}

// opContext bounds a single chaos operation.
//
// WHY THIS IS BOUNDED AT ALL, since the obvious reading is that it is belt and
// braces. `Database.TransactCtx` retries "bounded only by
// SetTransactionTimeout/SetTransactionRetryLimit (default unbounded)", and no
// scenario sets either. On context.Background() a shared cluster that dies
// mid-suite therefore does not fail these ops -- it hangs them, one after
// another, until the package-level 15-minute alarm fires. That is where the
// observed cost came from: 14 tests each waiting out their own deadline and a
// panic whose stack names whichever test was unlucky, not the container.
//
// A bound turns that into a fast, TYPED context.DeadlineExceeded at the first
// op of every subsequent scenario. Typed matters: it is what lets a caller use
// errors.Is rather than matching on message text, which cannot be spoofed by an
// error that merely quotes the phrase.
//
// It deliberately does NOT try to distinguish "cluster died" from "cluster is
// slow". An earlier attempt did, by classifying error strings, and was removed:
// the classifier's own signature list was the only thing pinning it, one of its
// signatures could never be produced by any error in the tree, and a false
// positive would have replaced every later scenario's real diagnosis with a
// guess. Bounding is strictly weaker and strictly honest.
//
// 30s is far above any healthy op here: the whole suite is 228 top-level tests
// (229 `func Test*` minus TestMain) and runs in a MEASURED ~90s against a live
// container, with the slowest single scenario a few seconds. It is far below
// the 900s package alarm, so a dead cluster surfaces as fast failures rather
// than one timeout.
//
// An earlier version of this sentence said "229 tests" and "well under a
// minute". Both were wrong -- the population counted TestMain, and the runtime
// was off by 1.5x -- and it was the premise this bound rests on. It survived a
// sweep that fixed the same claim elsewhere because it wraps mid-phrase.
func (s *Scenario) opContext() (context.Context, context.CancelFunc) {
	return chaosOpContext()
}

// suiteCtx bounds the WHOLE package, and every op context derives from it.
//
// Bounding each op alone does not fix the cascade it was meant to fix. With
// -test.parallel=1 the tests run one after another, so a container that dies
// early makes each of the ~228 remaining scenarios pay its own full op timeout:
// at 30s apiece, THIRTY tests exhaust the 900s package alarm and the run ends
// exactly as it did before, in a timeout naming an arbitrary test.
//
// A shared budget caps the TOTAL rather than the per-op cost. Once it is spent
// every derived context is already done, so the remaining tests fail instantly
// instead of each waiting. No classifier and no inference: an expired deadline
// is an observed fact, not a guess about why.
//
// It is set by TestMain and left nil elsewhere, which is why every read goes
// through suiteContext() rather than touching the var.
var suiteCtx context.Context

// suiteContext returns the package budget, or Background when unset -- a
// non-test caller (there are none today) must not inherit a nil context.
func suiteContext() context.Context {
	if suiteCtx == nil {
		return context.Background()
	}
	return suiteCtx
}

// chaosOpContext bounds ONE FDB operation, under the suite budget above. Package-level because the bound has
// to cover paths that never build a Scenario -- RunConcurrent, the
// ChaosTransactor compatibility wrappers, verification reads and the SPFresh
// driver all reach FDB directly, and an unbounded one of those hangs exactly
// like an unbounded scenario op.
func chaosOpContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(suiteContext(), 30*time.Second)
}

// chaosRunContext bounds a whole multi-operation run rather than one op.
//
// The budget is DERIVED from the workload, not fixed. A fixed two minutes was
// tried and is wrong: ConcurrentConfig.Duration is caller-chosen and the
// documented soak example is five minutes, so the context would expire mid-run
// while the workers -- which watch their own wall clock, not this ctx -- kept
// calling db.Run against a cancelled context in a hot error loop, then failed
// final validation.
//
// The grace is FIXED, not scaled, and it does cover the post-run verification:
// validateSnapshot runs on THIS context, and only the inner scans build their
// own chaosRunContext(0). Nothing it covers grows with Duration -- setup and
// drain do not, and the verified record set is bounded by cfg.MaxPKs (default
// 50), not by how long the workload ran.
func chaosRunContext(workload time.Duration) (context.Context, context.CancelFunc) {
	const grace = 2 * time.Minute
	if workload < 0 {
		workload = 0
	}
	return context.WithTimeout(suiteContext(), workload+grace)
}

// SaveRecord saves a record to the store and updates the model.
// The transaction goes through the ChaosTransactor (fault injection).
// On success, the model is updated. On failure, the test fails.
func (s *Scenario) SaveRecord(msg proto.Message) {
	s.t.Helper()
	ctx, cancelOp := s.opContext()
	defer cancelOp()
	_, err := s.chaosDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := s.openStore(rtx)
		if err != nil {
			return nil, err
		}
		_, err = store.SaveRecord(msg)
		return nil, err
	})
	if err != nil {
		s.t.Fatalf("chaos: SaveRecord at op %d (seed=%d): %v", s.opIndex, s.seed, err)
	}
	s.model.Save(msg)
	s.opIndex++
}

// DeleteRecord deletes a record by primary key and updates the model.
func (s *Scenario) DeleteRecord(pk tuple.Tuple) {
	s.t.Helper()
	ctx, cancelOp := s.opContext()
	defer cancelOp()
	_, err := s.chaosDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := s.openStore(rtx)
		if err != nil {
			return nil, err
		}
		_, err = store.DeleteRecord(pk)
		return nil, err
	})
	if err != nil {
		s.t.Fatalf("chaos: DeleteRecord at op %d (seed=%d): %v", s.opIndex, s.seed, err)
	}
	s.model.Delete(pk)
	s.opIndex++
}

// DeleteAllRecords deletes all records and resets the model.
func (s *Scenario) DeleteAllRecords() {
	s.t.Helper()
	ctx, cancelOp := s.opContext()
	defer cancelOp()
	_, err := s.chaosDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := s.openStore(rtx)
		if err != nil {
			return nil, err
		}
		return nil, store.DeleteAllRecords()
	})
	if err != nil {
		s.t.Fatalf("chaos: DeleteAllRecords at op %d (seed=%d): %v", s.opIndex, s.seed, err)
	}
	s.model.DeleteAll()
	s.opIndex++
}

// Verify compares the model against the actual store state.
// Uses the clean DB (no fault injection) to avoid spurious failures.
// Fails the test if any violations are found.
func (s *Scenario) Verify() {
	s.t.Helper()
	ctx, cancelOp := s.opContext()
	defer cancelOp()
	result, err := s.cleanDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := s.openStore(rtx)
		if err != nil {
			return nil, err
		}
		return Verify(store, s.model), nil
	})
	if err != nil {
		s.t.Fatalf("chaos: Verify at op %d (seed=%d): %v", s.opIndex, s.seed, err)
	}
	violations, _ := result.([]Violation)
	if len(violations) > 0 {
		msg := fmt.Sprintf("chaos: %d violation(s) at op %d (seed=%d):\n", len(violations), s.opIndex, s.seed)
		for _, v := range violations {
			msg += fmt.Sprintf("  - %s\n", v)
		}
		if len(s.chaos.Log) > 0 {
			msg += "fault log:\n"
			for _, entry := range s.chaos.Log {
				msg += fmt.Sprintf("  - op %d: fault %d\n", entry.OpIndex, entry.Fault)
			}
		}
		s.t.Fatal(msg)
	}
}

// openStore creates or opens the store within a transaction.
func (s *Scenario) openStore(rtx *recordlayer.FDBRecordContext) (*recordlayer.FDBRecordStore, error) {
	return recordlayer.NewStoreBuilder().
		SetContext(rtx).
		SetMetaDataProvider(s.metadata).
		SetSubspace(s.sub).
		CreateOrOpen()
}

// TrySaveRecord attempts to save a record and returns the error (if any).
// Unlike SaveRecord, it does NOT call t.Fatal on error — the caller handles it.
// Model is only updated on success.
func (s *Scenario) TrySaveRecord(msg proto.Message) error {
	s.t.Helper()
	ctx, cancelOp := s.opContext()
	defer cancelOp()
	_, err := s.chaosDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := s.openStore(rtx)
		if err != nil {
			return nil, err
		}
		_, err = store.SaveRecord(msg)
		return nil, err
	})
	if err == nil {
		s.model.Save(msg)
	}
	s.opIndex++
	return err
}

// FaultLog returns the list of injected faults so far.
func (s *Scenario) FaultLog() []FaultLogEntry {
	return s.chaos.Log
}

// Seed returns the scenario's random seed.
func (s *Scenario) Seed() uint64 {
	return s.seed
}

// mustOpCtx is chaosOpContext for call sites that pass a context straight into
// a helper and have nowhere to hang the cancel func.
//
// Leaking the cancel is deliberate and bounded: the context is
// garbage-collected when its timer fires, these are test-only paths, and the
// alternative -- leaving context.Background() -- is the unbounded hang this
// whole change exists to remove. A leaked 30s timer per SPFresh maintenance
// call is strictly cheaper than a suite that waits out its package alarm.
func mustOpCtx() context.Context {
	ctx, _ := chaosOpContext() //nolint:govet // see above: bounded, test-only
	return ctx
}
