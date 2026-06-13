package recordlayer

import (
	"context"
	"errors"
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/vectorcodec"
)

// spfreshRefineScanBatch bounds the membership-key snapshot scan per tx;
// spfreshRefineMoveBatch bounds pks re-evaluated+moved per move tx (each move
// is a sidecar read + a route + posting writes, so keep it well under the 5 s /
// 10 MB limits — the NPA move budget shape).
const (
	spfreshRefineScanBatch = 5000
	spfreshRefineMoveBatch = 100
)

// spfreshRefineAll is the RFC-104 validation prototype: it re-evaluates the
// closure copy-set of EVERY live vector against the CURRENT (converged) topology
// and atomically moves the ones whose set changed — generalizing the NPA
// reassignment (spfresh_npa.go) from split neighborhoods to the global
// population. It recovers the closure replication that fast ingest never fired
// (RFC-104 §Problem: a vector closure-replicated against the coarse
// insertion-time topology lands at 1.0× and is never re-evaluated as the
// topology refines).
//
// This is VALIDATION-ONLY — a one-shot full pass with no budget / round-robin
// cursor / lifecycle integration. Those are the production op (built only once
// this prototype proves recall recovers). Each pk's move reuses NPA's per-pk
// fence: a REAL read of the pk's MEMBERSHIP in the moving tx, so a concurrent
// update/delete aborts one side at the resolver. Idempotent: a pk already
// holding its optimal copy-set is a no-op.
//
// Returns the number of pks moved.
func spfreshRefineAll(ctx context.Context, db *FDBDatabase, s *spfreshStorage, config SPFreshConfig) (int, error) {
	// 1. Collect every live pk from the membership keyspace (batched snapshot
	//    scans — one unbounded read blows the 5 s tx limit at scale).
	var pks []tuple.Tuple
	mr, err := fdb.PrefixRange(s.membership.Bytes())
	if err != nil {
		return 0, fmt.Errorf("spfresh refine: membership range: %w", err)
	}
	begin := mr.Begin
	for {
		var nextBegin fdb.Key
		if rerr := spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
			nextBegin = nil
			kvs, kerr := rtx.Transaction().Snapshot().GetRange(
				fdb.KeyRange{Begin: begin, End: mr.End},
				fdb.RangeOptions{Limit: spfreshRefineScanBatch, Mode: fdb.StreamingModeWantAll},
			).GetSliceWithError()
			if kerr != nil {
				return kerr
			}
			for _, kv := range kvs {
				pk, uerr := s.membership.Unpack(kv.Key)
				if uerr != nil {
					return fmt.Errorf("spfresh refine: unpack membership key: %w", uerr)
				}
				pks = append(pks, pk)
			}
			if len(kvs) == spfreshRefineScanBatch {
				last := kvs[len(kvs)-1].Key
				nextBegin = append(append(fdb.Key{}, last...), 0x00) // after the last key
			}
			return nil
		}); rerr != nil {
			return 0, rerr
		}
		if nextBegin == nil {
			break
		}
		begin = nextBegin
	}

	// 2. Load the current topology into a routing cache once (read-only during
	//    refinement; the fill is drained so the topology is stable).
	cache := newSPFreshRoutingCache(0)
	if rerr := spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
		return cache.fullReload(rtx.Transaction(), s, s.generation)
	}); rerr != nil {
		return 0, fmt.Errorf("spfresh refine: load routing cache: %w", rerr)
	}

	// 3. Move pass: per-pk atomic closure re-evaluation against the cache,
	//    batched (the NPA move shape, but the candidate pool comes from ROUTING
	//    the vector — discovering fines the insertion-time topology rejected —
	//    not from current ∪ split-children).
	quantizer := newSPFreshQuantizer(config)
	kc := config.BuildAssignCells * 2
	if kc < 64 {
		kc = 64
	}
	moved := 0
	for lo := 0; lo < len(pks); lo += spfreshRefineMoveBatch {
		hi := min(lo+spfreshRefineMoveBatch, len(pks))
		batch := pks[lo:hi]
		batchMoved := 0
		if rerr := spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
			batchMoved = 0
			tx := rtx.Transaction()
			for _, pk := range batch {
				// Per-pk serialization fence: REAL read of membership.
				current, merr := spfreshReadMembership(tx, s, pk)
				if merr != nil {
					if errors.Is(merr, errSPFreshNotFound) {
						continue // deleted since the scan
					}
					return merr
				}
				// The vector lives in the fp16 sidecar (the exact re-rank store).
				sc, gerr := tx.Snapshot().Get(s.sidecarKey(pk)).Get()
				if gerr != nil {
					return gerr
				}
				if sc == nil {
					continue // no sidecar: nothing to re-evaluate against
				}
				vec, derr := vectorcodec.Deserialize(sc)
				if derr != nil {
					return derr
				}
				// Candidate pool = the vector ROUTED against the current topology
				// (the kc nearest ACTIVE/SEALED fines over its w nearest cells).
				routed, rerr := cache.route(tx, s, vec, config.BuildAssignCells, kc)
				if rerr != nil {
					return rerr
				}
				if len(routed) == 0 {
					continue
				}
				pool := make([]spfreshCandidate, 0, len(routed))
				vecByID := make(map[int64][]float64, len(routed))
				for _, r := range routed {
					pool = append(pool, spfreshCandidate{id: r.fineID, d2: r.d2, vec: r.vec})
					vecByID[r.fineID] = r.vec
				}
				spfreshSortCandidates(pool)
				kept := spfreshClosure(pool, config.Replication, config.Alpha)
				newSet := make([]int64, 0, len(kept))
				for _, k := range kept {
					newSet = append(newSet, k.id)
				}
				if spfreshSameIDSet(current, newSet) {
					continue // already optimal: idempotent no-op
				}
				keep := make(map[int64]bool, len(newSet))
				for _, id := range newSet {
					keep[id] = true
				}
				for _, id := range current {
					if !keep[id] {
						tx.Clear(s.postingKey(id, pk))
						spfreshCounterAdd(tx, s, spfreshCounterFine, id, -1)
					}
				}
				cur := make(map[int64]bool, len(current))
				for _, id := range current {
					cur[id] = true
				}
				for _, id := range newSet {
					if !cur[id] {
						residual := make([]float64, len(vec))
						for d := range vec {
							residual[d] = vec[d] - vecByID[id][d]
						}
						tx.Set(s.postingKey(id, pk), quantizer.Encode(residual))
						spfreshCounterAdd(tx, s, spfreshCounterFine, id, 1)
					}
				}
				tx.Set(s.membershipKey(pk), encodeMembership(newSet))
				batchMoved++
			}
			return nil
		}); rerr != nil {
			return moved, fmt.Errorf("spfresh refine: move batch at %d: %w", lo, rerr)
		}
		moved += batchMoved
	}
	return moved, nil
}

// RefineSPFreshIndexAll is the exported test/bench entry for the RFC-104
// refinement-recovery validation: one full pass refining every vector of the
// index against the current topology. Production callers will use the budgeted
// online op, not this.
func RefineSPFreshIndexAll(ctx context.Context, db *FDBDatabase, storeBuilder func(*FDBRecordContext) (*FDBRecordStore, error), indexName string) (int, error) {
	var s *spfreshStorage
	var config SPFreshConfig
	if err := spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
		store, serr := storeBuilder(rtx)
		if serr != nil {
			return serr
		}
		index := store.GetMetaData().GetIndex(indexName)
		if index == nil {
			return fmt.Errorf("spfresh refine: index %q not found", indexName)
		}
		config = parseSPFreshConfig(index)
		gen, gerr := spfreshReadGenerationSnapshot(rtx.Transaction(), newSPFreshStorage(store.indexSubspace(index), 0))
		if gerr != nil {
			return gerr
		}
		s = newSPFreshStorage(store.indexSubspace(index), gen)
		return nil
	}); err != nil {
		return 0, err
	}
	return spfreshRefineAll(ctx, db, s, config)
}
