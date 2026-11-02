//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

// bunchedMapRecorder records a BunchedMap operation's conflict ranges, writes
// and snapshot range reads in the format of BunchedMapSteps.recording
// (bunched_map_conformance.java).
type bunchedMapRecorder struct {
	fdb.WritableTransaction
	mu    sync.Mutex
	trace []string
}

func (r *bunchedMapRecorder) add(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.trace = append(r.trace, fmt.Sprintf(format, args...))
}

// raw is a key's bytes; fdb.Key formats as printable text.
func raw(k fdb.KeyConvertible) []byte { return []byte(k.FDBKey()) }

type bunchedMapRecorderSnapshot struct {
	fdb.ReadTransaction
	r *bunchedMapRecorder
}

func (r *bunchedMapRecorder) Snapshot() fdb.ReadTransaction {
	return bunchedMapRecorderSnapshot{ReadTransaction: r.WritableTransaction.Snapshot(), r: r}
}

func (s bunchedMapRecorderSnapshot) GetRange(rg fdb.Range, options fdb.RangeOptions) fdb.RangeResult {
	b, e := rg.FDBRangeKeySelectors()
	bs, es := b.FDBKeySelector(), e.FDBKeySelector()
	s.r.add("snapshot_range %x %t %d %x %t %d", raw(bs.Key), bs.OrEqual, bs.Offset, raw(es.Key), es.OrEqual, es.Offset)
	return s.ReadTransaction.GetRange(rg, options)
}

func (r *bunchedMapRecorder) AddReadConflictKey(key fdb.KeyConvertible) error {
	r.add("read_conflict_key %x", raw(key))
	return r.WritableTransaction.AddReadConflictKey(key)
}

func (r *bunchedMapRecorder) AddReadConflictRange(er fdb.ExactRange) error {
	b, e := er.FDBRangeKeys()
	r.add("read_conflict_range %x %x", raw(b), raw(e))
	return r.WritableTransaction.AddReadConflictRange(er)
}

func (r *bunchedMapRecorder) AddWriteConflictKey(key fdb.KeyConvertible) error {
	r.add("write_conflict_key %x", raw(key))
	return r.WritableTransaction.AddWriteConflictKey(key)
}

func (r *bunchedMapRecorder) AddWriteConflictRange(er fdb.ExactRange) error {
	b, e := er.FDBRangeKeys()
	r.add("write_conflict_range %x %x", raw(b), raw(e))
	return r.WritableTransaction.AddWriteConflictRange(er)
}

func (r *bunchedMapRecorder) Set(key fdb.KeyConvertible, value []byte) {
	r.add("set %x %x", raw(key), value)
	r.WritableTransaction.Set(key, value)
}

func (r *bunchedMapRecorder) Clear(key fdb.KeyConvertible) {
	r.add("clear %x", raw(key))
	r.WritableTransaction.Clear(key)
}

func (r *bunchedMapRecorder) AppendIfFits(key fdb.KeyConvertible, param []byte) {
	r.add("mutate APPEND_IF_FITS %x %x", raw(key), param)
	r.WritableTransaction.AppendIfFits(key, param)
}

type bunchedMapOp struct {
	Op    string `json:"op"`
	Map   string `json:"map"`
	Key   []any  `json:"key"`
	Value []int  `json:"value"`
}

type bunchedMapOpResult struct {
	Trace  []string         `json:"trace"`
	Counts map[string]int64 `json:"counts"`
	Old    []int            `json:"old"`
}

