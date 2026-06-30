package recordlayer

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"

	"google.golang.org/protobuf/proto"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/vectorcodec"
)

// spfreshIndexMaintainer is the record-layer integration of the SPFresh
// vector index (RFC-094). Phase 094.1 scope, stated honestly:
//
//   - the index is BUILD-THEN-READ: BuildSPFreshIndex bulk-builds a
//     generation from the store's existing records and flips it readable;
//   - foreground record writes are REJECTED while the index is present
//     (Update/UpdateWhileWriteOnly error) — the conflict-free foreground
//     write path is phase 094.2, the rebalancer 094.3 (RFC-094 §13);
//   - ScanByDistance serves kNN queries through the two-level routing cache
//     with the same TupleRange/IndexEntry contract as the HNSW index, so the
//     executor and (in 094.6) the planner reuse the existing vector surface.
type spfreshIndexMaintainer struct {
	standardIndexMaintainer
	config SPFreshConfig

	// rctx is the record context this maintainer lives in. The TX-LOCAL
	// routing cache (write path AND same-tx reads) lives in its session,
	// keyed by index subspace, so a write through one store instance and a
	// search through another in the same transaction share it — and the
	// process-global cache is never touched through a transaction carrying
	// uncommitted SPFresh topology (see txWriteCache).
	rctx *FDBRecordContext

	// timer is the context's StoreTimer (nil when uninstrumented — every
	// recording method is nil-receiver-safe). The TEXT index's
	// context.getTimer() idiom.
	timer *StoreTimer
}

func newSPFreshIndexMaintainer(
	index *Index,
	indexSubspace subspace.Subspace,
	tx fdb.WritableTransaction,
	store indexStoreContext,
	rctx *FDBRecordContext,
	timer *StoreTimer,
) (*spfreshIndexMaintainer, error) {
	config := parseSPFreshConfig(index)
	if err := ValidateSPFreshConfig(config); err != nil {
		return nil, fmt.Errorf("spfresh index %q: %w", index.Name, err)
	}
	if index.primaryKeyComponentPositions != nil {
		// The index stores TrimPrimaryKey'd tails; with PK components shared
		// into the index key the tail is not the full primary key, and
		// pinning it on scan entries would make the executor LoadRecord the
		// wrong key (codex 094.1 r2). Full-PK reconstruction lands with the
		// grouped/prefixed support; reject the shape until then.
		return nil, fmt.Errorf("spfresh index %q: primary-key components shared with the index key are not supported in 094.1", index.Name)
	}
	return &spfreshIndexMaintainer{
		standardIndexMaintainer: *newStandardIndexMaintainer(index, indexSubspace, tx, store),
		config:                  config,
		rctx:                    rctx,
		timer:                   timer,
	}, nil
}

// txWriteCache returns the transaction-local SPFresh routing cache for this
// index, or nil if this transaction has not written to it. Lives in the
// record context's session (keyed by index subspace) so a SaveRecord through
// one store instance and a ScanByDistance through another in the SAME
// transaction find the same cache — the maintainer instance is not the unit
// of transaction state, the context is.
func (m *spfreshIndexMaintainer) txWriteCache() *spfreshRoutingCache {
	if m.rctx == nil {
		return nil
	}
	if v := m.rctx.Session(m.writeCacheSessionKey()); v != nil {
		return v.(*spfreshRoutingCache)
	}
	return nil
}

func (m *spfreshIndexMaintainer) setTxWriteCache(c *spfreshRoutingCache) {
	if m.rctx != nil {
		m.rctx.PutSession(m.writeCacheSessionKey(), c)
	}
}

func (m *spfreshIndexMaintainer) writeCacheSessionKey() string {
	return "spfresh-writecache/" + string(m.indexSubspace.Bytes())
}

// spfreshCaches holds the per-process routing caches, one per (index
// subspace, generation) — the client-side state of RFC-094 §2. Browsing a
// retrained generation gets a fresh cache. Bounded ACROSS tenants: each
// cache is individually bounded (L2 LRU), but a multi-tenant serving fleet
// touches arbitrarily many indexes, so idle caches are evicted by TTL and
// the map is capped with oldest-first eviction (amortized inline — no
// background goroutine). Evicting a live pointer is safe: holders keep
// their snapshot; the next spfreshCacheFor reloads from FDB.
var (
	spfreshCaches        sync.Map     // string(subspace bytes)+gen -> *spfreshRoutingCache
	spfreshCacheCount    atomic.Int64 // approximate map size (eviction trigger)
	spfreshCacheForCalls atomic.Int64 // spfreshCacheFor call counter (sweep stride)
	spfreshCacheSweepMu  sync.Mutex   // single sweeper at a time (TryLock gate)

	// Eviction policy. Vars, not consts: the many-tenant soak tightens them.
	spfreshCacheIdleTTLMs   int64 = 15 * 60 * 1000 // idle eviction horizon
	spfreshCacheMaxEntries  int64 = 4096           // hard cap across tenants
	spfreshCacheSweepEveryN int64 = 256            // amortization stride
)

func spfreshCacheFor(indexSubspace subspace.Subspace, generation int64) *spfreshRoutingCache {
	key := fmt.Sprintf("%x/%d", indexSubspace.Bytes(), generation)
	now := spfreshNowMs()
	if c, ok := spfreshCaches.Load(key); ok {
		cache := c.(*spfreshRoutingCache)
		cache.lastUseMs.Store(now)
		spfreshMaybeEvictCaches(now)
		return cache
	}
	fresh := newSPFreshRoutingCache(0)
	fresh.lastUseMs.Store(now)
	c, loaded := spfreshCaches.LoadOrStore(key, fresh)
	if !loaded {
		spfreshCacheCount.Add(1)
	}
	cache := c.(*spfreshRoutingCache)
	cache.lastUseMs.Store(now)
	spfreshMaybeEvictCaches(now)
	return cache
}

// spfreshMaybeEvictCaches amortizes idle/over-cap eviction over cache hits:
// a full map sweep every spfreshCacheSweepEveryN calls, or immediately while
// the map is over its cap. At most ONE goroutine sweeps at a time — while
// over cap, every cache hit qualifies, and without the gate they would all
// run the full Range+sort concurrently until the first sweep lands.
func spfreshMaybeEvictCaches(nowMs int64) {
	if spfreshCacheForCalls.Add(1)%spfreshCacheSweepEveryN != 0 &&
		spfreshCacheCount.Load() <= spfreshCacheMaxEntries {
		return
	}
	if !spfreshCacheSweepMu.TryLock() {
		return // a concurrent sweep is already running
	}
	defer spfreshCacheSweepMu.Unlock()
	spfreshSweepCaches(nowMs)
}

