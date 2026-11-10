package recordlayer

import (
	"context"
	"errors"
	"fmt"
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

	It("refuses NULL routing at SaveRecord without mutations even when the caller commits", func() {
		b := partitionTestBuilder().SetStoreRecordVersions(true)
		b.AddIndex("Order", NewIndex("null_value", Concat(PartitionExpr(Field("coord_x"), 10), Field("price"))))
		b.AddIndex("Order", NewTextIndex("null_text", GroupBy(Nest("flower", Field("type")), PartitionExpr(Field("coord_x"), 10))))
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&demopb.Order{OrderId: proto.Int64(1), CoordX: proto.Int64(10), Price: proto.Int32(7), Flower: &demopb.Flower{Type: proto.String("connection timeout")}})
			Expect(err).NotTo(HaveOccurred())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		var before []fdb.KeyValue
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Open()
			Expect(err).NotTo(HaveOccurred())
			before, err = rc.Transaction().GetRange(root, fdb.RangeOptions{}).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			Expect(before).NotTo(BeEmpty())
			for _, record := range []proto.Message{&demopb.Order{OrderId: proto.Int64(2)}, &demopb.TypedRecord{Id: proto.Int64(2)}} {
				_, err := store.SaveRecord(record)
				var routing *PartitionRoutingError
				Expect(errors.As(err, &routing)).To(BeTrue(), "%v", err)
				Expect(routing.Value).To(BeNil())
				Expect(routing.Width).To(Equal(int64(10)))
				Expect(routing.Reason).To(Equal("source must be a non-NULL BIGINT"))
			}
			after, err := rc.Transaction().GetRange(root, fdb.RangeOptions{}).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			Expect(after).To(Equal(before))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			after, err := rc.Transaction().GetRange(root, fdb.RangeOptions{}).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			Expect(after).To(Equal(before))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("preserves both covering entries when the sources share a coordinate and logical ID", func() {
		b := partitionTestBuilder()
		p := func() KeyExpression { return PartitionExpr(Field("coord_x"), 10) }
		indexes := []*Index{
			NewIndex("cover_uncovered_source", KeyWithValue(Concat(p(), Field("price"), Field("quantity")), 2)),
			NewIndex("cover_source_in_key_and_value", KeyWithValue(Concat(p(), Field("price"), Field("coord_x"), Field("coord_x")), 3)),
			NewIndex("cover_id_in_key_and_value", KeyWithValue(Concat(p(), Field("price"), Field("order_id"), Field("order_id")), 3)),
		}
		for _, idx := range indexes {
			b.AddIndex("Order", idx)
		}
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		wire, err := md.ToProto()
		Expect(err).NotTo(HaveOccurred())
		_, err = RecordMetaDataFromProto(wire)
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		typeKey := md.GetRecordType("Order").GetRecordTypeKey()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			for _, source := range []int64{-11, -12} {
				_, err := store.SaveRecord(&demopb.Order{OrderId: proto.Int64(1), CoordX: proto.Int64(source), Price: proto.Int32(7), Quantity: proto.Int32(3)})
				Expect(err).NotTo(HaveOccurred())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Open()
			Expect(err).NotTo(HaveOccurred())
			for i, idx := range indexes {
				rows, err := AsList(ctx, store.ScanIndexRecords(idx.Name, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(rows).To(HaveLen(2), idx.Name)
				seen := map[int64]bool{}
				for _, row := range rows {
					msg := row.Record.Record.ProtoReflect()
					source := msg.Get(msg.Descriptor().Fields().ByName("coord_x")).Int()
					seen[source] = true
					pk := tuple.Tuple{typeKey, int64(-2), source, int64(1)}
					Expect(row.Record.PrimaryKey).To(Equal(pk))
					Expect(row.IndexEntry.PrimaryKey()).To(Equal(pk))
					included := int64(3)
					if i == 1 {
						included = source
					}
					if i == 2 {
						included = 1
					}
					Expect(row.IndexEntry.Value).To(Equal(tuple.Tuple{included}))
				}
				Expect(seen).To(Equal(map[int64]bool{-11: true, -12: true}))
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	for _, bodyInPK := range []bool{false, true} {
		It(fmt.Sprintf("preserves shared-token TEXT documents with body in PK=%t", bodyInPK), func() {
			b := partitionTestBuilder()
			p := PartitionExpr(Field("val_int64"), 10)
			groups := []KeyExpression{p}
			if bodyInPK {
				b.GetRecordType("TypedRecord").SetPrimaryKey(Concat(RecordTypeKey(), p, Field("val_int64"), Field("val_string")))
				groups = append(groups, Field("val_string"))
			}
			idx := NewTextIndex("shared_token", GroupBy(Field("val_string"), groups...))
			b.AddIndex("TypedRecord", idx)
			md, err := b.Build()
			Expect(err).NotTo(HaveOccurred())
			wire, err := md.ToProto()
			Expect(err).NotTo(HaveOccurred())
			_, err = RecordMetaDataFromProto(wire)
			Expect(err).NotTo(HaveOccurred())
			root := specSubspace()
			typeKey := md.GetRecordType("TypedRecord").GetRecordTypeKey()
			bodies := []string{"connection timeout", "connection error"}
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				for i, body := range bodies {
					_, err := store.SaveRecord(&demopb.TypedRecord{Id: proto.Int64(int64(i + 1)), ValInt64: proto.Int64(-11), ValString: proto.String(body)})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Open()
				Expect(err).NotTo(HaveOccurred())
				prefixes := []tuple.Tuple{{int64(-2), "connection"}}
				if bodyInPK {
					prefixes = []tuple.Tuple{{int64(-2), bodies[0], "connection"}, {int64(-2), bodies[1], "connection"}}
				}
				var rows []*FDBIndexedRecord
				for _, prefix := range prefixes {
					postings := store.ScanIndexByType(idx, IndexScanByTextToken, TupleRangeAllOf(prefix), nil, ForwardScan())
					fetched, err := AsList(ctx, store.FetchIndexRecords(postings, IndexOrphanError, nil))
					Expect(err).NotTo(HaveOccurred())
					if bodyInPK {
						Expect(fetched).To(HaveLen(1))
					} else {
						Expect(fetched).To(HaveLen(2))
					}
					rows = append(rows, fetched...)
				}
				Expect(rows).To(HaveLen(2))
				seen := map[string]bool{}
				for _, row := range rows {
					msg := row.Record.Record.ProtoReflect()
					body := msg.Get(msg.Descriptor().Fields().ByName("val_string")).String()
					seen[body] = true
					var logicalLast any = msg.Get(msg.Descriptor().Fields().ByName("id")).Int()
					if bodyInPK {
						logicalLast = body
					}
					pk := tuple.Tuple{typeKey, int64(-2), int64(-11), logicalLast}
					Expect(row.Record.PrimaryKey).To(Equal(pk))
					Expect(row.IndexEntry.PrimaryKey()).To(Equal(pk))
					Expect(row.IndexEntry.Value).To(Equal(tuple.Tuple{tuple.Tuple{int64(0)}}))
				}
				Expect(seen).To(Equal(map[string]bool{bodies[0]: true, bodies[1]: true}))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}

	for _, format := range []int32{formatVersionSaveUnsplitWithSuffix, formatVersionDefault} {
		for _, pending := range []bool{false, true} {
			It(fmt.Sprintf("reclaims format %d versions with pending=%t and preserves neighboring versions", format, pending), func() {
				b := partitionTestBuilder().SetStoreRecordVersions(true)
				md, err := b.Build()
				Expect(err).NotTo(HaveOccurred())
				root := specSubspace()
				typeKey := md.GetRecordType("Order").GetRecordTypeKey()
				targetPK := tuple.Tuple{typeKey, int64(-2), int64(-11), int64(1)}
				neighborPK := tuple.Tuple{typeKey, int64(1), int64(10), int64(1)}
				var neighborLocal int
				var neighborCommitted []byte
				open := func(rc *FDBRecordContext) *FDBRecordStore {
					store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).SetFormatVersion(format).CreateOrOpen()
					Expect(err).NotTo(HaveOccurred())
					Expect(store.useOldVersionFormat()).To(Equal(format < formatVersionSaveVersionWithRecord))
					return store
				}
				buffered := func(rc *FDBRecordContext, key fdb.Key) bool {
					r, err := fdb.PrefixRange(key)
					Expect(err).NotTo(HaveOccurred())
					return rc.HasVersionMutationsInRange(r.Begin.FDBKey(), r.End.FDBKey())
				}
				assertTargetEmpty := func(rc *FDBRecordContext) {
					for _, sub := range []int{RecordKey, RecordVersionKey} {
						r, err := fdb.PrefixRange(root.Sub(sub).Pack(tuple.Tuple{typeKey, int64(-2)}))
						Expect(err).NotTo(HaveOccurred())
						rows, err := rc.Transaction().GetRange(r, fdb.RangeOptions{}).GetSliceWithError()
						Expect(err).NotTo(HaveOccurred())
						Expect(rows).To(BeEmpty(), "target record/version keys must not be resurrected")
					}
				}
				seed := func(rc *FDBRecordContext, store *FDBRecordStore) {
					for _, source := range []int64{-11, 10} {
						saved, err := store.SaveRecord(&demopb.Order{OrderId: proto.Int64(1), CoordX: proto.Int64(source)})
						Expect(err).NotTo(HaveOccurred())
						Expect(saved.Version).NotTo(BeNil())
						Expect(saved.Version.IsComplete()).To(BeFalse())
						key := store.versionKey(saved.PrimaryKey)
						local, present := rc.GetLocalVersion(key)
						Expect(present).To(BeTrue(), "local-version positive control")
						Expect(local).To(Equal(saved.Version.GetLocalVersion()))
						Expect(buffered(rc, key)).To(BeTrue(), "pending-mutation positive control")
						if source == 10 {
							neighborLocal = local
						}
					}
				}
				clear := func(rc *FDBRecordContext, store *FDBRecordStore) {
					targetKey, neighborKey := store.versionKey(targetPK), store.versionKey(neighborPK)
					if pending {
						Expect(buffered(rc, targetKey)).To(BeTrue())
						Expect(buffered(rc, neighborKey)).To(BeTrue())
					} else {
						target, err := rc.Transaction().Get(targetKey).Get()
						Expect(err).NotTo(HaveOccurred())
						Expect(target).NotTo(BeEmpty(), "committed target-version positive control")
						neighborCommitted, err = rc.Transaction().Get(neighborKey).Get()
						Expect(err).NotTo(HaveOccurred())
						Expect(neighborCommitted).NotTo(BeEmpty(), "committed neighbor-version positive control")
					}
					body, err := store.LoadRecord(targetPK)
					Expect(err).NotTo(HaveOccurred())
					Expect(body).NotTo(BeNil())
					Expect(body.Version).NotTo(BeNil())
					Expect(store.ClearPartitionData("Order", -2)).To(Succeed())
					assertTargetEmpty(rc)
					Expect(buffered(rc, targetKey)).To(BeFalse())
					_, targetCached := rc.GetLocalVersion(targetKey)
					Expect(targetCached).To(BeFalse())
					if pending {
						Expect(buffered(rc, neighborKey)).To(BeTrue())
						local, present := rc.GetLocalVersion(neighborKey)
						Expect(present).To(BeTrue())
						Expect(local).To(Equal(neighborLocal))
					} else {
						version, err := rc.Transaction().Get(neighborKey).Get()
						Expect(err).NotTo(HaveOccurred())
						Expect(version).To(Equal(neighborCommitted))
					}
				}
				_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
					store := open(rc)
					seed(rc, store)
					if pending {
						clear(rc, store)
					}
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
				if !pending {
					_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) { clear(rc, open(rc)); return nil, nil })
					Expect(err).NotTo(HaveOccurred())
				}
				_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
					store := open(rc)
					assertTargetEmpty(rc)
					target, err := store.LoadRecord(targetPK)
					Expect(err).NotTo(HaveOccurred())
					Expect(target).To(BeNil())
					neighbor, err := store.LoadRecord(neighborPK)
					Expect(err).NotTo(HaveOccurred())
					Expect(neighbor).NotTo(BeNil())
					Expect(neighbor.Version).NotTo(BeNil())
					Expect(neighbor.Version.IsComplete()).To(BeTrue())
					Expect(neighbor.Version.GetLocalVersion()).To(Equal(neighborLocal))
					version, err := rc.Transaction().Get(store.versionKey(neighborPK)).Get()
					Expect(err).NotTo(HaveOccurred())
					Expect(version).NotTo(BeEmpty())
					if !pending {
						Expect(version).To(Equal(neighborCommitted))
					}
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			})
		}
	}

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
