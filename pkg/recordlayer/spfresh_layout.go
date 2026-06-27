package recordlayer

import (
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// SPFresh on-disk layout (RFC-094 §3).
//
// Everything except META lives under a GENERATION prefix: an int chosen at
// build/retrain start. The Readable flip atomically updates META's current
// generation; abandoned builds are GC'd by range-clearing their generation,
// and a retrain builds generation g+1, flips, and clears g after a grace
// period >= the staleness horizon. Writers fence on META's current-generation
// key (value check + conflict range — RFC-094 §3); queries against a
// superseded generation degrade to partial-at-worst under MVCC.

// Subspace ordinals under S/(generation).
const (
	spfreshSubCentroids  int64 = 0 // (cellID, fineID) -> centroid row; (cellID, HDR) -> coarse forward
	spfreshSubCoarse     int64 = 1 // (cellID) -> coarse row
	spfreshSubPostings   int64 = 2 // (fineID, pk...) -> residual code; (fineID, HDR) -> fine forward
	spfreshSubMembership int64 = 3 // (pk...) -> [fineID...]
	spfreshSubCounters   int64 = 4 // (kind, id) -> int64 LE, atomic ADD
	spfreshSubChangelog  int64 = 5 // (versionstamp) -> delta
	spfreshSubTasks      int64 = 6 // (kind, id) -> task row
	spfreshSubSidecar    int64 = 7 // (pk...) -> raw fp16 vector
	spfreshSubStaging    int64 = 8 // (cellID, pk...) -> raw fp16 vector (build-only)
)

// META keys live under S/("m") — a STRING subspace element, type-disjoint from
// the int64 generation prefixes by tuple encoding (0x02 vs the int type bytes),
// so no generation's clear-range can ever cover META. Pinned by
// TestSPFreshGenerationIsolation, which caught the bare-int layout colliding
// (META key 1 was generation 1's range start — GC would have destroyed the
// allocator).
const spfreshMetaPrefix = "m"

const (
	spfreshMetaGeneration   int64 = 0 // current readable generation (int64)
	spfreshMetaIDBlock      int64 = 1 // centroid/cell ID block allocator (int64)
	spfreshMetaHorizon      int64 = 2 // changelog GC horizon (version)
	spfreshMetaTransform    int64 = 3 // RaBitQ transform (rotator seed + centroid)
	spfreshMetaBuild        int64 = 4 // build ownership token (opaque 16 bytes; see spfreshVerifyBuilderToken)
	spfreshMetaRefineCursor int64 = 5 // RFC-104 refinement round-robin cursor (generation int64 + relative membership key)
)

// Centroid / coarse-cell lifecycle states (RFC-094 §6, §6b).
const (
	spfreshStateActive byte = 0
	spfreshStateSealed byte = 1
	// spfreshStateForward: split committed; children carried in the row/HDR.
	spfreshStateForward byte = 2
	spfreshStateDead    byte = 3
)

// Build cellfin task states (RFC-094 §8). Deliberately distinct: between the
// waves a cell's centroids exist but its postings do not — a build-time
// straggler routes to the live path only on FINALIZED.
const (
	// Split-task lifecycle states (§6; the task row's state byte).
	spfreshSplitTaskPending byte = 0 // trigger filed, not sealed
	spfreshSplitTaskSealed  byte = 1 // SEALED, childIDs minted in the row

	spfreshCellfinClaimed       byte = 0
	spfreshCellfinCentroidsDone byte = 1
	spfreshCellfinFinalized     byte = 2
)

// Counter kinds (RFC-094 §3 — fineIDs and cellIDs share the block allocator,
// so the two counter families must be namespaced or they alias).
const (
	spfreshCounterFine int64 = 0
	spfreshCounterCell int64 = 1
)

// Task kinds.
const (
	spfreshTaskSplit   int64 = 0
	spfreshTaskMerge   int64 = 1
	spfreshTaskCSplit  int64 = 2
	spfreshTaskCellfin int64 = 3
	// spfreshTaskNPA is the post-split neighborhood reassignment follow-up
	// (§6 step 3); id = the split PARENT's fineID, childA/childB carried in
	// the row. Enqueued by the SPLIT transaction itself.
	spfreshTaskNPA int64 = 4
)

// spfreshLiveTaskKinds is every kind the rebalancer executes — the sweeper's
// pending probe scans EXACTLY these. A new task kind MUST be added here, or
// tenants whose only pending work is the new kind are silently never swept.
// Cellfin is deliberately absent: build bookkeeping, not live maintenance.
var spfreshLiveTaskKinds = []int64{spfreshTaskSplit, spfreshTaskMerge, spfreshTaskCSplit, spfreshTaskNPA}

// spfreshHDR is the reserved posting/centroid header key element: tuple nil
// (encodes as 0x00), which sorts strictly before every legal pk encoding —
// sound because the record layer rejects null primary-key components, so no pk
// element can begin with 0x00 (RFC-094 §3, pinned by a property test against
// the tuple encoder). The header carries FORWARD payloads after a split.
var spfreshHDR tuple.TupleElement = nil

// spfreshStorage resolves an index subspace + generation to the concrete
// keyspaces. One instance per (index, generation); cheap to construct.
type spfreshStorage struct {
	index      subspace.Subspace // the index's root subspace (META lives here)
	generation int64

	centroids  subspace.Subspace
	coarse     subspace.Subspace
	postings   subspace.Subspace
	membership subspace.Subspace
	counters   subspace.Subspace
	changelog  subspace.Subspace
	tasks      subspace.Subspace
	sidecar    subspace.Subspace
	staging    subspace.Subspace
}

func newSPFreshStorage(indexSubspace subspace.Subspace, generation int64) *spfreshStorage {
	gen := indexSubspace.Sub(generation)
	return &spfreshStorage{
		index:      indexSubspace,
		generation: generation,
		centroids:  gen.Sub(spfreshSubCentroids),
		coarse:     gen.Sub(spfreshSubCoarse),
		postings:   gen.Sub(spfreshSubPostings),
		membership: gen.Sub(spfreshSubMembership),
		counters:   gen.Sub(spfreshSubCounters),
		changelog:  gen.Sub(spfreshSubChangelog),
		tasks:      gen.Sub(spfreshSubTasks),
		sidecar:    gen.Sub(spfreshSubSidecar),
		staging:    gen.Sub(spfreshSubStaging),
	}
}

// generationRange returns the clear-range covering this generation's entire
// keyspace (abandoned-build GC / post-retrain purge).
func (s *spfreshStorage) generationRange() (fdb.KeyRange, error) {
	r, err := fdb.PrefixRange(s.index.Sub(s.generation).Bytes())
	if err != nil {
		return fdb.KeyRange{}, fmt.Errorf("spfresh: generation range: %w", err)
	}
	return r, nil
}

// --- key builders ---

func (s *spfreshStorage) metaKey(k int64) fdb.Key {
	return fdb.Key(s.index.Pack(tuple.Tuple{spfreshMetaPrefix, k}))
}

// centroidKey is the fine-centroid row key (cellID, fineID).
func (s *spfreshStorage) centroidKey(cellID, fineID int64) fdb.Key {
	return fdb.Key(s.centroids.Pack(tuple.Tuple{cellID, fineID}))
}

// centroidHDRKey is the coarse-forward marker inside a cell's centroid range.
func (s *spfreshStorage) centroidHDRKey(cellID int64) fdb.Key {
	return fdb.Key(s.centroids.Pack(tuple.Tuple{cellID, spfreshHDR}))
}

// cellRange covers one cell's centroid rows (including its HDR).
func (s *spfreshStorage) cellRange(cellID int64) (fdb.KeyRange, error) {
	r, err := fdb.PrefixRange(s.centroids.Pack(tuple.Tuple{cellID}))
	if err != nil {
		return fdb.KeyRange{}, fmt.Errorf("spfresh: cell range: %w", err)
	}
	return r, nil
}

func (s *spfreshStorage) coarseKey(cellID int64) fdb.Key {
	return fdb.Key(s.coarse.Pack(tuple.Tuple{cellID}))
}

// postingKey is one posting entry (fineID, pk-elements...). The pk's elements
// are appended directly (not nested) — pk tuples never contain nil, so the HDR
// (nil) sorts strictly before every entry of the posting.
func (s *spfreshStorage) postingKey(fineID int64, pk tuple.Tuple) fdb.Key {
	t := make(tuple.Tuple, 0, 1+len(pk))
	t = append(t, fineID)
	t = append(t, pk...)
	return fdb.Key(s.postings.Pack(t))
}

func (s *spfreshStorage) postingHDRKey(fineID int64) fdb.Key {
	return fdb.Key(s.postings.Pack(tuple.Tuple{fineID, spfreshHDR}))
}

// postingRange covers one posting (HDR + all entries).
func (s *spfreshStorage) postingRange(fineID int64) (fdb.KeyRange, error) {
	r, err := fdb.PrefixRange(s.postings.Pack(tuple.Tuple{fineID}))
	if err != nil {
		return fdb.KeyRange{}, fmt.Errorf("spfresh: posting range: %w", err)
	}
	return r, nil
}

// postingPK extracts the pk from a posting key. Returns ok=false for the HDR.
func (s *spfreshStorage) postingPK(key fdb.Key) (tuple.Tuple, bool, error) {
	t, err := s.postings.Unpack(key)
	if err != nil {
		return nil, false, fmt.Errorf("spfresh: unpack posting key: %w", err)
	}
	if len(t) < 2 {
		return nil, false, fmt.Errorf("spfresh: posting key too short: %d elements", len(t))
	}
	if t[1] == nil {
		return nil, false, nil // HDR
	}
	return tuple.Tuple(t[1:]), true, nil
}

// postingPKSpan is the boxing-free postingPK for the query hot loop: the pk's
// elements are packed FLAT after the (fineID) prefix (see postingKey), so the
// raw key suffix is byte-identical to what sidecarKey/membershipKey append
// after their own prefixes — a stable dedup key, a direct sidecar-key suffix,
// and decodable with tuple.Unpack only for the final winners. prefixLen is
// len(postings.Pack({fineID})), computed once per posting fetch. ok=false for
// the HDR row (the nil element encodes as 0x00; entry pks never contain nil,
// so no entry suffix starts with 0x00). The returned span aliases key.
func (s *spfreshStorage) postingPKSpan(key fdb.Key, prefixLen int) ([]byte, bool, error) {
	if len(key) <= prefixLen {
		return nil, false, fmt.Errorf("spfresh: posting key too short: %d bytes under a %d-byte prefix", len(key), prefixLen)
	}
	span := key[prefixLen:]
	if span[0] == 0x00 {
		return nil, false, nil // HDR
	}
	return span, true, nil
}

func (s *spfreshStorage) membershipKey(pk tuple.Tuple) fdb.Key {
	return fdb.Key(s.membership.Pack(pk))
}

func (s *spfreshStorage) counterKey(kind, id int64) fdb.Key {
	return fdb.Key(s.counters.Pack(tuple.Tuple{kind, id}))
}

func (s *spfreshStorage) taskKey(kind, id int64) fdb.Key {
	return fdb.Key(s.tasks.Pack(tuple.Tuple{kind, id}))
}

func (s *spfreshStorage) sidecarKey(pk tuple.Tuple) fdb.Key {
	return fdb.Key(s.sidecar.Pack(pk))
}

// sidecarKeyFromSpan builds the sidecar key from a posting-key pk span
// without decoding it: both keys append the same flat-packed pk elements to
// their prefixes.
func (s *spfreshStorage) sidecarKeyFromSpan(span string) fdb.Key {
	prefix := s.sidecar.Bytes()
	k := make([]byte, 0, len(prefix)+len(span))
	k = append(k, prefix...)
	return fdb.Key(append(k, span...))
}

func (s *spfreshStorage) stagingKey(cellID int64, pk tuple.Tuple) fdb.Key {
	t := make(tuple.Tuple, 0, 1+len(pk))
	t = append(t, cellID)
	t = append(t, pk...)
	return fdb.Key(s.staging.Pack(t))
}

// stagingCellRange covers one cell's staged vectors.
func (s *spfreshStorage) stagingCellRange(cellID int64) (fdb.KeyRange, error) {
	r, err := fdb.PrefixRange(s.staging.Pack(tuple.Tuple{cellID}))
	if err != nil {
		return fdb.KeyRange{}, fmt.Errorf("spfresh: staging cell range: %w", err)
	}
	return r, nil
}
