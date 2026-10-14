package placementprobe

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// fakeCluster is an in-memory Cluster. Storage addresses come from the
// \xff/keyServers entries in the data (value: comma-separated addresses), read
// at the transaction's version, like libfdb_c's transactional lookup.
type fakeCluster struct {
	data     map[string][]byte
	versions []int64
	reads    int
	commits  int64
	basis    string
	// atVersion, when set for a version, mutates a copy of the data that the
	// transaction at that version sees (a change inside the probe bracket).
	atVersion  map[int64]func(map[string][]byte)
	guardReads [][]byte
}

var _ Cluster = (*fakeCluster)(nil)

func (f *fakeCluster) AddressBasis() string { return f.basis }

func (f *fakeCluster) ReadVersion() (int64, error) {
	f.reads++
	if f.reads <= len(f.versions) {
		return f.versions[f.reads-1], nil
	}
	return f.versions[len(f.versions)-1] + int64(f.reads), nil
}

func (f *fakeCluster) At(version int64) (Txn, error) {
	if version <= 0 {
		return nil, errors.New("fake: no read version")
	}
	view := make(map[string][]byte, len(f.data))
	for k, v := range f.data {
		view[k] = v
	}
	if mutate := f.atVersion[version]; mutate != nil {
		mutate(view)
	}
	return fakeTxn{data: view}, nil
}

type fakeTxn struct{ data map[string][]byte }