// A put of TEXT index maintenance issues exactly Java's conflict ranges: the
// signpost read (snapshot), the read conflict range over the entries a bunch
// rewrite depends on, write conflict keys and gap ranges, and nothing else.
var _ = Describe("BunchedMap operation conformance", func() {
	ctx := context.Background()

	It("issues Java's reads, conflict ranges, writes and counts for every put and remove", func() {
		goEnv, err := SetupTenantEnvironment(ctx, sharedContainer, fmt.Sprintf("bmgo_%s", uuid.New().String()))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = goEnv.Cleanup(ctx) })
		javaEnv, err := SetupTenantEnvironment(ctx, sharedContainer, fmt.Sprintf("bmjava_%s", uuid.New().String()))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = javaEnv.Cleanup(ctx) })

		const bunchSize = 4
		root := subspace.Sub("map")
		mapSubspace := func(name string) subspace.Subspace { return root.Sub(name) }

		// Map d starts as a bunch written by neither engine: entry 1's list
		// size and entry 2's delta are varints with a leading zero group,
		// entry 3's key packs the int 7 with a leading zero byte (so it sorts
		// last by its bytes). Java keeps a key's packed bytes
		// (Tuple.fromBytes) and decodes and re-encodes every list.
		rawD := []byte{0x20, 0x01, 0x01}
		rawD = append(rawD, 0x05, 0x02, 'w', 0x00, 0x15, 0x09, 0x80, 0x01, 0x03)
		rawD = append(rawD, 0x05, 0x02, 'w', 0x00, 0x15, 0x0b, 0x02, 0x80, 0x05)
		rawD = append(rawD, 0x06, 0x02, 'w', 0x00, 0x16, 0x00, 0x07, 0x02, 0x02, 0x02)
		for _, env := range []*TenantEnvironment{goEnv, javaEnv} {
			_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				rtx.Transaction().Set(mapSubspace("d").Pack(tuple.Tuple{"w", int64(1)}), rawD)
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		}

		wide := func(n int) []int {
			out := make([]int, n)
			for i := range out {
				out[i] = i
			}
			return out
		}
		put := func(m string, key []any, value []int) bunchedMapOp {
			return bunchedMapOp{Op: "put", Map: m, Key: key, Value: value}
		}
		remove := func(m string, key []any) bunchedMapOp {
			return bunchedMapOp{Op: "remove", Map: m, Key: key, Value: []int{}}
		}
		k := func(n int64, rest ...any) []any { return append([]any{"w", n}, rest...) }
		ops := []bunchedMapOp{
			put("b", k(1), []int{0}),
			put("a", k(10), []int{1, 2}),        // alone: no signpost
			put("a", k(20), []int{3}),           // APPEND_IF_FITS to the last bunch
			put("a", k(30), []int{}),            // an empty list appended
			put("b", k(2), []int{4}),            // the next map's keys bound the read
			put("a", k(15), []int{5, 9}),        // inside a bunch: rewritten
			put("a", k(40), []int{1}),           // full bunch, nothing after: alone
			put("a", k(35), []int{2}),           // full bunch: prepended to the next, gap range
			put("a", k(12), []int{7}),           // inside a full bunch: split
			put("a", k(20), []int{3}),           // same value: a read conflict key only
			put("a", k(20), []int{3, 4}),        // new value: rewritten
			put("a", k(5), []int{1}),            // before every key: prepended
			put("a", k(1), []int{}),             // prepended again: the bunch fills
			remove("a", k(99)),                  // absent
			remove("a", k(1)),                   // a first entry: a new signpost
			remove("a", k(5)),                   // again
			remove("a", k(15)),                  // the first entry of the bunch a split made
			put("c", k(1), wide(3000)),          // large lists: alone
			put("c", k(2), wide(3000)),          // appended
			put("c", k(3), wide(3000)),          // appended
			put("c", k(4), wide(3000)),          // too large to append: inserted after
			put("c", k(0), wide(3000)),          // too large to prepend: alone
			put("c", k(2, "x"), wide(3000)),     // too large in the middle: split
			put("c", k(2, "x"), wide(10)),       // shrunk in place
			put("d", k(10), []int{1}),           // rewrites the bunch written by neither
			put("d", k(7), []int{6}),            // canonical (w, 7) is not the stored key
			remove("d", k(11)),                  // the stored non-canonical delta: the signpost stays
			remove("c", k(0)),                   // the only entry of its bunch
			put("a", k(36, nil), nil),           // omitted position list
			put("a", k(36, int64(-1)), wide(3)), // a negative integer key element
		}

		opsJSON, err := json.Marshal(ops)
		Expect(err).NotTo(HaveOccurred())
		var javaResults []bunchedMapOpResult
		Expect(NewJavaInvoker().InvokeAs(ctx, "bunchedMapTrace", map[string]any{
			"clusterFile": javaEnv.ClusterFile,
			"subspace":    BytesToIntArray(root.Bytes()),
			"tenantName":  javaEnv.TenantName,
			"bunchSize":   bunchSize,
			"opsJson":     string(opsJSON),
		}, &javaResults)).To(Succeed())
		Expect(javaResults).To(HaveLen(len(ops)))
		// The operations reach every write path of put and remove: an
		// append, a rewrite, a gap claimed before and after a bunch, a split
		// (two bunches written by one put), a moved signpost, and a put that
		// only reads.
		kinds := map[string]int{}
		for i, r := range javaResults {
			GinkgoWriter.Printf("op %d %+v:\n", i, ops[i])
			sets := 0
			for _, line := range r.Trace {
				GinkgoWriter.Printf("  %s\n", line)
				kind := strings.Fields(line)[0]
				if kind == "mutate" {
					kind = strings.Join(strings.Fields(line)[:2], " ")
				}
				kinds[kind]++
				if kind == "set" {
					sets++
				}
			}
			if sets >= 2 {
				kinds["split"]++
			}
			if ops[i].Op == "remove" {
				clears := 0
				for _, line := range r.Trace {
					if strings.HasPrefix(line, "clear ") {
						clears++
					}
				}
				switch {
				case clears == 1 && sets == 0:
					kinds["remove alone"]++
				case clears == 1 && sets == 1:
					kinds["remove first"]++
				case clears == 0 && sets == 1:
					kinds["remove inner"]++
				}
			}
			if len(r.Trace) == 2 && strings.HasPrefix(r.Trace[1], "read_conflict_key") && ops[i].Op == "put" {
				kinds["unchanged"]++
			}
		}
		for _, kind := range []string{
			"snapshot_range", "read_conflict_key", "read_conflict_range", "write_conflict_key",
			"write_conflict_range", "set", "clear", "mutate APPEND_IF_FITS", "split", "unchanged",
			"remove alone", "remove first", "remove inner",
		} {
			Expect(kinds[kind]).To(BeNumerically(">", 0), "no operation issued %s: %v", kind, kinds)
		}

		for i, op := range ops {
			key := make(tuple.Tuple, len(op.Key))
			for j, e := range op.Key {
				if n, ok := e.(int); ok {
					e = int64(n)
				}
				key[j] = e
			}
			var got bunchedMapOpResult
			_, err := goEnv.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				timer := recordlayer.NewStoreTimer()
				m := recordlayer.NewInstrumentedBunchedMap(bunchSize, timer)
				rec := &bunchedMapRecorder{WritableTransaction: rtx.Transaction()}
				var old []int
				var found bool
				var err error
				if op.Op == "put" {
					old, found, err = m.Put(rec, mapSubspace(op.Map), key, op.Value)
				} else {
					old, found, err = m.Remove(rec, mapSubspace(op.Map), key)
				}
				if err != nil {
					return nil, err
				}
				got = bunchedMapOpResult{Trace: rec.trace, Counts: map[string]int64{}}
				if found {
					got.Old = old
					if got.Old == nil {
						got.Old = []int{}
					}
				}
				for _, ev := range []recordlayer.Event{
					recordlayer.CountSaveIndexKey, recordlayer.CountSaveIndexKeyBytes, recordlayer.CountSaveIndexValueBytes,
					recordlayer.CountLoadIndexKey, recordlayer.CountLoadIndexKeyBytes, recordlayer.CountLoadIndexValueBytes,
					recordlayer.CountDeleteIndexKey, recordlayer.CountDeleteIndexKeyBytes, recordlayer.CountDeleteIndexValueBytes,
				} {
					if n := timer.GetCount(ev); n != 0 {
						got.Counts[ev.Name] = n
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred(), "op %d %+v", i, op)
			want := javaResults[i]
			for name, n := range want.Counts {
				if n == 0 {
					delete(want.Counts, name)
				}
			}
			Expect(got.Trace).To(Equal(want.Trace), "op %d %+v", i, op)
			Expect(got.Counts).To(Equal(want.Counts), "op %d %+v", i, op)
			Expect(got.Old).To(Equal(want.Old), "op %d %+v", i, op)
		}

		// Both tenants hold the same map, byte for byte.
		dump := func(env *TenantEnvironment) []string {
			var out []string
			_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				kvs, err := rtx.Transaction().GetRange(root, fdb.RangeOptions{}).GetSliceWithError()
				for _, kv := range kvs {
					out = append(out, hex.EncodeToString(kv.Key)+"="+hex.EncodeToString(kv.Value))
				}
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
			return out
		}
		Expect(dump(goEnv)).To(Equal(dump(javaEnv)))
	})
})