// spfreshSweepCaches is the unamortized sweep body: TTL-evict idle caches,
// then enforce the cross-tenant cap oldest-first (with hysteresis).
func spfreshSweepCaches(nowMs int64) {
	type entry struct {
		key     string
		lastUse int64
	}
	var entries []entry
	spfreshCaches.Range(func(k, v any) bool {
		entries = append(entries, entry{key: k.(string), lastUse: v.(*spfreshRoutingCache).lastUseMs.Load()})
		return true
	})
	spfreshCacheCount.Store(int64(len(entries))) // re-sync the approximation
	evict := func(key string) {
		if _, loaded := spfreshCaches.LoadAndDelete(key); loaded {
			spfreshCacheCount.Add(-1)
		}
	}
	live := 0
	for _, e := range entries {
		if nowMs-e.lastUse > spfreshCacheIdleTTLMs {
			evict(e.key)
		} else {
			live++
		}
	}
	if int64(live) <= spfreshCacheMaxEntries {
		return
	}
	// Over cap even after TTL: drop oldest-first down to 90% of the cap
	// (hysteresis — evicting to the cap exactly would re-trigger a full
	// sweep on every subsequent miss while the fleet hovers at the cap).
	target := spfreshCacheMaxEntries - spfreshCacheMaxEntries/10
	sort.Slice(entries, func(i, j int) bool { return entries[i].lastUse < entries[j].lastUse })
	excess := int64(live) - target
	for _, e := range entries {
		if excess <= 0 {
			break
		}
		if nowMs-e.lastUse > spfreshCacheIdleTTLMs {
			continue // already evicted above
		}
		evict(e.key)
		excess--
	}
}

// Update is the foreground write path (RFC-094 §5, phase 094.2): delete-old
// then insert-new inside the caller's transaction. The insert re-reads
// membership itself, so a same-pk save (the common update) is correct either
// way; the explicit delete branch covers evaluated entries whose vector
// changed shape or vanished.
func (m *spfreshIndexMaintainer) Update(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	if oldRecord == nil && newRecord == nil {
		return nil
	}
	wc, err := m.spfreshResolveForWrite()
	if err != nil {
		return err
	}

	if oldRecord != nil {
		entries, eerr := m.evaluateIndex(oldRecord)
		if eerr != nil {
			return fmt.Errorf("evaluate spfresh index %q for old record: %w", m.index.Name, eerr)
		}
		for _, entry := range entries {
			// Delete is membership-driven by pk: it never needs the old vector
			// decoded (mirrors the HNSW delete contract — a record saved
			// unindexable by an older binary must stay deletable).
			pk, terr := m.index.TrimPrimaryKey(entry.primaryKey)
			if terr != nil {
				return terr
			}
			if derr := m.spfreshDelete(wc.storage, pk); derr != nil {
				return derr
			}
		}
	}

	if newRecord != nil {
		entries, eerr := m.evaluateIndex(newRecord)
		if eerr != nil {
			return fmt.Errorf("evaluate spfresh index %q for new record: %w", m.index.Name, eerr)
		}
		for _, entry := range entries {
			vec, verr := spfreshEntryVector(m.index, entry)
			if verr != nil {
				return verr
			}
			if vec == nil {
				continue // absent/null vector: unindexed
			}
			if len(vec) != m.config.NumDimensions {
				return fmt.Errorf("spfresh index %q: record vector has %d dimensions, index has %d", m.index.Name, len(vec), m.config.NumDimensions)
			}
			pk, terr := m.index.TrimPrimaryKey(entry.primaryKey)
			if terr != nil {
				return terr
			}
			if ierr := m.spfreshInsert(wc, pk, vec); ierr != nil {
				return ierr
			}
		}
	}
	return nil
}

// UpdateWhileWriteOnly is the §8 build/foreground staging interleaving: a
// record save during a first build routes itself by the build's visible
// progress. The window logic, with the fence each window depends on:
//
//   - readable generation present (post-flip, pre-MarkIndexReadable): the
//     build is done — the live Update path applies. The generation read is
//     REAL, so a flip committing mid-write aborts us and the retry goes live.
//   - no generation, no COARSE table (pre-coarse window): NO-OP — the
//     assignment scan runs strictly AFTER the coarse table commits, so its
//     read versions cover every record committed in this window.
//   - COARSE present, cellfin row not FINALIZED: stage (fp16 + sidecar) into
//     the nearest coarse cell — wave B's closing REAL read picks it up or
//     conflicts and re-runs. The cellfin read is REAL: it is the §8
//     straggler-side fence — a FINALIZED transition mid-flight aborts THIS
//     transaction at the resolver, and the retry routes live.
//   - cellfin FINALIZED: live §5 insert routed within that cell (its
//     centroids and postings exist; cross-cell closure would target cells
//     whose postings may not exist yet — their residency is reconciled by
//     the 094.3 NPA/rebalancer machinery like any boundary drift).
func (m *spfreshIndexMaintainer) UpdateWhileWriteOnly(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	if oldRecord == nil && newRecord == nil {
		return nil
	}
	metaStorage := newSPFreshStorage(m.indexSubspace, 0)
	if _, gerr := spfreshReadGenerationForWrite(m.tx, metaStorage); gerr == nil {
		return m.Update(oldRecord, newRecord)
	} else if !errors.Is(gerr, errSPFreshNotFound) {
		return gerr
	}

	// First build in flight (or not started): target generation 1.
	storage := newSPFreshStorage(m.indexSubspace, 1)
	// REAL read — see spfreshLoadAllCoarseForWrite: this conflict range is
	// what makes the "pre-coarse no-op" decision safe to act on.
	cellIDs, cellRows, err := spfreshLoadAllCoarseForWrite(m.tx, storage)
	if err != nil {
		return err
	}

	if oldRecord != nil {
		entries, eerr := m.evaluateIndex(oldRecord)
		if eerr != nil {
			return fmt.Errorf("evaluate spfresh index %q for old record: %w", m.index.Name, eerr)
		}
		for _, entry := range entries {
			pk, terr := m.index.TrimPrimaryKey(entry.primaryKey)
			if terr != nil {
				return terr
			}
			// Live copies (membership-driven; no-op if absent) AND any staged
			// copy: the routing is deterministic, so the staged key — written
			// by this pk's earlier save or by the assignment scan — lives at
			// the same nearest-cell slot this delete computes.
			if derr := m.spfreshDelete(storage, pk); derr != nil {
				return derr
			}
			// An undecodable vector was never staged (every staging writer —
			// the scan and the insert path — errors before writing one), so
			// skipping the staged-copy clear for it is sound; don't swallow
			// the distinction silently (Torvalds 094.2 #3).
			vec, verr := spfreshEntryVector(m.index, entry)
			if verr == nil && vec != nil && len(cellIDs) > 0 {
				cell := spfreshNearestCellOf(vec, cellIDs, cellRows)
				m.tx.Clear(storage.stagingKey(cell, pk))
			}
		}
	}

	if newRecord != nil {
		entries, eerr := m.evaluateIndex(newRecord)
		if eerr != nil {
			return fmt.Errorf("evaluate spfresh index %q for new record: %w", m.index.Name, eerr)
		}
		for _, entry := range entries {
			vec, verr := spfreshEntryVector(m.index, entry)
			if verr != nil {
				return verr
			}
			if vec == nil {
				continue
			}
			if len(vec) != m.config.NumDimensions {
				return fmt.Errorf("spfresh index %q: record vector has %d dimensions, index has %d", m.index.Name, len(vec), m.config.NumDimensions)
			}
			pk, terr := m.index.TrimPrimaryKey(entry.primaryKey)
			if terr != nil {
				return terr
			}
			if len(cellIDs) == 0 {
				continue // pre-coarse window: the assignment scan covers this record
			}
			cell := spfreshNearestCellOf(vec, cellIDs, cellRows)

			// The §8 straggler-side fence: REAL-read the cellfin row.
			state := spfreshCellfinClaimed
			if data, gerr := m.tx.Get(storage.taskKey(spfreshTaskCellfin, cell)).Get(); gerr != nil {
				return gerr
			} else if data != nil {
				row, derr := decodeTaskRow(data)
				if derr != nil {
					return derr
				}
				state = row.state
			}
			if state == spfreshCellfinFinalized {
				// Live path, routed within this finalized cell.
				rows, _, _, lerr := spfreshLoadCell(m.tx, storage, cell)
				if lerr != nil {
					return lerr
				}
				routed := make([]spfreshRouted, 0, len(rows))
				for _, r := range rows {
					cvec, cverr := r.row.vector()
					if cverr != nil {
						return cverr
					}
					routed = append(routed, spfreshRouted{
						cellID: cell, fineID: r.fineID, vec: cvec,
						d2: spfreshSquaredDistance(vec, cvec),
					})
				}
				sort.Slice(routed, func(i, j int) bool {
					if routed[i].d2 != routed[j].d2 {
						return routed[i].d2 < routed[j].d2
					}
					return routed[i].fineID < routed[j].fineID
				})
				if ierr := m.spfreshInsertRouted(storage, routed, pk, vec); ierr != nil {
					return ierr
				}
				continue
			}
			// Staging path: identical bytes to the build's own assignment
			// writes; wave B assigns it with everything else.
			fp16 := vectorcodec.SerializeHalf(vec)
			spfreshSaveStaging(m.tx, storage, cell, pk, fp16)
			if m.config.Sidecar {
				spfreshSaveSidecar(m.tx, storage, pk, fp16)
			}
		}
	}
	return nil
}

