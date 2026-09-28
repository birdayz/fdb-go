package recordlayer

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// rtreeLayoutCheck walks a stored R-tree from its root and checks what Java's
// adapters would have written for it: every node in the configured layout
// only, leaf Hilbert values stored or not as configured, and the node slot
// index holding exactly one entry per child slot of every intermediate node
// (level = the child's, a leaf being 0) or nothing when the option is off.
// It returns the item count.
func rtreeLayoutCheck(tx fdb.ReadTransaction, storage *rtreeStorage) int {
	want := map[string]bool{}
	items := 0
	var walk func(id []byte) int
	walk = func(id []byte) int {
		leaf, inter, err := storage.fetchNode(tx, id)
		Expect(err).NotTo(HaveOccurred())
		Expect(leaf != nil || inter != nil).To(BeTrue(), "node %x missing", id)
		prefix := storage.subspace.Pack(tuple.Tuple{id})
		r, err := fdb.PrefixRange(prefix)
		Expect(err).NotTo(HaveOccurred())
		kvs, err := tx.GetRange(r, fdb.RangeOptions{}).GetSliceWithError()
		Expect(err).NotTo(HaveOccurred())
		if storage.config.Storage == RTreeStorageBySlot {
			for _, kv := range kvs {
				Expect(len(kv.Key)).To(BeNumerically(">", len(prefix)), "a BY_SLOT tree holds a pair at the node key itself")
			}
		} else {
			Expect(kvs).To(HaveLen(1), "a BY_NODE node is one pair")
			Expect(kvs[0].Key).To(Equal(fdb.Key(prefix)))
		}
		if leaf != nil {
			items += len(leaf.slots)
			Expect(len(leaf.slots)).To(BeNumerically(">", 0))
			var hvs []any
			for _, kv := range kvs {
				if storage.config.Storage == RTreeStorageBySlot {
					t, err := tuple.Unpack(kv.Key[len(prefix):])
					Expect(err).NotTo(HaveOccurred())
					hvs = append(hvs, t[1])
					continue
				}
				t, err := tuple.Unpack(kv.Value)
				Expect(err).NotTo(HaveOccurred())
				for _, slot := range t[1].(tuple.Tuple) {
					hvs = append(hvs, slot.(tuple.Tuple)[0])
				}
			}
			Expect(hvs).To(HaveLen(len(leaf.slots)))
			for _, hv := range hvs {
				Expect(hv == nil).To(Equal(!storage.config.StoreHilbertValues), "leaf slot Hilbert value %v with StoreHilbertValues %t", hv, storage.config.StoreHilbertValues)
			}
			for i := 1; i < len(leaf.slots); i++ {
				Expect(compareHilbertValueAndKey(leaf.slots[i-1].HilbertValue, leaf.slots[i-1].ItemKey(), leaf.slots[i].HilbertValue, leaf.slots[i].ItemKey())).To(Equal(-1), "leaf slots out of order")
			}
			return 0
		}
		height := -1
		for _, slot := range inter.slots {
			h := walk(slot.ChildID) + 1
			if height == -1 {
				height = h
			}
			Expect(h).To(Equal(height), "unbalanced tree")
			// Built here, not by nodeSlotIndexEntries: NodeSlotIndexAdapter.
			// createIndexKeyTuple's (child level, largest Hilbert value, the
			// largest key's items, child id), under the secondary subspace's
			// indicator 0.
			k := append(tuple.Tuple{int64(h - 1), slot.LargestHV}, slot.LargestKey...)
			want[string(storage.nodeSlotIndex.Pack(append(k, slot.ChildID)))] = true
		}
		return height
	}
	if leaf, inter, err := storage.fetchNode(tx, rootNodeID); err == nil && (leaf != nil || inter != nil) {
		walk(rootNodeID)
	}
	r, err := fdb.PrefixRange(storage.nodeSlotIndex.Bytes())
	Expect(err).NotTo(HaveOccurred())
	kvs, err := tx.GetRange(r, fdb.RangeOptions{}).GetSliceWithError()
	Expect(err).NotTo(HaveOccurred())
	got := map[string]bool{}
	for _, kv := range kvs {
		Expect(kv.Value).To(BeEmpty())
		got[string(kv.Key)] = true
	}
	if !storage.config.UseNodeSlotIndex {
		Expect(got).To(BeEmpty(), "the node slot index is off but has entries")
	} else {
		Expect(got).To(Equal(want), "the node slot index is not the tree's child slots")
	}
	return items
}

