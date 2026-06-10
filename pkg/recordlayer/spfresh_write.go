package recordlayer

import (
	"errors"
	"fmt"
	"hash/fnv"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/vectorcodec"
)

// SPFresh foreground write path (RFC-094 §5), running inside the caller's
// record-save transaction. The conflict story, restated at the site that
// depends on it:
//
//   - the generation read is REAL — a build flip committing mid-insert aborts
//     this write at the resolver (it must re-route into the new generation);
//     inserts never conflict each other on it (read-read).
//   - fine-centroid STATE reads are REAL — a split SEALing the centroid aborts
//     a straggler insert; an insert that commits first is in the frozen
//     posting the split reads (RFC-094 §6, sound both directions).
//   - the MEMBERSHIP read is REAL — the same-pk serialization point between
//     concurrent writers of one record and between writers and split moves.
//   - posting/sidecar/membership writes are BLIND, counters are atomic ADDs —
//     inserts of distinct pks into the same posting never conflict each other.
//   - the split/merge probes are SAMPLED snapshot counter reads; the trigger
//     write is a REAL-read Set-if-absent on the deterministic task key
//     (the conflict range is the point — a live claim must not be clobbered).

const (
	// spfreshInsertProbeCells is the routing width (cells) for inserts — wide
	// enough to surface r·closure candidates plus non-ACTIVE fallbacks without
	// paying the query path's full w.
	spfreshInsertProbeCells = 8
	// spfreshInsertCandidates is the fine-candidate pool: the closure picks at
	// most Replication from the front; the tail is the next-nearest fallback
	// supply when fenced candidates turn out SEALED/FORWARD/absent.
	spfreshInsertCandidates = 16
	// spfreshProbeSampleEvery samples 1-in-N writes for the counter probe
	// (RFC-094 §5: probes are sampled so the trigger key never becomes a
	// per-write hot read).
	spfreshProbeSampleEvery = 8
)

// spfreshWriteContext carries the per-write resolved state.
type spfreshWriteContext struct {
	storage *spfreshStorage
	cache   *spfreshRoutingCache
}

// spfreshResolveForWrite resolves the readable generation with the REAL-read
// write fence and ensures the routing cache is loaded. No readable generation
// means the index was never built: the 094.x contract is build-then-write.
func (m *spfreshIndexMaintainer) spfreshResolveForWrite() (*spfreshWriteContext, error) {
	metaStorage := newSPFreshStorage(m.indexSubspace, 0)
	gen, err := spfreshReadGenerationForWrite(m.tx, metaStorage)
	if err != nil {
		if errors.Is(err, errSPFreshNotFound) {
			return nil, fmt.Errorf("spfresh index %q: no readable generation — bulk-build the index before writing records (RFC-094 build-then-write)", m.index.Name)
		}
		return nil, err
	}
	storage := newSPFreshStorage(m.indexSubspace, gen)
	cache := spfreshCacheFor(m.indexSubspace, gen)
	if !cache.ready(gen) {
		if err := cache.fullReload(m.tx, storage, gen); err != nil {
			return nil, fmt.Errorf("spfresh index %q: routing reload: %w", m.index.Name, err)
		}
	}
	return &spfreshWriteContext{storage: storage, cache: cache}, nil
}

// spfreshInsert indexes one (pk, vector): route on cache → closure copy-set →
// REAL state fence per chosen centroid (non-ACTIVE/absent drops to the
// next-nearest) → membership/posting/sidecar writes + counter ADDs → sampled
// split probe. An existing membership row (update) is cleared from keys
// derived from this same-tx read.
func (m *spfreshIndexMaintainer) spfreshInsert(wc *spfreshWriteContext, pk tuple.Tuple, vec []float64) error {
	routed, err := wc.cache.routeForWrite(m.tx, wc.storage, vec, spfreshInsertProbeCells, spfreshInsertCandidates)
	if err != nil {
		return fmt.Errorf("spfresh index %q: route insert: %w", m.index.Name, err)
	}
	return m.spfreshInsertRouted(wc.storage, routed, pk, vec)
}

