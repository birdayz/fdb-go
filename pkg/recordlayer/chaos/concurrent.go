package chaos

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
)

// ConcurrentConfig controls concurrent chaos testing parameters.
type ConcurrentConfig struct {
	// Seed for the PRNG. Each worker gets seed + workerID.
	Seed uint64

	// Workers is the number of concurrent goroutines (default 4).
	Workers int

	// Duration is how long to run (default 5s).
	Duration time.Duration

	// MaxPKs bounds the primary key range [0, MaxPKs) (default 50).
	MaxPKs int64

	// ValidateEvery controls validation interval (default 1s).
	ValidateEvery time.Duration
}

// RunConcurrent runs concurrent chaos testing against a real FDB store.
// Multiple worker goroutines hammer the same subspace with random Save/Delete
// operations. A validator goroutine periodically takes a snapshot-consistent
// read and verifies that derived state (indexes, counts) matches the records.
//
// No ChaosTransactor — real FDB transaction conflicts provide the chaos.
// FDB conflict errors are expected and silently retried by Run().
func RunConcurrent(t testing.TB, realDB fdb.Database, metadata *recordlayer.RecordMetaData, cfg ConcurrentConfig) {
	t.Helper()

	// Apply defaults.
	if cfg.Workers <= 0 {
		cfg.Workers = 4
	}
	if cfg.Duration <= 0 {
		cfg.Duration = 5 * time.Second
	}
	if cfg.MaxPKs <= 0 {
		cfg.MaxPKs = 50
	}
	if cfg.ValidateEvery <= 0 {
		cfg.ValidateEvery = 1 * time.Second
	}

	db := recordlayer.NewFDBDatabase(realDB)
	sub := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
	ctx := context.Background()

	// Create the store once to initialize the header.
	_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		return recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(metadata).
			SetSubspace(sub).
			CreateOrOpen()
	})
	if err != nil {
		t.Fatalf("concurrent: failed to initialize store: %v", err)
	}

	// Track total operations and violations.
	var totalOps atomic.Int64
	var violationsMu sync.Mutex
	var allViolations []Violation

	// Signal workers to stop.
	deadline := time.Now().Add(cfg.Duration)

	// Worker goroutines.
	var wg sync.WaitGroup
	for w := 0; w < cfg.Workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(cfg.Seed+uint64(workerID), 0))

			for time.Now().Before(deadline) {
				pk := rng.Int64N(cfg.MaxPKs)
				r := rng.Float64()

				switch {
				case r < 0.6:
					// 60% saves
					price := rng.Int32N(10000)
					qty := rng.Int32N(100)
					_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
						store, err := openStore(rtx, metadata, sub)
						if err != nil {
							return nil, err
						}
						return store.SaveRecord(&gen.Order{
							OrderId:  proto.Int64(pk),
							Price:    proto.Int32(price),
							Quantity: proto.Int32(qty),
						})
					})
					if err != nil {
						// Conflict errors are retried by Run(); other errors are unexpected.
						t.Errorf("concurrent: worker %d save error: %v", workerID, err)
					}

				case r < 0.9:
					// 30% deletes
					_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
						store, err := openStore(rtx, metadata, sub)
						if err != nil {
							return nil, err
						}
						return store.DeleteRecord(tuple.Tuple{pk})
					})
					if err != nil {
						t.Errorf("concurrent: worker %d delete error: %v", workerID, err)
					}

				default:
					// 10% delete all
					_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
						store, err := openStore(rtx, metadata, sub)
						if err != nil {
							return nil, err
						}
						return nil, store.DeleteAllRecords()
					})
					if err != nil {
						t.Errorf("concurrent: worker %d delete-all error: %v", workerID, err)
					}
				}

				totalOps.Add(1)
			}
		}(w)
	}

	// Validator goroutine — periodically snapshot-validates the store.
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(cfg.ValidateEvery)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				if time.Now().After(deadline) {
					return
				}
				violations := validateSnapshot(ctx, db, metadata, sub)
				if len(violations) > 0 {
					violationsMu.Lock()
					allViolations = append(allViolations, violations...)
					violationsMu.Unlock()
				}
			default:
				if time.Now().After(deadline) {
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
	}()

	wg.Wait()

	// Final validation after all workers stop.
	finalViolations := validateSnapshot(ctx, db, metadata, sub)

	violationsMu.Lock()
	allViolations = append(allViolations, finalViolations...)
	violationsMu.Unlock()

	if len(allViolations) > 0 {
		msg := fmt.Sprintf("concurrent: %d violation(s) (seed=%d, workers=%d, ops=%d):\n",
			len(allViolations), cfg.Seed, cfg.Workers, totalOps.Load())
		for _, v := range allViolations {
			msg += fmt.Sprintf("  - %s\n", v)
		}
		t.Fatal(msg)
	}

	t.Logf("concurrent: completed %d ops across %d workers (seed=%d, duration=%s)",
		totalOps.Load(), cfg.Workers, cfg.Seed, cfg.Duration)
}