func (t fakeTxn) sorted() []string {
	keys := make([]string, 0, len(t.data))
	for k := range t.data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (t fakeTxn) Get(key []byte) ([]byte, bool, error) {
	v, ok := t.data[string(key)]
	return v, ok, nil
}

func (t fakeTxn) Range(begin, end []byte, fromLastLessOrEqual bool) ([]KV, error) {
	keys := t.sorted()
	start := sort.SearchStrings(keys, string(begin))
	if fromLastLessOrEqual && (start == len(keys) || keys[start] != string(begin)) && start > 0 {
		start--
	}
	var out []KV
	for _, k := range keys[start:] {
		if k >= string(end) {
			break
		}
		out = append(out, KV{Key: []byte(k), Value: t.data[k]})
	}
	return out, nil
}

func (t fakeTxn) Addresses(keys [][]byte) ([][]string, error) {
	out := make([][]string, len(keys))
	for i, k := range keys {
		at := concat(KeyServersPrefix, k)
		kvs, _ := t.Range(at, concat(at, []byte{0}), true) // the entry covering k
		if len(kvs) == 0 {
			return nil, errors.New("fake: no keyServers entry")
		}
		out[i] = strings.Split(string(kvs[0].Value), ",")
	}
	return out, nil
}

func (f *fakeCluster) Commit(guard func(read Reader) error, writes []Mutation) (int64, error) {
	read := func(key []byte) ([]byte, bool, error) {
		f.guardReads = append(f.guardReads, append([]byte(nil), key...))
		v, ok := f.data[string(key)]
		return v, ok, nil
	}
	if guard != nil {
		if err := guard(read); err != nil {
			return 0, err
		}
	}
	for _, m := range writes {
		switch m.Op {
		case OpSet:
			f.data[string(m.Key)] = append([]byte(nil), m.Value...)
		case OpClear:
			delete(f.data, string(m.Key))
		case OpClearRange:
			for k := range f.data {
				if k >= string(m.Key) && k < string(m.End) {
					delete(f.data, k)
				}
			}
		}
	}
	f.commits++
	return 5000 + f.commits, nil
}

func pack(prefix []byte, t tuple.Tuple) []byte { return concat(prefix, t.Pack()) }

func ddValue(mode int32) []byte {
	v := make([]byte, 4)
	binary.LittleEndian.PutUint32(v, uint32(mode))
	return v
}

func ksKey(k []byte) string { return string(concat(KeyServersPrefix, k)) }

var (
	writeToken = []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	testSHA    = strings.Repeat("a", 64)
	testID     = ClusterIdentity{ConnectionStringID: "fdbbenchvpc:mn20261014", ClusterFileSHA256: strings.Repeat("c", 64)}
)

// synthetic is the cluster test_placementprobe.py describes: 20 orders;
// keyServers entries at "", "\x01", prefix+(1,10) and prefix+(1,1000000);
// DD mode 0 with the data-distribution-mode lock as moveKeysLock owner; read
// versions 1001 and 1002; a store header that is out of scope.
func synthetic(prefix []byte, suffixes []int64) *fakeCluster {
	f := &fakeCluster{
		data: map[string][]byte{
			string(DDModeKey):            ddValue(0),
			string(MoveKeysLockOwnerKey): DDModeLock,
			string(MoveKeysLockWriteKey): writeToken,
		},
		versions: []int64{1001, 1002},
		basis:    AddressTransactional,
	}
	f.data[ksKey(nil)] = []byte("10.88.0.11:4500,10.88.0.12:4500")
	f.data[ksKey([]byte{0x01})] = []byte("10.88.0.11:4504,10.88.0.12:4502")
	f.data[ksKey(pack(prefix, tuple.Tuple{int64(1), int64(10)}))] = []byte("10.88.0.12:4503,10.88.0.13:4502")
	f.data[ksKey(pack(prefix, tuple.Tuple{int64(1), int64(1_000_000)}))] = []byte("10.88.0.13:4503,10.88.0.11:4505")
	f.data[string(pack(prefix, tuple.Tuple{int64(0)}))] = []byte("store header: out of scope")
	for i := int64(0); i < 20; i++ {
		for _, s := range suffixes {
			f.data[string(pack(prefix, tuple.Tuple{int64(1), i, s}))] = []byte("v")
		}
		f.data[string(pack(prefix, tuple.Tuple{int64(2), IndexName, i, i}))] = []byte{}
	}
	return f
}

func owned(t *testing.T, name string) OwnedStore {
	t.Helper()
	p := stores[name].tuple.Pack()
	n, rows := len(p), 20
	r := FixtureReceipt{Phase: "fixture", Payload: stores[name].payload, PrefixBytes: &n, Rows: &rows}
	s, err := OpenStore(name, r)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func input(s OwnedStore) ProbeInput {
	return ProbeInput{Store: s, FixtureSHA256: testSHA, Cluster: testID}
}

func asJSON(t *testing.T, v any) any {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestProbeMatchesGoldens(t *testing.T) {
	for _, tc := range []struct {
		store    string
		suffixes []int64
	}{{"benchfetch/small", []int64{0}}, {"benchfetch/split", []int64{1, 2}}} {
		t.Run(tc.store, func(t *testing.T) {
			s := owned(t, tc.store)
			doc, err := Probe(synthetic(s.prefix, tc.suffixes), input(s))
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(filepath.Join("testdata", "probe-"+strings.ReplaceAll(tc.store, "/", "-")+".golden.json"))
			if err != nil {
				t.Fatal(err)
			}
			var golden any
			if err := json.Unmarshal(raw, &golden); err != nil {
				t.Fatal(err)
			}
			if got := asJSON(t, doc); !reflect.DeepEqual(got, golden) {
				gotJSON, _ := json.MarshalIndent(got, "", "  ")
				t.Fatalf("probe differs from golden (regenerate only via test_placementprobe.py --write-goldens):\n%s", gotJSON)
			}
		})
	}
}

func TestProbeListsOnlyTheTwoDataSubspaces(t *testing.T) {
	s := owned(t, "benchfetch/small")
	f := synthetic(s.prefix, []int64{0})
	ballast := BallastKey(s.prefix, 3)
	f.data[string(ballast)] = []byte("leftover")
	odd := pack(s.prefix, tuple.Tuple{int64(1), "not-an-int", int64(0)})
	f.data[string(odd)] = []byte("x")
	doc, err := Probe(f, input(s))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]ProbeKey{}
	for _, k := range doc.Keys {
		seen[k.KeyHex] = k
	}
	if _, ok := seen[hex.EncodeToString(pack(s.prefix, tuple.Tuple{int64(0)}))]; ok {
		t.Fatal("store header (metadata) must not be listed")
	}
	if k, ok := seen[hex.EncodeToString(ballast)]; !ok || k.ID == nil || *k.ID != BallastFirstID+3 {
		t.Fatalf("leftover ballast must be listed faithfully with its ID, got %+v", k)
	}
	if k, ok := seen[hex.EncodeToString(odd)]; !ok || k.ID != nil {
		t.Fatalf("a non-integer primary key must be listed with a null id, got %+v", k)
	}
	if len(doc.Keys) != 20*2+2 {
		t.Fatalf("got %d keys", len(doc.Keys))
	}
}

func TestProbeRequiresDDOffWithTheModeLock(t *testing.T) {
	s := owned(t, "benchfetch/small")
	cases := map[string]func(map[string][]byte){
		"mode key absent (on)": func(d map[string][]byte) { delete(d, string(DDModeKey)) },
		"mode 1":               func(d map[string][]byte) { d[string(DDModeKey)] = ddValue(1) },
		"mode 2":               func(d map[string][]byte) { d[string(DDModeKey)] = ddValue(2) },
		"owner is a data distributor": func(d map[string][]byte) {
			d[string(MoveKeysLockOwnerKey)] = uidBytes(7, 7)
		},
		"owner absent": func(d map[string][]byte) { delete(d, string(MoveKeysLockOwnerKey)) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := synthetic(s.prefix, []int64{0})
			mutate(f.data)
			if _, err := Probe(f, input(s)); err == nil {
				t.Fatal("probe emitted without DD off and the mode lock")
			}
		})
	}
	f := synthetic(s.prefix, []int64{0})
	delete(f.data, string(DDModeKey))
	in := input(s)
	in.Diagnostic = true
	doc, err := Probe(f, in)
	if err != nil {
		t.Fatal(err)
	}
	if doc.DDMode.RequiredOff || doc.DDMode.AtV1.Mode != 1 || doc.DDMode.AtV1.KeyPresent {
		t.Fatalf("diagnostic probe must record DD on: %+v", doc.DDMode)
	}
}

// A toggle (0 -> 1 -> 0) and relocation inside the bracket can leave every
// boundary key identical; the write token and the keyServers bytes catch it.
func TestProbeRefusesChangesInsideTheBracket(t *testing.T) {
	s := owned(t, "benchfetch/small")
	relocate := func(d map[string][]byte) {
		d[ksKey(pack(s.prefix, tuple.Tuple{int64(1), int64(10)}))] = []byte("10.88.0.11:4506,10.88.0.13:4504")
	}
	newToken := func(d map[string][]byte) { d[string(MoveKeysLockWriteKey)] = uidBytes(99, 99) }
	newBoundary := func(d map[string][]byte) {
		d[ksKey(pack(s.prefix, tuple.Tuple{int64(1), int64(15)}))] = []byte("10.88.0.12:4503,10.88.0.13:4502")
	}
	for name, tc := range map[string]struct {
		mutate func(map[string][]byte)
		want   string
	}{
		"toggle and relocate, identical boundaries": {func(d map[string][]byte) { newToken(d); relocate(d) }, "write token"},
		"relocation alone":                          {relocate, "keyServers"},
		"token alone":                               {newToken, "write token"},
		"new boundary":                              {newBoundary, "keyServers"},
	} {
		t.Run(name, func(t *testing.T) {
			f := synthetic(s.prefix, []int64{0})
			f.atVersion = map[int64]func(map[string][]byte){1002: tc.mutate}
			_, err := Probe(f, input(s))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error about %s", err, tc.want)
			}
		})
	}
}