// tupleCompareElement encodes a tuple element for TupleCompareSteps
// (tuple_compare_steps.java).
func tupleCompareElement(e any) map[string]any {
	switch v := e.(type) {
	case nil:
		return map[string]any{"t": "null"}
	case int64:
		return map[string]any{"t": "long", "v": strconv.FormatInt(v, 10)}
	case uint64:
		return map[string]any{"t": "bigint", "v": strconv.FormatUint(v, 10)}
	case *big.Int:
		return map[string]any{"t": "bigint", "v": v.String()}
	case string:
		return map[string]any{"t": "string", "v": v}
	case []byte:
		ints := make([]int, len(v))
		for i, b := range v {
			ints[i] = int(b)
		}
		return map[string]any{"t": "bytes", "v": ints}
	case float32:
		return map[string]any{"t": "float", "v": strconv.FormatInt(int64(int32(math.Float32bits(v))), 10)}
	case float64:
		return map[string]any{"t": "double", "v": strconv.FormatUint(math.Float64bits(v), 10)}
	case bool:
		return map[string]any{"t": "bool", "v": v}
	case tuple.UUID:
		return map[string]any{"t": "uuid", "v": uuid.UUID(v).String()}
	case tuple.Versionstamp:
		b := v.Bytes()
		ints := make([]int, len(b))
		for i, x := range b {
			ints[i] = int(x)
		}
		return map[string]any{"t": "versionstamp", "v": ints}
	case tuple.Tuple:
		return map[string]any{"t": "tuple", "v": tupleCompareElements(v)}
	}
	panic(fmt.Sprintf("unsupported element %T", e))
}

