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

// fakeCluster is an in-memory Cluster. Shard i (the number of global
// boundaries <= key) is served by shards[i].
type fakeCluster struct {
	data       map[string][]byte
	boundaries [][]byte
	shards     [][]string
	versions   []int64
	reads      int
	commits    int64
	// boundariesAtV2, when set, replaces the boundary list from the second
	// boundary read on (a shard move during the probe).
	boundariesAtV2 [][]byte
	boundaryReads  int
	guardReads     [][]byte
}

var _ Cluster = (*fakeCluster)(nil)

func (f *fakeCluster) ReadVersion() (int64, error) {
	f.reads++
	if f.reads <= len(f.versions) {
		return f.versions[f.reads-1], nil
	}
	return f.versions[len(f.versions)-1] + int64(f.reads), nil
}

func inRange(k, begin, end []byte) bool {
	return bytes.Compare(k, begin) >= 0 && bytes.Compare(k, end) < 0
}

func (f *fakeCluster) BoundaryKeys(begin, end []byte, version int64) ([][]byte, error) {
	if version <= 0 {
		return nil, errors.New("fake: boundary read without a read version")
	}
	f.boundaryReads++
	all := f.boundaries
	if f.boundariesAtV2 != nil && f.boundaryReads > 1 {
		all = f.boundariesAtV2
	}
	out := [][]byte{}
	for _, b := range all {
		if inRange(b, begin, end) {
			out = append(out, b)
		}
	}
	return out, nil
}

func (f *fakeCluster) Keys(begin, end []byte, version int64) ([][]byte, error) {
	if version <= 0 {
		return nil, errors.New("fake: key read without a read version")
	}
	var out [][]byte
	for k := range f.data {
		if inRange([]byte(k), begin, end) {
			out = append(out, []byte(k))
		}
	}
	sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i], out[j]) < 0 })
	return out, nil
}

func (f *fakeCluster) Addresses(key []byte) ([]string, error) {
	shard := 0
	for _, b := range f.boundaries {
		if bytes.Compare(b, key) <= 0 {
			shard++
		}
	}
	return append([]string(nil), f.shards[shard]...), nil
}