func TestProbeRefusesALocationCacheBackend(t *testing.T) {
	s := owned(t, "benchfetch/small")
	f := synthetic(s.prefix, []int64{0})
	f.basis = AddressLocationCache
	if _, err := Probe(f, input(s)); err == nil || !strings.Contains(err.Error(), "diagnostic") {
		t.Fatalf("got %v", err)
	}
	in := input(s)
	in.Diagnostic = true
	doc, err := Probe(f, in)
	if err != nil || doc.AddressesBasis.Kind != AddressLocationCache {
		t.Fatalf("diagnostic probe: %v", err)
	}
}

func TestProbeAndOperationsRefuseAForeignPrefix(t *testing.T) {
	small := owned(t, "benchfetch/small")
	split := owned(t, "benchfetch/split")
	forged := OwnedStore{name: "benchfetch/small", prefix: split.prefix}
	f := synthetic(split.prefix, []int64{1, 2})
	before := len(f.data)
	if _, err := Probe(f, input(forged)); err == nil {
		t.Fatal("probe accepted a foreign prefix")
	}
	if _, err := WriteBallast(f, forged, 1<<20); err == nil {
		t.Fatal("ballast-write accepted a foreign prefix")
	}
	if _, err := ClearBallast(f, forged); err == nil {
		t.Fatal("ballast-clear accepted a foreign prefix")
	}
	if _, err := VerifyBallastEmpty(f, OwnedStore{}); err == nil {
		t.Fatal("a zero OwnedStore was accepted")
	}
	control, _ := OpenControlStore("benchfetch/control-split")
	forgedControl := OwnedStore{name: "benchfetch/control-split", prefix: small.prefix}
	if _, err := MutateControl(f, forgedControl, "stray-suffix"); err == nil {
		t.Fatal("control-mutate accepted a foreign prefix")
	}
	if _, err := ClearControl(f, forgedControl); err == nil {
		t.Fatal("control-clear accepted a foreign prefix")
	}
	if _, err := ClearControl(f, small); err == nil {
		t.Fatal("control-clear accepted a benchmark store")
	}
	if _, err := WriteBallast(f, control, 1<<20); err == nil {
		t.Fatal("ballast-write accepted a control store")
	}
	if f.commits != 0 || len(f.data) != before {
		t.Fatalf("a refused operation wrote: %d commits", f.commits)
	}
	n, rows := len(split.prefix), 20
	if _, err := OpenStore("benchfetch/small", FixtureReceipt{
		Phase: "fixture", Payload: "small", PrefixBytes: &n, Rows: &rows, StorePrefixHex: hex.EncodeToString(split.prefix),
	}); err == nil {
		t.Fatal("OpenStore accepted another store's prefix")
	}
}

