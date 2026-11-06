//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"encoding/hex"
	"fmt"
	"math"
	"math/big"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type textBoundaryBunch struct {
	token    string
	keys     []tuple.Tuple
	position int
}

var _ = Describe("TEXT native boundary and comparator contrasts", Ordered, ContinueOnFailure, func() {
	ctx := context.Background()
	var env *TenantEnvironment
	var md *recordlayer.RecordMetaData
	var java *JavaInvoker
	BeforeAll(func() {
		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, "tx_boundary_"+uuid.New().String())
		Expect(err).NotTo(HaveOccurred())
		md, err = textQueryMetaData()
		Expect(err).NotTo(HaveOccurred())
		java = NewJavaInvoker()
	})
	AfterAll(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	seed := func(ss subspace.Subspace, bunches []textBoundaryBunch) {
		_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			index := store.IndexSubspace(md.GetIndex("name_text"))
			for _, bunch := range bunches {
				var entries []recordlayer.BunchedEntry[tuple.Tuple, []int]
				for _, key := range bunch.keys {
					entries = append(entries, recordlayer.BunchedEntry[tuple.Tuple, []int]{Key: key, Value: []int{bunch.position}})
				}
				data, err := recordlayer.TextIndexBunchedSerializerInstance().SerializeEntries(entries)
				if err != nil {
					return nil, err
				}
				key := append(tuple.Tuple{bunch.token}, bunch.keys[0]...)
				rtx.Transaction().Set(index.Pack(key), data)
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	}
	runGo := func(ss subspace.Subspace, spec TextQuerySpec) textPage {
		out, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
			if err != nil {
				return nil, err
			}
			scan := recordlayer.NewTextScan(md.GetIndex(spec.Index), nil, goTextComparison(spec), textQuerySuffixRange(spec))
			cursor, err := scan.Scan(store, continuationBytes(spec), recordlayer.NewScanProperties(goExecuteProperties(spec)).WithReverse(spec.Reverse))
			if err != nil {
				return nil, err
			}
			return drainGoPage(ctx, cursor, entryItem)
		})
		Expect(err).NotTo(HaveOccurred(), spec.String())
		return out.(textPage)
	}
	runJava := func(ss subspace.Subspace, spec TextQuerySpec) textPage {
		var out map[string]any
		Expect(java.InvokeAs(ctx, "textQueryScan", map[string]any{"clusterFile": env.ClusterFile, "tenantName": env.TenantName, "subspace": BytesToIntArray(ss.Bytes()), "specJson": spec.String()}, &out)).To(Succeed())
		return javaPage(out)
	}
	// Assert each engine's native page, including its native order, terminal
	// reason and exact same-engine continuation replay. Never sort or dedup.
	assertPages := func(spec TextQuerySpec, want []string, run func(TextQuerySpec) textPage) {
		remaining := append([]string(nil), want...)
		for pages := 0; pages < 30; pages++ {
			page := run(spec)
			Expect(run(spec)).To(Equal(page), "same native page replay")
			n := len(remaining)
			if spec.Limit > 0 {
				n = min(n, spec.Limit)
			}
			var expected []string
			expected = append(expected, remaining[:n]...)
			Expect(page.Items).To(Equal(expected), spec.String())
			remaining = remaining[n:]
			if spec.Limit > 0 && n == spec.Limit {
				Expect(page.Reason).To(Equal("RETURN_LIMIT_REACHED"))
				Expect(page.Continuation).NotTo(BeNil())
				spec.Continuation = page.Continuation
			} else {
				Expect(page.Reason).To(Equal("SOURCE_EXHAUSTED"))
				Expect(page.Continuation).To(BeNil())
				Expect(remaining).To(BeEmpty())
				return
			}
		}
		Fail("native text pages did not exhaust")
	}
	encodedBound := func(value any) *string { return strPtr(hex.EncodeToString(tuple.Tuple{value}.Pack())) }
	item := func(token string, key any, position int64) string {
		return entryItem(&recordlayer.IndexEntry{Key: tuple.Tuple{token, key}, Value: tuple.Tuple{tuple.Tuple{position}}})
	}

	It("pins upper-only, zero and exclusive suffix lows against literal logical and physical oracles", func() {
		for _, tc := range []struct {
			name             string
			groups           [][]any
			low, high        *string
			exclusive        bool
			goKeys, javaKeys []any
		}{
			{"zero", [][]any{{int64(-1), int64(0), int64(1)}, {int64(2), int64(3), int64(4)}, {int64(5)}}, encodedBound(int64(0)), encodedBound(int64(4)), false, []any{int64(0), int64(1), int64(2), int64(3)}, []any{int64(2), int64(3), int64(4)}},
			{"upper only", [][]any{{int64(-1), int64(0), int64(1)}, {int64(2), int64(3), int64(4)}, {int64(5)}}, nil, encodedBound(int64(4)), false, []any{int64(-1), int64(0), int64(1), int64(2), int64(3)}, []any{int64(-1), int64(0), int64(1), int64(2), int64(3), int64(4)}},
			{"exclusive zero", [][]any{{int64(-1), int64(0), int64(1)}, {int64(2), int64(3), int64(4)}, {int64(5)}}, encodedBound(int64(0)), encodedBound(int64(4)), true, []any{int64(1), int64(2), int64(3)}, []any{int64(2), int64(3), int64(4)}},
			{"exclusive string", [][]any{{"a", "m", "n"}, {"o", "p", "q"}, {"z"}}, encodedBound("m"), encodedBound("q"), true, []any{"n", "o", "p"}, []any{"o", "p", "q"}},
		} {
			ss := subspace.Sub("literal_boundary", tc.name)
			var bunches []textBoundaryBunch
			for _, group := range tc.groups {
				var keys []tuple.Tuple
				for _, key := range group {
					keys = append(keys, tuple.Tuple{key})
				}
				bunches = append(bunches, textBoundaryBunch{token: "tok", keys: keys})
			}
			seed(ss, bunches)
			for _, reverse := range []bool{false, true} {
				for _, limit := range []int{0, 1, 2} {
					spec := TextQuerySpec{Index: "name_text", Type: "TEXT_CONTAINS_ALL", Text: strPtr("tok"), SuffixLowTuple: tc.low, SuffixHighTuple: tc.high, SuffixLowExclusive: tc.exclusive, Reverse: reverse, Limit: limit}
					for _, engine := range []struct {
						keys []any
						run  func(subspace.Subspace, TextQuerySpec) textPage
					}{{tc.goKeys, runGo}, {tc.javaKeys, runJava}} {
						var want []string
						for _, key := range engine.keys {
							want = append(want, item("tok", key, 0))
						}
						if reverse {
							for i, j := 0, len(want)-1; i < j; i, j = i+1, j-1 {
								want[i], want[j] = want[j], want[i]
							}
						}
						assertPages(spec, want, func(s TextQuerySpec) textPage { return engine.run(ss, s) })
					}
				}
			}
		}
	})

	It("pins actual Java UUID ALL and ANY scans without normalizing either engine", func() {
		low, middle, high := tuple.UUID{0x10}, tuple.UUID{0x20}, tuple.UUID{0x90}
		for _, typ := range []string{"TEXT_CONTAINS_ALL", "TEXT_CONTAINS_ANY"} {
			ss := subspace.Sub("uuid_comparator", typ)
			bKeys := []tuple.Tuple{{high}}
			if typ == "TEXT_CONTAINS_ANY" {
				bKeys = []tuple.Tuple{{middle}, {high}}
			}
			seed(ss, []textBoundaryBunch{{token: "a", keys: []tuple.Tuple{{low}, {high}}, position: 0}, {token: "b", keys: bKeys, position: 1}})
			goWant := []string{item("a", high, 0)}
			var javaWant []string
			if typ == "TEXT_CONTAINS_ANY" {
				goWant = []string{item("a", low, 0), item("b", middle, 1), item("a", high, 0)}
				javaWant = []string{item("a", low, 0), item("a", high, 0), item("b", middle, 1), item("b", high, 1)}
			}
			for _, limit := range []int{0, 1, 2} {
				spec := TextQuerySpec{Index: "name_text", Type: typ, Text: strPtr("a b"), Limit: limit}
				assertPages(spec, goWant, func(s TextQuerySpec) textPage { return runGo(ss, s) })
				assertPages(spec, javaWant, func(s TextQuerySpec) textPage { return runJava(ss, s) })
			}
		}
	})

	It("retains raw Java comparator results for typed suffix contrasts", func() {
		for _, tc := range []struct {
			shape          string
			left, right    any
			javaComparison int
			javaError      string
			goOrder        []string
		}{
			{"uuid", tuple.UUID{0x10}, tuple.UUID{0x90}, 1, "", []string{"left", "right"}},
			{"string", "\U00010000", "\uE000", -2048, "", []string{"right", "left"}},
			{"float_nan", math.Float32frombits(0x7fc00001), math.Float32frombits(0x7fc00002), 0, "", []string{"left", "right"}},
			{"double_nan", math.Float64frombits(0x7ff8000000000001), math.Float64frombits(0x7ff8000000000002), 0, "", []string{"left", "right"}},
			{"nested_nan", tuple.Tuple{math.Float64frombits(0x7ff8000000000001)}, tuple.Tuple{math.Float64frombits(0x7ff8000000000002)}, 0, "", []string{"left", "right"}},
			{"mixed_integers", int64(1), new(big.Int).Lsh(big.NewInt(1), 80), 0, "java.lang.ClassCastException", []string{"left", "right"}},
		} {
			var raw map[string]any
			Expect(java.InvokeAs(ctx, "textMergeComparatorContrast", map[string]any{"shape": tc.shape}, &raw)).To(Succeed())
			Expect(raw["left"]).To(Equal(hex.EncodeToString(tuple.Tuple{tc.left}.Pack())))
			Expect(raw["right"]).To(Equal(hex.EncodeToString(tuple.Tuple{tc.right}.Pack())))
			if tc.javaError != "" {
				Expect(raw["error"]).To(Equal(tc.javaError))
				Expect(raw).NotTo(HaveKey("comparison"))
			} else {
				Expect(raw["comparison"]).To(Equal(float64(tc.javaComparison)))
				Expect(raw).NotTo(HaveKey("error"))
			}
			key := func(label string) (tuple.Tuple, error) {
				if label == "left" {
					return tuple.Tuple{tc.left}, nil
				}
				return tuple.Tuple{tc.right}, nil
			}
			children := []recordlayer.CursorFunction[string]{func(cont []byte) recordlayer.RecordCursor[string] {
				return recordlayer.FromListWithContinuation([]string{"left"}, cont)
			}, func(cont []byte) recordlayer.RecordCursor[string] {
				return recordlayer.FromListWithContinuation([]string{"right"}, cont)
			}}
			cursor, err := recordlayer.Union(key, false, children, nil)
			Expect(err).NotTo(HaveOccurred())
			page, err := drainGoPage(ctx, cursor, func(label string) string { return label })
			Expect(err).NotTo(HaveOccurred(), fmt.Sprint(tc.shape))
			Expect(page).To(Equal(textPage{Items: tc.goOrder, Reason: "SOURCE_EXHAUSTED"}))
		}
	})
})