// spfreshNearestCellOf routes a vector to its nearest ACTIVE coarse cell.
func spfreshNearestCellOf(vec []float64, cellIDs []int64, rows []spfreshCentroidRow) int64 {
	best, bestD := int64(0), 0.0
	for i, id := range cellIDs {
		if rows[i].state != spfreshStateActive {
			continue
		}
		cvec, err := rows[i].vector()
		if err != nil {
			continue
		}
		d := spfreshSquaredDistance(vec, cvec)
		if best == 0 || d < bestD {
			best, bestD = id, d
		}
	}
	return best
}

// Scan: vector indexes have no meaningful BY_VALUE range scan (entries are
// quantized residuals under centroid-assigned keys); mirror the HNSW index's
// contract by rejecting it.
func (m *spfreshIndexMaintainer) Scan(scanRange TupleRange, continuation []byte, scanProperties ScanProperties) RecordCursor[*IndexEntry] {
	return &errorCursor[*IndexEntry]{err: fmt.Errorf("spfresh index %q: BY_VALUE scan not supported; use BY_DISTANCE", m.index.Name)}
}

// DeleteWhere clears the whole index keyspace under the prefix. For 094.1
// only the full clear (empty prefix) is meaningful (no grouped SPFresh yet).
func (m *spfreshIndexMaintainer) DeleteWhere(prefix tuple.Tuple) error {
	if len(prefix) != 0 {
		return fmt.Errorf("spfresh index %q: grouped DeleteWhere not supported in 094.1", m.index.Name)
	}
	r, err := fdb.PrefixRange(m.indexSubspace.Bytes())
	if err != nil {
		return err
	}
	m.tx.ClearRange(r)
	return nil
}

// ScanByDistance executes a kNN query. Same TupleRange convention as the
// HNSW maintainer: Low = (serialized query vector [, prefix...]),
// High = (k [, efSearch]); efSearch maps onto the adaptive fine-probe width.
// Each IndexEntry: Key = (trimmedPK...), Value = nil (codes are residuals;
// exact vectors live in the sidecar/records). Distances are exact
// (sidecar-re-ranked) and ascending.
func (m *spfreshIndexMaintainer) ScanByDistance(
	scanRange TupleRange,
	continuation []byte,
	scanProperties ScanProperties,
) RecordCursor[*IndexEntry] {
	if scanRange.Low == nil || len(scanRange.Low) < 1 {
		return &errorCursor[*IndexEntry]{err: fmt.Errorf("SPFresh BY_DISTANCE scan requires query vector in TupleRange.Low")}
	}
	vecBytes, ok := scanRange.Low[0].([]byte)
	if !ok {
		return &errorCursor[*IndexEntry]{err: fmt.Errorf("SPFresh BY_DISTANCE scan: TupleRange.Low[0] must be []byte")}
	}
	queryVector, err := deserializeVector(vecBytes)
	if err != nil {
		return &errorCursor[*IndexEntry]{err: fmt.Errorf("SPFresh BY_DISTANCE scan: invalid query vector: %w", err)}
	}
	if len(queryVector) != m.config.NumDimensions {
		return &errorCursor[*IndexEntry]{err: fmt.Errorf("spfresh index %q: query vector has %d dimensions, index has %d", m.index.Name, len(queryVector), m.config.NumDimensions)}
	}
	if len(scanRange.Low) > 1 {
		return &errorCursor[*IndexEntry]{err: fmt.Errorf("spfresh index %q: prefixed (grouped) scans not supported in 094.1", m.index.Name)}
	}
	// High = (k [, kc [, w [, c [, ε]]]]): per-query tuning knobs (RFC-094 §4
	// / 094.4). kc keeps the HNSW efSearch slot; w and c extend it; ε is the
	// SPANN Eq. (3) pruning ratio (float or int; ≤ 0 disables pruning, absent
	// keeps the searcher default).
	k := 10
	efSearch, wProbe, cRerank := 0, 0, 0
	if scanRange.High != nil {
		ints := []*int{&k, &efSearch, &wProbe, &cRerank}
		for i := 0; i < len(scanRange.High) && i < len(ints); i++ {
			if v, ok := asInt64(scanRange.High[i]); ok && (v > 0 || (i == 3 && v == -1)) {
				// c = -1: estimates-only ranking, no sidecar re-rank wave
				// (the 094.4 sidecar A/B; distances are then approximate).
				*ints[i] = int(v)
			}
		}
	}
	epsilon, epsilonSet := 0.0, false
	if len(scanRange.High) > 4 {
		switch v := scanRange.High[4].(type) {
		case float64:
			epsilon, epsilonSet = v, true
		case int64:
			epsilon, epsilonSet = float64(v), true
		}
	}

	results, err := m.searchCurrentGeneration(queryVector, k, efSearch, wProbe, cRerank, epsilon, epsilonSet)
	if err != nil {
		return &errorCursor[*IndexEntry]{err: err}
	}
	entries := make([]*IndexEntry, len(results))
	for i, r := range results {
		// Index + pinned primaryKey make the entry usable through the normal
		// executor path (IndexEntry.PrimaryKey loads the record).
		entries[i] = &IndexEntry{Index: m.index, Key: r.PrimaryKey, Value: tuple.Tuple{nil}, primaryKey: r.PrimaryKey}
	}
	return FromListWithContinuation(entries, continuation)
}