func tupleCompareElements(t tuple.Tuple) []map[string]any {
	out := make([]map[string]any, len(t))
	for i, e := range t {
		out[i] = tupleCompareElement(e)
	}
	return out
}

var _ = Describe("Tuple natural order conformance", func() {
	It("orders tuples of every element type as Java's Tuple.compareTo", func() {
		vs := func(tv byte, user uint16) tuple.Versionstamp {
			var t [10]byte
			for i := range t {
				t[i] = tv
			}
			return tuple.Versionstamp{TransactionVersion: t, UserVersion: user}
		}
		huge, _ := new(big.Int).SetString("100000000000000000000000000000", 10)
		elements := []any{
			nil,
			int64(math.MinInt64), int64(-256), int64(-1), int64(0), int64(1), int64(255), int64(256), int64(math.MaxInt64),
			uint64(math.MaxUint64), huge, new(big.Int).Neg(huge), big.NewInt(1), big.NewInt(math.MinInt64),
			"", "a", "a\x00", "a\x00b", "ab", "b", "é", "\U0001F600", "\uFFFF",
			[]byte{},
			[]byte{0},
			[]byte{0, 0xff},
			[]byte{1},
			[]byte{0xff},
			float32(math.Inf(-1)), float32(-1), float32(math.Copysign(0, -1)), float32(0), float32(1.5), float32(math.Inf(1)), float32(math.NaN()),
			math.Inf(-1), -2.5, math.Copysign(0, -1), 0.0, 1e-300, 2.5, math.Inf(1), math.NaN(),
			false, true,
			tuple.UUID{},
			tuple.UUID{0x80},
			tuple.UUID{0, 0, 0, 0, 0, 0, 0, 0, 0xff},
			vs(0, 0), vs(0, 1), vs(1, 0), vs(0xfe, 0xffff), vs(0xff, 0), vs(0xff, 5),
			tuple.Tuple{},
			tuple.Tuple{nil},
			tuple.Tuple{nil, nil},
			tuple.Tuple{int64(1)},
			tuple.Tuple{int64(1), nil},
			tuple.Tuple{int64(1), int64(2)},
			tuple.Tuple{"a"},
			tuple.Tuple{tuple.Tuple{nil}},
			tuple.Tuple{tuple.Tuple{}, int64(1)},
		}
		var tuples []tuple.Tuple
		for _, e := range elements {
			tuples = append(tuples, tuple.Tuple{e})
		}
		tuples = append(
			tuples,
			tuple.Tuple{}, tuple.Tuple{nil, int64(1)}, tuple.Tuple{int64(1), "a"}, tuple.Tuple{int64(1), "a", nil},
			tuple.Tuple{"writer-0042", int64(1004), int64(0)}, tuple.Tuple{"writer-0042", int64(1004), int64(1)},
			tuple.Tuple{"writer-0042", int64(1005)}, tuple.Tuple{"writer-0042", int64(1004), int64(0), vs(0xff, 1)},
		)
		encoded := make([][]map[string]any, len(tuples))
		for i, t := range tuples {
			encoded[i] = tupleCompareElements(t)
		}
		tuplesJSON, err := json.Marshal(encoded)
		Expect(err).NotTo(HaveOccurred())
		var java []int
		Expect(NewJavaInvoker().InvokeAs(context.Background(), "compareTuplesNatural", map[string]any{
			"tuplesJson": string(tuplesJSON),
		}, &java)).To(Succeed())
		Expect(java).To(HaveLen(len(tuples) * len(tuples)))
		for i, a := range tuples {
			for j, b := range tuples {
				Expect(recordlayer.CompareTuples(a, b)).To(Equal(java[i*len(tuples)+j]), "%v vs %v", a, b)
			}
		}
	})
})
