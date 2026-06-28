package recordlayer

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

var _ = Describe("Store version access", func() {
	var (
		ctx context.Context
		md  *RecordMetaData
	)

	BeforeEach(func() {
		ctx = context.Background()
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		var err error
		md, err = builder.Build()
		Expect(err).NotTo(HaveOccurred())
	})

	Describe("GetFormatVersion", func() {
		It("returns the current format version", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				Expect(store.GetFormatVersion()).To(Equal(int32(formatVersionCurrent)))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("GetUserVersion / SetUserVersion", func() {
		It("defaults to 0", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				Expect(store.GetUserVersion()).To(Equal(int32(0)))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("persists across reopens", func() {
			ss := specSubspace()

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				return nil, store.SetUserVersion(42)
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				if err != nil {
					return nil, err
				}
				Expect(store.GetUserVersion()).To(Equal(int32(42)))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("GetMetaDataVersion", func() {
		It("returns the metadata version from the header", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				Expect(store.GetMetaDataVersion()).To(Equal(int32(md.Version())))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("packVersion / unpackVersion roundtrip", func() {
		It("roundtrips a complete version with local version 0", func() {
			globalVer := []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01}
			v, err := NewCompleteVersion(globalVer, 0)
			Expect(err).NotTo(HaveOccurred())

			packed, err := packVersion(v)
			Expect(err).NotTo(HaveOccurred())
			Expect(packed).NotTo(BeEmpty())

			roundtripped, err := unpackVersion(packed)
			Expect(err).NotTo(HaveOccurred())
			Expect(roundtripped.IsComplete()).To(BeTrue())
			Expect(roundtripped.GetLocalVersion()).To(Equal(0))

			gv, err := roundtripped.GetGlobalVersion()
			Expect(err).NotTo(HaveOccurred())
			Expect(gv).To(Equal(globalVer))
		})

		It("roundtrips a complete version with non-zero local version", func() {
			globalVer := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A}
			v, err := NewCompleteVersion(globalVer, 42)
			Expect(err).NotTo(HaveOccurred())

			packed, err := packVersion(v)
			Expect(err).NotTo(HaveOccurred())

			roundtripped, err := unpackVersion(packed)
			Expect(err).NotTo(HaveOccurred())
			Expect(roundtripped.Equal(v)).To(BeTrue())
		})

		It("roundtrips a version with max local version", func() {
			globalVer := []byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0x00, 0x11, 0x22, 0x33, 0x44}
			v, err := NewCompleteVersion(globalVer, 0xFFFF)
			Expect(err).NotTo(HaveOccurred())

			packed, err := packVersion(v)
			Expect(err).NotTo(HaveOccurred())

			roundtripped, err := unpackVersion(packed)
			Expect(err).NotTo(HaveOccurred())
			Expect(roundtripped.GetLocalVersion()).To(Equal(0xFFFF))
			Expect(roundtripped.Equal(v)).To(BeTrue())
		})

		It("packed value is a valid tuple with Versionstamp", func() {
			globalVer := []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01}
			v, err := NewCompleteVersion(globalVer, 5)
			Expect(err).NotTo(HaveOccurred())

			packed, err := packVersion(v)
			Expect(err).NotTo(HaveOccurred())

			// Verify it's a valid tuple containing a Versionstamp
			t, err := tuple.Unpack(packed)
			Expect(err).NotTo(HaveOccurred())
			Expect(t).To(HaveLen(1))
			vs, ok := t[0].(tuple.Versionstamp)
			Expect(ok).To(BeTrue())
			Expect(vs.TransactionVersion[:]).To(Equal(globalVer))
			Expect(vs.UserVersion).To(Equal(uint16(5)))
		})
	})

	Describe("unpackVersion error handling", func() {
		It("rejects empty input", func() {
			_, err := unpackVersion(nil)
			Expect(err).To(HaveOccurred())
		})

		It("rejects invalid tuple data", func() {
			_, err := unpackVersion([]byte{0xFF, 0xFE, 0xFD})
			Expect(err).To(HaveOccurred())
		})

		It("rejects tuple without Versionstamp", func() {
			packed := tuple.Tuple{int64(42)}.Pack()
			_, err := unpackVersion(packed)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not a Versionstamp"))
		})
	})

	Describe("LoadRecordVersion with complete versions", func() {
		var versionedMD *RecordMetaData

		BeforeEach(func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.SetStoreRecordVersions(true)
			var err error
			versionedMD, err = builder.Build()
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns nil for a non-existent record", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(versionedMD).SetSubspace(specSubspace()).
					CreateOrOpen()
				if err != nil {
					return nil, err
				}
				v, err := store.LoadRecordVersion(tuple.Tuple{int64(999)}, false)
				Expect(err).NotTo(HaveOccurred())
				Expect(v).To(BeNil())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("saves a record and loads its version after commit", func() {
			ss := specSubspace()

			// Save a record — creates an incomplete version
			_, _, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(versionedMD).SetSubspace(ss).
					CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// After commit, the version should be complete and loadable
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(versionedMD).SetSubspace(ss).
					Open()
				if err != nil {
					return nil, err
				}
				v, err := store.LoadRecordVersion(tuple.Tuple{int64(1)}, false)
				Expect(err).NotTo(HaveOccurred())
				Expect(v).NotTo(BeNil())
				Expect(v.IsComplete()).To(BeTrue())
				Expect(v.GetLocalVersion()).To(Equal(0))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("loads version with snapshot read", func() {
			ss := specSubspace()

			_, _, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(versionedMD).SetSubspace(ss).
					CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(versionedMD).SetSubspace(ss).
					Open()
				if err != nil {
					return nil, err
				}
				// Snapshot read should also work
				v, err := store.LoadRecordVersion(tuple.Tuple{int64(1)}, true)
				Expect(err).NotTo(HaveOccurred())
				Expect(v).NotTo(BeNil())
				Expect(v.IsComplete()).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns local version from cache for same-transaction save", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(versionedMD).SetSubspace(specSubspace()).
					CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				if err != nil {
					return nil, err
				}
				// Within the same transaction, should get incomplete version from local cache
				v, err := store.LoadRecordVersion(tuple.Tuple{int64(1)}, false)
				Expect(err).NotTo(HaveOccurred())
				Expect(v).NotTo(BeNil())
				Expect(v.IsComplete()).To(BeFalse())
				Expect(v.GetLocalVersion()).To(Equal(0))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("saveRecordVersion with complete version writes directly", func() {
			ss := specSubspace()

			globalVer := []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x42, 0x00, 0x00, 0x00, 0x01}
			completeVer, err := NewCompleteVersion(globalVer, 7)
			Expect(err).NotTo(HaveOccurred())

			// Create store, then directly call saveRecordVersion with a complete version.
			// Use a PK that has NO SaveRecord call — SaveRecord queues a
			// SET_VERSIONSTAMPED_VALUE mutation that would overwrite at commit.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(versionedMD).SetSubspace(ss).
					CreateOrOpen()
				if err != nil {
					return nil, err
				}
				si := &sizeInfo{}
				err = store.saveRecordVersion(tuple.Tuple{int64(99)}, completeVer, si)
				if err != nil {
					return nil, err
				}
				Expect(si.VersionedInline).To(BeTrue())
				Expect(si.KeyCount).To(Equal(1))
				Expect(si.KeySize).To(BeNumerically(">", 0))
				Expect(si.ValueSize).To(BeNumerically(">", 0))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify the complete version is stored and loadable
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(versionedMD).SetSubspace(ss).
					Open()
				if err != nil {
					return nil, err
				}
				v, err := store.LoadRecordVersion(tuple.Tuple{int64(99)}, false)
				Expect(err).NotTo(HaveOccurred())
				Expect(v).NotTo(BeNil())
				Expect(v.IsComplete()).To(BeTrue())
				Expect(v.GetLocalVersion()).To(Equal(7))
				gv, err := v.GetGlobalVersion()
				Expect(err).NotTo(HaveOccurred())
				Expect(gv).To(Equal(globalVer))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("saveRecordVersion with nil sizeInfo does not panic", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(versionedMD).SetSubspace(specSubspace()).
					CreateOrOpen()
				if err != nil {
					return nil, err
				}
				globalVer := []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01}
				v, err := NewCompleteVersion(globalVer, 0)
				if err != nil {
					return nil, err
				}
				// nil sizeInfo should not panic
				return nil, store.saveRecordVersion(tuple.Tuple{int64(1)}, v, nil)
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("assigns distinct local versions to multiple records", func() {
			ss := specSubspace()

			_, _, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(versionedMD).SetSubspace(ss).
					CreateOrOpen()
				if err != nil {
					return nil, err
				}
				for i := int64(0); i < 5; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i))})
					if err != nil {
						return nil, err
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// After commit, all should have the same global version but different local versions
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(versionedMD).SetSubspace(ss).
					Open()
				if err != nil {
					return nil, err
				}
				for i := int64(0); i < 5; i++ {
					v, err := store.LoadRecordVersion(tuple.Tuple{i}, false)
					Expect(err).NotTo(HaveOccurred())
					Expect(v).NotTo(BeNil())
					Expect(v.IsComplete()).To(BeTrue())
					Expect(v.GetLocalVersion()).To(Equal(int(i)))
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
