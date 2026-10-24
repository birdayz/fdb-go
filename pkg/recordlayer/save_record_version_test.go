package recordlayer

import (
	"context"
	"errors"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// Explicit-version saves follow Java's FDBRecordStore.recordVersionForSave
// (FDBRecordStore.java:621-633) for every VersionstampSaveBehavior.
var _ = Describe("SaveRecordWithVersion", func() {
	ctx := context.Background()

	metaData := func(storeVersions bool, indexes ...*Index) *RecordMetaData {
		builder := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.SetStoreRecordVersions(storeVersions)
		for _, idx := range indexes {
			builder.AddIndex("Order", idx)
		}
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}
	complete := func(b byte) *FDBRecordVersion {
		v, err := CompleteVersionFromBytes([]byte{0, 0, 0, 0, 0, 0, 0, 1, 0, b, 0, 7})
		Expect(err).NotTo(HaveOccurred())
		return v
	}
	order := func(id int64) *demopb.Order {
		return &demopb.Order{OrderId: proto.Int64(id), Price: proto.Int32(int32(id))}
	}
	run := func(md *RecordMetaData, ks subspace.Subspace, format int32, fn func(store *FDBRecordStore)) {
		GinkgoHelper()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			b := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks)
			if format != 0 {
				b.SetFormatVersion(format)
			}
			store, err := b.CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			fn(store)
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	}
	loadedVersion := func(store *FDBRecordStore, id int64) *FDBRecordVersion {
		GinkgoHelper()
		rec, err := store.LoadRecord(tuple.Tuple{id})
		Expect(err).NotTo(HaveOccurred())
		Expect(rec).NotTo(BeNil())
		return rec.Version
	}

	// want: "none", "assigned" (an incomplete version, complete after commit),
	// or "supplied" (exactly the supplied version).
	type saveCase struct {
		storeVersions bool
		behavior      VersionstampSaveBehavior
		supplied      bool
		want          string
	}
	cases := []saveCase{
		{true, VersionstampSaveBehaviorDefault, false, "assigned"},
		{false, VersionstampSaveBehaviorDefault, false, "none"},
		{true, VersionstampSaveBehaviorDefault, true, "supplied"},
		{false, VersionstampSaveBehaviorDefault, true, "supplied"},
		{true, VersionstampSaveBehaviorNoVersion, false, "none"},
		{false, VersionstampSaveBehaviorNoVersion, false, "none"},
		{true, VersionstampSaveBehaviorWithVersion, false, "assigned"},
		{false, VersionstampSaveBehaviorWithVersion, false, "assigned"},
		{true, VersionstampSaveBehaviorWithVersion, true, "supplied"},
		{false, VersionstampSaveBehaviorWithVersion, true, "supplied"},
		{true, VersionstampSaveBehaviorIfPresent, false, "none"},
		{false, VersionstampSaveBehaviorIfPresent, false, "none"},
		{true, VersionstampSaveBehaviorIfPresent, true, "supplied"},
		{false, VersionstampSaveBehaviorIfPresent, true, "supplied"},
	}
	for _, format := range []int32{0, formatVersionSaveUnsplitWithSuffix} {
		for _, c := range cases {
			It(describeSaveCase(format, c.storeVersions, c.behavior, c.supplied), func() {
				md := metaData(c.storeVersions)
				ks := specSubspace()
				var supplied *FDBRecordVersion
				if c.supplied {
					supplied = complete(3)
				}
				// Java reads no legacy-format version of a store whose metadata
				// stores none (loadRecordVersionAsync), though the save wrote it.
				readBack := c.want
				if format != 0 && !c.storeVersions {
					readBack = "none"
				}
				run(md, ks, format, func(store *FDBRecordStore) {
					saved, err := store.SaveRecordWithVersion(order(1), RecordExistenceCheckNone, supplied, c.behavior)
					Expect(err).NotTo(HaveOccurred())
					switch c.want {
					case "none":
						Expect(saved.Version).To(BeNil())
					case "assigned":
						Expect(saved.Version).NotTo(BeNil())
						Expect(saved.Version.IsComplete()).To(BeFalse())
						if readBack != "none" {
							Expect(loadedVersion(store, 1)).To(Equal(saved.Version), "read back in the saving transaction")
						}
					case "supplied":
						Expect(saved.Version).To(Equal(supplied))
					}
				})
				run(md, ks, format, func(store *FDBRecordStore) {
					v := loadedVersion(store, 1)
					switch readBack {
					case "none":
						Expect(v).To(BeNil())
					case "assigned":
						Expect(v).NotTo(BeNil())
						Expect(v.IsComplete()).To(BeTrue())
					case "supplied":
						Expect(v).To(Equal(supplied))
					}
				})
			})
		}
	}

	It("stores a supplied incomplete version, completed at commit", func() {
		md := metaData(false)
		ks := specSubspace()
		var supplied *FDBRecordVersion
		run(md, ks, 0, func(store *FDBRecordStore) {
			var err error
			supplied, err = IncompleteVersion(store.context.ClaimLocalVersion())
			Expect(err).NotTo(HaveOccurred())
			saved, err := store.SaveRecordWithVersion(order(1), RecordExistenceCheckNone, supplied, VersionstampSaveBehaviorDefault)
			Expect(err).NotTo(HaveOccurred())
			Expect(saved.Version).To(Equal(supplied))
		})
		run(md, ks, 0, func(store *FDBRecordStore) {
			v := loadedVersion(store, 1)
			Expect(v.IsComplete()).To(BeTrue())
			Expect(v.GetLocalVersion()).To(Equal(supplied.GetLocalVersion()))
		})
	})

	It("refuses a version with NO_VERSION before the existence check", func() {
		run(metaData(true), specSubspace(), 0, func(store *FDBRecordStore) {
			_, err := store.SaveRecordWithVersion(order(1), RecordExistenceCheckErrorIfNotExists, complete(3), VersionstampSaveBehaviorNoVersion)
			var coreErr *RecordCoreError
			Expect(errors.As(err, &coreErr)).To(BeTrue(), "%v", err)
			Expect(coreErr.Message).To(Equal(`Nonnull version supplied with a NO_VERSION behavior: FDBRecordVersion(\x00\x00\x00\x00\x00\x00\x00\x01\x00\x03\x00\x07)`))
		})
	})

	It("refuses a version with NO_VERSION before the primary key's collation check", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(FunctionExpr(CollateFuncJRE, Field("name")))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		run(md, specSubspace(), 0, func(store *FDBRecordStore) {
			customer := &demopb.Customer{CustomerId: proto.Int64(1), Name: proto.String("n")}
			_, err := store.SaveRecord(customer)
			var collation *GoOnlyCollationError
			Expect(errors.As(err, &collation)).To(BeTrue(), "the primary key's own refusal: %v", err)
			_, err = store.SaveRecordWithVersion(customer, RecordExistenceCheckNone, complete(3), VersionstampSaveBehaviorNoVersion)
			Expect(errors.As(err, &collation)).To(BeFalse(), "%v", err)
			var coreErr *RecordCoreError
			Expect(errors.As(err, &coreErr)).To(BeTrue(), "%v", err)
			Expect(coreErr.Message).To(HavePrefix("Nonnull version supplied with a NO_VERSION behavior"))
		})
	})

	It("refuses an unknown behavior without writing", func() {
		md := metaData(true)
		ks := specSubspace()
		run(md, ks, 0, func(store *FDBRecordStore) {
			_, err := store.SaveRecordWithVersion(order(1), RecordExistenceCheckNone, nil, VersionstampSaveBehavior(99))
			var argErr *IllegalArgumentError
			Expect(errors.As(err, &argErr)).To(BeTrue(), "%v", err)
			rec, err := store.LoadRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(rec).To(BeNil())
		})
	})

	It("clears the stored version when a re-save stores none", func() {
		md := metaData(true)
		ks := specSubspace()
		run(md, ks, 0, func(store *FDBRecordStore) {
			_, err := store.SaveRecordWithVersion(order(1), RecordExistenceCheckNone, complete(3), VersionstampSaveBehaviorDefault)
			Expect(err).NotTo(HaveOccurred())
		})
		run(md, ks, 0, func(store *FDBRecordStore) {
			_, err := store.SaveRecordWithVersion(order(1), RecordExistenceCheckNone, nil, VersionstampSaveBehaviorNoVersion)
			Expect(err).NotTo(HaveOccurred())
		})
		run(md, ks, 0, func(store *FDBRecordStore) {
			Expect(loadedVersion(store, 1)).To(BeNil())
		})
	})

	It("maintains a VERSION index with the supplied complete version", func() {
		idx := NewVersionIndex("order$version", VersionKey())
		md := metaData(true, idx)
		ks := specSubspace()
		v := complete(9)
		run(md, ks, 0, func(store *FDBRecordStore) {
			_, err := store.SaveRecordWithVersion(order(1), RecordExistenceCheckNone, v, VersionstampSaveBehaviorIfPresent)
			Expect(err).NotTo(HaveOccurred())
		})
		run(md, ks, 0, func(store *FDBRecordStore) {
			entries, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			stamp, err := v.ToVersionstamp()
			Expect(err).NotTo(HaveOccurred())
			Expect(entries[0].Key).To(Equal(tuple.Tuple{stamp, int64(1)}))
		})
	})

	It("honors the behavior on the override-lock and typed saves and keeps the default on the plain, pipelined and batch saves", func() {
		md := metaData(true)
		ks := specSubspace()
		v := complete(4)
		run(md, ks, 0, func(store *FDBRecordStore) {
			saved, err := store.OverrideLockSaveRecordWithVersion(order(1), RecordExistenceCheckNone, v, VersionstampSaveBehaviorIfPresent)
			Expect(err).NotTo(HaveOccurred())
			Expect(saved.Version).To(Equal(v))
			typed, err := NewTypedRecordStore[*demopb.Order](store, md.GetRecordType("Order"))
			Expect(err).NotTo(HaveOccurred())
			tsaved, err := typed.SaveRecordWithVersion(order(2), RecordExistenceCheckNone, nil, VersionstampSaveBehaviorNoVersion)
			Expect(err).NotTo(HaveOccurred())
			Expect(tsaved.Version).To(BeNil())
			for _, saveDefault := range []func() (*FDBStoredRecord[proto.Message], error){
				func() (*FDBStoredRecord[proto.Message], error) {
					return store.OverrideLockSaveRecord(order(3), RecordExistenceCheckNone)
				},
				func() (*FDBStoredRecord[proto.Message], error) {
					saved, err := store.SaveRecordsPipelined([]proto.Message{order(4)}, RecordExistenceCheckNone)
					if err != nil {
						return nil, err
					}
					return saved[0], nil
				},
				func() (*FDBStoredRecord[proto.Message], error) {
					saved, err := store.SaveRecordBatch([]proto.Message{order(5)})
					if err != nil {
						return nil, err
					}
					return saved[0], nil
				},
			} {
				saved, err := saveDefault()
				Expect(err).NotTo(HaveOccurred())
				Expect(saved.Version).NotTo(BeNil())
				Expect(saved.Version.IsComplete()).To(BeFalse())
			}
		})
		run(md, ks, 0, func(store *FDBRecordStore) {
			Expect(loadedVersion(store, 1)).To(Equal(v))
			Expect(loadedVersion(store, 2)).To(BeNil())
			for _, id := range []int64{3, 4, 5} {
				Expect(loadedVersion(store, id).IsComplete()).To(BeTrue())
			}
		})
	})
})

func describeSaveCase(format int32, storeVersions bool, behavior VersionstampSaveBehavior, supplied bool) string {
	name := behavior.String()
	versions := "unversioned metadata"
	if storeVersions {
		versions = "versioned metadata"
	}
	version := "no version"
	if supplied {
		version = "a complete version"
	}
	f := "default format"
	if format != 0 {
		f = "legacy format"
	}
	return name + " with " + version + " on " + versions + ", " + f
}
