package recordlayer

import (
	"context"
	"errors"
	"math"
	"strings"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("Partitioned physical layout", func() {
	ctx := context.Background()

	It("round trips physical PKs, fetches local indexes, and reclaims only the selected type and interval", func() {
		b := partitionTestBuilder().SetSplitLongRecords(true).SetStoreRecordVersions(true)
		p := func() KeyExpression { return PartitionExpr(Field("coord_x"), 10) }
		q := func() KeyExpression { return PartitionExpr(Field("val_int64"), 10) }
		value := NewIndex("local_value", Concat(p(), Field("price"), Field("coord_x"), Field("order_id")))
		fanout := NewIndex("local_fanout", Concat(p(), FanOut("tags")))
		cover := NewIndex("local_cover", KeyWithValue(Concat(p(), Field("price"), Field("coord_x"), Field("quantity")), 3))
		unique := NewIndex("local_unique", Concat(p(), Field("coord_x"), Field("order_id"))).SetUnique()
		partial := NewIndex("local_partial", Concat(p(), Field("price"))).SetPredicate(func(msg proto.Message) bool { return msg.(*demopb.Order).GetPrice() > 0 })
		text := NewTextIndex("local_text", GroupBy(Nest("flower", Field("type")), p(), Field("quantity")))
		typedValue := NewIndex("other_value", Concat(q(), Field("price"), Field("val_int64"), Field("id")))
		typedUnique := NewIndex("other_unique", Concat(q(), Field("val_int64"), Field("id"))).SetUnique()
		typedText := NewTextIndex("other_text", GroupBy(Field("val_string"), q()))
		for _, idx := range []*Index{value, fanout, cover, unique, partial, text} {
			b.AddIndex("Order", idx)
		}
		for _, idx := range []*Index{typedValue, typedUnique, typedText} {
			b.AddIndex("TypedRecord", idx)
		}
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		sources := []int64{math.MinInt64, -11, -1, 0, 10, math.MaxInt64}
		coordinates := []int64{-922337203685477581, -2, -1, 0, 1, 922337203685477580}
		orderType := md.GetRecordType("Order").GetRecordTypeKey()
		typedType := md.GetRecordType("TypedRecord").GetRecordTypeKey()
		assertTextEntries := func(store *FDBRecordStore, cleared bool) {
			for i, x := range sources {
				for _, entry := range []struct {
					idx    *Index
					pk     tuple.Tuple
					group  tuple.Tuple
					absent bool
				}{
					{text, tuple.Tuple{orderType, coordinates[i], x, int64(1)}, tuple.Tuple{coordinates[i], int64(3)}, cleared && x == -11},
					{typedText, tuple.Tuple{typedType, coordinates[i], x, int64(1)}, tuple.Tuple{coordinates[i]}, false},
				} {
					version, err := store.context.Transaction().Get(fdb.Key(store.indexSecondarySubspace(entry.idx).Sub(int64(0)).Pack(entry.pk))).Get()
					Expect(err).NotTo(HaveOccurred())
					if entry.absent {
						Expect(version).To(BeNil())
					} else {
						Expect(version).NotTo(BeNil(), "tokenizer-version positive control")
						tokenizerVersion, err := getTextTokenizerVersion(entry.idx)
						Expect(err).NotTo(HaveOccurred())
						Expect(version).To(Equal(tuple.Tuple{int64(tokenizerVersion)}.Pack()))
					}
					for position, token := range []string{"connection", "timeout"} {
						prefix := append(append(tuple.Tuple{}, entry.group...), token)
						postings, err := AsList(ctx, store.ScanIndexByType(entry.idx, IndexScanByTextToken, TupleRangeAllOf(prefix), nil, ForwardScan()))
						Expect(err).NotTo(HaveOccurred())
						if entry.absent {
							Expect(postings).To(BeEmpty())
						} else {
							Expect(postings).To(HaveLen(1), "posting positive control for %s at %v", token, entry.pk)
							Expect(postings[0].PrimaryKey()).To(Equal(entry.pk))
							Expect(postings[0].Value).To(Equal(tuple.Tuple{tuple.Tuple{int64(position)}}))
						}
					}
				}
			}
		}
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			for i, x := range sources {
				order := &demopb.Order{OrderId: proto.Int64(1), CoordX: proto.Int64(x), Price: proto.Int32(7), Quantity: proto.Int32(3), Tags: []string{"connection timeout", "extra"}, Flower: &demopb.Flower{Type: proto.String("connection timeout")}}
				if x == -11 {
					order.VectorData = []byte(strings.Repeat("x", 120_000))
				}
				saved, err := store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())
				pk := tuple.Tuple{orderType, coordinates[i], x, int64(1)}
				Expect(saved.PrimaryKey).To(Equal(pk))
				trimmed, err := value.TrimPrimaryKey(pk)
				Expect(err).NotTo(HaveOccurred())
				Expect(trimmed).To(Equal(tuple.Tuple{orderType}))
				Expect(value.EntryPrimaryKey(tuple.Tuple{coordinates[i], int64(7), x, int64(1), orderType})).To(Equal(pk))
				Expect(store.AddUniquenessViolationWithExisting(unique, tuple.Tuple{coordinates[i], x, int64(1)}, pk, pk)).To(Succeed())
				typed, err := store.SaveRecord(&demopb.TypedRecord{Id: proto.Int64(1), ValInt64: proto.Int64(x), Price: proto.Int32(7), ValString: proto.String("connection timeout")})
				Expect(err).NotTo(HaveOccurred())
				Expect(typed.PrimaryKey).To(Equal(tuple.Tuple{typedType, coordinates[i], x, int64(1)}))
				Expect(store.AddUniquenessViolationWithExisting(typedUnique, tuple.Tuple{coordinates[i], x, int64(1)}, typed.PrimaryKey, typed.PrimaryKey)).To(Succeed())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Open()
			Expect(err).NotTo(HaveOccurred())
			for _, idx := range []*Index{value, cover, unique, partial, typedValue, typedUnique} {
				rows, err := AsList(ctx, store.ScanIndexRecords(idx.Name, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(rows).To(HaveLen(len(sources)), idx.Name)
				for _, row := range rows {
					Expect(row.IndexEntry.PrimaryKey()).To(Equal(row.Record.PrimaryKey))
				}
			}
			fanoutRows, err := AsList(ctx, store.ScanIndexRecords(fanout.Name, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(fanoutRows).To(HaveLen(2 * len(sources)))
			assertTextEntries(store, false)
			// Snapshot every non-target key, then verify byte-for-byte survival.
			before, err := rc.Transaction().GetRange(root, fdb.RangeOptions{}).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			Expect(before).NotTo(BeEmpty())
			targetPrefixes := [][]byte{
				store.recordsSubspace.Pack(tuple.Tuple{orderType, int64(-2)}),
				store.subspace.Sub(RecordVersionKey).Pack(tuple.Tuple{orderType, int64(-2)}),
				store.indexSecondarySubspace(text).Sub(int64(0)).Pack(tuple.Tuple{orderType, int64(-2)}),
				store.subspace.Sub(IndexUniquenessViolationsKey, unique.SubspaceTupleKey()).Pack(tuple.Tuple{int64(-2)}),
			}
			for _, idx := range []*Index{value, fanout, cover, unique, partial, text} {
				targetPrefixes = append(targetPrefixes, store.indexSubspace(idx).Pack(tuple.Tuple{int64(-2)}))
			}
			for i, prefix := range targetPrefixes {
				if i == 1 {
					continue
				} // Legacy versions are absent in the modern format.
				population := 0
				for _, kv := range before {
					if strings.HasPrefix(string(kv.Key), string(prefix)) {
						population++
					}
				}
				Expect(population).To(BeNumerically(">", 0), "target prefix %x needs a positive control", prefix)
				if i == 0 {
					Expect(population).To(BeNumerically(">", 2), "split fragments and inline version must exist")
				}
			}
			var survivors []fdb.KeyValue
			for _, kv := range before {
				target := false
				for _, prefix := range targetPrefixes {
					if strings.HasPrefix(string(kv.Key), string(prefix)) {
						target = true
					}
				}
				if !target {
					survivors = append(survivors, kv)
				}
			}
			Expect(survivors).NotTo(BeEmpty())
			Expect(store.ClearPartitionData("Order", -2)).To(Succeed())
			after, err := rc.Transaction().GetRange(root, fdb.RangeOptions{}).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			Expect(after).To(Equal(survivors), "all and only target physical/auxiliary keys must disappear")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Open()
			Expect(err).NotTo(HaveOccurred())
			for i, x := range sources {
				row, err := store.LoadRecord(tuple.Tuple{orderType, coordinates[i], x, int64(1)})
				Expect(err).NotTo(HaveOccurred())
				if x == -11 {
					Expect(row).To(BeNil())
				} else {
					Expect(row).NotTo(BeNil())
				}
				other, err := store.LoadRecord(tuple.Tuple{typedType, coordinates[i], x, int64(1)})
				Expect(err).NotTo(HaveOccurred())
				Expect(other).NotTo(BeNil())
			}
			assertTextEntries(store, true)
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	for _, rejection := range []string{"write-only", "heartbeat", "pending queue", "inconsistent expression", "intermingled sibling", "plain table", "unknown table"} {
		It("stages no clear when cleanup refuses "+rejection, func() {
			b := partitionTestBuilder()
			first := NewIndex("first", Concat(PartitionExpr(Field("coord_x"), 10), Field("price")))
			last := NewIndex("last", Concat(PartitionExpr(Field("coord_x"), 10), Field("quantity")))
			b.AddIndex("Order", first).AddIndex("Order", last)
			md, err := b.Build()
			Expect(err).NotTo(HaveOccurred())
			root := specSubspace()
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&demopb.Order{OrderId: proto.Int64(1), CoordX: proto.Int64(-11), Price: proto.Int32(7), Quantity: proto.Int32(3)})
				Expect(err).NotTo(HaveOccurred())
				switch rejection {
				case "write-only":
					_, err = store.MarkIndexWriteOnly(last.Name)
					Expect(err).NotTo(HaveOccurred())
				case "heartbeat":
					rc.Transaction().Set(heartbeatSubspace(root, last).Pack(tuple.Tuple{"unfenced-builder"}), []byte{1})
				case "pending queue":
					rc.Transaction().Set(store.indexingPendingWriteQueue(last, 0).entries.Pack(tuple.Tuple{int64(1)}), []byte{1})
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			var committed []fdb.KeyValue
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Open()
				Expect(err).NotTo(HaveOccurred())
				if rejection == "inconsistent expression" {
					old := last.RootExpression
					last.RootExpression = Concat(PartitionExpr(Field("coord_x"), 11), Field("quantity"))
					defer func() { last.RootExpression = old }()
				}
				if rejection == "intermingled sibling" {
					other := md.GetRecordType("Customer")
					old := other.PrimaryKey
					other.PrimaryKey = Concat(Field("customer_id"), Field("price"))
					defer func() { other.PrimaryKey = old }()
				}
				committed, err = rc.Transaction().GetRange(root, fdb.RangeOptions{}).GetSliceWithError()
				Expect(err).NotTo(HaveOccurred())
				Expect(committed).NotTo(BeEmpty())
				table := "Order"
				if rejection == "plain table" {
					table = "Customer"
				}
				if rejection == "unknown table" {
					table = "missing"
				}
				err = store.ClearPartitionData(table, -2)
				var cleanup *PartitionCleanupError
				var metadata *PartitionMetadataError
				Expect(errors.As(err, &cleanup) || errors.As(err, &metadata)).To(BeTrue(), "%v", err)
				after, err := rc.Transaction().GetRange(root, fdb.RangeOptions{}).GetSliceWithError()
				Expect(err).NotTo(HaveOccurred())
				Expect(after).To(Equal(committed))
				return nil, nil // Deliberately commit after refusing cleanup.
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				after, err := rc.Transaction().GetRange(root, fdb.RangeOptions{}).GetSliceWithError()
				Expect(err).NotTo(HaveOccurred())
				Expect(after).To(Equal(committed))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}
})
