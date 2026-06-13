package recordlayer

import (
	"context"
	"errors"
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
)

// The multi-tenant maintenance sweeper (RFC-094 §6 deployment shape, scaled
// out): SPFresh maintenance is caller-driven — something must call
// RebalanceSPFreshIndex per index — and at fleet scale that something is a
// sweeper worker iterating tenants: find indexes with pending task rows, do
// a bounded amount of their work, move on. Concurrent sweepers are safe by
// construction (unique lease owners, task-level exclusion — overlapping
// workers waste scans, nothing more), so a fleet can shard a tenant list or
// simply run the same one everywhere; sharding wastes less.

// SPFreshTenant names one index a sweeper drives: the store (the caller owns
// tenant discovery — store layout is application keyspace, not enumerable
// from inside the record layer) and the index within it.
type SPFreshTenant struct {
	StoreBuilder func(*FDBRecordContext) (*FDBRecordStore, error)
	IndexName    string
}

// SPFreshSweepOptions tunes one sweep pass.
type SPFreshSweepOptions struct {
	// MaxRoundsPerTenant bounds the scan-and-act rounds per tenant per
	// pass. 0 means the default (8).
	MaxRoundsPerTenant int
	// MaxActionsPerTenant is the per-tenant fairness budget: at most this
	// many lifecycle actions per pass, ENFORCED WITHIN a round too — a
	// whale tenant whose single scan finds thousands of independent task
	// rows must not monopolize the pass (codex MT P2). Undrained tenants
	// are reported, not errored — the next pass continues them. 0 means
	// the default (64).
	MaxActionsPerTenant int

	// Timer collects SPFresh maintenance instrumentation (per-kind action
	// counts, zombie cleanups, lease skips — see spfresh_metrics.go) across
	// the pass. Nil disables recording.
	Timer *StoreTimer
}

// SPFreshSweepResult summarizes one sweep pass.
type SPFreshSweepResult struct {
	Actions   int // lifecycle actions executed across all tenants
	Worked    int // tenants that had pending maintenance
	Undrained int // tenants whose budget ran out with work remaining
}

// SweepSPFreshIndexes makes ONE bounded pass over the tenants: a cheap
// pending-work probe per tenant (two snapshot reads), then a round-budgeted
// rebalance for the tenants that need it. Per-tenant failures are collected,
// not fatal — one corrupt tenant must not halt fleet maintenance; the pass
// continues and the joined error reports every failure. Callers loop passes
// on their own cadence and stop idling tenants however they like (the result
// says whether anything happened).
func SweepSPFreshIndexes(ctx context.Context, db *FDBDatabase, tenants []SPFreshTenant, opts SPFreshSweepOptions) (SPFreshSweepResult, error) {
	rounds := opts.MaxRoundsPerTenant
	if rounds <= 0 {
		rounds = 8
	}
	actions := opts.MaxActionsPerTenant
	if actions <= 0 {
		actions = 64
	}
	var result SPFreshSweepResult
	var errs []error
	for _, tenant := range tenants {
		if ctx.Err() != nil {
			return result, errors.Join(append(errs, ctx.Err())...)
		}
		pending, err := SPFreshHasPendingMaintenance(ctx, db, tenant.StoreBuilder, tenant.IndexName)
		if err != nil {
			errs = append(errs, fmt.Errorf("sweep %q: probe: %w", tenant.IndexName, err))
			continue
		}
		if !pending {
			continue
		}
		result.Worked++
		tenantActions, drained, err := rebalanceSPFreshIndexRounds(ctx, db, tenant.StoreBuilder, tenant.IndexName, rounds, actions, opts.Timer)
		result.Actions += tenantActions
		if err != nil {
			errs = append(errs, fmt.Errorf("sweep %q: %w", tenant.IndexName, err))
			continue
		}
		if !drained {
			result.Undrained++
		}
	}
	return result, errors.Join(errs...)
}

// SPFreshHasPendingMaintenance reports whether the index has any pending
// maintenance task rows: one snapshot generation read + one limit-1 snapshot
// range read — the cheap probe that lets a sweeper skip idle tenants without
// paying a full task scan. Un-bootstrapped indexes have no work by
// definition.
func SPFreshHasPendingMaintenance(ctx context.Context, db *FDBDatabase, storeBuilder func(*FDBRecordContext) (*FDBRecordStore, error), indexName string) (bool, error) {
	pending := false
	err := spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
		pending = false
		store, serr := storeBuilder(rtx)
		if serr != nil {
			return serr
		}
		index := store.GetMetaData().GetIndex(indexName)
		if index == nil {
			return fmt.Errorf("spfresh sweep: index %q not found", indexName)
		}
		if index.Type != IndexTypeVectorSPFresh {
			return fmt.Errorf("spfresh sweep: index %q has type %q", indexName, index.Type)
		}
		indexSubspace := store.indexSubspace(index)
		tx := rtx.Transaction()
		gen, gerr := spfreshReadGenerationSnapshot(tx, newSPFreshStorage(indexSubspace, 0))
		if gerr != nil {
			if errors.Is(gerr, errSPFreshNotFound) {
				return nil // never bootstrapped: no work
			}
			return gerr
		}
		s := newSPFreshStorage(indexSubspace, gen)
		// One limit-1 range per LIVE task kind, issued in parallel.
		// Deliberately NOT one range over the whole tasks prefix: legacy
		// Cellfin (build bookkeeping) rows leaked by pre-cleanup builds
		// would read as pending forever — the rebalancer skips that kind,
		// so the sweeper would revisit the tenant every pass for zero
		// actions (codex MT P2). The rebalancer also clears such rows on
		// sight; this filter covers tenants nobody has rebalanced yet.
		kinds := spfreshLiveTaskKinds
		futures := make([]fdb.RangeResult, 0, len(kinds))
		for _, kind := range kinds {
			r, rerr := fdb.PrefixRange(s.tasks.Pack(tuple.Tuple{kind}))
			if rerr != nil {
				return rerr
			}
			futures = append(futures, tx.Snapshot().GetRange(r, fdb.RangeOptions{Limit: 1}))
		}
		for _, f := range futures {
			kvs, kerr := f.GetSliceWithError()
			if kerr != nil {
				return kerr
			}
			if len(kvs) > 0 {
				pending = true
				return nil
			}
		}
		return nil
	})
	return pending, err
}
