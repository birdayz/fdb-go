package chaos

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"

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

// SaveRecord saves a record to the store and updates the model.
// The transaction goes through the ChaosTransactor (fault injection).
// On success, the model is updated. On failure, the test fails.
func (s *Scenario) SaveRecord(msg proto.Message) {
	s.t.Helper()
	ctx := context.Background()
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
	ctx := context.Background()
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
	ctx := context.Background()
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
	ctx := context.Background()
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
	ctx := context.Background()
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
