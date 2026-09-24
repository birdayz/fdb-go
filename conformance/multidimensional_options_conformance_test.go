//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

// A MULTIDIMENSIONAL index's R-tree options decide its bytes
// (MultiDimensionalIndexHelper.getConfig): rtreeStorage lays a node out as one
// pair (BY_NODE) or one pair per slot (BY_SLOT), rtreeStoreHilbertValues
// stores each leaf slot's Hilbert value or null (read only when rtreeStorage
// is set, and then absent means false), and rtreeUseNodeSlotIndex keeps the
// node slot index in the secondary subspace. For each combination, Java and
// Go write, delete and scan the same records in turn, and after every step
// the stored tree is checked against the layout the options name, read from
// the raw pairs: the kind of pair per node, null or stored Hilbert values in
// leaf slots, and one node slot index entry per non-root node. Java's deletes
// with the node slot index find their leaf through that index, so they also
// check the entries Go wrote.
var _ = Describe("MULTIDIMENSIONAL index R-tree options", func() {
	const indexName = "order_coord_md"
	for _, c := range []struct {
		name      string
		options   map[string]string
		bySlot    bool
		storeHV   bool
		slotIndex bool
	}{
		{"BY_SLOT", map[string]string{recordlayer.IndexOptionRTreeStorage: "BY_SLOT", recordlayer.IndexOptionRTreeStoreHilbertValues: "true"}, true, true, false},
		{"BY_SLOT without Hilbert values", map[string]string{recordlayer.IndexOptionRTreeStorage: "BY_SLOT"}, true, false, false},
		{"BY_SLOT with the node slot index", map[string]string{recordlayer.IndexOptionRTreeStorage: "BY_SLOT", recordlayer.IndexOptionRTreeStoreHilbertValues: "TRUE", recordlayer.IndexOptionRTreeUseNodeSlotIndex: "true"}, true, true, true},
		{"BY_NODE storage alone stores no Hilbert values", map[string]string{recordlayer.IndexOptionRTreeStorage: "BY_NODE"}, false, false, false},
		{"Hilbert false without storage keeps them", map[string]string{recordlayer.IndexOptionRTreeStoreHilbertValues: "false"}, false, true, false},
		{"BY_NODE with the node slot index", map[string]string{recordlayer.IndexOptionRTreeUseNodeSlotIndex: "True"}, false, true, true},
	} {
		It("Java and Go share the tree: "+c.name, func() {
			ctx := context.Background()
			env, err := SetupTenantEnvironment(ctx, sharedContainer, fmt.Sprintf("mdopt_%s", uuid.New().String()))
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = env.Cleanup(ctx) }()
			keyspace := subspace.Sub(tuple.Tuple{})
			java := NewJavaInvoker()

			// Small nodes, so 120 points make a tree several levels deep and
			// the deletes fuse nodes and lower the root.
			options := map[string]string{recordlayer.IndexOptionRTreeMinM: "2", recordlayer.IndexOptionRTreeMaxM: "4"}
			for k, v := range c.options {
				options[k] = v
			}
			index := recordlayer.NewMultidimensionalIndex(indexName, recordlayer.Dimensions(
				recordlayer.Concat(recordlayer.Field("coord_x"), recordlayer.Field("coord_y")), 0, 2))
			for k, v := range options {
				index.Options[k] = v
			}
			builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
			builder.AddIndex("Order", index)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			mdProto, err := md.ToProto()
			Expect(err).NotTo(HaveOccurred())
			mdBytes, err := proto.Marshal(mdProto)
			Expect(err).NotTo(HaveOccurred())

			coords := func(id int64) (int64, int64) { return (id * 7919) % 1000, (id * 104729) % 1000 }
			live := map[int64]bool{}
			javaParams := func() map[string]any {
				return map[string]any{
					"clusterFile": env.ClusterFile, "subspace": BytesToIntArray(keyspace.Bytes()),
					"tenantName": env.TenantName, "protoBytes": BytesToIntArray(mdBytes),
				}
			}
			javaSave := func(ids []int64) {
				var orders []map[string]int64
				for _, id := range ids {
					x, y := coords(id)
					orders = append(orders, map[string]int64{"orderId": id, "coordX": x, "coordY": y})
					live[id] = true
				}
				b, err := json.Marshal(orders)
				Expect(err).NotTo(HaveOccurred())
				params := javaParams()
				params["ordersJson"] = string(b)
				Expect(java.InvokeAs(ctx, "mdOptionsSaveOrders", params, nil)).To(Succeed())
			}
			javaDelete := func(ids []int64) {
				b, err := json.Marshal(ids)
				Expect(err).NotTo(HaveOccurred())
				params := javaParams()
				params["orderIdsJson"] = string(b)
				var deleted int64
				Expect(java.InvokeAs(ctx, "mdOptionsDeleteOrders", params, &deleted)).To(Succeed())
				want := int64(0)
				for _, id := range ids {
					if live[id] {
						want++
						delete(live, id)
					}
				}
				Expect(deleted).To(Equal(want))
			}
			goRun := func(f func(*recordlayer.FDBRecordStore)) {
				_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
					store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(keyspace).CreateOrOpen()
					Expect(err).NotTo(HaveOccurred())
					f(store)
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			}
			goSave := func(ids []int64) {
				goRun(func(store *recordlayer.FDBRecordStore) {
					for _, id := range ids {
						x, y := coords(id)
						_, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(id), CoordX: proto.Int64(x), CoordY: proto.Int64(y)})
						Expect(err).NotTo(HaveOccurred())
						live[id] = true
					}
				})
			}
			goDelete := func(ids []int64) {
				goRun(func(store *recordlayer.FDBRecordStore) {
					for _, id := range ids {
						deleted, err := store.DeleteRecord(tuple.Tuple{id})
						Expect(err).NotTo(HaveOccurred())
						Expect(deleted).To(Equal(live[id]))
						delete(live, id)
					}
				})
			}
			// check compares both engines' scans with the live records and
			// reads the stored tree's layout from its raw pairs.
			check := func(step string) {
				var goRows []IndexEntryResult
				goRun(func(store *recordlayer.FDBRecordStore) {
					entries, err := recordlayer.AsList(ctx, store.ScanIndex(md.GetIndex(indexName), recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan()))
					Expect(err).NotTo(HaveOccurred())
					for _, e := range entries {
						goRows = append(goRows, IndexEntryResult{Key: tupleToSlice(e.Key), PrimaryKey: tupleToSlice(e.PrimaryKey())})
					}
				})
				var javaRaw []map[string]any
				Expect(java.InvokeAs(ctx, "mdOptionsScan", javaParams(), &javaRaw)).To(Succeed())
				var javaRows []IndexEntryResult
				for _, m := range javaRaw {
					javaRows = append(javaRows, IndexEntryResult{Key: toInterfaceSlice(m["key"]), PrimaryKey: toInterfaceSlice(m["primaryKey"])})
				}
				Expect(goRows).To(HaveLen(len(live)), step)
				Expect(CompareIndexEntries(goRows, javaRows)).To(Succeed(), step)

				_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
					tx := rtx.Transaction()
					read := func(sub subspace.Subspace) []fdb.KeyValue {
						r, err := fdb.PrefixRange(sub.Bytes())
						Expect(err).NotTo(HaveOccurred())
						kvs, err := tx.GetRange(r, fdb.RangeOptions{}).GetSliceWithError()
						Expect(err).NotTo(HaveOccurred())
						return kvs
					}
					treeSub := keyspace.Sub(int64(2), indexName)
					nodes := map[string]bool{}
					// The child slots of every intermediate node, decoded from
					// the raw bytes in Java's layouts (ChildSlot: key
					// (smallestHV, smallestKey, largestHV, largestKey), value
					// (childId, mbr)), without Go's decoder.
					type childSlot struct {
						largestHV  any
						largestKey tuple.Tuple
						childID    []byte
					}
					children := map[string][]childSlot{}
					leaves := map[string]bool{}
					leafHVs := 0
					for _, kv := range read(treeSub) {
						t, err := treeSub.Unpack(kv.Key)
						Expect(err).NotTo(HaveOccurred())
						id := string(t[0].([]byte))
						nodes[id] = true
						if c.bySlot {
							Expect(len(t)).To(BeNumerically(">", 2), "%s: a BY_SLOT pair names a node, its kind and a slot", step)
							if t[1] == int64(0) {
								Expect(t[2] == nil).To(Equal(!c.storeHV), "%s: leaf slot Hilbert value %v", step, t[2])
								leaves[id] = true
								leafHVs++
								continue
							}
							Expect(t[1]).To(Equal(int64(1)), "%s: a BY_SLOT pair of kind %v", step, t[1])
							Expect(t).To(HaveLen(6), "%s: an intermediate BY_SLOT key is (node, kind, smallestHV, smallestKey, largestHV, largestKey)", step)
							v, err := tuple.Unpack(kv.Value)
							Expect(err).NotTo(HaveOccurred())
							children[id] = append(children[id], childSlot{t[4], t[5].(tuple.Tuple), v[0].([]byte)})
							continue
						}
						Expect(t).To(HaveLen(1), "%s: a BY_NODE pair is keyed by its node alone", step)
						v, err := tuple.Unpack(kv.Value)
						Expect(err).NotTo(HaveOccurred())
						if v[0] == int64(0) {
							leaves[id] = true
							for _, slot := range v[1].(tuple.Tuple) {
								Expect(slot.(tuple.Tuple)[0] == nil).To(Equal(!c.storeHV), "%s: leaf slot Hilbert value %v", step, slot.(tuple.Tuple)[0])
								leafHVs++
							}
							continue
						}
						Expect(v[0]).To(Equal(int64(1)), "%s: a BY_NODE node of kind %v", step, v[0])
						for _, raw := range v[1].(tuple.Tuple) {
							slot := raw.(tuple.Tuple)
							Expect(slot).To(HaveLen(6), "%s: an intermediate BY_NODE slot is (smallestHV, smallestKey, largestHV, largestKey, childId, mbr)", step)
							children[id] = append(children[id], childSlot{slot[2], slot[3].(tuple.Tuple), slot[4].([]byte)})
						}
					}
					Expect(leafHVs).To(Equal(len(live)), "%s: one leaf slot per record", step)
					var level func(id string) int64
					level = func(id string) int64 {
						if leaves[id] {
							return 0
						}
						Expect(children).To(HaveKey(id), "%s: node %x is named but not stored", step, id)
						return level(string(children[id][0].childID)) + 1
					}
					// The tree's shape: every stored node but one (the root) is
					// named as a child exactly once, every named child is
					// stored, and a node's children are all of one level (a
					// level is counted up from the leaves).
					parents := map[string]int{}
					for id, slots := range children {
						for _, slot := range slots {
							parents[string(slot.childID)]++
							Expect(level(string(slot.childID))).To(Equal(level(id)-1), "%s: node %x has a child %x of another level", step, id, slot.childID)
						}
					}
					roots := 0
					for id := range nodes {
						Expect(parents[id]).To(BeNumerically("<=", 1), "%s: node %x is the child of %d slots", step, id, parents[id])
						if parents[id] == 0 {
							roots++
						}
					}
					Expect(roots).To(Equal(min(len(nodes), 1)), "%s: one root", step)
					for id := range parents {
						Expect(nodes).To(HaveKey(id), "%s: child %x is not stored", step, id)
					}
					nsiSub := keyspace.Sub(int64(3), indexName, int64(0))
					var nsi []string
					for _, kv := range read(nsiSub) {
						Expect(kv.Value).To(BeEmpty())
						nsi = append(nsi, string(kv.Key))
					}
					if !c.slotIndex {
						Expect(nsi).To(BeEmpty(), "%s: node slot index entries without the option", step)
						return nil, nil
					}
					// One entry per child slot of every intermediate node:
					// NodeSlotIndexAdapter.createIndexKeyTuple's (level of the
					// child, largest Hilbert value, largest key's items, child
					// id), a leaf's level 0, whole keys compared.
					var want []string
					entries := 0
					for _, slots := range children {
						for _, slot := range slots {
							k := append(tuple.Tuple{level(string(slot.childID)), slot.largestHV}, slot.largestKey...)
							want = append(want, string(nsiSub.Pack(append(k, slot.childID))))
							entries++
						}
					}
					Expect(entries).To(Equal(max(len(nodes)-1, 0)), "%s: one child slot per node but the root, none in an empty tree", step)
					slices.Sort(want)
					slices.Sort(nsi)
					Expect(nsi).To(Equal(want), "%s: node slot index entries", step)
					fmt.Fprintf(GinkgoWriter, "MD_OPTIONS %q %s: records=%d nodes=%d slot-index entries=%d\n", c.name, step, len(live), len(nodes), len(nsi))
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			}
			ids := func(from, to int64, keep func(int64) bool) []int64 {
				var out []int64
				for id := from; id <= to; id++ {
					if keep(id) {
						out = append(out, id)
					}
				}
				return out
			}
			all := func(int64) bool { return true }

			javaSave(ids(1, 120, all))
			check("Java wrote 120")
			goDelete(ids(1, 120, func(id int64) bool { return id%2 == 0 }))
			check("Go deleted the even ids")
			goSave(ids(121, 200, all))
			check("Go wrote 80 more")
			javaDelete(ids(1, 200, func(id int64) bool { return id%3 == 0 }))
			check("Java deleted multiples of three")
			javaSave(ids(201, 230, all))
			goDelete(ids(1, 230, func(id int64) bool { return id%5 == 0 }))
			check("mixed")
			javaDelete(ids(1, 230, all))
			check("Java deleted every record")
			Expect(live).To(BeEmpty())
		})
	}
})