// validateSnapshot takes a snapshot-consistent read and verifies derived state.
// Returns violations (empty = consistent).
func validateSnapshot(ctx context.Context, db *recordlayer.FDBDatabase, metadata *recordlayer.RecordMetaData, sub subspace.Subspace) []Violation {
	result, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := openStore(rtx, metadata, sub)
		if err != nil {
			return nil, err
		}
		return VerifySnapshot(store, metadata), nil
	})
	if err != nil {
		return []Violation{{
			Invariant: "snapshot_validation_error",
			Expected:  "no error",
			Actual:    err.Error(),
		}}
	}
	violations, _ := result.([]Violation)
	return violations
}

// VerifySnapshot builds a model from the store's current records and verifies
// that derived state (indexes, counts) matches. Unlike Verify(), this does NOT
// require a pre-built model — it reconstructs one from the actual store data.
//
// Checks performed (snapshot-derivable only):
//   - Record count (GetRecordCount vs scanned records)
//   - VALUE index entries
//   - COUNT index values (recomputable from current records)
//   - SUM index values (recomputable from current records)
//   - RANK index entries
//   - PERMUTED_MIN/MAX index entries
//   - VERSION index entries
//   - Covering index value verification
//
// NOT checked (requires history tracking):
//   - COUNT_UPDATES (cumulative across all saves)
//   - MAX_EVER / MIN_EVER (needs full mutation history)
//   - MAX_EVER_VERSION (needs full mutation history)
func VerifySnapshot(store *recordlayer.FDBRecordStore, metadata *recordlayer.RecordMetaData) []Violation {
	model := buildModelFromStore(store, metadata)
	return verifySnapshotDerived(store, model)
}

// buildModelFromStore scans all records and builds a StoreModel.
func buildModelFromStore(store *recordlayer.FDBRecordStore, metadata *recordlayer.RecordMetaData) *StoreModel {
	model := NewStoreModel(metadata)
	ctx := context.Background()

	cursor := store.ScanRecords(nil, recordlayer.ForwardScan())
	defer func() { _ = cursor.Close() }()

	for {
		result, err := cursor.OnNext(ctx)
		if err != nil {
			break
		}
		if !result.HasNext() {
			break
		}
		rec := result.GetValue()

		// Insert directly into model.Records — bypass Save() to avoid
		// updating CountUpdates/MaxEver/MinEver which are history-dependent.
		pk := rec.PrimaryKey
		key := string(pk.Pack())
		model.Records[key] = &ModelRecord{
			PrimaryKey: pk,
			TypeName:   rec.RecordType.Name,
			Message:    proto.Clone(rec.Record),
		}
	}

	return model
}

// verifySnapshotDerived runs the subset of Verify checks that work with a
// snapshot-reconstructed model (no history-dependent checks).
func verifySnapshotDerived(store *recordlayer.FDBRecordStore, model *StoreModel) []Violation {
	var violations []Violation
	ctx := context.Background()

	// 1. Record count
	if model.metadata.GetRecordCountKey() != nil {
		actualCount, err := store.GetRecordCount()
		if err != nil {
			violations = append(violations, Violation{
				Invariant: "record_count_error",
				Expected:  "no error",
				Actual:    err.Error(),
			})
		} else if actualCount != model.Count() {
			violations = append(violations, Violation{
				Invariant: "record_count",
				Expected:  model.Count(),
				Actual:    actualCount,
			})
		}
	}

	// 2. VALUE index entries (includes covering index value verification)
	violations = append(violations, verifyValueIndexes(ctx, store, model)...)

	// 3. Snapshot-derivable atomic indexes: COUNT and SUM only.
	// COUNT_UPDATES, MAX/MIN_EVER are history-dependent — skip them.
	violations = append(violations, verifySnapshotAtomicIndexes(ctx, store, model)...)

	// 4. RANK index entries
	violations = append(violations, verifyRankIndexes(ctx, store, model)...)

	// 5. PERMUTED_MIN/MAX index entries
	violations = append(violations, verifyPermutedIndexes(ctx, store, model)...)

	// 6. VERSION index entries
	violations = append(violations, verifyVersionIndexes(ctx, store, model)...)

	return violations
}

// verifySnapshotAtomicIndexes checks only COUNT and SUM indexes (not COUNT_UPDATES
// or EVER indexes, which require mutation history).
func verifySnapshotAtomicIndexes(ctx context.Context, store *recordlayer.FDBRecordStore, model *StoreModel) []Violation {
	var violations []Violation
	md := model.metadata

	for _, idx := range md.GetAllIndexes() {
		switch idx.Type {
		case recordlayer.IndexTypeCount, recordlayer.IndexTypeCountNotNull:
			violations = append(violations, verifyCountIndex(ctx, store, model, md, idx)...)
		case recordlayer.IndexTypeSum:
			violations = append(violations, verifySumIndex(ctx, store, model, md, idx)...)
		}
	}

	return violations
}

// openStore creates or opens the store within a transaction context.
func openStore(rtx *recordlayer.FDBRecordContext, metadata *recordlayer.RecordMetaData, sub subspace.Subspace) (*recordlayer.FDBRecordStore, error) {
	return recordlayer.NewStoreBuilder().
		SetContext(rtx).
		SetMetaDataProvider(metadata).
		SetSubspace(sub).
		CreateOrOpen()
}
