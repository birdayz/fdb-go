package recordlayer

import (
	"container/list"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
)

// spfreshRoutingCache is the per-process two-level routing state (RFC-094 §2/§4):
// L1 (all coarse cells, pinned) + L2 (fine centroids per cell, LRU). Refreshed
// off the query path by applying changelog deltas; a horizon overrun or
// generation flip forces a full reload. Queries route on the cache and never
// pay a cache-maintenance round trip; inserts route on it too and are
// corrected by the authoritative state reads (§5).
//
// Concurrency: one mutex around all state. Routing scans run under RLock;
// refreshes under Lock. The scans are sub-millisecond (§4), so a single lock
// is not a contention concern at target QPS; revisit with profiles, not
// speculation.
type spfreshRoutingCache struct {
	mu sync.RWMutex

	// lastRefreshMs gates the amortized in-generation refresh (094.3: the
	// topology changes within a generation now). Atomic CAS so exactly one
	// query per interval pays the changelog read — never one per query (the
	// rev-2 hot-key anti-pattern).
	lastRefreshMs atomic.Int64

	generation int64
	cursor     fdb.Key // changelog position (nil = never refreshed)

	// L1: parallel slices, scanned linearly.
	coarseIDs  []int64
	coarseVecs [][]float64
	coarseFwd  map[int64][2]int64 // FORWARD cells -> children

	// L2: per-cell fine centroids, LRU-bounded.
	cells    map[int64]*spfreshCachedCell
	lru      *list.List // front = most recent; values are cellIDs
	maxCells int
}

type spfreshCachedCell struct {
	fineIDs []int64
	vecs    [][]float64
	states  []byte
	fwd     [2]int64 // coarse-forward children when the cell itself moved
	lruElem *list.Element
}

// errSPFreshEmptyRouting: the cache holds no coarse cells — never loaded, or
// the index is freshly bootstrapped and empty.
var errSPFreshEmptyRouting = errors.New("spfresh: routing cache has no coarse cells (reload required)")

// spfreshDefaultMaxCells bounds L2 residency per cache. At cellTarget=48 and
// 768D fp64-decoded vectors a cell is ~300 KB; 1024 cells ≈ 300 MB worst case
// — within the RFC's per-tenant budget; the multi-tenant global budget is the
// maintainer's concern (RFC-094 §3).
const spfreshDefaultMaxCells = 1024

func newSPFreshRoutingCache(maxCells int) *spfreshRoutingCache {
	if maxCells <= 0 {
		maxCells = spfreshDefaultMaxCells
	}
	return &spfreshRoutingCache{
		coarseFwd: make(map[int64][2]int64),
		cells:     make(map[int64]*spfreshCachedCell),
		lru:       list.New(),
		maxCells:  maxCells,
	}
}

// cloneForWrite returns a TX-LOCAL cache seeded with this cache's L1 (slice
// copies; the vectors themselves are shared read-only) and an EMPTY L2. The
// write path must never load state into the process-global cache through its
// own transaction: RYW makes it publish UNCOMMITTED writes (minted centroids,
// bootstrap cells), and an abort leaves every other writer routing on
// phantoms (caught by the concurrent foreground-fill benchmark).
func (c *spfreshRoutingCache) cloneForWrite() *spfreshRoutingCache {
	c.mu.RLock()
	defer c.mu.RUnlock()
	clone := newSPFreshRoutingCache(c.maxCells)
	clone.generation = c.generation
	clone.cursor = append(fdb.Key(nil), c.cursor...)
	clone.coarseIDs = append([]int64(nil), c.coarseIDs...)
	clone.coarseVecs = append([][]float64(nil), c.coarseVecs...)
	for k, v := range c.coarseFwd {
		clone.coarseFwd[k] = v
	}
	return clone
}

