package recordlayer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

type partitionVersionEffects struct {
	mutations map[string]versionMutation
	local     map[string]int
}

func partitionVersionEffectsOf(rc *FDBRecordContext) partitionVersionEffects {
	rc.versionMu.Lock()
	defer rc.versionMu.Unlock()
	out := partitionVersionEffects{mutations: maps.Clone(rc.versionMutations), local: maps.Clone(rc.localVersionCache)}
	for key, value := range out.mutations {
		value.value = bytes.Clone(value.value)
		out.mutations[key] = value
	}
	return out
}

func partitionWholeStore(rc *FDBRecordContext, store *FDBRecordStore) []fdb.KeyValue {
	GinkgoHelper()
	rows, err := rc.Transaction().GetRange(store.subspace, fdb.RangeOptions{}).GetSliceWithError()
	Expect(err).NotTo(HaveOccurred())
	return rows
}

var _ = Describe("Partition consistency preflight", func() {
	ctx := context.Background()
	for _, phase := range []string{"virgin", "committed", "pending"} {
		It("previews "+phase+" saves and deletes without persisted or pending-version effects", func() {
			md, err := partitionTestBuilder().SetStoreRecordVersions(true).Build()
			Expect(err).NotTo(HaveOccurred())
			root := specSubspace()
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store := partitionProtocolStore(rc, md, root)
				if phase == "committed" {
					_, err := store.SaveRecord(partitionOrder(1, -11, 7))
					return nil, err
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			var before []fdb.KeyValue
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store := partitionProtocolStore(rc, md, root)
				if phase == "pending" {
					_, err := store.SaveRecord(partitionOrder(1, -11, 7))
					Expect(err).NotTo(HaveOccurred())
					Expect(rc.HasVersionMutations()).To(BeTrue())
					Expect(rc.localVersionCache).NotTo(BeEmpty())
				}
				before = partitionWholeStore(rc, store)
				effects := partitionVersionEffectsOf(rc)
				preview, err := store.DryRunSaveRecord(partitionOrder(1, -11, 8), RecordExistenceCheckNone)
				Expect(err).NotTo(HaveOccurred())
				Expect(preview.PrimaryKey).To(Equal(tuple.Tuple{md.GetRecordType("Order").GetRecordTypeKey(), int64(-2), int64(-11), int64(1)}))
				found, err := store.DryRunDeleteRecord(preview.PrimaryKey)
				Expect(err).NotTo(HaveOccurred())
				Expect(found).To(Equal(phase != "virgin"))
				Expect(partitionWholeStore(rc, store)).To(Equal(before))
				Expect(partitionVersionEffectsOf(rc)).To(Equal(effects))
				if phase == "virgin" {
					Expect(partitionProtocolRows(rc, root)).To(BeEmpty())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store := partitionProtocolStore(rc, md, root)
				if phase != "pending" {
					Expect(partitionWholeStore(rc, store)).To(Equal(before))
				} else {
					row, err := store.LoadRecord(tuple.Tuple{md.GetRecordType("Order").GetRecordTypeKey(), int64(-2), int64(-11), int64(1)})
					Expect(err).NotTo(HaveOccurred())
					Expect(row.Record.(*demopb.Order).GetPrice()).To(Equal(int32(7)))
					Expect(row.Version.IsComplete()).To(BeTrue())
					Expect(partitionProtocolRows(rc, root)).To(HaveLen(4))
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}

	for _, corruption := range []string{"header", "active", "fence", "generation", "same-interval body", "cross-interval body", "wrong-type body"} {
		It("refuses corrupt existing dry-run routes without effects: "+corruption, func() {
			md, err := partitionTestBuilder().SetStoreRecordVersions(true).Build()
			Expect(err).NotTo(HaveOccurred())
			root := specSubspace()
			pk := tuple.Tuple{md.GetRecordType("Order").GetRecordTypeKey(), int64(-2), int64(-11), int64(1)}
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store := partitionProtocolStore(rc, md, root)
				_, err := store.SaveRecord(partitionOrder(1, -11, 7))
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&demopb.Customer{CustomerId: proto.Int64(2), Name: proto.String("plain")})
				Expect(err).NotTo(HaveOccurred())
				ss := partitionSubspace(root, md.GetRecordType("Order"))
				switch corruption {
				case "header":
					rc.Transaction().Clear(ss.Pack(tuple.Tuple{"header"}))
				case "active", "fence":
					rc.Transaction().Clear(ss.Pack(tuple.Tuple{corruption, int64(-2)}))
				case "generation":
					rc.Transaction().Set(ss.Pack(tuple.Tuple{"active", int64(-2)}), tuple.Tuple{int64(2)}.Pack())
				default:
					var message proto.Message = partitionOrder(1, -12, 7)
					if corruption == "cross-interval body" {
						message = partitionOrder(1, 10, 7)
					}
					rt := md.GetRecordType("Order")
					if corruption == "wrong-type body" {
						message = &demopb.Customer{CustomerId: proto.Int64(1)}
						rt = md.GetRecordType("Customer")
					}
					data, err := serializeUnionOver(message, rt, nil)
					Expect(err).NotTo(HaveOccurred())
					rc.Transaction().Set(root.Sub(RecordKey).Pack(append(append(tuple.Tuple{}, pk...), int64(0))), data)
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			var before []fdb.KeyValue
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store := partitionProtocolStore(rc, md, root)
				before = partitionWholeStore(rc, store)
				effects := partitionVersionEffectsOf(rc)
				_, err := store.DryRunSaveRecord(partitionOrder(1, -11, 8), RecordExistenceCheckNone)
				var state *PartitionStateError
				Expect(errors.As(err, &state)).To(BeTrue(), "%v", err)
				found, err := store.DryRunDeleteRecord(pk)
				Expect(found).To(BeFalse())
				Expect(errors.As(err, &state)).To(BeTrue(), "%v", err)
				_, err = store.DryRunSaveRecord(partitionOrder(1, -11, 8), RecordExistenceCheckErrorIfExists)
				var exists *RecordAlreadyExistsError
				Expect(errors.As(err, &exists)).To(BeTrue(), "existence precedes consistency: %v", err)
				if corruption == "wrong-type body" {
					_, err = store.DryRunSaveRecord(partitionOrder(1, -11, 8), RecordExistenceCheckErrorIfNotExistsOrTypeChanged)
					var changed *RecordTypeChangedError
					Expect(errors.As(err, &changed)).To(BeTrue())
				}
				found, err = store.DeleteRecord(pk)
				Expect(found).To(BeFalse())
				Expect(errors.As(err, &state)).To(BeTrue(), "real deletion must reject the same loaded corruption: %v", err)
				plainPreview, err := store.DryRunSaveRecord(&demopb.Customer{CustomerId: proto.Int64(2), Name: proto.String("preview")}, RecordExistenceCheckNone)
				Expect(err).NotTo(HaveOccurred(), "plain siblings retain Java dry-run semantics")
				found, err = store.DryRunDeleteRecord(plainPreview.PrimaryKey)
				Expect(err).NotTo(HaveOccurred())
				Expect(found).To(BeTrue())
				Expect(partitionWholeStore(rc, store)).To(Equal(before))
				Expect(partitionVersionEffectsOf(rc)).To(Equal(effects))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				Expect(partitionWholeStore(rc, partitionProtocolStore(rc, md, root))).To(Equal(before))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}

	It("refuses a partition body planted under a plain sibling key before touching either type's indexes", func() {
		b := partitionTestBuilder().SetStoreRecordVersions(true)
		b.AddIndex("Order", NewIndex("cross_type_partition", Concat(PartitionExpr(Field("coord_x"), 10), Field("price"))))
		b.AddIndex("Customer", NewIndex("cross_type_plain", Field("name")))
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		pk := tuple.Tuple{md.GetRecordType("Customer").GetRecordTypeKey(), int64(1)}
		_, err = sharedDB.Run(context.Background(), func(rc *FDBRecordContext) (any, error) {
			store := partitionProtocolStore(rc, md, root)
			_, err := store.SaveRecord(&demopb.Customer{CustomerId: proto.Int64(1), Name: proto.String("plain")})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(partitionOrder(2, 10, 7))
			Expect(err).NotTo(HaveOccurred())
			data, err := serializeUnionOver(partitionOrder(1, -11, 7), md.GetRecordType("Order"), nil)
			Expect(err).NotTo(HaveOccurred())
			rc.Transaction().Set(root.Sub(RecordKey).Pack(append(append(tuple.Tuple{}, pk...), int64(0))), data)
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		var before []fdb.KeyValue
		_, err = sharedDB.Run(context.Background(), func(rc *FDBRecordContext) (any, error) {
			store := partitionProtocolStore(rc, md, root)
			before = partitionWholeStore(rc, store)
			effects := partitionVersionEffectsOf(rc)
			record := &demopb.Customer{CustomerId: proto.Int64(1), Name: proto.String("replacement")}
			for _, attempt := range []func() error{
				func() error { _, err := store.SaveRecord(record); return err },
				func() error { _, err := store.SaveRecordBatch([]proto.Message{record}); return err },
				func() error { _, err := store.DryRunSaveRecord(record, RecordExistenceCheckNone); return err },
				func() error { _, err := store.DeleteRecord(pk); return err },
				func() error { _, err := store.DryRunDeleteRecord(pk); return err },
			} {
				var state *PartitionStateError
				Expect(errors.As(attempt(), &state)).To(BeTrue())
				Expect(partitionWholeStore(rc, store)).To(Equal(before))
				Expect(partitionVersionEffectsOf(rc)).To(Equal(effects))
			}
			return before, nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(context.Background(), func(rc *FDBRecordContext) (any, error) {
			Expect(partitionWholeStore(rc, partitionProtocolStore(rc, md, root))).To(Equal(before))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("keeps serialization and deserialization errors ahead of dry-run consistency failures", func() {
		md, err := partitionTestBuilder().Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionProtocolStore(rc, md, root)
			_, err := store.SaveRecord(partitionOrder(1, -11, 7))
			Expect(err).NotTo(HaveOccurred())
			rc.Transaction().Clear(partitionSubspace(root, md.GetRecordType("Order")).Pack(tuple.Tuple{"active", int64(-2)}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionProtocolStore(rc, md, root)
			record := partitionOrder(1, -11, 8)
			record.Flower = &demopb.Flower{Type: proto.String(string([]byte{0xff}))}
			_, err := store.DryRunSaveRecord(record, RecordExistenceCheckNone)
			var serialization *RecordSerializationError
			Expect(errors.As(err, &serialization)).To(BeTrue(), "%v", err)
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionProtocolStore(rc, md, root)
			pk := tuple.Tuple{md.GetRecordType("Order").GetRecordTypeKey(), int64(-2), int64(-11), int64(1), int64(0)}
			rc.Transaction().Set(root.Sub(RecordKey).Pack(pk), []byte{0xff})
			_, err := store.DryRunSaveRecord(partitionOrder(1, -11, 8), RecordExistenceCheckErrorIfExists)
			var deserialization *RecordDeserializationError
			Expect(errors.As(err, &deserialization)).To(BeTrue(), "%v", err)
			_, err = store.DryRunDeleteRecord(pk[:4])
			Expect(errors.As(err, &deserialization)).To(BeTrue())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

type partitionIOCount struct {
	fdb.WritableTransaction
	gets, ranges int
}

func (tx *partitionIOCount) Get(key fdb.KeyConvertible) fdb.FutureByteSlice {
	tx.gets++
	return tx.WritableTransaction.Get(key)
}

func (tx *partitionIOCount) GetRange(r fdb.Range, options fdb.RangeOptions) fdb.RangeResult {
	tx.ranges++
	return tx.WritableTransaction.GetRange(r, options)
}

var _ = Describe("Partition reset complexity", func() {
	It("uses constant Get/GetRange work across many occupied intervals", func() {
		ctx := context.Background()
		md, err := partitionTestBuilder().Build()
		Expect(err).NotTo(HaveOccurred())
		baselineGets, baselineRanges := -1, -1
		for _, intervals := range []int{1, 32, 128} {
			root := specSubspace().Sub(fmt.Sprintf("intervals_%d", intervals))
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store := partitionProtocolStore(rc, md, root)
				for i := 0; i < intervals; i++ {
					_, err := store.SaveRecord(partitionOrder(int64(i+1), int64(10*i), 7))
					Expect(err).NotTo(HaveOccurred())
					_, err = store.SaveRecord(&demopb.TypedRecord{Id: proto.Int64(int64(i + 1)), ValInt64: proto.Int64(int64(-10*i - 1))})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store := partitionProtocolStore(rc, md, root)
				Expect(partitionProtocolRows(rc, root)).To(HaveLen(4 + 4*intervals))
				native := rc.tx
				counter := &partitionIOCount{WritableTransaction: native}
				rc.tx = counter
				resetErr := store.DeleteAllRecords()
				rc.tx = native
				Expect(resetErr).NotTo(HaveOccurred())
				Expect(counter.gets).To(BeNumerically(">", 0), "positive read population")
				if baselineGets < 0 {
					baselineGets, baselineRanges = counter.gets, counter.ranges
				}
				Expect(counter.gets).To(Equal(baselineGets))
				Expect(counter.ranges).To(Equal(baselineRanges))
				Expect(counter.ranges).To(Equal(0), "populated headers need no physical landmark scan")
				Expect(partitionProtocolRows(rc, root)).To(HaveLen(4))
				rows, err := AsList(ctx, store.ScanRecords(nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(rows).To(BeEmpty())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		}
	})
})
