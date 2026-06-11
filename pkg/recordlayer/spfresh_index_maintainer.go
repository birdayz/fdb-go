package recordlayer

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"

	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/vectorcodec"
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

	// writeCache is the TX-LOCAL routing cache for this maintainer's write
	// path (one maintainer per store per transaction): seeded from the
	// process-global cache, reloaded only through THIS transaction, and
	// discarded with it — uncommitted RYW state never reaches the global
	// cache (see spfreshRoutingCache.cloneForWrite).
	writeCache *spfreshRoutingCache
}

func newSPFreshIndexMaintainer(
	index *Index,
	indexSubspace subspace.Subspace,
	tx fdb.Transaction,
	store indexStoreContext,
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
	}, nil
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

// searchCurrentGeneration resolves the readable generation, refreshes/loads
// the per-process cache, and runs the search in the maintainer's transaction.
func (m *spfreshIndexMaintainer) searchCurrentGeneration(query []float64, k, efSearch, wProbe, cRerank int, epsilon float64, epsilonSet bool) ([]spfreshSearchResult, error) {
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
				return nil, terr
			}
			if tok != nil {
				return nil, fmt.Errorf("spfresh index %q: a bulk build is in flight (or died before flipping) — retry after it completes, or rerun BuildSPFreshIndex", m.index.Name)
			}
			return nil, nil
		}
		return nil, fmt.Errorf("spfresh index %q: read generation: %w", m.index.Name, err)
	}
	storage := newSPFreshStorage(m.indexSubspace, gen)
	cache := spfreshCacheFor(m.indexSubspace, gen)

	// Queries pay ZERO cache-maintenance I/O on the common path (RFC-094 §4 —
	// the per-query changelog read was the rev-2-NAK'd hot-key anti-pattern,
	// and it cost ~half the measured p50 at SIFT-100k). With the 094.3
	// rebalancer the topology changes WITHIN a generation, so the cache
	// additionally refreshes on an amortized timer: one changelog read per
	// interval per process, not per query; between refreshes queries ride the
	// searcher's posting-HDR forward tolerance.
	if !cache.ready(gen) {
		if frerr := cache.fullReload(m.tx, storage, gen); frerr != nil {
			return nil, fmt.Errorf("spfresh index %q: routing reload: %w", m.index.Name, frerr)
		}
	} else if rerr := cache.maybeRefresh(m.tx, storage, gen); rerr != nil {
		return nil, fmt.Errorf("spfresh index %q: routing refresh: %w", m.index.Name, rerr)
	}

	searcher := newSPFreshSearcher(storage, m.config, cache)
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
	return searcher.search(m.tx, query, k)
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
			cursor := store.ScanRecords(continuation, props)
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
					batchInputs = append(batchInputs, spfreshBuildInput{pk: trimmedPK, vec: vec})
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

// BuildSPFreshIndex bulk-builds an SPFresh index over the store's existing
// records (RFC-094 §8) and flips it readable. The §8 step order is the
// foreground-interleaving contract: the COARSE table commits BEFORE the
// assignment scan, so every record save thereafter can route itself
// (UpdateWhileWriteOnly stages or goes live by cellfin state), and every
// record saved before it is covered by the assignment scan's later read
// versions. Double coverage (a save the scan also reads) is harmless —
// staging writes are idempotent Sets on the same key.
func BuildSPFreshIndex(ctx context.Context, db *FDBDatabase, storeBuilder func(*FDBRecordContext) (*FDBRecordStore, error), indexName string, seed int64) error {
	// Resolve the index once.
	var index *Index
	var indexSubspace subspace.Subspace
	var config SPFreshConfig
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
		return nil
	}); err != nil {
		return err
	}

	// Sample scan (§8 step 1): all records at current scale; reservoir
	// sampling caps this at production scale.
	var sample [][]float64
	if err := spfreshScanRecordBatches(ctx, db, storeBuilder, index, indexSubspace, spfreshScanBatchSize, nil, func(batch []spfreshBuildInput) error {
		for _, in := range batch {
			sample = append(sample, in.vec)
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
	if berr := builder.coarsePass(ctx, sample, seed); berr != nil {
		return berr
	}
	// The staging writes ride INSIDE each scan transaction: the scan's REAL
	// read of the record range is the delete fence (Torvalds 094.2 #2). The
	// batch is BYTE-bounded, not just row-bounded — a staging batch writes
	// fp16 STAGING + SIDECAR per record, and 1000 records × 4096 dims would
	// blow the 10 MB transaction limit (codex 094.2 r1 P2).
	if berr := spfreshScanRecordBatches(ctx, db, storeBuilder, index, indexSubspace, config.stagingScanBatch(), builder.stageInTx, nil); berr != nil {
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