func intPtr(v int) *int { return &v }

func TestParseFixtureReceipt(t *testing.T) {
	small := `{"phase":"fixture","payload":"small","payload_bytes":0,"chunks":1,"rows":1000,"split":false,"prefix_bytes":19,"case":"%s"}`
	split := `{"phase":"fixture","payload":"split","payload_bytes":100000,"chunks":2,"rows":1000,"split":true,"prefix_bytes":19,"case":"B"}`
	cell := `{"phase":"before","method":"SCAN_AND_FETCH","rows":10,"prefix_bytes":19,"case":"B"}`
	log := strings.Join([]string{
		"goos: linux",
		"    fixture_test.go:119: FDB_BENCH_AUDIT " + strings.Replace(small, "%s", "BenchmarkIndexFetch/payload=small", 1),
		"    fixture_test.go:119: FDB_BENCH_AUDIT " + strings.Replace(small, "%s", "BenchmarkIndexFetch/payload=small#01", 1),
		"    fixture_test.go:119: FDB_BENCH_AUDIT " + split,
		"    matrix_bench_test.go:150: FDB_BENCH_AUDIT " + cell,
		"PASS",
	}, "\n")
	r, err := ParseFixtureReceipt([]byte(log), "benchfetch/small")
	if err != nil {
		t.Fatal(err)
	}
	if r.Payload != "small" || *r.Rows != 1000 || *r.PrefixBytes != 19 || *r.Chunks != 1 {
		t.Fatalf("got %+v", r)
	}
	if r, err := ParseFixtureReceipt([]byte(log), "benchfetch/split"); err != nil || *r.Chunks != 2 {
		t.Fatalf("split: %+v %v", r, err)
	}
	if _, err := ParseFixtureReceipt([]byte(log), "benchfetch/planskip"); err == nil {
		t.Fatal("planskip has no payload filter, so two distinct receipts must be refused")
	}
	other := strings.Replace(small, `"rows":1000`, `"rows":999`, 1)
	if _, err := ParseFixtureReceipt([]byte(log+"\n FDB_BENCH_AUDIT "+strings.Replace(other, "%s", "X", 1)), "benchfetch/small"); err == nil {
		t.Fatal("two distinct small fixture receipts must be refused")
	}
	if _, err := ParseFixtureReceipt([]byte(`{"phase":"fixture","rows":1000,"prefix_bytes":22}`), "benchfetch/planskip"); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseFixtureReceipt([]byte(log), "benchfetch/other"); err == nil {
		t.Fatal("unknown store accepted")
	}
	if _, err := ParseFixtureReceipt([]byte("no receipts here\n"), "benchfetch/small"); err == nil {
		t.Fatal("missing receipt accepted")
	}
}

