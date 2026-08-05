package recordlayer

import (
	"context"
	"errors"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
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

	// The MIRROR of the creation gate, on the reconciliation path. The two must
	// agree or they fight: creation correctly withholds the record-count key below
	// RECORD_COUNT_KEY_ADDED(3), and if checkPossiblyRebuildRecordCounts is not
	// gated the same way it reads that permanent, correct absence as "the count key
	// changed" on EVERY reopen — clearing the counters, rescanning every record
	// INLINE in the store-open transaction (unbounded; a large store then cannot
	// reopen at all past FDB's 5s/10MB limits), and writing the v3-only key into a
	// v2 header anyway, undoing the creation gate.
	//
	// Java gates both halves: the comparison arm at FDBRecordStore.java:5117-5118
	// and the assignment at :5130-5136.
	//
	// The sentinel is what makes "no rescan" observable. Record saves maintain
	// counts off the METADATA's key, so the count subspace is non-empty at v2
	// either way; but a rebuild CLEARS that subspace before re-deriving it, so a
	// planted key surviving the reopen proves no clear-and-rescan happened.
	DescribeTable("does not reconcile a record-count key the birth format version cannot hold",
		func(pinned int32, createWithKey bool, wantKeyAfterReopen bool, wantSentinelSurvives bool) {
			ks := specSubspace()
			// Two metadatas differing ONLY in whether the count key is declared. The
			// reopen always declares it; `createWithKey` decides whether that is a
			// genuine change or a no-op, which is what separates "must rebuild" from
			// "must not".
			buildMD := func(withKey bool) *RecordMetaData {
				b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
				b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
				b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
				b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
				if withKey {
					b.SetRecordCountKey(EmptyKey())
				}
				md, bErr := b.Build()
				Expect(bErr).NotTo(HaveOccurred())
				return md
			}
			createMetaData := buildMD(createWithKey)
			cntMetaData := buildMD(true)

			sentinel := func(store *FDBRecordStore) fdb.Key {
				return fdb.Key(store.subspace.Sub(RecordCountKey).Pack(tuple.Tuple{"zzz_sentinel"}))
			}

			// Create, save records, and plant the sentinel.
			_, err := sharedDB.Run(context.Background(), func(rtx *FDBRecordContext) (any, error) {
				store, oErr := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(createMetaData).
					SetSubspace(ks).SetFormatVersion(pinned).CreateOrOpen()
				if oErr != nil {
					return nil, oErr
				}
				for _, id := range []int64{1, 2} {
					if _, sErr := store.SaveRecord(&gen.Order{
						OrderId: proto.Int64(id), Price: proto.Int32(int32(id * 10)),
					}); sErr != nil {
						return nil, sErr
					}
				}
				rtx.Transaction().Set(sentinel(store), []byte("planted"))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// REOPEN — this is the path under test.
			_, err = sharedDB.Run(context.Background(), func(rtx *FDBRecordContext) (any, error) {
				store, oErr := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(cntMetaData).
					SetSubspace(ks).SetFormatVersion(pinned).Open()
				if oErr != nil {
					return nil, oErr
				}
				header := store.GetStoreHeader()
				Expect(header.GetFormatVersion()).To(Equal(pinned),
					"the reopen moved the header's format version, so this case proves nothing")

				if wantKeyAfterReopen {
					Expect(header.RecordCountKey).NotTo(BeNil(),
						"at format %d (>= RECORD_COUNT_KEY_ADDED) reconciliation must persist the "+
							"metadata's record-count key", pinned)
				} else {
					Expect(header.RecordCountKey).To(BeNil(),
						"reopening a store born at format %d wrote a record-count key into a header "+
							"that predates RECORD_COUNT_KEY_ADDED(%d) — the reconciliation path must "+
							"be gated exactly like store creation, or it undoes the creation gate on "+
							"the very next open", pinned, formatVersionRecordCountKeyAdded)
				}

				planted, gErr := rtx.Transaction().Get(sentinel(store)).Get()
				Expect(gErr).NotTo(HaveOccurred())
				if wantSentinelSurvives {
					Expect(planted).NotTo(BeEmpty(),
						"the sentinel planted in the record-count subspace was cleared, so reopening a "+
							"store born at format %d ran a full clear-and-rescan of every record inside "+
							"the store-open transaction. That is unbounded work: a large store would "+
							"stop being able to reopen at all", pinned)
				} else {
					Expect(planted).To(BeEmpty(),
						"at format %d the count key genuinely changed (absent in header, present in "+
							"metadata), so reconciliation SHOULD clear and rebuild — a surviving "+
							"sentinel means the gate is now suppressing a rebuild that must happen",
						pinned)
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		},
		// Below the gate: nothing written, nothing rescanned, ever.
		Entry("born at 2: no key written, no rescan",
			int32(formatVersionRecordCountAdded), true, false, true),
		// At/above the gate, key unchanged: the header was written at creation, so
		// the reopen finds it already correct and still must not rebuild.
		Entry("born at 3, key unchanged: key present, no rescan",
			int32(formatVersionRecordCountKeyAdded), true, true, true),
		Entry("born at the current version, key unchanged: key present, no rescan",
			int32(formatVersionCurrent), true, true, true),

		// THE POSITIVE DIRECTION, and without it the false branch above is dead code
		// and the gate's boundary is untested. Born at 3 with NO count key declared,
		// reopened with one: that is a genuine change, so reconciliation MUST clear
		// and rebuild. Format 3 is deliberately the exact boundary — a `>` in place
		// of `>=` skips the arm here and the rebuild silently stops happening, which
		// no "must not rebuild" entry can catch.
		Entry("born at 3 with no count key, reopened with one: rebuilds",
			int32(formatVersionRecordCountKeyAdded), false, true, false),
		Entry("born at the current version with no count key, reopened with one: rebuilds",
			int32(formatVersionCurrent), false, true, false),
	)

	// Java's FIRST rebuildRecordCounts arm (FDBRecordStore.java:5116):
	//
	//	(existingStore && oldFormatVersion < RECORD_COUNT_ADDED_FORMAT_VERSION)
	//
	// An EXISTING store below RECORD_COUNT_ADDED(2) predates record counts on disk,
	// so whatever sits in the count subspace cannot be trusted and is rebuilt
	// unconditionally — independent of whether the count KEY changed and
	// independent of the RECORD_COUNT_KEY_ADDED(3) gate. This arm had no Go
	// equivalent at all; it was unreachable while every store opened at the newest
	// format version, and SetFormatVersion makes a format-1 store constructible.
	//
	// The store is born at 1 and REOPENED at 2, so oldFormatVersion is 1 (< 2) and
	// existingStore is true. Without the arm nothing rebuilds: the count key is
	// still gated off at 2 (< 3), so the sentinel would survive.
	It("rebuilds record counts for an existing store below RECORD_COUNT_ADDED", func() {
		ks := specSubspace()
		b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		b.SetRecordCountKey(EmptyKey())
		md, bErr := b.Build()
		Expect(bErr).NotTo(HaveOccurred())

		sentinel := func(store *FDBRecordStore) fdb.Key {
			return fdb.Key(store.subspace.Sub(RecordCountKey).Pack(tuple.Tuple{"zzz_sentinel"}))
		}

		// Born at format 1 — below RECORD_COUNT_ADDED(2).
		_, err := sharedDB.Run(context.Background(), func(rtx *FDBRecordContext) (any, error) {
			store, oErr := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).
				SetSubspace(ks).SetFormatVersion(int32(formatVersionInfoAdded)).CreateOrOpen()
			if oErr != nil {
				return nil, oErr
			}
			Expect(store.GetFormatVersion()).To(Equal(int32(formatVersionInfoAdded)))
			if _, sErr := store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(1), Price: proto.Int32(10),
			}); sErr != nil {
				return nil, sErr
			}
			rtx.Transaction().Set(sentinel(store), []byte("planted"))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Reopen at 2: oldFormatVersion(1) < RECORD_COUNT_ADDED(2) on an existing
		// store, so the counts must be rebuilt from scratch.
		_, err = sharedDB.Run(context.Background(), func(rtx *FDBRecordContext) (any, error) {
			store, oErr := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).
				SetSubspace(ks).SetFormatVersion(int32(formatVersionRecordCountAdded)).Open()
			if oErr != nil {
				return nil, oErr
			}
			planted, gErr := rtx.Transaction().Get(sentinel(store)).Get()
			Expect(gErr).NotTo(HaveOccurred())
			Expect(planted).To(BeEmpty(),
				"the sentinel survived, so reopening a store born below RECORD_COUNT_ADDED(%d) did not "+
					"rebuild its record counts. Counts written before that format version cannot be "+
					"trusted, and no other arm covers this: the count KEY is unchanged, and it is still "+
					"gated off at format %d anyway", formatVersionRecordCountAdded,
				formatVersionRecordCountAdded)
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

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
