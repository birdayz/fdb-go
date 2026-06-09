package recordlayer

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
)

// SPFresh storage primitives (RFC-094 §3): every FDB read/write the index
// performs, with the read semantics the lifecycle arguments depend on made
// explicit at each site — REAL reads are deliberate conflict fences; snapshot
// reads are deliberately conflict-free. Callers compose these inside their own
// transactions; nothing here retries.

// errSPFreshNotFound marks a genuine "row absent" result, as opposed to a
// transient FDB error. Callers use errors.Is to branch on it.
var errSPFreshNotFound = errors.New("spfresh: not found")

// --- META: generation, ID blocks, transform ---

// readGenerationForWrite REAL-reads META's current-generation key — the write
// fence (RFC-094 §3): the value check covers transactions that start after a
// flip, the conflict range covers in-flight writers racing one. Returns
// errSPFreshNotFound when no generation has ever been established.
func spfreshReadGenerationForWrite(tx fdb.Transaction, s *spfreshStorage) (int64, error) {
	data, err := tx.Get(s.metaKey(spfreshMetaGeneration)).Get()
	if err != nil {
		return 0, fmt.Errorf("spfresh: read generation: %w", err)
	}
	if data == nil {
		return 0, errSPFreshNotFound
	}
	if len(data) != 8 {
		return 0, fmt.Errorf("spfresh: generation value has %d bytes, want 8", len(data))
	}
	return int64(binary.LittleEndian.Uint64(data)), nil
}

// readGenerationSnapshot is the query-path variant: no conflict range (queries
// never abort anyone; staleness degrades to partial-at-worst under MVCC).
func spfreshReadGenerationSnapshot(tx fdb.ReadTransaction, s *spfreshStorage) (int64, error) {
	data, err := tx.Snapshot().Get(s.metaKey(spfreshMetaGeneration)).Get()
	if err != nil {
		return 0, fmt.Errorf("spfresh: read generation (snapshot): %w", err)
	}
	if data == nil {
		return 0, errSPFreshNotFound
	}
	if len(data) != 8 {
		return 0, fmt.Errorf("spfresh: generation value has %d bytes, want 8", len(data))
	}
	return int64(binary.LittleEndian.Uint64(data)), nil
}

// spfreshSetGeneration writes the current readable generation (the flip).
// The caller's transaction is the atomic flip boundary.
func spfreshSetGeneration(tx fdb.Transaction, s *spfreshStorage, gen int64) {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], uint64(gen))
	tx.Set(s.metaKey(spfreshMetaGeneration), buf[:])
}

// spfreshIDBlockSize is the number of IDs handed out per allocator claim —
// one REAL RMW per 2^16 IDs removes fleet-wide split serialization through a
// single key (RFC-094 §3; FDB-author r3 #4).
const spfreshIDBlockSize = 1 << 16

// spfreshClaimIDBlock claims [start, start+spfreshIDBlockSize) from the META
// allocator. REAL read-modify-write — contention scope is concurrent
// claimers only (split/merge/build txs), and each claim amortizes 65k IDs.
// IDs start at 1; 0 is reserved as "none".
func spfreshClaimIDBlock(tx fdb.Transaction, s *spfreshStorage) (start int64, err error) {
	key := s.metaKey(spfreshMetaIDBlock)
	data, err := tx.Get(key).Get()
	if err != nil {
		return 0, fmt.Errorf("spfresh: read ID allocator: %w", err)
	}
	next := int64(1)
	if data != nil {
		if len(data) != 8 {
			return 0, fmt.Errorf("spfresh: ID allocator value has %d bytes, want 8", len(data))
		}
		next = int64(binary.LittleEndian.Uint64(data))
	}
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], uint64(next+spfreshIDBlockSize))
	tx.Set(key, buf[:])
	return next, nil
}

// --- CENTROIDS / COARSE ---

func spfreshSaveCentroid(tx fdb.Transaction, s *spfreshStorage, cellID, fineID int64, row []byte) {
	tx.Set(s.centroidKey(cellID, fineID), row)
}

// spfreshReadCentroidForWrite REAL-reads a fine centroid's state row — the
// insert/lifecycle fence (RFC-094 §5/§6). Absent (moved by a coarse split, or
// never existed) returns errSPFreshNotFound; the caller re-routes.
func spfreshReadCentroidForWrite(tx fdb.Transaction, s *spfreshStorage, cellID, fineID int64) (spfreshCentroidRow, error) {
	data, err := tx.Get(s.centroidKey(cellID, fineID)).Get()
	if err != nil {
		return spfreshCentroidRow{}, fmt.Errorf("spfresh: read centroid (%d,%d): %w", cellID, fineID, err)
	}
	if data == nil {
		return spfreshCentroidRow{}, errSPFreshNotFound
	}
	return decodeCentroidRow(data)
}

