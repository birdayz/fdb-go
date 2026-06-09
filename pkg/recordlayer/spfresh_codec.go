package recordlayer

import (
	"encoding/binary"
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/vectorcodec"
)

// SPFresh value codecs (RFC-094 §3). Vector-bearing values are RAW fixed-width
// layouts (no tuple wrapping): tuple bytes-encoding escapes 0x00 → 0x00FF, and
// fp16 vectors are zero-heavy, which would silently eat the one-reply byte
// budgets the layout is sized against. Small structured values (tasks,
// changelog, HDR payloads) are tuple-encoded — they are tiny and benefit from
// self-describing flexibility.
//
// ID 0 is reserved as "none" (the block allocator starts at 1), so a zero
// child/cell field always means absent.

// spfreshCentroidRow is the decoded form of a CENTROIDS or COARSE row.
// Layout: [state:1][epoch:8 LE][childA:8 LE][childB:8 LE][raw fp16 vector...].
// children are meaningful for FORWARD rows only (fine split: child fineIDs;
// coarse split: child cellIDs) and zero otherwise.
type spfreshCentroidRow struct {
	state    byte
	epoch    int64
	childA   int64
	childB   int64
	vecBytes []byte // vectorcodec HALF bytes; decode lazily
}

const spfreshCentroidHeaderLen = 1 + 8 + 8 + 8

func encodeCentroidRow(state byte, epoch, childA, childB int64, vec []float64) []byte {
	vb := vectorcodec.SerializeHalf(vec)
	buf := make([]byte, spfreshCentroidHeaderLen, spfreshCentroidHeaderLen+len(vb))
	buf[0] = state
	binary.LittleEndian.PutUint64(buf[1:], uint64(epoch))
	binary.LittleEndian.PutUint64(buf[9:], uint64(childA))
	binary.LittleEndian.PutUint64(buf[17:], uint64(childB))
	return append(buf, vb...)
}

func decodeCentroidRow(data []byte) (spfreshCentroidRow, error) {
	if len(data) < spfreshCentroidHeaderLen {
		return spfreshCentroidRow{}, fmt.Errorf("spfresh: centroid row too short: %d bytes", len(data))
	}
	row := spfreshCentroidRow{
		state:    data[0],
		epoch:    int64(binary.LittleEndian.Uint64(data[1:])),
		childA:   int64(binary.LittleEndian.Uint64(data[9:])),
		childB:   int64(binary.LittleEndian.Uint64(data[17:])),
		vecBytes: data[spfreshCentroidHeaderLen:],
	}
	if row.state > spfreshStateDead {
		return spfreshCentroidRow{}, fmt.Errorf("spfresh: invalid centroid state %d", row.state)
	}
	return row, nil
}

// vector decodes the row's fp16 payload to float64 components.
func (r spfreshCentroidRow) vector() ([]float64, error) {
	return vectorcodec.Deserialize(r.vecBytes)
}

// --- membership: the pk's authoritative copy-set, [fineID:8 LE]* ---

func encodeMembership(fineIDs []int64) []byte {
	buf := make([]byte, 8*len(fineIDs))
	for i, id := range fineIDs {
		binary.LittleEndian.PutUint64(buf[i*8:], uint64(id))
	}
	return buf
}

func decodeMembership(data []byte) ([]int64, error) {
	if len(data)%8 != 0 {
		return nil, fmt.Errorf("spfresh: membership value length %d not a multiple of 8", len(data))
	}
	ids := make([]int64, len(data)/8)
	for i := range ids {
		ids[i] = int64(binary.LittleEndian.Uint64(data[i*8:]))
	}
	return ids, nil
}

// --- task rows: Tuple{owner, leaseDeadlineMs, state, childA, childB} ---

// spfreshTaskRow is a claimed/unclaimed maintenance task (RFC-094 §3/§6/§8).
// owner == "" means unclaimed. state is task-kind-specific (cellfin wave
// states; unused 0 otherwise). childA/childB carry allocated child IDs for
// split/csplit (recorded at SEAL so a commit_unknown retry resumes, never
// re-mints).
type spfreshTaskRow struct {
	owner           string
	leaseDeadlineMs int64
	state           byte
	childA          int64
	childB          int64
}

func encodeTaskRow(t spfreshTaskRow) []byte {
	return tuple.Tuple{t.owner, t.leaseDeadlineMs, int64(t.state), t.childA, t.childB}.Pack()
}

func decodeTaskRow(data []byte) (spfreshTaskRow, error) {
	tup, err := tuple.Unpack(data)
	if err != nil {
		return spfreshTaskRow{}, fmt.Errorf("spfresh: unpack task row: %w", err)
	}
	if len(tup) != 5 {
		return spfreshTaskRow{}, fmt.Errorf("spfresh: task row has %d elements, want 5", len(tup))
	}
	owner, ok := tup[0].(string)
	if !ok {
		return spfreshTaskRow{}, fmt.Errorf("spfresh: task owner not a string")
	}
	lease, ok := tup[1].(int64)
	if !ok {
		return spfreshTaskRow{}, fmt.Errorf("spfresh: task lease not an int64")
	}
	st, ok := tup[2].(int64)
	if !ok || st < 0 || st > 255 {
		return spfreshTaskRow{}, fmt.Errorf("spfresh: task state invalid")
	}
	childA, ok := tup[3].(int64)
	if !ok {
		return spfreshTaskRow{}, fmt.Errorf("spfresh: task childA not an int64")
	}
	childB, ok := tup[4].(int64)
	if !ok {
		return spfreshTaskRow{}, fmt.Errorf("spfresh: task childB not an int64")
	}
	return spfreshTaskRow{owner: owner, leaseDeadlineMs: lease, state: byte(st), childA: childA, childB: childB}, nil
}

