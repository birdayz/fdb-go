package recordlayer

import (
	"context"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Random puts and removes against a model map: every operation returns the
// model's old value, and after every transaction each map scans to the model
// in key order, passes VerifyIntegrity, and answers Get and ContainsKey.
var _ = Describe("BunchedMap model", func() {
	ctx := context.Background()

	for _, bunchSize := range []int{3, textIndexBunchSize} {
		It(fmt.Sprintf("matches a model map with bunch size %d", bunchSize), func() {
			root := specSubspace()
			rng := rand.New(rand.NewPCG(uint64(bunchSize), 11))
			m := NewBunchedMap(bunchSize)
			mapSubspaces := []subspace.Subspace{root.Sub("a"), root.Sub("b"), root.Sub("c")}
			type modelEntry struct {
				key   tuple.Tuple
				value []int
			}
			model := make([]map[string]modelEntry, len(mapSubspaces))
			for i := range model {
				model[i] = map[string]modelEntry{}
			}
			randomKey := func() tuple.Tuple {
				seq := int64(rng.IntN(120))
				switch rng.IntN(4) {
				case 0:
					return tuple.Tuple{"w", seq, int64(rng.IntN(2))}
				case 1:
					return tuple.Tuple{"w", seq}
				case 2:
					return tuple.Tuple{"v", seq, nil}
				default:
					return tuple.Tuple{"w", -seq, "x"}
				}
			}
			randomValue := func() []int {
				n := rng.IntN(5)
				if rng.IntN(15) == 0 {
					n = 1500 + rng.IntN(1500) // large lists cross the 10 KB bunch limit
				}
				out := make([]int, n)
				p := 0
				for i := range out {
					p += rng.IntN(3)
					out[i] = p
				}
				return out
			}
			type op struct {
				mi     int
				key    tuple.Tuple
				value  []int
				remove bool
			}
			for round := range 40 {
				ops := make([]op, 1+rng.IntN(12))
				for i := range ops {
					ops[i] = op{mi: rng.IntN(len(mapSubspaces)), key: randomKey(), remove: rng.IntN(3) == 0}
					if !ops[i].remove {
						ops[i].value = randomValue()
					}
				}
				// A retried transaction starts again from the committed model.
				var next []map[string]modelEntry
				_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
					tx := rc.Transaction()
					next = make([]map[string]modelEntry, len(model))
					for i := range model {
						next[i] = maps.Clone(model[i])
					}
					for _, o := range ops {
						packed := string(o.key.Pack())
						old, had := next[o.mi][packed]
						var got []int
						var found bool
						var err error
						if o.remove {
							got, found, err = m.Remove(tx, mapSubspaces[o.mi], o.key)
							delete(next[o.mi], packed)
						} else {
							got, found, err = m.Put(tx, mapSubspaces[o.mi], o.key, o.value)
							next[o.mi][packed] = modelEntry{key: o.key, value: o.value}
						}
						if err != nil {
							return nil, err
						}
						Expect(found).To(Equal(had), "%+v", o)
						if had {
							Expect(got).To(Equal(old.value), "%+v", o)
						}
					}
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
				model = next

				_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
					tx := rc.Transaction()
					for mi, ss := range mapSubspaces {
						Expect(m.VerifyIntegrity(tx, ss)).To(Succeed(), "round %d map %d", round, mi)
						want := slices.Collect(maps.Values(model[mi]))
						slices.SortFunc(want, func(a, b modelEntry) int { return compareTuples(a.key, b.key) })
						var got []modelEntry
						it := m.Scan(tx, ss, nil, 0, false)
						for it.HasNext() {
							e := it.Next()
							got = append(got, modelEntry{key: e.Key, value: e.Value})
						}
						Expect(it.Err()).NotTo(HaveOccurred())
						Expect(got).To(HaveLen(len(want)), "round %d map %d", round, mi)
						for i := range want {
							Expect(compareTuples(got[i].key, want[i].key)).To(BeZero(), "round %d map %d entry %d", round, mi, i)
							Expect(got[i].value).To(Equal(want[i].value), "round %d map %d entry %d", round, mi, i)
						}
						for _, e := range want[:min(len(want), 5)] {
							v, found, err := m.Get(tx, ss, e.key)
							Expect(err).NotTo(HaveOccurred())
							Expect(found).To(BeTrue())
							Expect(v).To(Equal(e.value))
							ok, err := m.ContainsKey(tx, ss, e.key)
							Expect(err).NotTo(HaveOccurred())
							Expect(ok).To(BeTrue())
						}
					}
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			}
		})
	}
})
