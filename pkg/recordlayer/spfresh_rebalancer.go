package recordlayer

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
)

// The rebalancer (RFC-094 §6): the autonomous consumer of the trigger tasks
// the write path files. In-process on writers by default — applications (and
// tests) call RebalanceSPFreshIndex on their own cadence; a dedicated runner
// is just another caller. Claims serialize transactionally through the task
// rows' lease machinery, so any number of concurrent rebalancers is safe:
// a live foreign lease skips the task, an expired one is reclaimed (crash
// recovery), and every lifecycle step is idempotent under commit_unknown
// retries (see the per-lifecycle files).
//
// The §5 "inline split at the 4×Lmax hard ceiling" maps here too: the write
// path files the trigger unconditionally past 2×Lmax, and a writer that
// wants the RFC's synchronous behavior calls RebalanceSPFreshIndex after its
// save commits (the record context has no after-commit hook infrastructure
// yet; when it grows one, wiring it is a one-liner around this entry point).

// spfreshTaskRef is one scanned task: kind, id, and (for fine lifecycles)
// the cell resolved at scan time — staleness is fine, every lifecycle
// re-verifies authoritatively and treats absent-at-cell as a zombie.
type spfreshTaskRef struct {
	kind   int64
	id     int64
	cellID int64
}

// spfreshRebalanceOnce scans the task queue once and runs every actionable
// task, in lifecycle order (splits before NPA before merges — splits enqueue
// NPAs; merges of split children respect the cooldown anyway). Returns the
// number of tasks acted on; tasks under live foreign leases are skipped.
func spfreshRebalanceOnce(ctx context.Context, db *FDBDatabase, s *spfreshStorage, config SPFreshConfig, owner string, seed int64) (int, error) {
	// Scan (snapshot — the queue is advisory; claims are the authority).
	var refs []spfreshTaskRef
	err := spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
		refs = refs[:0]
		tx := rtx.Transaction()
		r, rerr := fdb.PrefixRange(s.tasks.Bytes())
		if rerr != nil {
			return rerr
		}
		kvs, gerr := tx.Snapshot().GetRange(r, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).GetSliceWithError()
		if gerr != nil {
			return gerr
		}
		for _, kv := range kvs {
			t, uerr := s.tasks.Unpack(kv.Key)
			if uerr != nil || len(t) != 2 {
				return fmt.Errorf("spfresh rebalance: malformed task key")
			}
			kind, kok := t[0].(int64)
			id, iok := t[1].(int64)
			if !kok || !iok {
				return fmt.Errorf("spfresh rebalance: malformed task key elements")
			}
			if kind == spfreshTaskCellfin {
				continue // build machinery, not a rebalancer concern
			}
			ref := spfreshTaskRef{kind: kind, id: id}
			if kind == spfreshTaskSplit || kind == spfreshTaskMerge {
				cellID, ferr := spfreshFindCentroidCell(tx, s, id)
				if ferr != nil {
					if errors.Is(ferr, errSPFreshNotFound) {
						// Centroid gone entirely: the lifecycle's zombie rule
						// will delete the task; give it cell 0 to land there.
						ref.cellID = 0
					} else {
						return ferr
					}
				} else {
					ref.cellID = cellID
				}
			}
			refs = append(refs, ref)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}

	// Lifecycle order: split (0) < NPA (4, after splits) < merge (1) <
	// coarse split (2). Within a kind, id order for determinism.
	order := map[int64]int{spfreshTaskSplit: 0, spfreshTaskNPA: 1, spfreshTaskMerge: 2, spfreshTaskCSplit: 3}
	sort.Slice(refs, func(i, j int) bool {
		if order[refs[i].kind] != order[refs[j].kind] {
			return order[refs[i].kind] < order[refs[j].kind]
		}
		return refs[i].id < refs[j].id
	})

	worked := 0
	for _, ref := range refs {
		switch ref.kind {
		case spfreshTaskSplit:
			out, serr := spfreshSealFine(ctx, db, s, owner, ref.cellID, ref.id)
			if serr != nil {
				return worked, serr
			}
			if !out.proceed {
				continue // zombie cleaned or foreign lease
			}
			if serr := spfreshSplitFine(ctx, db, s, config, owner, ref.cellID, ref.id, seed+ref.id); serr != nil {
				return worked, serr
			}
			worked++
		case spfreshTaskNPA:
			if nerr := spfreshNPARun(ctx, db, s, config, owner, ref.id); nerr != nil {
				return worked, nerr
			}
			worked++
		case spfreshTaskMerge:
			if merr := spfreshMergeFine(ctx, db, s, config, owner, ref.cellID, ref.id); merr != nil {
				return worked, merr
			}
			worked++
		case spfreshTaskCSplit:
			if cerr := spfreshCoarseSplit(ctx, db, s, config, owner, ref.id, seed+ref.id); cerr != nil {
				return worked, cerr
			}
			worked++
		}
	}
	return worked, nil
}

