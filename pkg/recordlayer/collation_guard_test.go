package recordlayer

import (
	"context"
	"errors"
	"fmt"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

func TestCollationFunctionIn(t *testing.T) {
	t.Parallel()
	for _, name := range []string{CollateFuncJRE, CollateFuncICU} {
		collated := FunctionExpr(name, Field("name"))
		if createsDuplicates(collated) || !createsDuplicates(FunctionExpr(name, FanOut("names"))) {
			t.Fatalf("%s must delegate fan-out to its arguments", name)
		}
		for shape, expr := range map[string]KeyExpression{
			"function":          collated,
			"function argument": FunctionExpr("substring", collated),
			"cardinality":       CardinalityExpr(collated),
			"concat":            Concat(Field("id"), collated),
			"list":              ListExpr(Field("id"), collated),
			"nest":              Nest("customer", collated),
			"grouping":          GroupAll(collated),
			"value":             KeyWithValue(Concat(Field("id"), collated), 1),
			"split":             Split(collated, 1),
			"dimensions":        Dimensions(Concat(Field("id"), collated), 0, 1),
		} {
			if got := collationFunctionIn(expr); got != name {
				t.Errorf("%s/%s: got %q", name, shape, got)
			}
		}
	}
	for _, expr := range []KeyExpression{nil, EmptyKey(), Field("name"), LiteralExpr("collate_jre"), Concat(Field("id"), Nest("customer", Field("name")))} {
		if got := collationFunctionIn(expr); got != "" {
			t.Errorf("%T is not collated, got %q", expr, got)
		}
	}
}

func TestGoOnlyCollationError(t *testing.T) {
	t.Parallel()
	for _, err := range []*GoOnlyCollationError{
		{Function: CollateFuncJRE, IndexName: "names"},
		{Function: CollateFuncICU, RecordTypeName: "Customer"},
		{Function: CollateFuncJRE, RecordCountKey: true},
	} {
		var metadata *MetaDataError
		if !errors.As(err, &metadata) || !IsMetaDataException(err) {
			t.Fatalf("not a metadata error: %v", err)
		}
		if err.Error() == "" {
			t.Fatal("empty error")
		}
	}
}

var _ = Describe("Go-only collation guard", func() {
	ctx := context.Background()
	build := func(configure func(*RecordMetaDataBuilder)) *RecordMetaData {
		b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		b.GetRecordType("Order").SetPrimaryKey(Concat(RecordTypeKey(), Field("order_id")))
		b.GetRecordType("Customer").SetPrimaryKey(Concat(RecordTypeKey(), Field("customer_id")))
		b.GetRecordType("TypedRecord").SetPrimaryKey(Concat(RecordTypeKey(), Field("id")))
		configure(b)
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}
	run := func(md *RecordMetaData, optedIn bool, body func(*FDBRecordStore) error) error {
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).
				SetGoOnlyCollation(optedIn).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			return nil, body(store)
		})
		return err
	}
	guard := func(err error, function string) *GoOnlyCollationError {
		var refusal *GoOnlyCollationError
		Expect(errors.As(err, &refusal)).To(BeTrue(), "expected collation guard, got %v", err)
		Expect(refusal.Function).To(Equal(function))
		return refusal
	}
	contents := func(store *FDBRecordStore) []fdb.KeyValue {
		kvs, err := store.context.Transaction().GetRange(store.subspace, fdb.RangeOptions{}).GetSliceWithError()
		Expect(err).NotTo(HaveOccurred())
		return kvs
	}
	customer := func(id int64) *gen.Customer {
		return &gen.Customer{CustomerId: proto.Int64(id), Name: proto.String("Résumé")}
	}

	for _, name := range []string{CollateFuncJRE, CollateFuncICU} {
		It(name+" guards index access and writes without blocking metadata or raw records", func() {
			index := NewIndex("collated_names", Concat(RecordTypeKey(), FunctionExpr(name, Field("name"))))
			md := build(func(b *RecordMetaDataBuilder) { b.AddIndex("Customer", index) })
			var pk tuple.Tuple
			Expect(run(md, true, func(store *FDBRecordStore) error {
				for _, b := range []*StoreBuilder{store.AsBuilder(), store.CopyBuilder(store.context), store.AsBuilder().copyBuilder()} {
					Expect(b.goOnlyCollation).To(BeTrue())
					copy, err := b.Build()
					Expect(err).NotTo(HaveOccurred())
					Expect(copy.goOnlyCollation).To(BeTrue())
				}
				record, err := store.SaveRecord(customer(1))
				Expect(err).NotTo(HaveOccurred())
				pk = record.PrimaryKey
				entries, err := AsList(ctx, store.ScanIndex(index, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(1))
				return nil
			})).To(Succeed())

			Expect(run(md, false, func(store *FDBRecordStore) error {
				loaded, err := store.LoadRecord(pk)
				Expect(err).NotTo(HaveOccurred())
				Expect(loaded).NotTo(BeNil())
				records, err := AsList(ctx, store.ScanRecords(nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(records).To(HaveLen(1))
				// A dry run never evaluates secondary indexes.
				_, err = store.DryRunSaveRecord(customer(2), RecordExistenceCheckNone)
				Expect(err).NotTo(HaveOccurred())
				before := contents(store)
				_, err = store.SaveRecord(customer(2))
				Expect(guard(err, name).IndexName).To(Equal(index.Name))
				_, err = store.SaveRecordBatch([]proto.Message{customer(2)})
				guard(err, name)
				_, err = store.SaveRecordBatch([]proto.Message{&gen.Order{OrderId: proto.Int64(99)}, customer(2)})
				guard(err, name)
				_, err = store.DeleteRecord(pk)
				guard(err, name)
				guard(store.RebuildIndex(index), name)
				_, err = store.GetIndexMaintainer(index)
				guard(err, name)
				_, err = store.ValidateIndex(ctx, index)
				guard(err, name)
				_, err = AsList(ctx, store.ScanIndex(index, TupleRangeAll, nil, ForwardScan()))
				guard(err, name)
				_, err = AsList(ctx, store.ScanIndexByType(index, IndexScanByValue, TupleRangeAll, nil, ForwardScan()))
				guard(err, name)
				Expect(contents(store)).To(Equal(before), "refusal must precede all mutations, even if the caller commits")
				// The guard is scoped to the indexed type, not the whole store.
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1)})
				Expect(err).NotTo(HaveOccurred())
				// Range clearing copies existing prefix bytes; it evaluates no collation.
				Expect(store.DeleteRecordsWhere(pk[:1])).To(Succeed())
				_, err = store.GetIndexMaintainer(index)
				guard(err, name) // A raw-access cache entry must not bypass the guard.
				_, err = store.MarkIndexDisabled(index.Name)
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(customer(3))
				Expect(err).NotTo(HaveOccurred())
				return nil
			})).To(Succeed())
		})

		It(name+" guards every primary-key evaluation but allows raw-key reads and deletion", func() {
			md := build(func(b *RecordMetaDataBuilder) {
				b.GetRecordType("Customer").SetPrimaryKey(FunctionExpr(name, Field("name")))
			})
			var pk tuple.Tuple
			var queued []byte
			Expect(run(md, true, func(store *FDBRecordStore) error {
				record, err := store.SaveRecord(customer(1))
				Expect(err).NotTo(HaveOccurred())
				pk = record.PrimaryKey
				queued, err = store.serializePendingRecord(record)
				return err
			})).To(Succeed())
			Expect(run(md, false, func(store *FDBRecordStore) error {
				before := contents(store)
				_, err := store.SaveRecord(customer(2))
				Expect(guard(err, name).RecordTypeName).To(Equal("Customer"))
				_, err = store.SaveRecordBatch([]proto.Message{customer(2)})
				guard(err, name)
				_, err = store.DryRunSaveRecord(customer(2), RecordExistenceCheckNone)
				guard(err, name)
				_, err = store.deserializePendingRecord(queued)
				guard(err, name)
				Expect(contents(store)).To(Equal(before))
				loaded, err := store.LoadRecord(pk)
				Expect(err).NotTo(HaveOccurred())
				Expect(loaded).NotTo(BeNil())
				deleted, err := store.DeleteRecord(pk)
				Expect(err).NotTo(HaveOccurred())
				Expect(deleted).To(BeTrue())
				return nil
			})).To(Succeed())
		})
	}

	for _, source := range []bool{false, true} {
		It(fmt.Sprintf("refuses an online rebuild before its clearing transaction commits (source %v)", source), func() {
			index := NewIndex("collated_names", FunctionExpr(CollateFuncJRE, Field("name")))
			target := index
			md := build(func(b *RecordMetaDataBuilder) {
				b.AddIndex("Customer", index)
				if source {
					target = NewIndex("plain_names", Field("name"))
					b.AddIndex("Customer", target)
				}
			})
			var before []fdb.KeyValue
			Expect(run(md, true, func(store *FDBRecordStore) error {
				_, err := store.SaveRecord(customer(1))
				Expect(err).NotTo(HaveOccurred())
				before = contents(store)
				return nil
			})).To(Succeed())
			builder := NewOnlineIndexerBuilder().SetDatabase(sharedDB).SetMetaData(md).SetSubspace(specSubspace()).
				SetIndex(target).SetPolicy(&IndexingPolicy{IfReadable: DesiredActionRebuild})
			if source {
				builder.SetSourceIndex(index)
			}
			indexer, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			_, err = indexer.BuildIndex(ctx)
			guard(err, CollateFuncJRE)
			Expect(run(md, true, func(store *FDBRecordStore) error {
				Expect(contents(store)).To(Equal(before), "a refused online rebuild must not clear the existing index")
				return nil
			})).To(Succeed())
			builder.SetRecordStoreBuilder(NewStoreBuilder().SetGoOnlyCollation(true))
			indexer, err = builder.Build()
			Expect(err).NotTo(HaveOccurred())
			_, err = indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(run(md, true, func(store *FDBRecordStore) error {
				entries, err := AsList(ctx, store.ScanIndex(index, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(1))
				return nil
			})).To(Succeed())
		})
	}

	for _, primaryKey := range []bool{false, true} {
		It(fmt.Sprintf("preflights the whole bare-key batch before any writes (primary key %v)", primaryKey), func() {
			md := build(func(b *RecordMetaDataBuilder) {
				collated := FunctionExpr(CollateFuncJRE, Field("name"))
				if primaryKey {
					b.GetRecordType("Customer").SetPrimaryKey(collated)
				} else {
					b.AddIndex("Customer", NewIndex("collated_names", collated))
				}
			})
			_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(specSubspace()).
					SetFormatVersion(4).SetGoOnlyCollation(true).Create()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.omitUnsplitRecordSuffix()).To(BeTrue())
				store, err = store.AsBuilder().SetGoOnlyCollation(false).Build()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.ensureStoreStateLoadedErr()).To(Succeed())
				before := contents(store)
				_, err = store.SaveRecordBatch([]proto.Message{&gen.Order{OrderId: proto.Int64(99)}, customer(2)})
				guard(err, CollateFuncJRE)
				Expect(contents(store)).To(Equal(before), "the legacy fallback must refuse before its first save")
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}

	It("guards legacy collated count writes, but not updates or raw count reads", func() {
		countKey := FunctionExpr(CollateFuncJRE, LiteralExpr("a"))
		md := build(func(b *RecordMetaDataBuilder) { b.SetRecordCountKey(countKey) })
		var pk tuple.Tuple
		Expect(run(md, true, func(store *FDBRecordStore) error {
			record, err := store.SaveRecord(customer(1))
			Expect(err).NotTo(HaveOccurred())
			pk = record.PrimaryKey
			return nil
		})).To(Succeed())
		Expect(run(md, false, func(store *FDBRecordStore) error {
			count, err := store.GetRecordCount()
			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(Equal(int64(1)))
			before := contents(store)
			_, err = store.SaveRecord(customer(2))
			Expect(guard(err, CollateFuncJRE).RecordCountKey).To(BeTrue())
			_, err = store.SaveRecordBatch([]proto.Message{customer(2)})
			guard(err, CollateFuncJRE)
			_, err = store.DeleteRecord(pk)
			guard(err, CollateFuncJRE)
			guard(store.rebuildRecordCounts(countKey), CollateFuncJRE)
			Expect(contents(store)).To(Equal(before))
			_, err = store.SaveRecord(customer(1))
			Expect(err).NotTo(HaveOccurred(), "an update does not change the legacy count")
			_, err = store.SaveRecordBatch([]proto.Message{customer(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(store.UpdateRecordCountState(gen.DataStoreInfo_DISABLED)).To(Succeed())
			_, err = store.SaveRecord(customer(2))
			Expect(err).NotTo(HaveOccurred())
			return store.DeleteAllRecords()
		})).To(Succeed())
	})
})