// resolveSearcher resolves the readable generation, refreshes/loads the
// per-process routing cache, and constructs a searcher (timer wired) bound to
// the maintainer's transaction — the shared front half of every SPFresh query
// path (one-shot top-k AND the RFC-156 Phase C ordered stream). It applies NO
// per-query knobs (efSearch/w/c/ε); the caller does. A (nil, nil, nil) return
// means the index is empty (no generation, no in-flight build) — the caller
// reports zero results.
func (m *spfreshIndexMaintainer) resolveSearcher() (*spfreshSearcher, *spfreshStorage, error) {
	// Resolve the generation (snapshot — queries never conflict).
	metaStorage := newSPFreshStorage(m.indexSubspace, 0) // gen 0: META access only
	gen, err := spfreshReadGenerationSnapshot(m.tx, metaStorage)
	if err != nil {
		if errors.Is(err, errSPFreshNotFound) {
			// No generation: either nothing was ever inserted or built (§6b
			// insert-first — zero rows), or a bulk build holds the token and
			// has not flipped yet — that index is BUILDING, and silently
			// reporting it empty would be a lie (codex 094.4 r2; same
			// distinction the insert path draws). Snapshot read: queries
			// take no conflict ranges.
			tok, terr := m.tx.Snapshot().Get(metaStorage.metaKey(spfreshMetaBuild)).Get()
			if terr != nil {
				return nil, nil, terr
			}
			if tok != nil {
				return nil, nil, fmt.Errorf("spfresh index %q: a bulk build is in flight (or died before flipping) — retry after it completes, or rerun BuildSPFreshIndex", m.index.Name)
			}
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("spfresh index %q: read generation: %w", m.index.Name, err)
	}
	storage := newSPFreshStorage(m.indexSubspace, gen)
	var cache *spfreshRoutingCache
	if wc := m.txWriteCache(); wc != nil && wc.ready(gen) {
		// This transaction already wrote to this index: route on the
		// TX-LOCAL cache. Two reasons, both load-bearing: (a) RYW gives a
		// same-transaction INSERT→SELECT the inserted record; (b) the
		// process-global cache must NEVER be loaded or refreshed through a
		// transaction carrying uncommitted SPFresh topology — RYW would
		// publish phantom bootstrap cells / minted centroids globally, and
		// an abort leaves every later query routing on a topology that does
		// not exist (for a §6b cold-start index there is no generation flip
		// to ever flush it). The write path has guarded against this since
		// cloneForWrite; this is the read-side half (Torvalds final-gauntlet
		// S1). No refresh either: the changelog range may contain this tx's
		// own unresolved versionstamped deltas.
		cache = wc
	} else {
		cache = spfreshCacheFor(m.indexSubspace, gen)
		// Queries pay ZERO cache-maintenance I/O on the common path (RFC-094
		// §4 — the per-query changelog read was the rev-2-NAK'd hot-key
		// anti-pattern, and it cost ~half the measured p50 at SIFT-100k).
		// With the 094.3 rebalancer the topology changes WITHIN a
		// generation, so the cache additionally refreshes on an amortized
		// timer: one changelog read per interval per process, not per query;
		// between refreshes queries ride the searcher's posting-HDR forward
		// tolerance.
		if !cache.ready(gen) {
			if frerr := cache.fullReload(m.tx, storage, gen); frerr != nil {
				return nil, nil, fmt.Errorf("spfresh index %q: routing reload: %w", m.index.Name, frerr)
			}
		} else if rerr := cache.maybeRefresh(m.tx, storage, gen); rerr != nil {
			return nil, nil, fmt.Errorf("spfresh index %q: routing refresh: %w", m.index.Name, rerr)
		}
	}

	searcher := newSPFreshSearcher(storage, m.config, cache)
	searcher.timer = m.timer
	return searcher, storage, nil
}

// searchCurrentGeneration resolves the readable generation, refreshes/loads
// the per-process cache, and runs the search in the maintainer's transaction.
func (m *spfreshIndexMaintainer) searchCurrentGeneration(query []float64, k, efSearch, wProbe, cRerank int, epsilon float64, epsilonSet bool) ([]spfreshSearchResult, error) {
	searcher, storage, err := m.resolveSearcher()
	if err != nil {
		return nil, err
	}
	if searcher == nil {
		return nil, nil // empty index
	}
	if efSearch > 0 {
		// The HNSW-style efSearch slot sets the fine-probe width DIRECTLY
		// (094.4: sweeps need to tune DOWN as well as up).
		searcher.kc = efSearch
		searcher.c = max(searcher.c, 4*k)
	}
	if wProbe > 0 {
		searcher.w = wProbe
	}
	if cRerank > 0 {
		searcher.c = max(cRerank, k)
	} else if cRerank == -1 {
		searcher.noRerank = true
	}
	if epsilonSet {
		searcher.epsilon = epsilon // ≤ 0 disables pruning
	}
	results, serr := searcher.search(m.tx, query, k)
	if serr != nil {
		return nil, serr
	}
	// Read-path envelope repair: a capped posting read is proof the posting is
	// past the split-dispatch envelope with its tail invisible to queries —
	// the split trigger was lost (every lifecycle that can balloon a posting
	// re-files on its own, but the cap-hit signal is the regression net for
	// the whole class). Re-file from here so the envelope heals from reads
	// instead of trusting every lifecycle forever.
	if ferr := m.spfreshFileSplitsForCapped(storage, searcher.capped); ferr != nil {
		return nil, fmt.Errorf("spfresh index %q: read-path split re-file: %w", m.index.Name, ferr)
	}
	return results, nil
}

// ScanByDistanceOrderedStream is the RFC-156 Phase C distance-ordered STREAMING
// scan: a demand-driven cursor that widens in batches as the consumer drains it,
// bounded by the default budget cap. It shares the BY_DISTANCE TupleRange/
// IndexEntry contract with ScanByDistance — Low = (query vector), High =
// (k, kc, w, c[, ε]) — but ignores the k/c top-k caps (the budget replaces them)
// and honours only the probe knobs (kc = efSearch, w, ε). The executor routes
// an ordered-stream SPFresh plan here; HNSW (no posting cells to widen) keeps the
// fixed-horizon ScanByDistance path.
func (m *spfreshIndexMaintainer) ScanByDistanceOrderedStream(scanRange TupleRange, continuation []byte, scanProperties ScanProperties) RecordCursor[*IndexEntry] {
	return m.newOrderedStreamCursor(scanRange, defaultSPFreshStreamBudget(), continuation, scanProperties)
}

// newOrderedStreamCursor builds the streaming cursor with an explicit budget.
// ScanByDistanceOrderedStream supplies the calibrated default; white-box tests
// pass a small budget to force/observe truncation and the bounded-memory cap
// without a huge corpus.
func (m *spfreshIndexMaintainer) newOrderedStreamCursor(scanRange TupleRange, budget spfreshStreamBudget, continuation []byte, _ ScanProperties) RecordCursor[*IndexEntry] {
	if scanRange.Low == nil || len(scanRange.Low) < 1 {
		return &errorCursor[*IndexEntry]{err: fmt.Errorf("SPFresh BY_DISTANCE stream requires query vector in TupleRange.Low")}
	}
	vecBytes, ok := scanRange.Low[0].([]byte)
	if !ok {
		return &errorCursor[*IndexEntry]{err: fmt.Errorf("SPFresh BY_DISTANCE stream: TupleRange.Low[0] must be []byte")}
	}
	queryVector, derr := deserializeVector(vecBytes)
	if derr != nil {
		return &errorCursor[*IndexEntry]{err: fmt.Errorf("SPFresh BY_DISTANCE stream: invalid query vector: %w", derr)}
	}
	if len(queryVector) != m.config.NumDimensions {
		return &errorCursor[*IndexEntry]{err: fmt.Errorf("spfresh index %q: query vector has %d dimensions, index has %d", m.index.Name, len(queryVector), m.config.NumDimensions)}
	}
	if len(scanRange.Low) > 1 {
		return &errorCursor[*IndexEntry]{err: fmt.Errorf("spfresh index %q: prefixed (grouped) scans not supported in 094.1", m.index.Name)}
	}

	// High = (k, kc, w, c[, ε]). Streaming ignores k (index 0) and c (index 3) —
	// the budget cap replaces the top-k/re-rank limits — and honours only the
	// probe knobs: kc (= efSearch, index 1), w (index 2), and ε (index 4).
	efSearch, wProbe := 0, 0
	if len(scanRange.High) > 1 {
		if v, ok := asInt64(scanRange.High[1]); ok && v > 0 {
			efSearch = int(v)
		}
	}
	if len(scanRange.High) > 2 {
		if v, ok := asInt64(scanRange.High[2]); ok && v > 0 {
			wProbe = int(v)
		}
	}
	epsilon, epsilonSet := 0.0, false
	if len(scanRange.High) > 4 {
		switch v := scanRange.High[4].(type) {
		case float64:
			epsilon, epsilonSet = v, true
		case int64:
			epsilon, epsilonSet = float64(v), true
		}
	}

	skip, perr := parseStreamPosition(continuation)
	if perr != nil {
		return &errorCursor[*IndexEntry]{err: perr}
	}

	searcher, storage, rerr := m.resolveSearcher()
	if rerr != nil {
		return &errorCursor[*IndexEntry]{err: rerr}
	}
	if searcher == nil {
		return Empty[*IndexEntry]() // empty index
	}
	if efSearch > 0 {
		searcher.kc = efSearch
	}
	if wProbe > 0 {
		searcher.w = wProbe
	}
	if epsilonSet {
		searcher.epsilon = epsilon // ≤ 0 disables pruning
	}

	f, ferr := searcher.searchInit(m.tx, queryVector)
	if ferr != nil {
		return &errorCursor[*IndexEntry]{err: ferr}
	}
	f.streamInit(budget)
	return &spfreshStreamCursor{m: m, f: f, storage: storage, skip: skip}
}

// spfreshFileSplitsForCapped re-files split tasks for postings whose search
// fetch hit the 4×Lmax+1 cap. Conflict discipline: queries stay conflict-free
// in every steady state — the task-row gate is a SNAPSHOT read, so once a
// split is pending (the whole heal window) queries take no conflict ranges
// here; only the one query that actually files pays spfreshTaskSetIfAbsent's
// REAL read + write (the same Set-if-absent the insert-path probe uses — a
// live claim must not be clobbered). The insert path's csplit starvation
// guard applies unchanged: issuance stays paused for cells with a pausing
// coarse split (the csplit move re-files for oversized rows it relocates —
// the pause-window repair), so the read path cannot re-introduce the
// fine-split/csplit starvation the pause exists to prevent. A cap-hit on a
// FORWARD parent files a task the seal step's zombie rule deletes — no
// special case needed.
func (m *spfreshIndexMaintainer) spfreshFileSplitsForCapped(storage *spfreshStorage, capped []spfreshRouted) error {
	seen := make(map[int64]bool, len(capped))
	for _, rt := range capped {
		if seen[rt.fineID] {
			continue
		}
		seen[rt.fineID] = true
		data, gerr := m.tx.Snapshot().Get(storage.taskKey(spfreshTaskSplit, rt.fineID)).Get()
		if gerr != nil {
			return gerr
		}
		if data != nil {
			continue // already pending: zero conflict surface taken
		}
		// The pause check must run against the fine's CURRENT cell, not the
		// cell the (possibly stale) cache routed through: a completed coarse
		// split may have moved the fine into a cell whose own csplit is now
		// PAUSING, and checking the old cell would file a split straight
		// through the starvation guard (codex delta P2). Snapshot resolve —
		// rare path (cap-hit only), and a fine that is gone entirely gets
		// nothing filed (its lifecycle already retired it).
		cellID, cerr := spfreshFindCentroidCellSnapshot(m.tx, storage, rt.fineID, rt.cellID)
		if cerr != nil {
			if errors.Is(cerr, errSPFreshNotFound) {
				continue
			}
			return cerr
		}
		paused, perr := spfreshCSplitPaused(m.tx.Snapshot(), storage, cellID)
		if perr != nil {
			return perr
		}
		if paused {
			continue
		}
		filed, terr := spfreshTaskSetIfAbsent(m.tx, storage, spfreshTaskSplit, rt.fineID)
		if terr != nil {
			return terr
		}
		if filed {
			m.timer.Increment(CountSPFreshReadPathSplitFiles)
		}
	}
	return nil
}

// spfreshScanBatchSize is the records-per-transaction limit of the build's
// record-collection scan (one unbounded scan blows the 5 s tx limit and
// retries forever). Variable so the continuation path is exercised by a
// small deterministic regression, not only by the env-gated SIFT benchmark.
var spfreshScanBatchSize = 1000

// spfreshScanRecordBatches scans the store's records in CONTINUATION-BATCHED
// transactions — one unbounded scan blows the 5 s FDB transaction limit at
// scale and the retry loop restarts it from scratch forever (caught by the
// SIFT-100k benchmark hanging in exactly that loop). Each tx reads up to
// spfreshScanBatchSize records and evaluates the index entries.
//
// Exactly one of the callbacks is non-nil:
//   - inTx runs INSIDE the scan transaction — index writes that must commit
//     atomically with the scan's REAL record-range read (the assignment
//     scan's delete fence). Its writes must be idempotent: the tx can retry.
//   - post runs AFTER the transaction succeeds — pure in-memory collection
//     (the sample scan), which must NOT run per attempt or retries duplicate.
func spfreshScanRecordBatches(
	ctx context.Context,
	db *FDBDatabase,
	storeBuilder func(*FDBRecordContext) (*FDBRecordStore, error),
	index *Index,
	indexSubspace subspace.Subspace,
	batchSize int,
	inTx func(rtx *FDBRecordContext, batch []spfreshBuildInput) error,
	post func(batch []spfreshBuildInput) error,
) error {
	return spfreshScanRecordRange(ctx, db, storeBuilder, index, indexSubspace,
		nil, nil, EndpointTypeTreeStart, EndpointTypeTreeEnd, batchSize, inTx, post)
}

// spfreshScanRecordRange is spfreshScanRecordBatches bounded to one half-open
// primary-key sub-range [low, high) — the unit of RFC-103 parallel staging. The
// high bound is held CONSTANT across every continuation-resumed batch; only the
// low end advances (lowEP -> EndpointTypeContinuation once a batch has been
// read), so a resumed batch can never escape its shard's range. ScanRecords
// forces highEP=TreeEnd on a continuation (store.go); a sharded scan must NOT.
// low/high nil with TreeStart/TreeEnd endpoints reproduces the full-range scan.
func spfreshScanRecordRange(
	ctx context.Context,
	db *FDBDatabase,
	storeBuilder func(*FDBRecordContext) (*FDBRecordStore, error),
	index *Index,
	indexSubspace subspace.Subspace,
	low, high tuple.Tuple,
	lowEP, highEP EndpointType,
	batchSize int,
	inTx func(rtx *FDBRecordContext, batch []spfreshBuildInput) error,
	post func(batch []spfreshBuildInput) error,
) error {
	scanBatch := batchSize
	var continuation []byte
	for first := true; first || continuation != nil; first = false {
		// Per-attempt staging: the body may RETRY (1007/1020); handing batches
		// to post inside it would duplicate them.
		var batchInputs []spfreshBuildInput
		var nextContinuation []byte
		err := spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
			batchInputs = batchInputs[:0]
			nextContinuation = nil
			store, serr := storeBuilder(rtx)
			if serr != nil {
				return serr
			}
			evaluator := newStandardIndexMaintainer(index, indexSubspace, rtx.Transaction(), store)

			// Isolation by mode. inTx (the assignment scan) MUST be
			// SERIALIZABLE: the scan's read conflict range over the record
			// keyspace is the delete fence — a delete committing after our
			// read version aborts this tx and the retry re-reads truth; a
			// snapshot scan stages ghosts (the pre-fix bug:
			// IsolationLevelSnapshot is Go's zero value, so the default was
			// silently fenceless). post (the sample scan) must stay SNAPSHOT:
			// it writes nothing, so a conflict range over every batch of the
			// whole dataset buys nothing and thrashes on aborts during
			// exactly the window UpdateWhileWriteOnly guarantees concurrent
			// writers exist (Torvalds 094.2 re-review).
			isolation := IsolationLevelSnapshot
			if inTx != nil {
				isolation = IsolationLevelSerializable
			}
			props := ScanProperties{ExecuteProperties: ExecuteProperties{
				IsolationLevel:   isolation,
				ReturnedRowLimit: scanBatch,
			}}
			// Hold the shard's high bound across every batch; advance only the
			// low end via the continuation (the per-shard fence is the read
			// conflict range over [low, high), which must not grow on resume).
			scanLowEP := lowEP
			if continuation != nil {
				scanLowEP = EndpointTypeContinuation
			}
			cursor := store.ScanRecordsInRange(low, high, scanLowEP, highEP, continuation, props)
			defer func() { _ = cursor.Close() }()
			for {
				result, cerr := cursor.OnNext(ctx)
				if cerr != nil {
					return cerr
				}
				if !result.HasNext() {
					if !result.GetNoNextReason().IsSourceExhausted() {
						cont, cterr := result.GetContinuation().ToBytes()
						if cterr != nil {
							return cterr
						}
						nextContinuation = cont
					}
					break
				}
				rec := result.GetValue()
				entries, eerr := evaluator.evaluateIndex(rec)
				if eerr != nil {
					return eerr
				}
				for _, entry := range entries {
					vec, verr := spfreshEntryVector(index, entry)
					if verr != nil {
						return verr
					}
					if vec == nil {
						continue // absent/null vector: unindexed
					}
					trimmedPK, terr := index.TrimPrimaryKey(entry.primaryKey)
					if terr != nil {
						return terr
					}
					batchInputs = append(batchInputs, spfreshBuildInput{pk: trimmedPK, fullPK: entry.primaryKey, vec: vec})
				}
			}
			if inTx != nil && len(batchInputs) > 0 {
				return inTx(rtx, batchInputs)
			}
			return nil
		})
		if err != nil {
			return err
		}
		if post != nil && len(batchInputs) > 0 {
			if err := post(batchInputs); err != nil {
				return err
			}
		}
		continuation = nextContinuation
	}
	return nil
}

