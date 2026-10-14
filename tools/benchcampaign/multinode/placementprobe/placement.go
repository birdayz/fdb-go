// Package placementprobe implements the multi-node campaign's placement probe
// (H5) and ballast/control writer (H6).
//
// The probe lists every physical key of a Record Layer store's record
// subspace prefix+(1,) and index subspace prefix+(2,), the store's shard
// boundaries, and the storage servers that hold each key, as the
// fdb-bench-placement-probe/v2 document that
// tools/benchcampaign/multinode/placement_receipt.py judges. Store metadata
// (header (0,), index state and other subspaces (3..10,)) is out of scope by
// design and is never listed.
//
// Versioning. Keys and boundaries are read at one read version V1. Storage
// addresses come from LocalityGetAddressesForKey, which in this client (and in
// libfdb_c, NativeAPI getAddressesForKey) answers from the client location
// cache and does NOT honour SetReadVersion. The probe therefore brackets the
// address lookups: it reads the data-distribution mode at V1, looks up every
// key's addresses in a fresh process (an empty location cache, so every shard
// is located from the cluster), then takes a second read version V2 and
// requires the boundary list at V2 to equal the list at V1 and, unless
// explicitly allowed, the data-distribution mode to be off (0) at both V1 and
// V2. With data distribution off no shard can move, so addresses located
// between V1 and V2 describe the shard map at V1. The receipt's per-shard
// consistency check and before/after compare remain the backstop.
//
// The ballast tool writes only into [prefix+(1,1000000), strinc(prefix+(1,)))
// and only while data distribution is on; it clears only that range and only
// while data distribution is off, reading \xff/dataDistributionMode inside the
// clearing transaction so a concurrent mode change conflicts the commit. Every
// prefix comes from the benchmark's own fixture receipt and must be one of the
// owned stores.
package placementprobe

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// Formats and fixed layout constants. They mirror placement_receipt.py.
const (
	ProbeFormat   = "fdb-bench-placement-probe/v2"
	ReceiptFormat = "fdb-bench-ballast-receipt/v1"
	Tool          = "placementprobe/v1"
	// AddressMethod documents how addresses were obtained; see the package comment.
	AddressMethod = "LocalityGetAddressesForKey: client location cache of a fresh process, NOT versioned; " +
		"bracketed by identical boundaries at read versions V1 and V2"

	RecordKey = 1
	IndexKey  = 2
	// BallastFirstID is the first record primary key the ballast uses; every
	// fixture ID is below it.
	BallastFirstID = 1_000_000
	// BallastValueBytes is the size of one ballast value.
	BallastValueBytes = 100_000
	// BallastKeysPerTransaction keeps each write transaction near 5 MB, below
	// FoundationDB's 10 MB transaction limit.
	BallastKeysPerTransaction = 50
	// ControlMutationID is the record the on-cluster mutation controls damage.
	ControlMutationID = 7
	// IndexName is the fixtures' VALUE index on price (= order ID).
	IndexName = "price_idx"
)

// DDModeKey is FoundationDB 7.3's dataDistributionModeKey
// (fdbclient/SystemData.cpp). Its value is a BinaryWriter-encoded int
// (4 bytes, little endian); absent means 1 (on). `fdbcli datadistribution off`
// writes 0 (ManagementAPI setDDMode).
var DDModeKey = []byte("\xff/dataDistributionMode")

// storeSpec describes one owned store.
type storeSpec struct {
	tuple   tuple.Tuple // nil for SQL: the prefix comes from the receipt
	payload string      // fixture receipt payload that must match, if any
	ballast bool        // ballast may be written/cleared
	control bool        // scratch control store (H6 qualification)
}

var stores = map[string]storeSpec{
	"benchfetch/small":         {tuple: tuple.Tuple{"benchfetch", "small"}, payload: "small", ballast: true},
	"benchfetch/split":         {tuple: tuple.Tuple{"benchfetch", "split"}, payload: "split", ballast: true},
	"benchfetch/planskip":      {tuple: tuple.Tuple{"benchfetch", "planskip"}, ballast: true},
	"benchfetch/control-small": {tuple: tuple.Tuple{"benchfetch", "control", "small"}, payload: "control-small", control: true},
	"benchfetch/control-split": {tuple: tuple.Tuple{"benchfetch", "control", "split"}, payload: "control-split", control: true},
	"sql/IDX_A":                {ballast: true},
}

// benchfetchPrefix is the tuple prefix every benchfetch store shares; a SQL
// store prefix must not overlap it.
var benchfetchPrefix = tuple.Tuple{"benchfetch"}.Pack()