// spfreshInsertRouted is the post-routing half of the insert; the WriteOnly
// staging path routes within a single FINALIZED cell instead of on the cache.
func (m *spfreshIndexMaintainer) spfreshInsertRouted(storage *spfreshStorage, routed []spfreshRouted, pk tuple.Tuple, vec []float64) error {
	// Fence: verify candidates ACTIVE with REAL state reads, in nearest-first
	// order, until Replication verified candidates are in hand. The cache said
	// ACTIVE; the authoritative row decides: SEALED/absent ⇒ drop and take the
	// next-nearest (RFC-094 §5 step 2); FORWARD ⇒ a stale cache routed us to a
	// split parent — FOLLOW its children from the authoritative row instead of
	// skipping, or an insert near a freshly split centroid fails with no
	// candidates until the cache reloads (codex 094.2 r1 P1). Worklist kept
	// d2-sorted as children are spliced in; visit budget bounds forward chains.
	// verified is kept d2-ASCENDING by sorted insertion: spfreshClosure
	// assumes nearest-first (verified[0] is its c1), and a followed FORWARD
	// child can be NEARER than an already-verified candidate — appending
	// would hand closure a wrong c1 and mis-assign the insert (codex 094.2
	// r2). The cutoff is sound for the same reason: stop only when the
	// sorted worklist's head can no longer improve the best Replication.
	verified := make([]spfreshCandidate, 0, m.config.Replication+2)
	vecs := make(map[int64][]float64, m.config.Replication)
	cells := make(map[int64]int64, m.config.Replication)
	work := append([]spfreshRouted(nil), routed...)
	seen := make(map[int64]bool, len(work))
	for examined := 0; len(work) > 0 && examined < 4*(len(routed)+2); examined++ {
		cand := work[0]
		work = work[1:]
		if seen[cand.fineID] {
			continue
		}
		seen[cand.fineID] = true
		if len(verified) >= m.config.Replication && cand.d2 >= verified[m.config.Replication-1].d2 {
			break
		}
		row, rerr := spfreshReadCentroidForWrite(m.tx, storage, cand.cellID, cand.fineID)
		if rerr != nil {
			if errors.Is(rerr, errSPFreshNotFound) {
				continue
			}
			return rerr
		}
		switch row.state {
		case spfreshStateActive:
			// verified below
		case spfreshStateForward:
			for _, childID := range []int64{row.childA, row.childB} {
				if childID == 0 || seen[childID] {
					continue
				}
				crow, cerr := spfreshReadCentroidForWrite(m.tx, storage, cand.cellID, childID)
				if cerr != nil {
					if errors.Is(cerr, errSPFreshNotFound) {
						continue
					}
					return cerr
				}
				cvec, cverr := crow.vector()
				if cverr != nil {
					return cverr
				}
				child := spfreshRouted{cellID: cand.cellID, fineID: childID, vec: cvec, d2: spfreshSquaredDistance(vec, cvec)}
				at := len(work)
				for i := range work {
					if child.d2 < work[i].d2 {
						at = i
						break
					}
				}
				work = append(work[:at], append([]spfreshRouted{child}, work[at:]...)...)
			}
			continue
		default:
			continue // SEALED/DEAD: next-nearest
		}
		cvec, verr := row.vector()
		if verr != nil {
			return verr
		}
		nc := spfreshCandidate{id: cand.fineID, d2: cand.d2}
		at := len(verified)
		for i := range verified {
			if nc.d2 < verified[i].d2 {
				at = i
				break
			}
		}
		verified = append(verified[:at], append([]spfreshCandidate{nc}, verified[at:]...)...)
		vecs[cand.fineID] = cvec
		cells[cand.fineID] = cand.cellID
	}
	if len(verified) == 0 {
		return fmt.Errorf("spfresh index %q: no ACTIVE fine centroid among %d routed candidates (routing cache stale beyond fallback depth)", m.index.Name, len(routed))
	}
	kept := spfreshClosure(verified, m.config.Replication, m.config.Alpha)

	// Same-pk serialization point: an existing copy-set means this is an
	// update — clear the old keys derived from THIS read (a split moving the
	// pk concurrently rewrites membership, so the resolver orders us).
	oldIDs, merr := spfreshReadMembership(m.tx, storage, pk)
	if merr != nil && !errors.Is(merr, errSPFreshNotFound) {
		return merr
	}
	for _, fineID := range oldIDs {
		m.tx.Clear(storage.postingKey(fineID, pk))
		spfreshCounterAdd(m.tx, storage, spfreshCounterFine, fineID, -1)
	}

	quantizer := newSPFreshQuantizer(m.config)
	fp16 := vectorcodec.SerializeHalf(vec)
	newIDs := make([]int64, 0, len(kept))
	for _, c := range kept {
		cvec := vecs[c.id]
		residual := make([]float64, len(vec))
		for d := range vec {
			residual[d] = vec[d] - cvec[d]
		}
		m.tx.Set(storage.postingKey(c.id, pk), quantizer.Encode(residual))
		spfreshCounterAdd(m.tx, storage, spfreshCounterFine, c.id, 1)
		newIDs = append(newIDs, c.id)
	}
	m.tx.Set(storage.membershipKey(pk), encodeMembership(newIDs))
	if m.config.Sidecar {
		m.tx.Set(storage.sidecarKey(pk), fp16)
	}

	// Sampled split probe (RFC-094 §5 step 2, trigger only — the consuming
	// rebalancer and the 4×Lmax inline split are 094.3). Deterministic by pk
	// hash so tests can pin it; the unconditional 2×Lmax branch bounds how far
	// an unlucky sampling run can overshoot before a trigger lands.
	for _, fineID := range newIDs {
		count, cerr := spfreshCounterReadSnapshot(m.tx, storage, spfreshCounterFine, fineID)
		if cerr != nil {
			return cerr
		}
		if count <= int64(m.config.Lmax) {
			continue
		}
		if spfreshSampledProbe(pk) || count > int64(2*m.config.Lmax) {
			if _, terr := spfreshTaskSetIfAbsent(m.tx, storage, spfreshTaskSplit, fineID); terr != nil {
				return terr
			}
		}
	}
	return nil
}