// spfreshShardRange is one half-open primary-key sub-range of the record
// keyspace, scanned by a single staging shard (RFC-103). low/high are full
// record PKs; the endpoint types tile the keyspace gaplessly with ±∞ ends.
type spfreshShardRange struct {
	low, high     tuple.Tuple
	lowEP, highEP EndpointType
}

// spfreshShardRanges tiles the record keyspace into half-open PK ranges around
// the boundaries: [TreeStart, b₀) [b₀, b₁) … [b_{n-1}, TreeEnd). Boundaries must
// be sorted ascending and distinct (the sampler guarantees it). Empty
// boundaries ⇒ a single full-range shard (today's serial scan). The ±∞ ends keep
// the union equal to the whole record range, so the per-shard delete fences
// (read-conflict ranges) cover every key with no gap and no overlap.
func spfreshShardRanges(boundaries []tuple.Tuple) []spfreshShardRange {
	if len(boundaries) == 0 {
		return []spfreshShardRange{{lowEP: EndpointTypeTreeStart, highEP: EndpointTypeTreeEnd}}
	}
	ranges := make([]spfreshShardRange, 0, len(boundaries)+1)
	ranges = append(ranges, spfreshShardRange{high: boundaries[0], lowEP: EndpointTypeTreeStart, highEP: EndpointTypeRangeExclusive})
	for i := 0; i+1 < len(boundaries); i++ {
		ranges = append(ranges, spfreshShardRange{low: boundaries[i], high: boundaries[i+1], lowEP: EndpointTypeRangeInclusive, highEP: EndpointTypeRangeExclusive})
	}
	ranges = append(ranges, spfreshShardRange{low: boundaries[len(boundaries)-1], lowEP: EndpointTypeRangeInclusive, highEP: EndpointTypeTreeEnd})
	return ranges
}