var _ = Describe("RTree storage layouts and the node slot index", func() {
	ctx := context.Background()
	for _, storageKind := range []RTreeStorage{RTreeStorageByNode, RTreeStorageBySlot} {
		for _, slotIndex := range []bool{false, true} {
			for _, storeHV := range []bool{true, false} {
				name := fmt.Sprintf("%s, node slot index %t, Hilbert values stored %t", storageKind, slotIndex, storeHV)
				It("keeps Java's layout through splits, fuses and root changes: "+name, func() {
					ks := specSubspace()
					config := DefaultRTreeConfig(2)
					// Small nodes, so a few hundred points build a tree several
					// levels deep and deletes fuse and promote.
					config.MinM, config.MaxM, config.SplitS = 2, 4, 2
					config.Storage, config.UseNodeSlotIndex, config.StoreHilbertValues = storageKind, slotIndex, storeHV
					newStorage := func() *rtreeStorage {
						return newRTreeStorage(ks.Sub("tree"), config).withNodeSlotIndex(ks.Sub("nsi"))
					}
					rng := rand.New(rand.NewPCG(7, uint64(len(name))))
					type item struct{ x, y, pk int64 }
					live := map[int64]item{}
					var next int64
					step := func(inserts, deletes int) {
						_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
							storage := newStorage()
							rt, err := NewRTree(storage, config)
							Expect(err).NotTo(HaveOccurred())
							for range inserts {
								next++
								it := item{rng.Int64N(1000), rng.Int64N(1000), next}
								live[it.pk] = it
								Expect(rt.InsertOrUpdate(rtx.Transaction(), Point{Coordinates: tuple.Tuple{it.x, it.y}}, tuple.Tuple{it.pk}, tuple.Tuple{it.pk * 10})).To(Succeed())
							}
							keys := slices.Sorted(func(yield func(int64) bool) {
								for k := range live {
									if !yield(k) {
										return
									}
								}
							})
							rng.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
							for _, k := range keys[:min(deletes, len(keys))] {
								it := live[k]
								delete(live, k)
								Expect(rt.Delete(rtx.Transaction(), Point{Coordinates: tuple.Tuple{it.x, it.y}}, tuple.Tuple{it.pk})).To(Succeed())
							}
							return nil, nil
						})
						Expect(err).NotTo(HaveOccurred())
						_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
							storage := newStorage()
							Expect(rtreeLayoutCheck(rtx.Transaction(), storage)).To(Equal(len(live)))
							rt, err := NewRTree(storage, config)
							Expect(err).NotTo(HaveOccurred())
							scanned, err := rt.Scan(rtx.Transaction(), nil, nil, nil)
							Expect(err).NotTo(HaveOccurred())
							Expect(scanned).To(HaveLen(len(live)))
							for _, s := range scanned {
								it, ok := live[s.KeySuffix[0].(int64)]
								Expect(ok).To(BeTrue())
								Expect(s.Point.Coordinates).To(Equal(tuple.Tuple{it.x, it.y}))
								Expect(s.Value).To(Equal(tuple.Tuple{it.pk * 10}))
							}
							return nil, nil
						})
						Expect(err).NotTo(HaveOccurred())
					}
					step(300, 0)
					step(50, 200)
					step(0, 140)
					step(40, 0)
					step(0, 1000)
					_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
						for _, sub := range []subspace.Subspace{ks.Sub("tree"), ks.Sub("nsi")} {
							r, err := fdb.PrefixRange(sub.Bytes())
							Expect(err).NotTo(HaveOccurred())
							kvs, err := rtx.Transaction().GetRange(r, fdb.RangeOptions{}).GetSliceWithError()
							Expect(err).NotTo(HaveOccurred())
							Expect(kvs).To(BeEmpty(), "an emptied tree left pairs behind")
						}
						return nil, nil
					})
					Expect(err).NotTo(HaveOccurred())
				})
			}
		}
	}
})