// spfreshCellRow is one entry from a cell load: a fine centroid or the HDR.
type spfreshCellRow struct {
	fineID int64
	row    spfreshCentroidRow
}

// spfreshLoadCell snapshot-reads a whole cell's centroid rows (the L2 cache
// fill — one reply at target fill). Returns the rows in key order and, when
// present, the coarse-forward HDR payload (cellA, cellB != 0).
func spfreshLoadCell(tx fdb.ReadTransaction, s *spfreshStorage, cellID int64) (rows []spfreshCellRow, fwdA, fwdB int64, err error) {
	r, err := s.cellRange(cellID)
	if err != nil {
		return nil, 0, 0, err
	}
	kvs, err := tx.Snapshot().GetRange(r, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).GetSliceWithError()
	if err != nil {
		return nil, 0, 0, fmt.Errorf("spfresh: load cell %d: %w", cellID, err)
	}
	for _, kv := range kvs {
		t, uerr := s.centroids.Unpack(kv.Key)
		if uerr != nil || len(t) != 2 {
			return nil, 0, 0, fmt.Errorf("spfresh: malformed centroid key in cell %d", cellID)
		}
		if t[1] == nil { // HDR: coarse-forward marker
			a, b, herr := decodeCellHDR(kv.Value)
			if herr != nil {
				return nil, 0, 0, herr
			}
			fwdA, fwdB = a, b
			continue
		}
		fineID, ok := t[1].(int64)
		if !ok {
			return nil, 0, 0, fmt.Errorf("spfresh: centroid key fineID not int64 in cell %d", cellID)
		}
		row, derr := decodeCentroidRow(kv.Value)
		if derr != nil {
			return nil, 0, 0, derr
		}
		rows = append(rows, spfreshCellRow{fineID: fineID, row: row})
	}
	return rows, fwdA, fwdB, nil
}

func spfreshSaveCoarse(tx fdb.Transaction, s *spfreshStorage, cellID int64, row []byte) {
	tx.Set(s.coarseKey(cellID), row)
}

// spfreshLoadAllCoarse snapshot-reads the full coarse table (the L1 cache —
// ~2.5k rows at 10M, a few replies, off the query path).
func spfreshLoadAllCoarse(tx fdb.ReadTransaction, s *spfreshStorage) (ids []int64, rows []spfreshCentroidRow, err error) {
	r, err := fdb.PrefixRange(s.coarse.Bytes())
	if err != nil {
		return nil, nil, fmt.Errorf("spfresh: coarse range: %w", err)
	}
	kvs, err := tx.Snapshot().GetRange(r, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).GetSliceWithError()
	if err != nil {
		return nil, nil, fmt.Errorf("spfresh: load coarse table: %w", err)
	}
	for _, kv := range kvs {
		t, uerr := s.coarse.Unpack(kv.Key)
		if uerr != nil || len(t) != 1 {
			return nil, nil, fmt.Errorf("spfresh: malformed coarse key")
		}
		id, ok := t[0].(int64)
		if !ok {
			return nil, nil, fmt.Errorf("spfresh: coarse cellID not int64")
		}
		row, derr := decodeCentroidRow(kv.Value)
		if derr != nil {
			return nil, nil, derr
		}
		ids = append(ids, id)
		rows = append(rows, row)
	}
	return ids, rows, nil
}

// --- POSTINGS ---

type spfreshPostingEntry struct {
	pk   tuple.Tuple
	code []byte
}

// spfreshLoadPostingSnapshot reads one posting (query path: snapshot, capped
// at limit rows — the fetch-cap backpressure guard, RFC-094 §4). The HDR, when
// present, is first in key order by construction and is returned decoded.
func spfreshLoadPostingSnapshot(tx fdb.ReadTransaction, s *spfreshStorage, fineID int64, limit int) (entries []spfreshPostingEntry, fwdCell, fwdA, fwdB int64, err error) {
	r, err := s.postingRange(fineID)
	if err != nil {
		return nil, 0, 0, 0, err
	}
	kvs, err := tx.Snapshot().GetRange(r, fdb.RangeOptions{Limit: limit, Mode: fdb.StreamingModeWantAll}).GetSliceWithError()
	if err != nil {
		return nil, 0, 0, 0, fmt.Errorf("spfresh: load posting %d: %w", fineID, err)
	}
	for _, kv := range kvs {
		pk, isEntry, perr := s.postingPK(kv.Key)
		if perr != nil {
			return nil, 0, 0, 0, perr
		}
		if !isEntry { // HDR
			c, a, b, herr := decodePostingHDR(kv.Value)
			if herr != nil {
				return nil, 0, 0, 0, herr
			}
			fwdCell, fwdA, fwdB = c, a, b
			continue
		}
		entries = append(entries, spfreshPostingEntry{pk: pk, code: kv.Value})
	}
	return entries, fwdCell, fwdA, fwdB, nil
}

