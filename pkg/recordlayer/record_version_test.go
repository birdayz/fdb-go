package recordlayer

import (
	"context"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
)

var _ = Describe("FDBRecordVersion", func() {
	It("IncompleteVersion", func() {
		v, err := IncompleteVersion(42)
		Expect(err).NotTo(HaveOccurred())
		Expect(v.IsComplete()).To(BeFalse())
		Expect(v.GetLocalVersion()).To(Equal(42))
		b := v.ToBytes()
		Expect(len(b)).To(Equal(VersionBytes))
		for i := 0; i < GlobalVersionBytes; i++ {
			Expect(b[i]).To(Equal(byte(0xFF)))
		}
	})

	It("CompleteVersion", func() {
		global := make([]byte, GlobalVersionBytes)
		global[0] = 0x01
		global[7] = 0x42
		v, err := NewCompleteVersion(global, 7)
		Expect(err).NotTo(HaveOccurred())
		Expect(v.IsComplete()).To(BeTrue())
		Expect(v.GetLocalVersion()).To(Equal(7))
		gv, err := v.GetGlobalVersion()
		Expect(err).NotTo(HaveOccurred())
		Expect(gv).To(Equal(global))
	})

	It("WithCommittedVersion", func() {
		incomplete, _ := IncompleteVersion(3)
		committed := make([]byte, GlobalVersionBytes)
		committed[7] = 0x99
		complete, err := incomplete.WithCommittedVersion(committed)
		Expect(err).NotTo(HaveOccurred())
		Expect(complete.IsComplete()).To(BeTrue())
		Expect(complete.GetLocalVersion()).To(Equal(3))
		gv, err := complete.GetGlobalVersion()
		Expect(err).NotTo(HaveOccurred())
		Expect(gv).To(Equal(committed))
	})

	It("InvalidInputs", func() {
		_, err := IncompleteVersion(-1)
		Expect(err).To(HaveOccurred())
		_, err = IncompleteVersion(0x10000)
		Expect(err).To(HaveOccurred())
		_, err = NewCompleteVersion([]byte{1, 2, 3}, 0)
		Expect(err).To(HaveOccurred())
	})

	It("RoundTrip", func() {
		original, _ := NewCompleteVersion(
			[]byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x01},
			256,
		)
		parsed, err := CompleteVersionFromBytes(original.ToBytes())
		Expect(err).NotTo(HaveOccurred())
		Expect(parsed.GetLocalVersion()).To(Equal(original.GetLocalVersion()))
		parsedGV, err := parsed.GetGlobalVersion()
		Expect(err).NotTo(HaveOccurred())
		originalGV, err := original.GetGlobalVersion()
		Expect(err).NotTo(HaveOccurred())
		Expect(parsedGV).To(Equal(originalGV))
	})
})