// RebalanceSPFreshIndex drains the index's task queue to quiescence: scan,
// act, repeat until a round acts on nothing (splits enqueue NPA follow-ups,
// so multiple rounds are normal). Bounded against pathological re-triggering.
// Returns the total number of lifecycle actions taken.
func RebalanceSPFreshIndex(ctx context.Context, db *FDBDatabase, storeBuilder func(*FDBRecordContext) (*FDBRecordStore, error), indexName string) (int, error) {
	var indexSubspace subspace.Subspace
	var config SPFreshConfig
	if err := spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
		store, serr := storeBuilder(rtx)
		if serr != nil {
			return serr
		}
		index := store.GetMetaData().GetIndex(indexName)
		if index == nil {
			return fmt.Errorf("spfresh rebalance: index %q not found", indexName)
		}
		if index.Type != IndexTypeVectorSPFresh {
			return fmt.Errorf("spfresh rebalance: index %q has type %q", indexName, index.Type)
		}
		config = parseSPFreshConfig(index)
		indexSubspace = store.indexSubspace(index)
		return nil
	}); err != nil {
		return 0, err
	}

	// The readable generation is the one being maintained.
	var gen int64
	if err := spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
		g, gerr := spfreshReadGenerationSnapshot(rtx.Transaction(), newSPFreshStorage(indexSubspace, 0))
		if gerr != nil {
			return fmt.Errorf("spfresh rebalance: no readable generation: %w", gerr)
		}
		gen = g
		return nil
	}); err != nil {
		return 0, err
	}
	s := newSPFreshStorage(indexSubspace, gen)

	owner := fmt.Sprintf("rebalance-%s", indexName)
	total := 0
	const maxRounds = 32
	var loopErr error
	for round := 0; round < maxRounds; round++ {
		worked, err := spfreshRebalanceOnce(ctx, db, s, config, owner, int64(round)*7919)
		if err != nil {
			return total, err
		}
		total += worked
		if worked == 0 {
			loopErr = nil
			break
		}
		loopErr = fmt.Errorf("spfresh rebalance: task queue did not quiesce in %d rounds (%d actions) — re-trigger loop?", maxRounds, total)
	}
	// GC: retired topology past the cooldown horizon (the same window that
	// guards split↔merge oscillation guards stale readers' grace period).
	if _, err := spfreshGCSweep(ctx, db, s, config, int64(config.CooldownSec)*1000); err != nil {
		return total, err
	}
	if total > 0 {
		// The rebalancer just changed the topology it routes on: refresh the
		// process-local cache eagerly (the §4 "maintainer timer" action —
		// other processes converge via the amortized query-path refresh).
		if err := spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
			return spfreshCacheFor(indexSubspace, gen).fullReload(rtx.Transaction(), s, gen)
		}); err != nil {
			return total, err
		}
	}
	return total, loopErr
}