// fullReload rebuilds L1 from FDB and drops L2 (generation flip, horizon
// overrun, or first use). Uses snapshot reads; sets the changelog cursor to
// the current tail so subsequent refreshes are incremental.
func (c *spfreshRoutingCache) fullReload(tx fdb.ReadTransaction, s *spfreshStorage, generation int64) error {
	ids, rows, err := spfreshLoadAllCoarse(tx, s)
	if err != nil {
		return err
	}
	coarseIDs := make([]int64, 0, len(ids))
	coarseVecs := make([][]float64, 0, len(ids))
	coarseFwd := make(map[int64][2]int64)
	for i, id := range ids {
		switch rows[i].state {
		case spfreshStateActive:
			vec, verr := rows[i].vector()
			if verr != nil {
				return verr
			}
			coarseIDs = append(coarseIDs, id)
			coarseVecs = append(coarseVecs, vec)
		case spfreshStateForward:
			coarseFwd[id] = [2]int64{rows[i].childA, rows[i].childB}
		}
	}
	// Advance the cursor to the changelog tail: deltas before this reload are
	// subsumed by the snapshot we just read. An EMPTY changelog must still
	// anchor the cursor (the bare changelog prefix is a valid exclusive lower
	// bound — every real entry sorts above it), or a cold-started cache would
	// report "reload required" forever (cursor nil = never loaded).
	last := fdb.Key(s.changelog.Bytes())
	// Floor the cursor at the GC horizon: after a trim that emptied the log,
	// anchoring at the bare prefix would leave cursor < horizon and the next
	// refresh would force ANOTHER full reload, every interval, until a new
	// delta lands (codex 094.3 r2).
	if horizon, herr := tx.Snapshot().Get(s.metaKey(spfreshMetaHorizon)).Get(); herr != nil {
		return fmt.Errorf("spfresh: read GC horizon: %w", herr)
	} else if horizon != nil && string(horizon) > string(last) {
		last = fdb.Key(horizon)
	}
	for {
		deltas, l, derr := spfreshReadDeltasSince(tx, s, last, 1000)
		if derr != nil {
			return derr
		}
		last = l
		if len(deltas) < 1000 {
			break
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.generation = generation
	c.cursor = last
	c.coarseIDs = coarseIDs
	c.coarseVecs = coarseVecs
	c.coarseFwd = coarseFwd
	c.cells = make(map[int64]*spfreshCachedCell)
	c.lru.Init()
	return nil
}

// ready reports whether the cache is loaded for the given generation — the
// query path's zero-I/O check (RFC-094 §4: queries never pay a cache-
// maintenance round trip; refresh runs on the maintainer's timer in 094.3,
// and in 094.1 the topology is static per generation, so a loaded cache stays
// valid until the generation changes).
func (c *spfreshRoutingCache) ready(currentGeneration int64) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cursor != nil && c.generation == currentGeneration && len(c.coarseIDs) > 0
}

// spfreshRefreshIntervalMs is the amortized refresh cadence: between
// refreshes, queries ride the searcher's ≤depth-1 posting-HDR tolerance and
// inserts the write fence's forward-follow.
const spfreshRefreshIntervalMs = 1000

// maybeRefresh applies changelog deltas at most once per interval — the
// in-process form of §4's "refresh runs on the maintainer's timer". Exactly
// one caller per interval wins the CAS and pays the read; a horizon overrun
// or topology delta needing L1 escalates to a full reload.
func (c *spfreshRoutingCache) maybeRefresh(tx fdb.ReadTransaction, s *spfreshStorage, currentGeneration int64) error {
	now := spfreshNowMs()
	last := c.lastRefreshMs.Load()
	if now-last < spfreshRefreshIntervalMs || !c.lastRefreshMs.CompareAndSwap(last, now) {
		return nil
	}
	if err := c.refresh(tx, s, currentGeneration); err != nil {
		if errors.Is(err, errSPFreshNotFound) {
			return c.fullReload(tx, s, currentGeneration)
		}
		return err
	}
	return nil
}

// refresh applies changelog deltas since the cursor (the background-timer
// body, RFC-094 §4). Returns errSPFreshNotFound when the cache needs a full
// reload instead (generation flip observed, or the cursor predates the GC
// horizon — detected by a generation delta or the caller comparing
// generations).
func (c *spfreshRoutingCache) refresh(tx fdb.ReadTransaction, s *spfreshStorage, currentGeneration int64) error {
	c.mu.RLock()
	gen, cursor := c.generation, c.cursor
	c.mu.RUnlock()
	if gen != currentGeneration || cursor == nil {
		return errSPFreshNotFound // full reload required
	}
	// GC horizon: a cursor that predates the changelog trim boundary has lost
	// its incremental history — the deltas it would have applied are gone.
	horizon, herr := tx.Snapshot().Get(s.metaKey(spfreshMetaHorizon)).Get()
	if herr != nil {
		return fmt.Errorf("spfresh: read GC horizon: %w", herr)
	}
	if horizon != nil && string(cursor) < string(horizon) {
		return errSPFreshNotFound // full reload required
	}

	deltas, last, err := spfreshReadDeltasSince(tx, s, cursor, 10000)
	if err != nil {
		return err
	}
	if len(deltas) == 0 {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	for _, d := range deltas {
		switch d.op {
		case spfreshOpGeneration:
			// Flip observed mid-stream: everything after is another world.
			return errSPFreshNotFound
		case spfreshOpAddCell:
			// New cell: nothing to do until its centroids are routed to; L1
			// gains it via the coarse row on the next cell-level delta or
			// reload. Conservative: force reload to pick up its vector.
			return errSPFreshNotFound
		case spfreshOpForwardCell:
			cellID := d.ids[0]
			c.coarseFwd[cellID] = [2]int64{d.ids[1], d.ids[2]}
			for i, id := range c.coarseIDs {
				if id == cellID {
					c.coarseIDs = append(c.coarseIDs[:i], c.coarseIDs[i+1:]...)
					c.coarseVecs = append(c.coarseVecs[:i], c.coarseVecs[i+1:]...)
					break
				}
			}
			c.evictCellLocked(cellID)
		case spfreshOpDeadCell:
			delete(c.coarseFwd, d.ids[0])
			c.evictCellLocked(d.ids[0])
		case spfreshOpAddFine, spfreshOpForwardFine, spfreshOpDeadFine:
			// Fine-level topology: evict the affected cell(s); they reload on
			// next use. (addFine carries the cell; forward/dead carry fineIDs
			// whose cell we may not have cached — evict by scan.)
			if d.op == spfreshOpAddFine {
				c.evictCellLocked(d.ids[0])
			} else {
				c.evictCellsContainingLocked(d.ids[0])
			}
		}
	}
	c.cursor = last
	return nil
}

// evictCell drops one L2 cell (lock-taking variant — lifecycle code that
// changed a cell's contents in its own transaction calls this post-write).
func (c *spfreshRoutingCache) evictCell(cellID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evictCellLocked(cellID)
}

func (c *spfreshRoutingCache) evictCellLocked(cellID int64) {
	if cell, ok := c.cells[cellID]; ok {
		c.lru.Remove(cell.lruElem)
		delete(c.cells, cellID)
	}
}

func (c *spfreshRoutingCache) evictCellsContainingLocked(fineID int64) {
	for cellID, cell := range c.cells {
		for _, id := range cell.fineIDs {
			if id == fineID {
				c.lru.Remove(cell.lruElem)
				delete(c.cells, cellID)
				break
			}
		}
	}
}

// ensureCell returns the cached cell, loading it (one range read, one reply at
// target fill) on miss and following a coarse-forward HDR if the cell moved.
func (c *spfreshRoutingCache) ensureCell(tx fdb.ReadTransaction, s *spfreshStorage, cellID int64) (*spfreshCachedCell, error) {
	c.mu.RLock()
	cell, ok := c.cells[cellID]
	c.mu.RUnlock()
	if ok {
		c.mu.Lock()
		c.lru.MoveToFront(cell.lruElem)
		c.mu.Unlock()
		return cell, nil
	}

	rows, fwdA, fwdB, err := spfreshLoadCell(tx, s, cellID)
	if err != nil {
		return nil, err
	}
	// An empty, unforwarded cell is a VALID state: the §6b cold-start
	// bootstrap creates exactly one until the first insert mints a centroid.
	// It caches as an empty cell (zero candidates) rather than erroring —
	// queries against an empty index return zero rows.
	cell = &spfreshCachedCell{fwd: [2]int64{fwdA, fwdB}}
	for _, r := range rows {
		vec, verr := r.row.vector()
		if verr != nil {
			return nil, verr
		}
		cell.fineIDs = append(cell.fineIDs, r.fineID)
		cell.vecs = append(cell.vecs, vec)
		cell.states = append(cell.states, r.row.state)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.cells[cellID]; ok {
		c.lru.MoveToFront(existing.lruElem)
		return existing, nil
	}
	cell.lruElem = c.lru.PushFront(cellID)
	c.cells[cellID] = cell
	for c.lru.Len() > c.maxCells {
		oldest := c.lru.Back()
		c.lru.Remove(oldest)
		delete(c.cells, oldest.Value.(int64))
	}
	return cell, nil
}

// spfreshRouted is one routed fine centroid: where it lives, its CACHED state
// (the authoritative row may have moved on — write fences re-read), and its
// vector (needed for residual encode/score).
type spfreshRouted struct {
	cellID int64
	fineID int64
	state  byte
	vec    []float64
	d2     float64
}

// route selects the kc nearest readable fine centroids for the query: scan
// L1 → probe the w nearest cells (loading missed L2 cells; following coarse
// forwards one hop) → scan their fine centroids (RFC-094 §4). Deterministic
// tie-breaks by id. Reads route ACTIVE and SEALED (a sealed posting still
// holds its members until SPLIT commits).
func (c *spfreshRoutingCache) route(tx fdb.ReadTransaction, s *spfreshStorage, query []float64, w, kc int) ([]spfreshRouted, error) {
	return c.routeStates(tx, s, query, w, kc, false)
}

// routeForWrite is the insert variant: SEALED candidates are KEPT (a row
// cached as SEALED may be FORWARD in storage by now — the write fence must
// see it to follow the children; dropping it broke the forward-follow
// recovery during the post-split staleness window, codex 094.2 r3) but they
// do NOT count toward the kc budget — the fence rejects true-SEALED anyway,
// so letting them consume slots starved inserts of ACTIVE fallbacks when a
// split wave sealed many nearby centroids (codex 094.2 r2).
func (c *spfreshRoutingCache) routeForWrite(tx fdb.ReadTransaction, s *spfreshStorage, query []float64, w, kc int) ([]spfreshRouted, error) {
	return c.routeStates(tx, s, query, w, kc, true)
}

func (c *spfreshRoutingCache) routeStates(tx fdb.ReadTransaction, s *spfreshStorage, query []float64, w, kc int, writeBudget bool) ([]spfreshRouted, error) {
	c.mu.RLock()
	ids := c.coarseIDs
	vecs := c.coarseVecs
	c.mu.RUnlock()
	if len(ids) == 0 {
		return nil, errSPFreshEmptyRouting
	}

	cells := spfreshNearestK(query, ids, vecs, w)

	var routed []spfreshRouted
	seenCells := map[int64]bool{}
	var probe func(cellID int64, depth int) error
	probe = func(cellID int64, depth int) error {
		if seenCells[cellID] || depth > 2 {
			return nil
		}
		seenCells[cellID] = true
		cell, err := c.ensureCell(tx, s, cellID)
		if err != nil {
			return err
		}
		if len(cell.fineIDs) == 0 {
			// Coarse-forwarded: follow one hop to the children.
			if cell.fwd[0] != 0 {
				if err := probe(cell.fwd[0], depth+1); err != nil {
					return err
				}
				return probe(cell.fwd[1], depth+1)
			}
			return nil
		}
		for i, fineID := range cell.fineIDs {
			// ACTIVE and SEALED both route: a SEALED posting still holds its
			// members until SPLIT commits (filtering it from queries hid them
			// for the whole seal window — codex 094.2 r1), and a row cached
			// as SEALED may already be FORWARD in storage (the write fence
			// follows it to the children — codex 094.2 r3).
			if cell.states[i] != spfreshStateActive && cell.states[i] != spfreshStateSealed {
				continue
			}
			routed = append(routed, spfreshRouted{
				cellID: cellID,
				fineID: fineID,
				state:  cell.states[i],
				vec:    cell.vecs[i],
				d2:     spfreshSquaredDistance(query, cell.vecs[i]),
			})
		}
		return nil
	}
	for _, cand := range cells {
		if err := probe(cand.id, 0); err != nil {
			return nil, err
		}
	}

	sort.Slice(routed, func(i, j int) bool {
		if routed[i].d2 != routed[j].d2 {
			return routed[i].d2 < routed[j].d2
		}
		return routed[i].fineID < routed[j].fineID
	})
	if writeBudget {
		// Separate budgets per state, one sorted pass: up to kc ACTIVE and
		// up to kc SEALED. SEALED rides along for the fence to resolve (it
		// may be FORWARD in storage — codex r3) but can never crowd out the
		// ACTIVE fallbacks: a single combined cap did exactly that when a
		// split wave left more than the cap's worth of SEALED rows sorted
		// ahead of the first ACTIVE one (codex r4). Total ≤ 2·kc bounds the
		// fence's worst-case state reads.
		kept := routed[:0]
		active, sealed := 0, 0
		for _, r := range routed {
			if active >= kc {
				break
			}
			switch r.state {
			case spfreshStateActive:
				active++
			case spfreshStateSealed:
				if sealed >= kc {
					continue
				}
				sealed++
			}
			kept = append(kept, r)
		}
		return kept, nil
	}
	if kc < len(routed) {
		routed = routed[:kc]
	}
	return routed, nil
}