var _ = Describe("FDBRecordVersion Comparison", func() {
	makeComplete := func(globalBytes []byte, local int) *FDBRecordVersion {
		v, err := NewCompleteVersion(globalBytes, local)
		Expect(err).NotTo(HaveOccurred())
		return v
	}

	makeIncomplete := func(local int) *FDBRecordVersion {
		v, err := IncompleteVersion(local)
		Expect(err).NotTo(HaveOccurred())
		return v
	}

	zeroGlobal := make([]byte, GlobalVersionBytes)

	Describe("Equal", func() {
		It("EqualCompleteVersions", func() {
			g := make([]byte, GlobalVersionBytes)
			g[0] = 0x01
			g[7] = 0x42
			a := makeComplete(g, 5)
			b := makeComplete(g, 5)
			Expect(a.Equal(b)).To(BeTrue())
			Expect(b.Equal(a)).To(BeTrue())
		})

		It("EqualIncompleteVersions", func() {
			a := makeIncomplete(42)
			b := makeIncomplete(42)
			Expect(a.Equal(b)).To(BeTrue())
		})

		It("UnequalVersions", func() {
			g1 := make([]byte, GlobalVersionBytes)
			g1[0] = 0x01
			g2 := make([]byte, GlobalVersionBytes)
			g2[0] = 0x02
			a := makeComplete(g1, 0)
			b := makeComplete(g2, 0)
			Expect(a.Equal(b)).To(BeFalse())

			// Same global, different local
			c := makeComplete(g1, 1)
			Expect(a.Equal(c)).To(BeFalse())

			// Complete vs incomplete
			d := makeIncomplete(0)
			Expect(a.Equal(d)).To(BeFalse())

			// Different incomplete locals
			e := makeIncomplete(1)
			f := makeIncomplete(2)
			Expect(e.Equal(f)).To(BeFalse())
		})

		It("NilHandling", func() {
			a := makeComplete(zeroGlobal, 0)
			Expect(a.Equal(nil)).To(BeFalse())
			var nilV *FDBRecordVersion
			Expect(nilV.Equal(nil)).To(BeTrue())
			Expect(nilV.Equal(a)).To(BeFalse())
		})
	})

	Describe("Less", func() {
		It("CompleteSortsBeforeIncomplete", func() {
			complete := makeComplete(zeroGlobal, 0)
			incomplete := makeIncomplete(0)
			Expect(complete.Less(incomplete)).To(BeTrue())
			Expect(incomplete.Less(complete)).To(BeFalse())
		})

		It("LexicographicOrdering", func() {
			g1 := make([]byte, GlobalVersionBytes)
			g1[0] = 0x01
			g2 := make([]byte, GlobalVersionBytes)
			g2[0] = 0x02
			a := makeComplete(g1, 0)
			b := makeComplete(g2, 0)
			Expect(a.Less(b)).To(BeTrue())
			Expect(b.Less(a)).To(BeFalse())

			// Same global, different local
			c := makeComplete(g1, 1)
			d := makeComplete(g1, 2)
			Expect(c.Less(d)).To(BeTrue())
			Expect(d.Less(c)).To(BeFalse())

			// Equal versions: not less
			e := makeComplete(g1, 5)
			f := makeComplete(g1, 5)
			Expect(e.Less(f)).To(BeFalse())
		})

		It("IncompleteOrderingByLocalVersion", func() {
			a := makeIncomplete(1)
			b := makeIncomplete(2)
			// Incomplete versions have 0xFF global bytes, so ordering is by local version bytes
			Expect(a.Less(b)).To(BeTrue())
			Expect(b.Less(a)).To(BeFalse())
		})

		It("NilHandling", func() {
			a := makeComplete(zeroGlobal, 0)
			var nilV *FDBRecordVersion
			// nil < non-nil
			Expect(nilV.Less(a)).To(BeTrue())
			// non-nil not < nil
			Expect(a.Less(nil)).To(BeFalse())
			// nil not < nil
			Expect(nilV.Less(nil)).To(BeFalse())
		})
	})

	Describe("String", func() {
		It("CompleteVersion", func() {
			g := make([]byte, GlobalVersionBytes)
			g[0] = 0xAB
			v := makeComplete(g, 1)
			s := v.String()
			Expect(s).To(ContainSubstring("complete=true"))
			Expect(s).To(ContainSubstring("ab"))
		})

		It("IncompleteVersion", func() {
			v := makeIncomplete(0)
			s := v.String()
			Expect(s).To(ContainSubstring("complete=false"))
		})

		It("NilVersion", func() {
			var nilV *FDBRecordVersion
			Expect(nilV.String()).To(Equal("FDBRecordVersion(nil)"))
		})
	})
})