func TestResolvePrefix(t *testing.T) {
	smallPrefix := stores["benchfetch/small"].tuple.Pack()
	ok := FixtureReceipt{Phase: "fixture", Payload: "small", PrefixBytes: intPtr(len(smallPrefix)), Rows: intPtr(1000)}
	p, err := ResolvePrefix("benchfetch/small", ok)
	if err != nil || !bytes.Equal(p, smallPrefix) {
		t.Fatalf("got %x %v", p, err)
	}
	sqlPrefix := []byte{0x15, 0x1a, 0x15, 0x02}
	bad := map[string]struct {
		store string
		r     FixtureReceipt
	}{
		"unknown store":       {"benchfetch/nope", ok},
		"wrong payload":       {"benchfetch/split", ok},
		"wrong prefix length": {"benchfetch/small", FixtureReceipt{Phase: "fixture", Payload: "small", PrefixBytes: intPtr(3), Rows: intPtr(1000)}},
		"wrong prefix bytes": {"benchfetch/small", FixtureReceipt{
			Phase: "fixture", Payload: "small", PrefixBytes: intPtr(len(smallPrefix)), Rows: intPtr(1000),
			StorePrefixHex: hex.EncodeToString(stores["benchfetch/split"].tuple.Pack()),
		}},
		"not a fixture phase": {"benchfetch/small", FixtureReceipt{Phase: "before", Payload: "small", PrefixBytes: intPtr(len(smallPrefix)), Rows: intPtr(1000)}},
		"no rows":             {"benchfetch/small", FixtureReceipt{Phase: "fixture", Payload: "small", PrefixBytes: intPtr(len(smallPrefix))}},
		"sql without hex":     {"sql/IDX_A", FixtureReceipt{Phase: "fixture", PrefixBytes: intPtr(4), Rows: intPtr(1000)}},
		"sql system prefix":   {"sql/IDX_A", FixtureReceipt{Phase: "fixture", PrefixBytes: intPtr(2), Rows: intPtr(1000), StorePrefixHex: "ff02"}},
		"sql overlapping benchfetch": {"sql/IDX_A", FixtureReceipt{
			Phase: "fixture", PrefixBytes: intPtr(len(smallPrefix)), Rows: intPtr(1000), StorePrefixHex: hex.EncodeToString(smallPrefix),
		}},
	}
	for name, tc := range bad {
		if _, err := ResolvePrefix(tc.store, tc.r); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	p, err = ResolvePrefix("sql/IDX_A", FixtureReceipt{
		Phase: "fixture", PrefixBytes: intPtr(4), Rows: intPtr(1000), StorePrefixHex: hex.EncodeToString(sqlPrefix),
	})
	if err != nil || !bytes.Equal(p, sqlPrefix) {
		t.Fatalf("sql: %x %v", p, err)
	}
}

func TestParseClusterIdentity(t *testing.T) {
	id, err := ParseClusterIdentity([]byte("# generated\nfdbbenchvpc:mn20261014@10.88.0.11:4500,10.88.0.12:4500,10.88.0.13:4500\n"))
	if err != nil || id.ConnectionStringID != "fdbbenchvpc:mn20261014" || len(id.ClusterFileSHA256) != 64 {
		t.Fatalf("%+v %v", id, err)
	}
	for _, bad := range []string{"", "# only a comment\n", "no-at-sign\n", "@10.88.0.11:4500\n", "noid@10.88.0.11:4500\n"} {
		if _, err := ParseClusterIdentity([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestBallastRangeSitsBetweenRecordsAndIndex(t *testing.T) {
	prefix := stores["benchfetch/split"].tuple.Pack()
	begin, end, err := BallastRange(prefix)
	if err != nil {
		t.Fatal(err)
	}
	record, index := Subspaces(prefix)
	if !bytes.Equal(BallastKey(prefix, 0), begin) {
		t.Fatal("first ballast key must be the range start")
	}
	lastFixture := pack(prefix, tuple.Tuple{int64(1), int64(999), int64(2)})
	if bytes.Compare(lastFixture, begin) >= 0 || !bytes.HasPrefix(begin, record) {
		t.Fatal("ballast must start after every fixture record, inside the record subspace")
	}
	if !bytes.Equal(end, index) {
		t.Fatalf("ballast must end exactly at the index subspace: %x vs %x", end, index)
	}
}

func TestDDModeDecodingAndLockConstant(t *testing.T) {
	if m, err := DDMode(nil, false); err != nil || m != 1 {
		t.Fatalf("absent: %d %v", m, err)
	}
	if m, err := DDMode(ddValue(0), true); err != nil || m != 0 {
		t.Fatalf("off: %d %v", m, err)
	}
	if m, err := DDMode([]byte{2, 0, 0, 0}, true); err != nil || m != 2 {
		t.Fatalf("little endian: %d %v", m, err)
	}
	if _, err := DDMode([]byte("0"), true); err == nil {
		t.Fatal("a non-4-byte value must be refused")
	}
	// UID(6345, 3425) as two little-endian uint64.
	if hex.EncodeToString(DDModeLock) != "c918000000000000610d000000000000" {
		t.Fatalf("dataDistributionModeLock encoding %x", DDModeLock)
	}
}

func TestWriteBallastOnlyWithDDOnIntoAnEmptyRange(t *testing.T) {
	s := owned(t, "benchfetch/small")
	f := synthetic(s.prefix, []int64{0})
	if _, err := WriteBallast(f, s, 1<<20); !errors.Is(err, ErrDDMode) {
		t.Fatalf("DD off must refuse ballast writes, got %v", err)
	}
	delete(f.data, string(DDModeKey)) // absent = on
	before := len(f.data)
	r, err := WriteBallast(f, s, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	want := (1<<20 + BallastValueBytes - 1) / BallastValueBytes
	if r.KeysWritten != want || len(f.data) != before+want || r.Transactions != 1 {
		t.Fatalf("receipt %+v, keys %d", r, len(f.data)-before)
	}
	begin, end, _ := BallastRange(s.prefix)
	for i := 0; i < want; i++ {
		k := BallastKey(s.prefix, i)
		if bytes.Compare(k, begin) < 0 || bytes.Compare(k, end) >= 0 || !bytes.Equal(f.data[string(k)], BallastValue(i)) {
			t.Fatalf("ballast %d misplaced or wrong value", i)
		}
	}
	if !bytes.Equal(f.guardReads[len(f.guardReads)-1], DDModeKey) {
		t.Fatal("the DD mode must be read inside the writing transaction")
	}
	if _, err := WriteBallast(f, s, 1<<20); err == nil {
		t.Fatal("a non-empty ballast range must be refused")
	}
	big := synthetic(s.prefix, []int64{0})
	delete(big.data, string(DDModeKey))
	r, err = WriteBallast(big, s, 16<<20)
	if err != nil || r.Transactions != (r.KeysWritten+BallastKeysPerTransaction-1)/BallastKeysPerTransaction || r.Transactions < 2 {
		t.Fatalf("batching: %+v %v", r, err)
	}
}

func TestClearBallastOnlyWithDDOffAndOnlyTheBallast(t *testing.T) {
	s := owned(t, "benchfetch/small")
	f := synthetic(s.prefix, []int64{0})
	delete(f.data, string(DDModeKey))
	if _, err := WriteBallast(f, s, 1<<20); err != nil {
		t.Fatal(err)
	}
	for _, mode := range [][]byte{nil, ddValue(1), ddValue(2)} {
		delete(f.data, string(DDModeKey))
		if mode != nil {
			f.data[string(DDModeKey)] = mode
		}
		if _, err := ClearBallast(f, s); !errors.Is(err, ErrDDMode) {
			t.Fatalf("mode %v: clear must be refused, got %v", mode, err)
		}
	}
	f.data[string(DDModeKey)] = ddValue(0)
	r, err := ClearBallast(f, s)
	if err != nil || len(r.CommittedVersions) != 1 {
		t.Fatalf("%+v %v", r, err)
	}
	if !reflect.DeepEqual(f.data, synthetic(s.prefix, []int64{0}).data) {
		t.Fatal("clearing must remove exactly the ballast: fixture, index, header and system keys untouched")
	}
	v, err := VerifyBallastEmpty(f, s)
	if err != nil || v.Empty == nil || !*v.Empty || *v.KeysFound != 0 || v.ReadVersion <= 0 {
		t.Fatalf("verify: %+v %v", v, err)
	}
	f.data[string(BallastKey(s.prefix, 9))] = []byte("x")
	v, err = VerifyBallastEmpty(f, s)
	if err == nil || v == nil || *v.Empty || *v.KeysFound != 1 {
		t.Fatalf("non-empty verify must return a failing receipt: %+v %v", v, err)
	}
}

func TestControlLifecycle(t *testing.T) {
	s, err := OpenControlStore("benchfetch/control-split")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenControlStore("benchfetch/small"); err == nil {
		t.Fatal("a benchmark store is not a control store")
	}
	f := synthetic(stores["benchfetch/small"].tuple.Pack(), []int64{0})
	r, fx, err := WriteControlFixture(f, s, 20)
	if err != nil {
		t.Fatal(err)
	}
	if r.KeysWritten != 60 || *fx.Rows != 20 || *fx.Chunks != 2 || *fx.PrefixBytes != len(s.prefix) ||
		fx.StorePrefixHex != hex.EncodeToString(s.prefix) || fx.Payload != "control-split" {
		t.Fatalf("receipt %+v fixture %+v", r, fx)
	}
	reopened, err := OpenStore("benchfetch/control-split", fx)
	if err != nil || !bytes.Equal(reopened.prefix, s.prefix) {
		t.Fatalf("the control fixture receipt must resolve its own prefix: %v", err)
	}
	if _, _, err := WriteControlFixture(f, s, 20); err == nil {
		t.Fatal("a non-empty control store must be refused")
	}
	doc, err := Probe(f, input(reopened))
	if err != nil || len(doc.Keys) != 60 {
		t.Fatalf("probe of the control store: %v", err)
	}
	key := func(sfx int64) string {
		return string(pack(s.prefix, tuple.Tuple{int64(1), int64(ControlMutationID), sfx}))
	}
	if _, err := MutateControl(f, s, "delete-chunk"); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.data[key(2)]; ok {
		t.Fatal("chunk 2 must be deleted")
	}
	if _, err := MutateControl(f, s, "delete-chunk"); err == nil {
		t.Fatal("deleting an absent chunk must be refused")
	}
	for defect, sfx := range map[string]int64{"stray-suffix": 3, "version-key": -1} {
		if _, err := MutateControl(f, s, defect); err != nil {
			t.Fatal(err)
		}
		if _, ok := f.data[key(sfx)]; !ok {
			t.Fatalf("%s must write suffix %d", defect, sfx)
		}
	}
	smallControl, _ := OpenControlStore("benchfetch/control-small")
	if _, err := MutateControl(f, smallControl, "stray-suffix"); err == nil {
		t.Fatal("mutations run on the split control store only")
	}
	if _, err := MutateControl(f, s, "bogus"); err == nil {
		t.Fatal("unknown defect accepted")
	}
	c, err := ClearControl(f, s)
	if err != nil || !*c.Empty {
		t.Fatalf("clear: %+v %v", c, err)
	}
	end, _ := strinc(s.prefix)
	for k := range f.data {
		if k >= string(s.prefix) && k < string(end) {
			t.Fatal("control store not empty")
		}
	}
}

func TestWriteJSONExclusive(t *testing.T) {
	dir := t.TempDir()
	if inside, _ := InsideGitWorktree(filepath.Join(dir, "x.json")); inside {
		t.Skip("temporary directory lies inside a Git work tree")
	}
	path := filepath.Join(dir, "r.json")
	if err := WriteJSONExclusive(path, map[string]int{"a": 1}); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSONExclusive(path, map[string]int{"a": 2}); err == nil {
		t.Fatal("overwrote an existing receipt")
	}
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSONExclusive(filepath.Join(repo, "sub", "r.json"), 1); err == nil {
		t.Fatal("wrote inside a Git work tree")
	}
}