// spfreshLoadPostingForSplit REAL-reads one posting — the split-tx read whose
// conflict range is load-bearing (RFC-094 §6: a concurrent update/delete
// clearing a parent key must abort this split so its retry sees truth; a
// snapshot read here would resurrect a moved/deleted entry).
func spfreshLoadPostingForSplit(tx fdb.Transaction, s *spfreshStorage, fineID int64) ([]spfreshPostingEntry, error) {
	r, err := s.postingRange(fineID)
	if err != nil {
		return nil, err
	}
	kvs, err := tx.GetRange(r, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).GetSliceWithError()
	if err != nil {
		return nil, fmt.Errorf("spfresh: load posting %d for split: %w", fineID, err)
	}
	entries := make([]spfreshPostingEntry, 0, len(kvs))
	for _, kv := range kvs {
		pk, isEntry, perr := s.postingPK(kv.Key)
		if perr != nil {
			return nil, perr
		}
		if !isEntry {
			continue // a pre-existing HDR (re-split of a residual) carries no entry
		}
		entries = append(entries, spfreshPostingEntry{pk: pk, code: kv.Value})
	}
	return entries, nil
}

// --- MEMBERSHIP ---

// spfreshReadMembership REAL-reads a pk's copy-set: the same-pk serialization
// point for insert/update/delete vs splits (RFC-094 §5).
func spfreshReadMembership(tx fdb.Transaction, s *spfreshStorage, pk tuple.Tuple) ([]int64, error) {
	data, err := tx.Get(s.membershipKey(pk)).Get()
	if err != nil {
		return nil, fmt.Errorf("spfresh: read membership: %w", err)
	}
	if data == nil {
		return nil, errSPFreshNotFound
	}
	return decodeMembership(data)
}

// --- COUNTERS (advisory; exact only at reconciliation) ---

func spfreshCounterAdd(tx fdb.Transaction, s *spfreshStorage, kind, id int64, delta int64) {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], uint64(delta))
	tx.Add(s.counterKey(kind, id), buf[:])
}

// spfreshCounterSet writes an exact value (split/merge reconciliation).
func spfreshCounterSet(tx fdb.Transaction, s *spfreshStorage, kind, id, value int64) {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], uint64(value))
	tx.Set(s.counterKey(kind, id), buf[:])
}

// spfreshCounterReadSnapshot reads a counter without a conflict range (the
// sampled trigger probe; reading your own ADD forces a real storage read via
// RYW but takes no range — RFC-094 §5).
func spfreshCounterReadSnapshot(tx fdb.ReadTransaction, s *spfreshStorage, kind, id int64) (int64, error) {
	data, err := tx.Snapshot().Get(s.counterKey(kind, id)).Get()
	if err != nil {
		return 0, fmt.Errorf("spfresh: read counter (%d,%d): %w", kind, id, err)
	}
	if data == nil {
		return 0, nil
	}
	if len(data) != 8 {
		return 0, fmt.Errorf("spfresh: counter value has %d bytes, want 8", len(data))
	}
	return int64(binary.LittleEndian.Uint64(data)), nil
}

// --- TASKS (deterministic keys; REAL-read probes; lease claims) ---

// spfreshTaskSetIfAbsent REAL-reads the task row and Sets an unclaimed task
// only when absent. The read's conflict range is the point (RFC-094 §3): a
// claim committing concurrently aborts this probe at the resolver — a blind
// or snapshot-checked Set could clobber a live claim's lease/childIDs (the
// rev-4 RFC race). Returns true when this tx wrote the task.
func spfreshTaskSetIfAbsent(tx fdb.Transaction, s *spfreshStorage, kind, id int64) (bool, error) {
	key := s.taskKey(kind, id)
	data, err := tx.Get(key).Get()
	if err != nil {
		return false, fmt.Errorf("spfresh: probe task (%d,%d): %w", kind, id, err)
	}
	if data != nil {
		return false, nil
	}
	tx.Set(key, encodeTaskRow(spfreshTaskRow{}))
	return true, nil
}