var _ = Describe("RecordVersioning", func() {
	ctx := context.Background()

	newMeta := func() *RecordMetaData {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.SetStoreRecordVersions(true)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}

	It("VersionStoredOnSave", func() {
		metaData := newMeta()
		ks := specSubspace()

		// Save with RunWithVersionstamp to get committed versionstamp
		_, vs, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			if err != nil {
				return nil, err
			}

			// Same tx: version should be incomplete (from cache)
			version, err := store.LoadRecordVersion(tuple.Tuple{int64(1)}, false)
			if err != nil {
				return nil, err
			}
			Expect(version).NotTo(BeNil())
			Expect(version.IsComplete()).To(BeFalse())
			Expect(version.GetLocalVersion()).To(Equal(0))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(vs).NotTo(BeNil())
		Expect(len(vs)).To(Equal(GlobalVersionBytes))

		// Read back in new transaction
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}
			version, err := store.LoadRecordVersion(tuple.Tuple{int64(1)}, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(version).NotTo(BeNil())
			Expect(version.IsComplete()).To(BeTrue())
			Expect(version.GetLocalVersion()).To(Equal(0))
			vGV, err := version.GetGlobalVersion()
			Expect(err).NotTo(HaveOccurred())
			Expect(vGV).To(Equal(vs))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("MultipleRecordsSequentialLocalVersions", func() {
		metaData := newMeta()
		ks := specSubspace()

		_, _, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			for i := int64(0); i < 5; i++ {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i))})
				if err != nil {
					return nil, err
				}
			}
			for i := int64(0); i < 5; i++ {
				v, err := store.LoadRecordVersion(tuple.Tuple{i}, false)
				Expect(err).NotTo(HaveOccurred())
				Expect(v).NotTo(BeNil())
				Expect(v.GetLocalVersion()).To(Equal(int(i)))
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("VersionClearedOnDelete", func() {
		metaData := newMeta()
		ks := specSubspace()

		// Save
		_, _, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		// Delete and verify
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}
			deleted, err := store.DeleteRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Verify version gone
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}
			v, err := store.LoadRecordVersion(tuple.Tuple{int64(1)}, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(v).To(BeNil())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("DeleteInSameTxCleansUpIncompleteVersionMutation", func() {
		metaData := newMeta()
		ks := specSubspace()

		// Save and delete in the SAME transaction.
		// The incomplete version mutation must be dequeued, otherwise it gets
		// flushed at commit and writes a version for a deleted record.
		_, _, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			if err != nil {
				return nil, err
			}
			// Delete in same transaction
			deleted, err := store.DeleteRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// Verify: no pending version mutations in context
			Expect(rtx.HasVersionMutations()).To(BeFalse())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Verify in a new transaction: no version key exists
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}
			v, err := store.LoadRecordVersion(tuple.Tuple{int64(1)}, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(v).To(BeNil())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("SaveRecord returns incomplete version on FDBStoredRecord", func() {
		metaData := newMeta()
		ks := specSubspace()

		_, _, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			stored, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			Expect(stored.HasVersion()).To(BeTrue())
			Expect(stored.Version.IsComplete()).To(BeFalse())
			Expect(stored.Version.GetLocalVersion()).To(Equal(0))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("LoadRecord returns complete version on FDBStoredRecord", func() {
		metaData := newMeta()
		ks := specSubspace()

		_, vs, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}
			loaded, err := store.LoadRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(loaded).NotTo(BeNil())
			Expect(loaded.HasVersion()).To(BeTrue())
			Expect(loaded.Version.IsComplete()).To(BeTrue())
			loadedGV, err := loaded.Version.GetGlobalVersion()
			Expect(err).NotTo(HaveOccurred())
			Expect(loadedGV).To(Equal(vs))
			Expect(loaded.Version.GetLocalVersion()).To(Equal(0))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("LoadRecord has no version when versioning disabled", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		metaData, buildErr := builder.Build()
		Expect(buildErr).NotTo(HaveOccurred())
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			stored, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			Expect(stored.HasVersion()).To(BeFalse())

			loaded, err := store.LoadRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(loaded.HasVersion()).To(BeFalse())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("VersionNotStoredWhenDisabled", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		metaData, buildErr := builder.Build()
		Expect(buildErr).NotTo(HaveOccurred())
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			if err != nil {
				return nil, err
			}
			v, err := store.LoadRecordVersion(tuple.Tuple{int64(1)}, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(v).To(BeNil())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("VersionsClearedByDeleteAllRecords", func() {
		metaData := newMeta()
		ks := specSubspace()

		// Save
		_, _, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			for i := int64(0); i < 3; i++ {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i))})
				if err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Delete all
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}
			if err := store.DeleteAllRecords(); err != nil {
				return nil, err
			}
			for i := int64(0); i < 3; i++ {
				v, err := store.LoadRecordVersion(tuple.Tuple{i}, false)
				Expect(err).NotTo(HaveOccurred())
				Expect(v).To(BeNil())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("UpdateRecordGetsNewVersion", func() {
		metaData := newMeta()
		ks := specSubspace()

		// Save v1
		_, vs1, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		// Save v2 (update)
		_, vs2, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(200)})
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		Expect(vs1).NotTo(Equal(vs2))

		// Read back — should have latest version
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}
			v, err := store.LoadRecordVersion(tuple.Tuple{int64(1)}, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(v).NotTo(BeNil())
			Expect(v.IsComplete()).To(BeTrue())
			vGV2, err := v.GetGlobalVersion()
			Expect(err).NotTo(HaveOccurred())
			Expect(vGV2).To(Equal(vs2))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
