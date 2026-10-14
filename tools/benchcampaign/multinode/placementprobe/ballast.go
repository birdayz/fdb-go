package placementprobe

import (
	"encoding/hex"
	"errors"
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// ErrDDMode reports a data-distribution mode that forbids the operation.
var ErrDDMode = errors.New("data-distribution mode forbids this operation")

// BallastReceipt is the H6 receipt for every write, clear and verification.
type BallastReceipt struct {
	Format            string          `json:"format"`
	Tool              string          `json:"tool"`
	Action            string          `json:"action"`
	Store             string          `json:"store"`
	StorePrefixHex    string          `json:"store_prefix_hex"`
	Cluster           ClusterIdentity `json:"cluster"`
	RangeHex          [2]string       `json:"range_hex"`
	DDModeRequired    string          `json:"dd_mode_required"`
	Defect            string          `json:"defect,omitempty"`
	KeysWritten       int             `json:"keys_written,omitempty"`
	ValueBytes        int             `json:"value_bytes,omitempty"`
	TotalBytes        int64           `json:"total_bytes,omitempty"`
	Transactions      int             `json:"transactions,omitempty"`
	CommittedVersions []int64         `json:"committed_versions,omitempty"`
	ReadVersion       int64           `json:"read_version,omitempty"`
	KeysFound         *int            `json:"keys_found,omitempty"`
	Empty             *bool           `json:"empty,omitempty"`
}

func newReceipt(action string, s OwnedStore, begin, end []byte, dd string) *BallastReceipt {
	return &BallastReceipt{
		Format: ReceiptFormat, Tool: Tool, Action: action, Store: s.name,
		StorePrefixHex: hex.EncodeToString(s.prefix),
		RangeHex:       [2]string{hex.EncodeToString(begin), hex.EncodeToString(end)},
		DDModeRequired: dd,
	}
}

// BallastKey is ballast entry i: prefix+(1, 1000000+i).
func BallastKey(prefix []byte, i int) []byte {
	return concat(prefix, tuple.Tuple{int64(RecordKey), int64(BallastFirstID + i)}.Pack())
}

// BallastValue is ballast entry i's deterministic value.
func BallastValue(i int) []byte {
	v := make([]byte, BallastValueBytes)
	for j := range v {
		v[j] = byte((i*31 + j) % 251)
	}
	return v
}

// ddGuard returns a Commit guard that reads the data-distribution mode in the
// committing transaction and accepts only the wanted state ("on" = mode 1,
// "off" = mode 0). The read joins the conflict set, so a concurrent change of
// the mode makes the commit retry and re-check.
func ddGuard(want string) func(Reader) error {
	return func(read Reader) error {
		v, present, err := read(DDModeKey)
		if err != nil {
			return err
		}
		mode, err := DDMode(v, present)
		if err != nil {
			return err
		}
		if (want == "on" && mode != 1) || (want == "off" && mode != 0) {
			return fmt.Errorf("%w: mode %d (key present %t), need %s", ErrDDMode, mode, present, want)
		}
		return nil
	}
}

// countKeys counts keys in [begin, end) at a fresh read version.
func countKeys(c Cluster, begin, end []byte) (int64, int, error) {
	v, err := c.ReadVersion()
	if err != nil {
		return 0, 0, err
	}
	t, err := c.At(v)
	if err != nil {
		return 0, 0, err
	}
	kvs, err := t.Range(begin, end, false)
	if err != nil {
		return 0, 0, err
	}
	return v, len(kvs), nil
}

// WriteBallast writes ceil(totalBytes / 100000) ballast values into the
// ballast range of an owned benchmark store, only while data distribution is
// on and only into an empty ballast range.
func WriteBallast(c Cluster, s OwnedStore, totalBytes int64) (*BallastReceipt, error) {
	if err := s.check(true, false); err != nil {
		return nil, err
	}
	if totalBytes <= 0 {
		return nil, errors.New("ballast size must be positive")
	}
	begin, end, err := BallastRange(s.prefix)
	if err != nil {
		return nil, err
	}
	r := newReceipt("ballast-write", s, begin, end, "on")
	_, found, err := countKeys(c, begin, end)
	if err != nil {
		return nil, err
	}
	if found != 0 {
		return nil, fmt.Errorf("ballast range already holds %d keys; clear it (with DD off) first", found)
	}
	n := int((totalBytes + BallastValueBytes - 1) / BallastValueBytes)
	for start := 0; start < n; start += BallastKeysPerTransaction {
		var writes []Mutation
		for i := start; i < n && i < start+BallastKeysPerTransaction; i++ {
			writes = append(writes, Mutation{Op: OpSet, Key: BallastKey(s.prefix, i), Value: BallastValue(i)})
		}
		v, err := c.Commit(ddGuard("on"), writes)
		if err != nil {
			return nil, fmt.Errorf("ballast batch at %d: %w", start, err)
		}
		r.CommittedVersions = append(r.CommittedVersions, v)
		r.Transactions++
		r.KeysWritten += len(writes)
	}
	r.ValueBytes = BallastValueBytes
	r.TotalBytes = int64(r.KeysWritten) * BallastValueBytes
	return r, nil
}

// ClearBallast clears exactly the ballast range, only while data distribution
// is off (mode key present and 0, read in the clearing transaction).
func ClearBallast(c Cluster, s OwnedStore) (*BallastReceipt, error) {
	if err := s.check(true, false); err != nil {
		return nil, err
	}
	begin, end, err := BallastRange(s.prefix)
	if err != nil {
		return nil, err
	}
	r := newReceipt("ballast-clear", s, begin, end, "off")
	v, err := c.Commit(ddGuard("off"), []Mutation{{Op: OpClearRange, Key: begin, End: end}})
	if err != nil {
		return nil, err
	}
	r.CommittedVersions = []int64{v}
	r.Transactions = 1
	return r, nil
}

func verifyEmpty(c Cluster, action string, s OwnedStore, begin, end []byte) (*BallastReceipt, error) {
	r := newReceipt(action, s, begin, end, "any")
	v, found, err := countKeys(c, begin, end)
	if err != nil {
		return nil, err
	}
	empty := found == 0
	r.ReadVersion, r.KeysFound, r.Empty = v, &found, &empty
	if !empty {
		return r, fmt.Errorf("range is not empty: %d keys at read version %d", found, v)
	}
	return r, nil
}

// VerifyBallastEmpty reads the whole ballast range at a fresh read version.
// It returns the receipt even when the range is not empty (with an error).
func VerifyBallastEmpty(c Cluster, s OwnedStore) (*BallastReceipt, error) {
	if err := s.check(true, false); err != nil {
		return nil, err
	}
	begin, end, err := BallastRange(s.prefix)
	if err != nil {
		return nil, err
	}
	return verifyEmpty(c, "ballast-verify-empty", s, begin, end)
}

// ControlKeys returns the canonical fixture keys of a control store:
// index prefix+(2,"price_idx",i,i) and records prefix+(1,i,0) (small) or
// prefix+(1,i,1), prefix+(1,i,2) (split).
func ControlKeys(s OwnedStore, rows int) ([]Mutation, int, error) {
	if err := s.check(false, true); err != nil {
		return nil, 0, err
	}
	suffixes := []int64{0}
	if stores[s.name].payload == "control-split" {
		suffixes = []int64{1, 2}
	}
	var writes []Mutation
	for i := 0; i < rows; i++ {
		id := int64(i)
		for _, sfx := range suffixes {
			writes = append(writes, Mutation{
				Op: OpSet, Key: concat(s.prefix, tuple.Tuple{int64(RecordKey), id, sfx}.Pack()),
				Value: []byte(fmt.Sprintf("control-%d-%d", i, sfx)),
			})
		}
		writes = append(writes, Mutation{
			Op: OpSet, Key: concat(s.prefix, tuple.Tuple{int64(IndexKey), IndexName, id, id}.Pack()), Value: []byte{},
		})
	}
	return writes, len(suffixes), nil
}

// WriteControlFixture writes a canonical fixture into an empty scratch control
// store and returns its fixture receipt for the probe.
func WriteControlFixture(c Cluster, s OwnedStore, rows int) (*BallastReceipt, FixtureReceipt, error) {
	if rows <= 0 {
		return nil, FixtureReceipt{}, errors.New("rows must be positive")
	}
	writes, chunks, err := ControlKeys(s, rows)
	if err != nil {
		return nil, FixtureReceipt{}, err
	}
	end, err := strinc(s.prefix)
	if err != nil {
		return nil, FixtureReceipt{}, err
	}
	_, found, err := countKeys(c, s.prefix, end)
	if err != nil {
		return nil, FixtureReceipt{}, err
	}
	if found != 0 {
		return nil, FixtureReceipt{}, fmt.Errorf("control store %s already holds %d keys; control-clear it first", s.name, found)
	}
	r := newReceipt("control-write", s, s.prefix, end, "any")
	const batch = 500
	for start := 0; start < len(writes); start += batch {
		stop := min(start+batch, len(writes))
		v, err := c.Commit(nil, writes[start:stop])
		if err != nil {
			return nil, FixtureReceipt{}, err
		}
		r.CommittedVersions = append(r.CommittedVersions, v)
		r.Transactions++
		r.KeysWritten += stop - start
	}
	prefixBytes, rowsCopy, chunksCopy := len(s.prefix), rows, chunks
	fx := FixtureReceipt{
		Phase: "fixture", Payload: stores[s.name].payload, PrefixBytes: &prefixBytes, Rows: &rowsCopy,
		Chunks: &chunksCopy, StorePrefixHex: hex.EncodeToString(s.prefix), Source: Tool + " control-write",
	}
	return r, fx, nil
}

// Control defects for the on-cluster mutation controls (protocol
// placement.controls (2)); all damage record ControlMutationID of the split
// control store.
var controlDefects = map[string]struct {
	suffix  int64
	op      Op
	present bool // the key must exist (delete) or must not (insert) beforehand
}{
	"delete-chunk": {suffix: 2, op: OpClear, present: true},
	"stray-suffix": {suffix: 3, op: OpSet, present: false},
	"version-key":  {suffix: -1, op: OpSet, present: false},
}

// MutateControl applies one defect to the split control store.
func MutateControl(c Cluster, s OwnedStore, defect string) (*BallastReceipt, error) {
	if err := s.check(false, true); err != nil {
		return nil, err
	}
	if stores[s.name].payload != "control-split" {
		return nil, errors.New("mutation controls run on benchfetch/control-split")
	}
	d, ok := controlDefects[defect]
	if !ok {
		return nil, fmt.Errorf("unknown defect %q (delete-chunk, stray-suffix, version-key)", defect)
	}
	key := concat(s.prefix, tuple.Tuple{int64(RecordKey), int64(ControlMutationID), d.suffix}.Pack())
	guard := func(read Reader) error {
		_, present, err := read(key)
		if err != nil {
			return err
		}
		if present != d.present {
			return fmt.Errorf("defect %s: key %x present=%t, expected %t", defect, key, present, d.present)
		}
		return nil
	}
	m := Mutation{Op: d.op, Key: key}
	if d.op == OpSet {
		m.Value = []byte("control-defect")
	}
	v, err := c.Commit(guard, []Mutation{m})
	if err != nil {
		return nil, err
	}
	r := newReceipt("control-mutate", s, key, key, "any")
	r.Defect, r.CommittedVersions, r.Transactions = defect, []int64{v}, 1
	return r, nil
}

// ClearControl clears a whole scratch control store and verifies it is empty.
func ClearControl(c Cluster, s OwnedStore) (*BallastReceipt, error) {
	if err := s.check(false, true); err != nil {
		return nil, err
	}
	end, err := strinc(s.prefix)
	if err != nil {
		return nil, err
	}
	v, err := c.Commit(nil, []Mutation{{Op: OpClearRange, Key: s.prefix, End: end}})
	if err != nil {
		return nil, err
	}
	r, err := verifyEmpty(c, "control-clear", s, s.prefix, end)
	if r != nil {
		r.CommittedVersions, r.Transactions = []int64{v}, 1
	}
	return r, err
}