// spfreshStageRecordsSharded runs the staging scan over the shard ranges. A
// single full-range shard runs inline (byte-identical to the pre-RFC-103 serial
// scan). Multiple shards run concurrently, one goroutine each (their count is
// bounded by the small fan-out); the first error cancels the rest — so they tear
// down their in-flight tx instead of finishing a 5 s scan — and the call waits
// for every goroutine before returning. A failed shard re-runs the WHOLE staging
// pass on the build's retry; staging Sets are idempotent (same (cell, recordPK)
// key, same fp16 value), so a shard that committed before the failure is
// harmlessly re-Set. The shard goroutines share only immutable builder state
// (storage/token/config and the post-coarse frozen coarseVec/cellIDs nearestCell
// reads); the wave-A fine-ID allocator is untouched here, so there is no shared
// mutable state.
func spfreshStageRecordsSharded(
	ctx context.Context,
	db *FDBDatabase,
	storeBuilder func(*FDBRecordContext) (*FDBRecordStore, error),
	index *Index,
	indexSubspace subspace.Subspace,
	batchSize int,
	inTx func(rtx *FDBRecordContext, batch []spfreshBuildInput) error,
	ranges []spfreshShardRange,
) error {
	if len(ranges) == 1 {
		r := ranges[0]
		return spfreshScanRecordRange(ctx, db, storeBuilder, index, indexSubspace, r.low, r.high, r.lowEP, r.highEP, batchSize, inTx, nil)
	}
	// storeBuilder was only ever called serially before sharding; the fan-out
	// now calls it from S goroutines. A caller that closes over a REUSABLE
	// StoreBuilder (SetContext mutates it) would race, so serialize the cheap
	// store construction behind a mutex — each call binds a fresh store to its
	// own transaction. The scans themselves still run fully concurrently
	// (codex impl review P2).
	var sbMu sync.Mutex
	safeStoreBuilder := func(rtx *FDBRecordContext) (*FDBRecordStore, error) {
		sbMu.Lock()
		defer sbMu.Unlock()
		return storeBuilder(rtx)
	}
	shardCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	var errOnce sync.Once
	var firstErr error
	for _, r := range ranges {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := spfreshScanRecordRange(shardCtx, db, safeStoreBuilder, index, indexSubspace, r.low, r.high, r.lowEP, r.highEP, batchSize, inTx, nil); err != nil {
				errOnce.Do(func() {
					firstErr = err
					cancel()
				})
			}
		}()
	}
	wg.Wait()
	return firstErr
}