// spfreshDelete removes one pk: membership-driven (no tombstones, RFC-094 §5)
// — clear the posting copies named by the same-tx membership read, the
// sidecar, and the membership row; counter −1s; sampled merge probe. A pk
// with no membership row was never indexed: no-op.
func (m *spfreshIndexMaintainer) spfreshDelete(storage *spfreshStorage, pk tuple.Tuple) error {
	ids, err := spfreshReadMembership(m.tx, storage, pk)
	if err != nil {
		if errors.Is(err, errSPFreshNotFound) {
			return nil
		}
		return err
	}
	for _, fineID := range ids {
		m.tx.Clear(storage.postingKey(fineID, pk))
		spfreshCounterAdd(m.tx, storage, spfreshCounterFine, fineID, -1)
	}
	m.tx.Clear(storage.membershipKey(pk))
	m.tx.Clear(storage.sidecarKey(pk))

	if spfreshSampledProbe(pk) {
		for _, fineID := range ids {
			count, cerr := spfreshCounterReadSnapshot(m.tx, storage, spfreshCounterFine, fineID)
			if cerr != nil {
				return cerr
			}
			if count < int64(m.config.Lmin()) {
				if _, terr := spfreshTaskSetIfAbsent(m.tx, storage, spfreshTaskMerge, fineID); terr != nil {
					return terr
				}
			}
		}
	}
	return nil
}

// spfreshSampledProbe selects ~1-in-spfreshProbeSampleEvery pks for the
// counter probe. Hash-based, not random: a transaction retry must make the
// same decision, and tests pin probe behavior with chosen pks.
func spfreshSampledProbe(pk tuple.Tuple) bool {
	h := fnv.New64a()
	_, _ = h.Write(pk.Pack())
	return h.Sum64()%spfreshProbeSampleEvery == 0
}
