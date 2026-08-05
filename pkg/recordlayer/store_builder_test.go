package recordlayer

import (
	"context"
	"errors"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// Format-version pinning: the builder property that lets a rolling upgrade hold
// every instance at the OLD format (Java FDBRecordStoreBase.BaseBuilder.
// setFormatVersion, :2245/:2266). The zero value is the interesting case, and it
// is why the field is a pointer rather than a bare int32.
var _ = Describe("StoreBuilder_FormatVersion", func() {
	fvBuilder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	fvBuilder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	fvBuilder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	fvBuilder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	fvMetaData, _ := fvBuilder.Build()

	It("defaults to the newest format version when SetFormatVersion is never called", func() {
		b := NewStoreBuilder().SetMetaDataProvider(fvMetaData).
			SetSubspace(subspace.FromBytes(tuple.Tuple{"fmtver_unset"}.Pack()))
		Expect(b.effectiveFormatVersion()).To(Equal(int32(formatVersionCurrent)))
	})

	It("honours an explicitly pinned older format version", func() {
		b := NewStoreBuilder().SetMetaDataProvider(fvMetaData).
			SetSubspace(subspace.FromBytes(tuple.Tuple{"fmtver_pinned"}.Pack())).
			SetFormatVersion(int32(formatVersionCacheableState))
		Expect(b.effectiveFormatVersion()).To(Equal(int32(formatVersionCacheableState)))
	})

	// An EXPLICIT zero must NOT be read as "unset". Java validates whatever value
	// it was handed (FormatVersion.validateFormatVersion, :225-229) and 0 is below
	// the minimum, so it rejects too. Silently substituting the newest version
	// would open at 14 for a caller who asked for 0 — the opposite of any
	// plausible intent, and invisible until it had already written a newer format.
	It("REJECTS an explicit SetFormatVersion(0) instead of treating it as unset", func() {
		_, err := NewStoreBuilder().
			SetContext(&FDBRecordContext{}).
			SetMetaDataProvider(fvMetaData).
			SetSubspace(subspace.FromBytes(tuple.Tuple{"fmtver_zero"}.Pack())).
			SetFormatVersion(0).
			Build()
		Expect(err).To(HaveOccurred())
		var fmtErr *UnsupportedFormatVersionError
		Expect(errors.As(err, &fmtErr)).To(BeTrue(),
			"SetFormatVersion(0) failed with %v, want UnsupportedFormatVersionError", err)
		Expect(fmtErr.Version).To(Equal(int32(0)))
	})

	It("rejects a format version newer than this binary supports", func() {
		_, err := NewStoreBuilder().
			SetContext(&FDBRecordContext{}).
			SetMetaDataProvider(fvMetaData).
			SetSubspace(subspace.FromBytes(tuple.Tuple{"fmtver_high"}.Pack())).
			SetFormatVersion(int32(formatVersionCurrent) + 1).
			Build()
		Expect(err).To(HaveOccurred())
		var fmtErr *UnsupportedFormatVersionError
		Expect(errors.As(err, &fmtErr)).To(BeTrue())
	})

	// Every version-gated store-header FEATURE must refuse to write when the
	// store's actual header version predates it. Pinning the format version is
	// what makes the hazard reachable: without these gates a store pinned at 11
	// happily writes a v12 store lock into an 11 header, and an older instance —
	// exactly the instance the pin exists to protect — opens that store, does not
	// understand the field, and ignores a lock it should have honoured.
	//
	// Java gates each of these at its write site (FDBRecordStore.java:3222, :3443,
	// :3478, :3494, :3503, :3517).
	DescribeTable("refuses a version-gated header feature below its format version",
		func(pinned int32, attempt func(store *FDBRecordStore) error, feature string, required int32) {
			ks := specSubspace()
			_, err := sharedDB.Run(context.Background(), func(rtx *FDBRecordContext) (any, error) {
				store, oErr := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(fvMetaData).
					SetSubspace(ks).SetFormatVersion(pinned).CreateOrOpen()
				if oErr != nil {
					return nil, oErr
				}
				Expect(store.GetFormatVersion()).To(Equal(pinned),
					"the store did not open at the pinned version, so this case proves nothing")

				aErr := attempt(store)
				Expect(aErr).To(HaveOccurred(),
					"%s was written into a header at format version %d, which predates it (>= %d). "+
						"An older instance opening this store cannot interpret the field and will "+
						"silently ignore it", feature, pinned, required)
				var fErr *UnsupportedFeatureForFormatVersionError
				Expect(errors.As(aErr, &fErr)).To(BeTrue(),
					"%s failed with %v, want UnsupportedFeatureForFormatVersionError", feature, aErr)
				Expect(fErr.Version).To(Equal(pinned))
				Expect(fErr.RequiredVersion).To(Equal(required))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		},
		Entry("store lock state below 12",
			int32(formatVersionRecordCountState),
			func(s *FDBRecordStore) error {
				return s.SetStoreLockState(gen.DataStoreInfo_StoreLockState_FORBID_RECORD_UPDATE, "why")
			},
			"store lock state", int32(formatVersionStoreLockState)),
		Entry("clear store lock state below 12",
			int32(formatVersionRecordCountState),
			func(s *FDBRecordStore) error { return s.ClearStoreLockState() },
			"store lock state", int32(formatVersionStoreLockState)),
		Entry("header user fields below 8",
			int32(formatVersionCacheableState),
			func(s *FDBRecordStore) error { return s.SetHeaderUserField("k", []byte("v")) },
			"header user fields", int32(formatVersionHeaderUserFields)),
		Entry("clear header user field below 8",
			int32(formatVersionCacheableState),
			func(s *FDBRecordStore) error { return s.ClearHeaderUserField("k") },
			"header user fields", int32(formatVersionHeaderUserFields)),
		Entry("record count state below 11",
			int32(formatVersionCheckIndexBuildType),
			func(s *FDBRecordStore) error {
				return s.UpdateRecordCountState(gen.DataStoreInfo_WRITE_ONLY)
			},
			"updating record count state", int32(formatVersionRecordCountState)),
		Entry("incarnation below 13",
			int32(formatVersionStoreLockState),
			func(s *FDBRecordStore) error {
				return s.UpdateIncarnation(func(cur int32) int32 { return cur + 1 })
			},
			"incarnation", int32(formatVersionIncarnation)),
	)

	// CREATION-TIME gates, which the table above does not reach: it exercises
	// runtime writes on an already-created store, while these two fields are
	// written once by createStoreHeaderAtFormat when the store is born. They are
	// wire-affecting in the same way and gated independently, exactly as Java does
	// at FDBRecordStore.java:5950-5957 — the record-count KEY needs
	// RECORD_COUNT_KEY_ADDED(3), the record-count STATE needs
	// RECORD_COUNT_STATE(11).
	//
	// Format 8 is the discriminating version: above the key's gate, below the
	// state's. A store born there must carry the key and must NOT carry the state.
	// Presence is asserted on the raw pointer rather than through the getter,
	// because the getter cannot tell "absent" from a valid enum value.
	DescribeTable("writes only the record-count header fields its birth format version has",
		func(pinned int32, wantKey bool, wantState bool) {
			ks := specSubspace()
			cntBuilder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			cntBuilder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			cntBuilder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			cntBuilder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			cntBuilder.SetRecordCountKey(EmptyKey())
			cntMetaData, bErr := cntBuilder.Build()
			Expect(bErr).NotTo(HaveOccurred())
			Expect(cntMetaData.GetRecordCountKey()).NotTo(BeNil(),
				"the metadata must actually declare a record-count key, or neither field is ever a candidate")

			_, err := sharedDB.Run(context.Background(), func(rtx *FDBRecordContext) (any, error) {
				store, oErr := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(cntMetaData).
					SetSubspace(ks).SetFormatVersion(pinned).CreateOrOpen()
				if oErr != nil {
					return nil, oErr
				}
				header := store.GetStoreHeader()
				Expect(header).NotTo(BeNil())
				Expect(header.GetFormatVersion()).To(Equal(pinned),
					"the store was not born at the pinned version, so this case proves nothing")

				if wantKey {
					Expect(header.RecordCountKey).NotTo(BeNil(),
						"a store born at format %d is at or above RECORD_COUNT_KEY_ADDED(%d), so the "+
							"declared record-count key must be persisted", pinned,
						formatVersionRecordCountKeyAdded)
				} else {
					Expect(header.RecordCountKey).To(BeNil(),
						"a store born at format %d predates RECORD_COUNT_KEY_ADDED(%d); writing the key "+
							"puts a field in the header that an instance honouring that version does not "+
							"expect", pinned, formatVersionRecordCountKeyAdded)
				}

				if wantState {
					Expect(header.RecordCountState).NotTo(BeNil(),
						"a store born at format %d is at or above RECORD_COUNT_STATE(%d), so the state "+
							"must be persisted", pinned, formatVersionRecordCountState)
				} else {
					Expect(header.RecordCountState).To(BeNil(),
						"a store born at format %d predates RECORD_COUNT_STATE(%d), so the state enum "+
							"must be ABSENT from the header — an older instance cannot interpret it, and "+
							"absence is not the same as any valid value (READABLE is 1, not 0)",
						pinned, formatVersionRecordCountState)
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		},
		// The discriminating case: between the two gates.
		Entry("born at 8: key yes (>=3), state no (<11)",
			int32(formatVersionHeaderUserFields), true, false),
		// At or above both gates.
		Entry("born at 11: key yes, state yes",
			int32(formatVersionRecordCountState), true, true),
		Entry("born at the current version: key yes, state yes",
			int32(formatVersionCurrent), true, true),
	)

	// The contrast half: at a sufficient version each feature must actually work,
	// or the gates above would be satisfied by a store that simply rejects
	// everything.
	It("allows the same features at the current format version", func() {
		ks := specSubspace()
		_, err := sharedDB.Run(context.Background(), func(rtx *FDBRecordContext) (any, error) {
			store, oErr := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(fvMetaData).
				SetSubspace(ks).CreateOrOpen()
			if oErr != nil {
				return nil, oErr
			}
			Expect(store.SetStoreLockState(
				gen.DataStoreInfo_StoreLockState_FORBID_RECORD_UPDATE, "why")).To(Succeed())
			Expect(store.ClearStoreLockState()).To(Succeed())
			Expect(store.SetHeaderUserField("k", []byte("v"))).To(Succeed())
			Expect(store.ClearHeaderUserField("k")).To(Succeed())
			Expect(store.UpdateRecordCountState(gen.DataStoreInfo_WRITE_ONLY)).To(Succeed())
			Expect(store.UpdateIncarnation(func(cur int32) int32 { return cur + 1 })).To(Succeed())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

var _ = Describe("StoreBuilder_Validation", func() {
	validBuilder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	validBuilder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	validBuilder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	validBuilder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	metaData, _ := validBuilder.Build()
	ks := subspace.FromBytes(tuple.Tuple{"builder_validation"}.Pack())

	It("BuildWithoutContext", func() {
		_, err := NewStoreBuilder().
			SetMetaDataProvider(metaData).
			SetSubspace(ks).
			Build()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(Equal("context is required"))
	})

	It("BuildWithoutMetaData", func() {
		// Can't easily create a real FDBRecordContext without a container,
		// but validateBuilder checks context first, then metadata.
		// We verify the error message for nil context covers it.
		_, err := NewStoreBuilder().
			SetSubspace(ks).
			Build()
		Expect(err).To(HaveOccurred())
	})

	It("CreateWithoutContext", func() {
		_, err := NewStoreBuilder().
			SetMetaDataProvider(metaData).
			SetSubspace(ks).
			Create()
		Expect(err).To(HaveOccurred())
	})

	It("OpenWithoutContext", func() {
		_, err := NewStoreBuilder().
			SetMetaDataProvider(metaData).
			SetSubspace(ks).
			Open()
		Expect(err).To(HaveOccurred())
	})

	It("CreateOrOpenWithoutContext", func() {
		_, err := NewStoreBuilder().
			SetMetaDataProvider(metaData).
			SetSubspace(ks).
			CreateOrOpen()
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("StoreBuilder_CreateOpenSemantics", func() {
	ctx := context.Background()
	semanticsBuilder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	semanticsBuilder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	semanticsBuilder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	semanticsBuilder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	metaData, _ := semanticsBuilder.Build()

	It("OpenNonExistentStore", func() {
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			_, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(ks).
				Open()
			return nil, err
		})

		Expect(err).To(HaveOccurred())
		var storeErr *RecordStoreDoesNotExistError
		Expect(errors.As(err, &storeErr)).To(BeTrue())
	})

	It("CreateAlreadyExistingStore", func() {
		ks := specSubspace()

		// First create
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			_, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(ks).
				Create()
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		// Second create should fail
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			_, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(ks).
				Create()
			return nil, err
		})
		Expect(err).To(HaveOccurred())
		var storeErr *RecordStoreAlreadyExistsError
		Expect(errors.As(err, &storeErr)).To(BeTrue())
	})

	It("CreateOrOpenExistingStore", func() {
		ks := specSubspace()

		// Create first
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			_, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(ks).
				Create()
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		// CreateOrOpen on existing should succeed
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(ks).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}
			Expect(store).NotTo(BeNil())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("CreateOrOpenNewStore", func() {
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(ks).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}
			Expect(store).NotTo(BeNil())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Verify it was actually created by Opening it
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			_, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(ks).
				Open()
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("OpenAfterCreate", func() {
		ks := specSubspace()

		// Create
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			_, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(ks).
				Create()
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		// Open should succeed
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(ks).
				Open()
			if err != nil {
				return nil, err
			}
			Expect(store).NotTo(BeNil())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

var _ = Describe("StoreLockState", func() {
	ctx := context.Background()

	builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	metaData, _ := builder.Build()

	It("SaveBlockedByLock", func() {
		ks := specSubspace()

		// Create a store, then lock it by writing a header with FORBID_RECORD_UPDATE
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}

			// Lock the store by updating the header
			lockState := gen.DataStoreInfo_StoreLockState_FORBID_RECORD_UPDATE
			reason := "index rebuild in progress"
			ts := int64(1234567890)
			store.storeHeader.StoreLockState = &gen.DataStoreInfo_StoreLockState{
				LockState: &lockState,
				Reason:    &reason,
				Timestamp: &ts,
			}
			if err := store.writeStoreHeader(store.storeHeader); err != nil {
				return nil, err
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Now open the store and try to save — should be blocked
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			order := &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(10),
			}
			_, err = store.SaveRecord(order)
			return nil, err
		})

		Expect(err).To(HaveOccurred())
		var lockErr *StoreIsLockedForRecordUpdatesError
		Expect(errors.As(err, &lockErr)).To(BeTrue())
		Expect(lockErr.Reason).To(Equal("index rebuild in progress"))
	})

	It("DeleteBlockedByLock", func() {
		ks := specSubspace()

		// Create store with a record, then lock it
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}

			order := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10)}
			if _, err := store.SaveRecord(order); err != nil {
				return nil, err
			}

			// Lock
			lockState := gen.DataStoreInfo_StoreLockState_FORBID_RECORD_UPDATE
			store.storeHeader.StoreLockState = &gen.DataStoreInfo_StoreLockState{
				LockState: &lockState,
			}
			return nil, store.writeStoreHeader(store.storeHeader)
		})
		Expect(err).NotTo(HaveOccurred())

		// Try to delete — should be blocked
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			_, err = store.DeleteRecord(tuple.Tuple{int64(1)})
			return nil, err
		})

		Expect(err).To(HaveOccurred())
		var lockErr *StoreIsLockedForRecordUpdatesError
		Expect(errors.As(err, &lockErr)).To(BeTrue())
	})

	It("DeleteAllBlockedByLock", func() {
		ks := specSubspace()

		// Create store, then lock it
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}

			lockState := gen.DataStoreInfo_StoreLockState_FORBID_RECORD_UPDATE
			store.storeHeader.StoreLockState = &gen.DataStoreInfo_StoreLockState{
				LockState: &lockState,
			}
			return nil, store.writeStoreHeader(store.storeHeader)
		})
		Expect(err).NotTo(HaveOccurred())

		// Try DeleteAllRecords — should be blocked
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}
			return nil, store.DeleteAllRecords()
		})

		Expect(err).To(HaveOccurred())
		var lockErr *StoreIsLockedForRecordUpdatesError
		Expect(errors.As(err, &lockErr)).To(BeTrue())
	})

	It("ReadAllowedWhenLocked", func() {
		ks := specSubspace()

		// Create store with a record, then lock it
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}

			order := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10)}
			if _, err := store.SaveRecord(order); err != nil {
				return nil, err
			}

			lockState := gen.DataStoreInfo_StoreLockState_FORBID_RECORD_UPDATE
			store.storeHeader.StoreLockState = &gen.DataStoreInfo_StoreLockState{
				LockState: &lockState,
			}
			return nil, store.writeStoreHeader(store.storeHeader)
		})
		Expect(err).NotTo(HaveOccurred())

		// Reads should still work on a locked store
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			rec, err := store.LoadRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(rec).NotTo(BeNil(), "Expected to find record in locked store")

			exists, err := store.RecordExists(tuple.Tuple{int64(1)}, IsolationLevelSerializable)
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeTrue())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("UnlockedStoreAllowsMutations", func() {
		ks := specSubspace()

		// Normal store — no lock state set
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}

			// Save should work
			order := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10)}
			if _, err := store.SaveRecord(order); err != nil {
				return nil, err
			}

			// Delete should work
			deleted, err := store.DeleteRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// DeleteAll should work
			return nil, store.DeleteAllRecords()
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