// --- HDR FORWARD payloads ---

// encodePostingHDR is the fine-split forward marker value: the children's
// cellID (children live in the parent's cell at split time, but a stale client
// must NEVER derive the cell from its own routing — RFC-094 §3) plus the two
// child fineIDs.
func encodePostingHDR(cellID, childA, childB int64) []byte {
	return tuple.Tuple{cellID, childA, childB}.Pack()
}

func decodePostingHDR(data []byte) (cellID, childA, childB int64, err error) {
	tup, err := tuple.Unpack(data)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("spfresh: unpack posting HDR: %w", err)
	}
	if len(tup) != 3 {
		return 0, 0, 0, fmt.Errorf("spfresh: posting HDR has %d elements, want 3", len(tup))
	}
	ids := make([]int64, 3)
	for i, e := range tup {
		v, ok := e.(int64)
		if !ok {
			return 0, 0, 0, fmt.Errorf("spfresh: posting HDR element %d not an int64", i)
		}
		ids[i] = v
	}
	return ids[0], ids[1], ids[2], nil
}

// encodeCellHDR is the coarse-split forward marker value inside the old cell's
// centroid range: the two child cellIDs.
func encodeCellHDR(cellA, cellB int64) []byte {
	return tuple.Tuple{cellA, cellB}.Pack()
}

func decodeCellHDR(data []byte) (cellA, cellB int64, err error) {
	tup, err := tuple.Unpack(data)
	if err != nil {
		return 0, 0, fmt.Errorf("spfresh: unpack cell HDR: %w", err)
	}
	if len(tup) != 2 {
		return 0, 0, fmt.Errorf("spfresh: cell HDR has %d elements, want 2", len(tup))
	}
	a, ok := tup[0].(int64)
	if !ok {
		return 0, 0, fmt.Errorf("spfresh: cell HDR element 0 not an int64")
	}
	b, ok := tup[1].(int64)
	if !ok {
		return 0, 0, fmt.Errorf("spfresh: cell HDR element 1 not an int64")
	}
	return a, b, nil
}

// --- changelog deltas: Tuple{op, ids...} ---

// Changelog operations (RFC-094 §3/§4): the client cache applies these in
// versionstamp order to track topology. Fine-level entry/vector contents are
// NOT logged — only topology (centroid/cell add/forward/dead and generation
// flips); postings are always read live.
const (
	spfreshOpAddFine     int64 = 0 // (cellID, fineID): new fine centroid
	spfreshOpForwardFine int64 = 1 // (fineID, childA, childB)
	spfreshOpDeadFine    int64 = 2 // (fineID)
	spfreshOpAddCell     int64 = 3 // (cellID)
	spfreshOpForwardCell int64 = 4 // (cellID, cellA, cellB)
	spfreshOpDeadCell    int64 = 5 // (cellID)
	spfreshOpGeneration  int64 = 6 // (generation): readable generation flipped
)

type spfreshDelta struct {
	op  int64
	ids []int64
}

func encodeDelta(d spfreshDelta) []byte {
	t := make(tuple.Tuple, 0, 1+len(d.ids))
	t = append(t, d.op)
	for _, id := range d.ids {
		t = append(t, id)
	}
	return t.Pack()
}

func decodeDelta(data []byte) (spfreshDelta, error) {
	tup, err := tuple.Unpack(data)
	if err != nil {
		return spfreshDelta{}, fmt.Errorf("spfresh: unpack delta: %w", err)
	}
	if len(tup) < 1 {
		return spfreshDelta{}, fmt.Errorf("spfresh: empty delta")
	}
	op, ok := tup[0].(int64)
	if !ok || op < spfreshOpAddFine || op > spfreshOpGeneration {
		return spfreshDelta{}, fmt.Errorf("spfresh: invalid delta op")
	}
	wantIDs := map[int64]int{
		spfreshOpAddFine: 2, spfreshOpForwardFine: 3, spfreshOpDeadFine: 1,
		spfreshOpAddCell: 1, spfreshOpForwardCell: 3, spfreshOpDeadCell: 1,
		spfreshOpGeneration: 1,
	}[op]
	if len(tup)-1 != wantIDs {
		return spfreshDelta{}, fmt.Errorf("spfresh: delta op %d has %d ids, want %d", op, len(tup)-1, wantIDs)
	}
	ids := make([]int64, wantIDs)
	for i := 0; i < wantIDs; i++ {
		v, ok := tup[1+i].(int64)
		if !ok {
			return spfreshDelta{}, fmt.Errorf("spfresh: delta id %d not an int64", i)
		}
		ids[i] = v
	}
	return spfreshDelta{op: op, ids: ids}, nil
}