// spfreshBoundarySampleCap bounds the PK reservoir the boundary sampler keeps;
// the S-1 quantile boundaries are drawn from it. Far more than enough for the
// handful of shards the staging fan-out uses.
const spfreshBoundarySampleCap = 4096

// spfreshPKSampler captures evenly-count-spaced full record PKs in scan order to
// derive staging shard boundaries (RFC-103). It is fed by the already-serial
// sample scan (zero extra I/O) and keeps ≤ 2·cap PKs via systematic decimation:
// when the buffer fills it drops every other entry and doubles the stride, so
// the retained PKs stay evenly spaced by record count. The sample scan learns
// totalN only as it goes, so the resulting quantiles are APPROXIMATE; that only
// affects shard balance, never correctness (the staged set is shard-count- and
// split-quality-invariant).
type spfreshPKSampler struct {
	pks    []tuple.Tuple
	stride int
	seen   int
	cap    int
}

func newSPFreshPKSampler(capN int) *spfreshPKSampler {
	return &spfreshPKSampler{stride: 1, cap: capN}
}

func (s *spfreshPKSampler) observe(pk tuple.Tuple) {
	if s.seen%s.stride == 0 {
		s.pks = append(s.pks, pk)
		if len(s.pks) >= 2*s.cap {
			w := 0
			for i := 0; i < len(s.pks); i += 2 {
				s.pks[w] = s.pks[i]
				w++
			}
			s.pks = s.pks[:w]
			s.stride *= 2
		}
	}
	s.seen++
}

// boundaries returns up to shards-1 interior PK boundaries, evenly spaced by
// record count and strictly ascending. The records are scanned in ascending key
// order and PKs are unique, so the retained reservoir is strictly ascending and
// the distinct quantile indices yield strictly ascending, distinct boundaries.
// Fewer than `shards` candidates ⇒ no boundaries ⇒ a single serial shard.
func (s *spfreshPKSampler) boundaries(shards int) []tuple.Tuple {
	if shards <= 1 || len(s.pks) < shards {
		return nil
	}
	out := make([]tuple.Tuple, 0, shards-1)
	for i := 1; i < shards; i++ {
		// Copy, don't alias the sampler's internal buffer — the boundaries
		// outlive the sampler and a future caller might mutate them (@claude).
		out = append(out, append(tuple.Tuple(nil), s.pks[i*len(s.pks)/shards]...))
	}
	return out
}

// BuildSPFreshIndex bulk-builds an SPFresh index over the store's existing
// records (RFC-094 §8) and flips it readable. The §8 step order is the
// foreground-interleaving contract: the COARSE table commits BEFORE the
// assignment scan, so every record save thereafter can route itself
// (UpdateWhileWriteOnly stages or goes live by cellfin state), and every
// record saved before it is covered by the assignment scan's later read
// versions. Double coverage (a save the scan also reads) is harmless —
// staging writes are idempotent Sets on the same key.
func BuildSPFreshIndex(ctx context.Context, db *FDBDatabase, storeBuilder func(*FDBRecordContext) (*FDBRecordStore, error), indexName string, seed int64) error {
	return buildSPFreshIndex(ctx, db, storeBuilder, indexName, seed, spfreshBuildStagingShards)
}

// spfreshBuildStagingShards is the default fan-out for the parallel staging scan
// (RFC-103): S disjoint record-PK sub-ranges scanned concurrently to hide the
// synchronous pure-Go client's per-batch round-trip latency. Tests pin S=1 vs
// S=8 (buildSPFreshIndex) for the byte-identical-staged-set determinism check.
const spfreshBuildStagingShards = 8