// Stores returns the owned store names, sorted.
func Stores() []string {
	names := make([]string, 0, len(stores))
	for name := range stores {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// FixtureReceipt is the benchmark's untimed fixture receipt (the JSON after
// FDB_BENCH_AUDIT with phase "fixture"), or the H6 control equivalent.
type FixtureReceipt struct {
	Phase          string `json:"phase"`
	Payload        string `json:"payload,omitempty"`
	PrefixBytes    *int   `json:"prefix_bytes"`
	Rows           *int   `json:"rows"`
	Chunks         *int   `json:"chunks,omitempty"`
	StorePrefixHex string `json:"store_prefix_hex,omitempty"`
	Source         string `json:"source,omitempty"`
}

const auditMarker = "FDB_BENCH_AUDIT "

// ParseFixtureReceipt accepts either one JSON object or a benchmark log, from
// which it takes the FDB_BENCH_AUDIT lines with phase "fixture" (and, for
// stores that have one, the matching payload). Exactly one distinct receipt
// must remain; the per-benchmark "case" field is ignored for that comparison.
func ParseFixtureReceipt(data []byte, store string) (FixtureReceipt, error) {
	spec, ok := stores[store]
	if !ok {
		return FixtureReceipt{}, fmt.Errorf("store %q is not an owned store (%s)", store, strings.Join(Stores(), ", "))
	}
	var candidates []string
	if trimmed := bytes.TrimSpace(data); len(trimmed) > 0 && trimmed[0] == '{' {
		candidates = append(candidates, string(trimmed))
	} else {
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(make([]byte, 0, 1<<16), 1<<24)
		for sc.Scan() {
			line := sc.Text()
			if i := strings.Index(line, auditMarker); i >= 0 {
				candidates = append(candidates, line[i+len(auditMarker):])
			}
		}
		if err := sc.Err(); err != nil {
			return FixtureReceipt{}, fmt.Errorf("read fixture receipt: %w", err)
		}
	}
	distinct := map[string]FixtureReceipt{}
	for _, c := range candidates {
		var r FixtureReceipt
		if err := json.Unmarshal([]byte(c), &r); err != nil {
			return FixtureReceipt{}, fmt.Errorf("malformed fixture receipt %q: %w", c, err)
		}
		if r.Phase != "fixture" || (spec.payload != "" && r.Payload != spec.payload) {
			continue
		}
		canon, err := json.Marshal(r) // drops "case" and any other field
		if err != nil {
			return FixtureReceipt{}, err
		}
		distinct[string(canon)] = r
	}
	if len(distinct) != 1 {
		return FixtureReceipt{}, fmt.Errorf("found %d distinct fixture receipts for %s, need exactly 1", len(distinct), store)
	}
	var only FixtureReceipt
	for _, r := range distinct {
		only = r
	}
	return only, nil
}

// ResolvePrefix returns the owned store's key prefix, cross-checked against
// the fixture receipt. Unknown stores and any mismatch are refused.
func ResolvePrefix(store string, r FixtureReceipt) ([]byte, error) {
	spec, ok := stores[store]
	if !ok {
		return nil, fmt.Errorf("store %q is not an owned store", store)
	}
	if r.Phase != "fixture" {
		return nil, errors.New("fixture receipt phase is not \"fixture\"")
	}
	if spec.payload != "" && r.Payload != spec.payload {
		return nil, fmt.Errorf("fixture receipt payload %q, store %s needs %q", r.Payload, store, spec.payload)
	}
	if r.PrefixBytes == nil || r.Rows == nil || *r.Rows <= 0 {
		return nil, errors.New("fixture receipt lacks prefix_bytes or a positive rows")
	}
	var prefix []byte
	if spec.tuple == nil {
		if r.StorePrefixHex == "" {
			return nil, fmt.Errorf("store %s needs store_prefix_hex in its fixture receipt (H4)", store)
		}
		p, err := hex.DecodeString(r.StorePrefixHex)
		if err != nil {
			return nil, fmt.Errorf("store_prefix_hex: %w", err)
		}
		if len(p) == 0 || p[0] == 0xff {
			return nil, errors.New("store prefix is empty or in the system keyspace")
		}
		if bytes.HasPrefix(p, benchfetchPrefix) || bytes.HasPrefix(benchfetchPrefix, p) {
			return nil, errors.New("SQL store prefix overlaps the benchfetch stores")
		}
		prefix = p
	} else {
		prefix = spec.tuple.Pack()
		if r.StorePrefixHex != "" && r.StorePrefixHex != hex.EncodeToString(prefix) {
			return nil, fmt.Errorf("fixture receipt store_prefix_hex %s differs from %s's prefix %x", r.StorePrefixHex, store, prefix)
		}
	}
	if *r.PrefixBytes != len(prefix) {
		return nil, fmt.Errorf("fixture receipt prefix_bytes %d != %d for %s", *r.PrefixBytes, len(prefix), store)
	}
	return prefix, nil
}

// ControlPrefix returns a scratch control store's fixed tuple prefix. Control
// stores have no benchmark fixture receipt until control-write emits one.
func ControlPrefix(store string) ([]byte, error) {
	spec, ok := stores[store]
	if !ok || !spec.control {
		return nil, fmt.Errorf("store %q is not a scratch control store", store)
	}
	return spec.tuple.Pack(), nil
}

// concat returns a fresh a||b (never aliasing either argument).
func concat(a, b []byte) []byte {
	out := make([]byte, 0, len(a)+len(b))
	return append(append(out, a...), b...)
}

func strinc(key []byte) ([]byte, error) { return fdb.Strinc(key) }

// Subspaces returns the record and index data subspaces of prefix.
func Subspaces(prefix []byte) (record, index []byte) {
	return concat(prefix, tuple.Tuple{int64(RecordKey)}.Pack()), concat(prefix, tuple.Tuple{int64(IndexKey)}.Pack())
}

// BallastRange is [prefix+(1,1000000), strinc(prefix+(1,))).
func BallastRange(prefix []byte) (begin, end []byte, err error) {
	record, _ := Subspaces(prefix)
	end, err = strinc(record)
	if err != nil {
		return nil, nil, err
	}
	return concat(prefix, tuple.Tuple{int64(RecordKey), int64(BallastFirstID)}.Pack()), end, nil
}

// DDMode decodes a dataDistributionMode value; absent means 1 (on).
func DDMode(value []byte, present bool) (int32, error) {
	if !present {
		return 1, nil
	}
	if len(value) != 4 {
		return 0, fmt.Errorf("dataDistributionMode value has %d bytes, want 4", len(value))
	}
	return int32(binary.LittleEndian.Uint32(value)), nil
}

// DDModeRead is one observation of the data-distribution mode.
type DDModeRead struct {
	ReadVersion int64 `json:"read_version"`
	Mode        int32 `json:"mode"`
	KeyPresent  bool  `json:"key_present"`
}

// ProbeKey is one physical key with its storage addresses.
type ProbeKey struct {
	Kind      string   `json:"kind"`
	ID        *int64   `json:"id"`
	KeyHex    string   `json:"key_hex"`
	Addresses []string `json:"addresses"`
}

// AddressBasis records how the addresses were obtained.
type AddressBasis struct {
	Method                       string `json:"method"`
	LocatedAfterReadVersion      int64  `json:"located_after_read_version"`
	LocatedBeforeReadVersion     int64  `json:"located_before_read_version"`
	BoundariesRecheckedIdentical bool   `json:"boundaries_rechecked_identical"`
}

// DDModes records the data-distribution mode around the probe.
type DDModes struct {
	RequiredOff bool       `json:"required_off"`
	AtV1        DDModeRead `json:"at_v1"`
	AtV2        DDModeRead `json:"at_v2"`
}

// ProbeDoc is the fdb-bench-placement-probe/v2 document.
type ProbeDoc struct {
	Format                string       `json:"format"`
	Tool                  string       `json:"tool"`
	Store                 string       `json:"store"`
	StorePrefixHex        string       `json:"store_prefix_hex"`
	FixtureReceiptSHA256  string       `json:"fixture_receipt_sha256"`
	ReadVersion           int64        `json:"read_version"`
	BoundariesReadVersion int64        `json:"boundaries_read_version"`
	BoundaryRangeHex      [2]string    `json:"boundary_range_hex"`
	BoundariesComplete    bool         `json:"boundaries_complete"`
	BoundariesHex         []string     `json:"boundaries_hex"`
	ScannedSubspacesHex   [2]string    `json:"scanned_subspaces_hex"`
	KeysComplete          bool         `json:"keys_complete"`
	Keys                  []ProbeKey   `json:"keys"`
	AddressesBasis        AddressBasis `json:"addresses_basis"`
	DDMode                DDModes      `json:"dd_mode"`
}

// ProbeInput selects the store to probe.
type ProbeInput struct {
	Store         string
	Prefix        []byte
	FixtureSHA256 string
	// AllowDDOn permits a diagnostic probe while data distribution is on; the
	// document records it. Placement receipts used for labels require off.
	AllowDDOn bool
}

// keyID decodes the primary-key ID: element 1 of a record key (1, id, ...),
// the last element of an index key (2, index, value..., id). nil when the key
// does not decode to an integer there; the receipt then rejects it.
func keyID(kind string, prefix, key []byte) *int64 {
	t, err := tuple.Unpack(key[len(prefix):])
	if err != nil || len(t) < 2 {
		return nil
	}
	e := t[len(t)-1]
	if kind == "record" {
		e = t[1]
	}
	v, ok := e.(int64)
	if !ok {
		return nil
	}
	return &v
}

func hexAll(keys [][]byte) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = hex.EncodeToString(k)
	}
	return out
}