// spfreshTaskClaim REAL-reads and claims a task row: unclaimed, lease-expired,
// or already-ours rows are (re)claimed with the new lease; a live foreign
// lease returns errSPFreshNotFound (nothing to do here). Absent rows error
// with errSPFreshNotFound too — enqueue first.
func spfreshTaskClaim(tx fdb.Transaction, s *spfreshStorage, kind, id int64, owner string, leaseDeadlineMs, nowMs int64) (spfreshTaskRow, error) {
	key := s.taskKey(kind, id)
	data, err := tx.Get(key).Get()
	if err != nil {
		return spfreshTaskRow{}, fmt.Errorf("spfresh: read task (%d,%d): %w", kind, id, err)
	}
	if data == nil {
		return spfreshTaskRow{}, errSPFreshNotFound
	}
	row, err := decodeTaskRow(data)
	if err != nil {
		return spfreshTaskRow{}, err
	}
	if row.owner != "" && row.owner != owner && row.leaseDeadlineMs > nowMs {
		return spfreshTaskRow{}, errSPFreshNotFound // live foreign lease
	}
	row.owner = owner
	row.leaseDeadlineMs = leaseDeadlineMs
	tx.Set(key, encodeTaskRow(row))
	return row, nil
}

// --- CHANGELOG (versionstamped, ordered; distinct user-versions per tx) ---

// spfreshAppendDeltas writes the deltas with versionstamped keys; the 2-byte
// user-version disambiguates multiple entries in one transaction (RFC-094 §3;
// FDB-author r3 #6).
func spfreshAppendDeltas(tx fdb.Transaction, s *spfreshStorage, deltas []spfreshDelta) error {
	for i, d := range deltas {
		if i > 0xffff {
			return fmt.Errorf("spfresh: too many deltas in one tx: %d", len(deltas))
		}
		key, err := s.changelog.PackWithVersionstamp(tuple.Tuple{tuple.IncompleteVersionstamp(uint16(i))})
		if err != nil {
			return fmt.Errorf("spfresh: pack changelog key: %w", err)
		}
		tx.SetVersionstampedKey(key, encodeDelta(d))
	}
	return nil
}

// spfreshReadDeltasSince snapshot-reads changelog entries with keys strictly
// greater than from (an opaque key from a previous read; nil = from the
// start). Returns the deltas in commit order and the last key for the next
// incremental read.
func spfreshReadDeltasSince(tx fdb.ReadTransaction, s *spfreshStorage, from fdb.Key, limit int) (deltas []spfreshDelta, last fdb.Key, err error) {
	r, err := fdb.PrefixRange(s.changelog.Bytes())
	if err != nil {
		return nil, nil, fmt.Errorf("spfresh: changelog range: %w", err)
	}
	if from != nil {
		r.Begin = fdb.Key(append(append([]byte{}, from...), 0x00))
	}
	kvs, err := tx.Snapshot().GetRange(r, fdb.RangeOptions{Limit: limit, Mode: fdb.StreamingModeWantAll}).GetSliceWithError()
	if err != nil {
		return nil, nil, fmt.Errorf("spfresh: read changelog: %w", err)
	}
	for _, kv := range kvs {
		d, derr := decodeDelta(kv.Value)
		if derr != nil {
			return nil, nil, derr
		}
		deltas = append(deltas, d)
		last = append(last[:0:0], kv.Key...)
	}
	if last == nil {
		last = from
	}
	return deltas, last, nil
}

// --- SIDECAR / STAGING ---

func spfreshSaveSidecar(tx fdb.Transaction, s *spfreshStorage, pk tuple.Tuple, fp16 []byte) {
	tx.Set(s.sidecarKey(pk), fp16)
}

func spfreshSaveStaging(tx fdb.Transaction, s *spfreshStorage, cellID int64, pk tuple.Tuple, fp16 []byte) {
	tx.Set(s.stagingKey(cellID, pk), fp16)
}

// spfreshLoadStagingCell REAL-reads a cell's staged vectors — the wave-B
// finalizer read whose conflict range serializes it against stragglers that
// commit during its window (RFC-094 §8); stragglers committing before its
// read version are returned as data and must be processed, not cleared.
func spfreshLoadStagingCell(tx fdb.Transaction, s *spfreshStorage, cellID int64) (pks []tuple.Tuple, vecs [][]byte, err error) {
	r, err := s.stagingCellRange(cellID)
	if err != nil {
		return nil, nil, err
	}
	kvs, err := tx.GetRange(r, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).GetSliceWithError()
	if err != nil {
		return nil, nil, fmt.Errorf("spfresh: load staging cell %d: %w", cellID, err)
	}
	for _, kv := range kvs {
		t, uerr := s.staging.Unpack(kv.Key)
		if uerr != nil || len(t) < 2 {
			return nil, nil, fmt.Errorf("spfresh: malformed staging key in cell %d", cellID)
		}
		pks = append(pks, tuple.Tuple(t[1:]))
		vecs = append(vecs, kv.Value)
	}
	return pks, vecs, nil
}