// buildSPFreshIndex is BuildSPFreshIndex with an explicit staging shard count.
func buildSPFreshIndex(ctx context.Context, db *FDBDatabase, storeBuilder func(*FDBRecordContext) (*FDBRecordStore, error), indexName string, seed int64, shards int) error {
	// Resolve the index once.
	var index *Index
	var indexSubspace subspace.Subspace
	var config SPFreshConfig
	// shardSafe gates the parallel staging fan-out (RFC-103): a shard boundary
	// is a record PK, and a cut tears a split record only if some record's PK is
	// that boundary minus an integer suffix. Fixed-arity indexed PKs can't be,
	// and ALL record types carrying a RecordTypeKey prefix (or a single type)
	// makes every type's keyspace disjoint — so no boundary lands in a foreign
	// record's chunks. The collision-prone no-type-prefix store degrades to S=1.
	var shardSafe bool
	if err := spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
		store, serr := storeBuilder(rtx)
		if serr != nil {
			return serr
		}
		index = store.GetMetaData().GetIndex(indexName)
		if index == nil {
			return fmt.Errorf("spfresh build: index %q not found", indexName)
		}
		if index.Type != IndexTypeVectorSPFresh {
			return fmt.Errorf("spfresh build: index %q has type %q", indexName, index.Type)
		}
		config = parseSPFreshConfig(index)
		if verr := ValidateSPFreshConfig(config); verr != nil {
			return verr
		}
		indexSubspace = store.indexSubspace(index)
		md := store.GetMetaData()
		shardSafe = len(md.RecordTypes()) == 1 || md.PrimaryKeyHasRecordTypePrefix()
		return nil
	}); err != nil {
		return err
	}

	// Sample scan (§8 step 1) with RESERVOIR SAMPLING (algorithm R,
	// deterministic via the build seed): the coarse k-means only needs a
	// representative sample — training it on every record made the build's
	// clustering cost O(N²·r/(avgFill·cellTarget)·d), unbounded in practice
	// at SIFT-1M. K₀ still derives from the FULL count (totalN below). The
	// cap keeps ≥ ~30 sample points per coarse centroid up to ~2.5M records;
	// beyond that, raise the cap or move to hierarchical sampling (§8).
	var sample [][]float64
	totalN := 0
	rng := &splittableRandom{seed: splitMixLong(seed), gamma: goldenGamma}
	// Piggy-back staging shard-boundary capture on the serial sample scan
	// (RFC-103): it already visits every record in PK order, so collecting
	// count-quantile full PKs here costs no extra I/O. Only when sharding can
	// actually run.
	canShard := shards > 1 && shardSafe
	var boundarySampler *spfreshPKSampler
	if canShard {
		boundarySampler = newSPFreshPKSampler(spfreshBoundarySampleCap)
	}
	if err := spfreshScanRecordBatches(ctx, db, storeBuilder, index, indexSubspace, spfreshScanBatchSize, nil, func(batch []spfreshBuildInput) error {
		for _, in := range batch {
			if boundarySampler != nil {
				boundarySampler.observe(in.fullPK)
			}
			if totalN < spfreshCoarseSampleCap {
				sample = append(sample, in.vec)
			} else if j := int(uint64(rng.nextLong()) % uint64(totalN+1)); j < spfreshCoarseSampleCap {
				sample[j] = in.vec
			}
			totalN++
		}
		return nil
	}); err != nil {
		return err
	}
	if len(sample) == 0 {
		return fmt.Errorf("spfresh build: no records with vectors for index %q", indexName)
	}

	// Builds target generation current+1 (1 for a first build): a REBUILD into
	// the live generation would mix stale postings with new ones and leave
	// process caches 'ready' on stale routing (Torvalds/codex 094.1 review).
	var oldGen int64
	if err := spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
		g, gerr := spfreshReadGenerationSnapshot(rtx.Transaction(), newSPFreshStorage(indexSubspace, 0))
		if gerr != nil {
			if errors.Is(gerr, errSPFreshNotFound) {
				oldGen = 0
				return nil
			}
			return gerr
		}
		oldGen = g
		return nil
	}); err != nil {
		return err
	}
	storage := newSPFreshStorage(indexSubspace, oldGen+1)
	builder := newSPFreshBuilder(db, storage, config, fmt.Sprintf("build-%s", indexName))
	// Abandoned-build GC at the entry point (RFC-094 §3): a prior build into
	// this same target that aborted PRE-FLIP left build-state/cell residue a
	// fresh run would mistake for its own commit retries — publishing stale
	// centroids for the newly scanned inputs (codex 094.1 r2). The target is
	// not readable (the flip never happened), so clearing it is safe.
	if err := spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
		// CAS fence (codex r3): another builder may have flipped oldGen+1
		// readable since the snapshot read above — clearing it then would
		// destroy the LIVE generation. Re-verify with a REAL read (the
		// conflict range also serializes against a concurrent flip).
		cur, cerr := spfreshReadGenerationForWrite(rtx.Transaction(), newSPFreshStorage(indexSubspace, 0))
		if cerr != nil && !errors.Is(cerr, errSPFreshNotFound) {
			return cerr
		}
		if cerr == nil && cur != oldGen {
			return fmt.Errorf("spfresh build: concurrent build detected (generation moved %d -> %d); retry the build", oldGen, cur)
		}
		r, rerr := storage.generationRange()
		if rerr != nil {
			return rerr
		}
		rtx.Transaction().ClearRange(r)
		// Take build ownership atomically with the clear: an older build
		// still in flight loses the token and its remaining transactions
		// abort at the resolver instead of interleaving writes into the
		// prefix we just cleared (codex 094.1 r4).
		spfreshTakeBuilderToken(rtx.Transaction(), storage, builder.token)
		return nil
	}); err != nil {
		return err
	}
	// §8 steps 2–6, with the assignment scan as a SECOND record pass AFTER
	// the coarse table commits (the interleaving contract — see the function
	// comment). Records saved post-coarse may be staged twice (by themselves
	// and by this scan); the Sets are idempotent.
	if berr := builder.coarsePass(ctx, sample, totalN, seed); berr != nil {
		return berr
	}
	// The staging writes ride INSIDE each scan transaction: the scan's REAL
	// read of the record range is the delete fence (Torvalds 094.2 #2). The
	// batch is BYTE-bounded, not just row-bounded — a staging batch writes
	// fp16 STAGING + SIDECAR per record, and 1000 records × 4096 dims would
	// blow the 10 MB transaction limit (codex 094.2 r1 P2).
	//
	// RFC-103: shard the staging scan into S disjoint half-open PK sub-ranges
	// scanned concurrently — the synchronous client makes a serial scan
	// latency-bound (one round-trip per batch). Boundaries are count-quantile
	// record PKs captured on the serial sample scan above; each shard keeps its
	// own SERIALIZABLE delete fence over its disjoint sub-range. Sharding is
	// gated (canShard) on a prefix-safe keyspace; S=1, an unsafe keyspace, or
	// too few records all degrade to the single serial scan.
	var stagingShards []spfreshShardRange
	if canShard {
		stagingShards = spfreshShardRanges(boundarySampler.boundaries(shards))
	} else {
		stagingShards = spfreshShardRanges(nil)
	}
	if berr := spfreshStageRecordsSharded(ctx, db, storeBuilder, index, indexSubspace, config.stagingScanBatch(), builder.stageInTx, stagingShards); berr != nil {
		return berr
	}
	if berr := builder.finalize(ctx, seed); berr != nil {
		return berr
	}
	if oldGen > 0 {
		// Clear the superseded generation. 094.1 is build-then-read with no
		// concurrent writers; the staleness grace period for live readers
		// arrives with 094.3's horizon machinery (RFC-094 §3).
		if err := spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
			r, rerr := newSPFreshStorage(indexSubspace, oldGen).generationRange()
			if rerr != nil {
				return rerr
			}
			rtx.Transaction().ClearRange(r)
			return nil
		}); err != nil {
			return err
		}
	}

	// Record the build in the index's range set (the bulk build covered the
	// full record space in one pass), so MarkIndexReadable's unbuilt-range
	// check passes — the same completion bookkeeping OnlineIndexer performs.
	return spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
		store, serr := storeBuilder(rtx)
		if serr != nil {
			return serr
		}
		rangeSet := NewIndexingRangeSet(store.subspace, index)
		_, ierr := rangeSet.InsertRange(rtx.Transaction(), nil, rangeSetFinalKey, false)
		return ierr
	})
}

// spfreshEntryVector extracts the vector from an evaluated index entry
// (KeyWithValue puts it in the value; plain expressions in the key) — the
// same convention the HNSW maintainer uses.
func spfreshEntryVector(index *Index, entry indexEntry) ([]float64, error) {
	if _, ok := index.RootExpression.(*KeyWithValueExpression); ok && len(entry.value) > 0 {
		return tupleToVector(entry.value)
	}
	return tupleToVector(entry.key)
}