func readDDMode(c Cluster, version int64) (DDModeRead, error) {
	v, present, err := c.Get(DDModeKey, version, true)
	if err != nil {
		return DDModeRead{}, fmt.Errorf("read data-distribution mode: %w", err)
	}
	mode, err := DDMode(v, present)
	if err != nil {
		return DDModeRead{}, err
	}
	return DDModeRead{ReadVersion: version, Mode: mode, KeyPresent: present}, nil
}

// Probe builds the placement probe document. It never judges placement; the
// receipt does. It fails (and emits nothing) when the boundaries change
// between V1 and V2 or, unless AllowDDOn, data distribution is not off.
func Probe(c Cluster, in ProbeInput) (*ProbeDoc, error) {
	end, err := strinc(in.Prefix)
	if err != nil {
		return nil, err
	}
	record, index := Subspaces(in.Prefix)
	recordEnd, err := strinc(record)
	if err != nil {
		return nil, err
	}
	indexEnd, err := strinc(index)
	if err != nil {
		return nil, err
	}
	v1, err := c.ReadVersion()
	if err != nil {
		return nil, fmt.Errorf("read version V1: %w", err)
	}
	if v1 <= 0 {
		return nil, fmt.Errorf("read version V1 %d is not positive", v1)
	}
	dd1, err := readDDMode(c, v1)
	if err != nil {
		return nil, err
	}
	if !in.AllowDDOn && dd1.Mode != 0 {
		return nil, fmt.Errorf("data distribution mode is %d at V1, must be 0 (off)", dd1.Mode)
	}
	b1, err := c.BoundaryKeys(in.Prefix, end, v1)
	if err != nil {
		return nil, fmt.Errorf("boundaries at V1: %w", err)
	}
	doc := &ProbeDoc{
		Format:                ProbeFormat,
		Tool:                  Tool,
		Store:                 in.Store,
		StorePrefixHex:        hex.EncodeToString(in.Prefix),
		FixtureReceiptSHA256:  in.FixtureSHA256,
		ReadVersion:           v1,
		BoundariesReadVersion: v1,
		BoundaryRangeHex:      [2]string{hex.EncodeToString(in.Prefix), hex.EncodeToString(end)},
		BoundariesComplete:    true, // unlimited boundary read
		BoundariesHex:         hexAll(b1),
		ScannedSubspacesHex:   [2]string{hex.EncodeToString(record), hex.EncodeToString(index)},
		KeysComplete:          true, // unlimited key scans at V1
		Keys:                  []ProbeKey{},
	}
	for _, sub := range []struct {
		kind       string
		begin, end []byte
	}{{"record", record, recordEnd}, {"index", index, indexEnd}} {
		keys, err := c.Keys(sub.begin, sub.end, v1)
		if err != nil {
			return nil, fmt.Errorf("scan %s subspace at V1: %w", sub.kind, err)
		}
		for _, k := range keys {
			addrs, err := c.Addresses(k)
			if err != nil {
				return nil, fmt.Errorf("addresses for %x: %w", k, err)
			}
			addrs = append([]string(nil), addrs...)
			sort.Strings(addrs)
			doc.Keys = append(doc.Keys, ProbeKey{
				Kind: sub.kind, ID: keyID(sub.kind, in.Prefix, k), KeyHex: hex.EncodeToString(k), Addresses: addrs,
			})
		}
	}
	v2, err := c.ReadVersion()
	if err != nil {
		return nil, fmt.Errorf("read version V2: %w", err)
	}
	if v2 < v1 {
		return nil, fmt.Errorf("read version V2 %d precedes V1 %d", v2, v1)
	}
	dd2, err := readDDMode(c, v2)
	if err != nil {
		return nil, err
	}
	if !in.AllowDDOn && dd2.Mode != 0 {
		return nil, fmt.Errorf("data distribution mode is %d at V2, must be 0 (off)", dd2.Mode)
	}
	b2, err := c.BoundaryKeys(in.Prefix, end, v2)
	if err != nil {
		return nil, fmt.Errorf("boundaries at V2: %w", err)
	}
	if !sameKeys(b1, b2) {
		return nil, errors.New("shard boundaries changed between V1 and V2; placement is moving, no probe emitted")
	}
	doc.AddressesBasis = AddressBasis{
		Method: AddressMethod, LocatedAfterReadVersion: v1, LocatedBeforeReadVersion: v2,
		BoundariesRecheckedIdentical: true,
	}
	doc.DDMode = DDModes{RequiredOff: !in.AllowDDOn, AtV1: dd1, AtV2: dd2}
	return doc, nil
}

func sameKeys(a, b [][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}