func (f *fakeCluster) Get(key []byte, version int64, system bool) ([]byte, bool, error) {
	if len(key) > 0 && key[0] == 0xff && !system {
		return nil, false, errors.New("fake: system key without system option")
	}
	v, ok := f.data[string(key)]
	return v, ok, nil
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
				if inRange([]byte(k), m.Key, m.End) {
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

// synthetic is the cluster test_placementprobe.py describes: ROWS=20 orders,
// boundaries at "\x01", prefix+(1,10) and prefix+(1,1000000), DD mode 0,
// read versions 1001 and 1002, and a header key that is out of scope.
func synthetic(prefix []byte, suffixes []int64) *fakeCluster {
	f := &fakeCluster{
		data: map[string][]byte{string(DDModeKey): ddValue(0)},
		boundaries: [][]byte{
			{0x01}, pack(prefix, tuple.Tuple{int64(1), int64(10)}), pack(prefix, tuple.Tuple{int64(1), int64(1_000_000)}),
		},
		shards: [][]string{
			{"10.88.0.11:4500", "10.88.0.12:4500"},
			{"10.88.0.11:4504", "10.88.0.12:4502"},
			{"10.88.0.12:4503", "10.88.0.13:4502"},
			{"10.88.0.13:4503", "10.88.0.11:4505"},
		},
		versions: []int64{1001, 1002},
	}
	f.data[string(pack(prefix, tuple.Tuple{int64(0)}))] = []byte("store header: out of scope")
	for i := int64(0); i < 20; i++ {
		for _, s := range suffixes {
			f.data[string(pack(prefix, tuple.Tuple{int64(1), i, s}))] = []byte("v")
		}
		f.data[string(pack(prefix, tuple.Tuple{int64(2), IndexName, i, i}))] = []byte{}
	}
	return f
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
			prefix := stores[tc.store].tuple.Pack()
			doc, err := Probe(synthetic(prefix, tc.suffixes), ProbeInput{
				Store: tc.store, Prefix: prefix, FixtureSHA256: strings.Repeat("a", 64),
			})
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
	prefix := stores["benchfetch/small"].tuple.Pack()
	f := synthetic(prefix, []int64{0})
	ballast := BallastKey(prefix, 3)
	f.data[string(ballast)] = []byte("leftover")
	odd := pack(prefix, tuple.Tuple{int64(1), "not-an-int", int64(0)})
	f.data[string(odd)] = []byte("x")
	doc, err := Probe(f, ProbeInput{Store: "benchfetch/small", Prefix: prefix})
	if err != nil {
		t.Fatal(err)
	}
	header := hex.EncodeToString(pack(prefix, tuple.Tuple{int64(0)}))
	seen := map[string]ProbeKey{}
	for _, k := range doc.Keys {
		seen[k.KeyHex] = k
	}
	if _, ok := seen[header]; ok {
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

func TestProbeRequiresDDOffUnlessDiagnostic(t *testing.T) {
	prefix := stores["benchfetch/small"].tuple.Pack()
	for _, tc := range []struct {
		name    string
		value   []byte
		present bool
	}{{"absent key means on", nil, false}, {"mode 1", ddValue(1), true}, {"mode 2", ddValue(2), true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := synthetic(prefix, []int64{0})
			delete(f.data, string(DDModeKey))
			if tc.present {
				f.data[string(DDModeKey)] = tc.value
			}
			if _, err := Probe(f, ProbeInput{Store: "benchfetch/small", Prefix: prefix}); err == nil {
				t.Fatal("probe ran with data distribution not off")
			}
		})
	}
	f := synthetic(prefix, []int64{0})
	delete(f.data, string(DDModeKey))
	doc, err := Probe(f, ProbeInput{Store: "benchfetch/small", Prefix: prefix, AllowDDOn: true})
	if err != nil {
		t.Fatal(err)
	}
	if doc.DDMode.RequiredOff || doc.DDMode.AtV1.Mode != 1 || doc.DDMode.AtV1.KeyPresent {
		t.Fatalf("diagnostic probe must record DD on: %+v", doc.DDMode)
	}
}

func TestProbeRefusesMovingBoundaries(t *testing.T) {
	prefix := stores["benchfetch/small"].tuple.Pack()
	f := synthetic(prefix, []int64{0})
	f.boundariesAtV2 = append(append([][]byte(nil), f.boundaries...), pack(prefix, tuple.Tuple{int64(1), int64(15)}))
	sort.Slice(f.boundariesAtV2, func(i, j int) bool { return bytes.Compare(f.boundariesAtV2[i], f.boundariesAtV2[j]) < 0 })
	if _, err := Probe(f, ProbeInput{Store: "benchfetch/small", Prefix: prefix}); err == nil ||
		!strings.Contains(err.Error(), "changed between V1 and V2") {
		t.Fatalf("got %v", err)
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

func TestDDModeDecoding(t *testing.T) {
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
}

func TestWriteBallastOnlyWithDDOnIntoAnEmptyRange(t *testing.T) {
	prefix := stores["benchfetch/small"].tuple.Pack()
	f := synthetic(prefix, []int64{0})
	if _, err := WriteBallast(f, "benchfetch/small", prefix, 1<<20); !errors.Is(err, ErrDDMode) {
		t.Fatalf("DD off must refuse ballast writes, got %v", err)
	}
	delete(f.data, string(DDModeKey)) // absent = on
	before := len(f.data)
	r, err := WriteBallast(f, "benchfetch/small", prefix, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	want := (1<<20 + BallastValueBytes - 1) / BallastValueBytes
	if r.KeysWritten != want || len(f.data) != before+want || r.Transactions != 1 {
		t.Fatalf("receipt %+v, keys %d", r, len(f.data)-before)
	}
	begin, end, _ := BallastRange(prefix)
	for i := 0; i < want; i++ {
		k := BallastKey(prefix, i)
		if !inRange(k, begin, end) || !bytes.Equal(f.data[string(k)], BallastValue(i)) {
			t.Fatalf("ballast %d misplaced or wrong value", i)
		}
	}
	if !bytes.Equal(f.guardReads[len(f.guardReads)-1], DDModeKey) {
		t.Fatal("the DD mode must be read inside the writing transaction")
	}
	if _, err := WriteBallast(f, "benchfetch/small", prefix, 1<<20); err == nil {
		t.Fatal("a non-empty ballast range must be refused")
	}
	for _, store := range []string{"benchfetch/control-small", "benchfetch/nope"} {
		if _, err := WriteBallast(f, store, prefix, 1<<20); err == nil {
			t.Fatalf("%s accepted", store)
		}
	}
	big := synthetic(prefix, []int64{0})
	delete(big.data, string(DDModeKey))
	r, err = WriteBallast(big, "benchfetch/small", prefix, 16<<20)
	if err != nil || r.Transactions != (r.KeysWritten+BallastKeysPerTransaction-1)/BallastKeysPerTransaction || r.Transactions < 2 {
		t.Fatalf("batching: %+v %v", r, err)
	}
}

func TestClearBallastOnlyWithDDOffAndOnlyTheBallast(t *testing.T) {
	prefix := stores["benchfetch/small"].tuple.Pack()
	f := synthetic(prefix, []int64{0})
	delete(f.data, string(DDModeKey))
	if _, err := WriteBallast(f, "benchfetch/small", prefix, 1<<20); err != nil {
		t.Fatal(err)
	}
	for _, mode := range [][]byte{nil, ddValue(1), ddValue(2)} {
		delete(f.data, string(DDModeKey))
		if mode != nil {
			f.data[string(DDModeKey)] = mode
		}
		if _, err := ClearBallast(f, "benchfetch/small", prefix); !errors.Is(err, ErrDDMode) {
			t.Fatalf("mode %v: clear must be refused, got %v", mode, err)
		}
	}
	f.data[string(DDModeKey)] = ddValue(0)
	r, err := ClearBallast(f, "benchfetch/small", prefix)
	if err != nil || len(r.CommittedVersions) != 1 {
		t.Fatalf("%+v %v", r, err)
	}
	fresh := synthetic(prefix, []int64{0})
	if !reflect.DeepEqual(f.data, fresh.data) {
		t.Fatal("clearing must remove exactly the ballast: fixture, index, header and DD key untouched")
	}
	v, err := VerifyBallastEmpty(f, "benchfetch/small", prefix)
	if err != nil || v.Empty == nil || !*v.Empty || *v.KeysFound != 0 || v.ReadVersion <= 0 {
		t.Fatalf("verify: %+v %v", v, err)
	}
	f.data[string(BallastKey(prefix, 9))] = []byte("x")
	v, err = VerifyBallastEmpty(f, "benchfetch/small", prefix)
	if err == nil || v == nil || *v.Empty || *v.KeysFound != 1 {
		t.Fatalf("non-empty verify must return a failing receipt: %+v %v", v, err)
	}
}

func TestControlLifecycle(t *testing.T) {
	store := "benchfetch/control-split"
	prefix, err := ControlPrefix(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ControlPrefix("benchfetch/small"); err == nil {
		t.Fatal("a benchmark store is not a control store")
	}
	f := synthetic(stores["benchfetch/small"].tuple.Pack(), []int64{0})
	r, fx, err := WriteControlFixture(f, store, prefix, 20)
	if err != nil {
		t.Fatal(err)
	}
	if r.KeysWritten != 60 || *fx.Rows != 20 || *fx.Chunks != 2 || *fx.PrefixBytes != len(prefix) ||
		fx.StorePrefixHex != hex.EncodeToString(prefix) || fx.Payload != "control-split" {
		t.Fatalf("receipt %+v fixture %+v", r, fx)
	}
	if got, err := ResolvePrefix(store, fx); err != nil || !bytes.Equal(got, prefix) {
		t.Fatalf("the control fixture receipt must resolve its own prefix: %v", err)
	}
	if _, _, err := WriteControlFixture(f, store, prefix, 20); err == nil {
		t.Fatal("a non-empty control store must be refused")
	}
	doc, err := Probe(f, ProbeInput{Store: store, Prefix: prefix})
	if err != nil || len(doc.Keys) != 60 {
		t.Fatalf("probe of the control store: %v", err)
	}
	key := func(s int64) string {
		return string(pack(prefix, tuple.Tuple{int64(1), int64(ControlMutationID), s}))
	}
	if _, err := MutateControl(f, store, prefix, "delete-chunk"); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.data[key(2)]; ok {
		t.Fatal("chunk 2 must be deleted")
	}
	if _, err := MutateControl(f, store, prefix, "delete-chunk"); err == nil {
		t.Fatal("deleting an absent chunk must be refused")
	}
	for defect, s := range map[string]int64{"stray-suffix": 3, "version-key": -1} {
		if _, err := MutateControl(f, store, prefix, defect); err != nil {
			t.Fatal(err)
		}
		if _, ok := f.data[key(s)]; !ok {
			t.Fatalf("%s must write suffix %d", defect, s)
		}
	}
	if _, err := MutateControl(f, "benchfetch/control-small", stores["benchfetch/control-small"].tuple.Pack(), "stray-suffix"); err == nil {
		t.Fatal("mutations run on the split control store only")
	}
	if _, err := MutateControl(f, store, prefix, "bogus"); err == nil {
		t.Fatal("unknown defect accepted")
	}
	c, err := ClearControl(f, store, prefix)
	if err != nil || !*c.Empty {
		t.Fatalf("clear: %+v %v", c, err)
	}
	end, _ := strinc(prefix)
	for k := range f.data {
		if inRange([]byte(k), prefix, end) {
			t.Fatal("control store not empty")
		}
	}
	if _, err := ClearControl(f, "benchfetch/small", prefix); err == nil {
		t.Fatal("control-clear must refuse a benchmark store")
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
