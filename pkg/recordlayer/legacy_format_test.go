package recordlayer

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
)

// Legacy-format wire compatibility: a Go client must read/write stores created by
// older Java Record Layer code, where (a) record versions live in the separate
// RecordVersionKey(8) subspace (FormatVersion < SAVE_VERSION_WITH_RECORD=6) and
// (b) unsplit records may be stored at the bare primary key with no suffix
// (omit_unsplit_record_suffix). See FDBRecordStore.useOldVersionFormat() in Java.
var _ = Describe("Legacy format compatibility", func() {
	var ctx context.Context

	BeforeEach(func() { ctx = context.Background() })

	// legacyMetaData builds Order/Customer/TypedRecord metadata with the given
	// split/version settings (and a fixed metadata version so a later open does not
	// trigger an index rebuild — only a format upgrade).
	legacyMetaData := func(split, versions bool) *RecordMetaData {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.SetSplitLongRecords(split)
		builder.SetStoreRecordVersions(versions)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}

	order := func(id int64, price int32) *gen.Order {
		return &gen.Order{OrderId: proto.Int64(id), Price: proto.Int32(price)}
	}

	// completeGlobalFor returns a deterministic, non-incomplete 10-byte global version.
	completeGlobalFor := func(seed byte) []byte {
		return []byte{0, 0, 0, 0, 0, 0, 0, seed + 1, 0, 0}
	}

	// layDownLegacy writes a store header and records directly into FDB at the
	// legacy on-disk locations, bypassing the modern write path. This simulates a
	// store created by old Java code. Versions (if enabled) are written to the
	// separate RecordVersionKey(8) subspace as raw 12-byte FDBRecordVersion bytes.
	layDownLegacy := func(rtx *FDBRecordContext, ss subspace.Subspace, md *RecordMetaData, formatVersion int32, omit bool, orders []*gen.Order) {
		tx := rtx.Transaction()

		fv := formatVersion
		mdv := int32(md.Version())
		uv := int32(0)
		hdr := &gen.DataStoreInfo{FormatVersion: &fv, MetaDataversion: &mdv, UserVersion: &uv}
		if omit {
			hdr.OmitUnsplitRecordSuffix = proto.Bool(true)
		}
		hb, err := hdr.MarshalVT()
		Expect(err).NotTo(HaveOccurred())
		tx.Set(fdb.Key(ss.Pack(tuple.Tuple{StoreInfoKey})), hb)

		rt := md.GetRecordType("Order")
		recSub := ss.Sub(RecordKey)
		verSub := ss.Sub(RecordVersionKey)
		for i, o := range orders {
			data, serErr := serializeUnion(o, rt)
			Expect(serErr).NotTo(HaveOccurred())
			pk := tuple.Tuple{o.GetOrderId()}
			if omit {
				tx.Set(fdb.Key(recSub.Pack(pk)), data)
			} else {
				tx.Set(fdb.Key(recSub.Pack(appendToTuple(pk, unsplitRecord))), data)
			}
			if md.IsStoreRecordVersions() {
				ver, vErr := NewCompleteVersion(completeGlobalFor(byte(i)), i)
				Expect(vErr).NotTo(HaveOccurred())
				tx.Set(fdb.Key(verSub.Pack(pk)), ver.ToBytes())
			}
		}
	}

	rawGet := func(rtx *FDBRecordContext, key []byte) []byte {
		v, err := rtx.Transaction().Get(fdb.Key(key)).Get()
		Expect(err).NotTo(HaveOccurred())
		return v
	}

	openLegacy := func(rtx *FDBRecordContext, ss subspace.Subspace, md *RecordMetaData) *FDBRecordStore {
		store, err := NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).
			SetSkipPossiblyRebuild(true). // do not migrate — exercise the legacy paths directly
			Open()
		Expect(err).NotTo(HaveOccurred())
		return store
	}

	Describe("reading a legacy store without migrating", func() {
		It("reads bare-key records + subspace-8 versions (format 4, omit=true)", func() {
			ss := specSubspace()
			md := legacyMetaData(false, true)
			orders := []*gen.Order{order(1, 100), order(2, 200), order(3, 300)}

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				layDownLegacy(rtx, ss, md, 4, true, orders)
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store := openLegacy(rtx, ss, md)
				Expect(store.useOldVersionFormat()).To(BeTrue())
				Expect(store.omitUnsplitRecordSuffix()).To(BeTrue())

				// Point load + version.
				rec, lErr := store.LoadRecord(tuple.Tuple{int64(2)})
				Expect(lErr).NotTo(HaveOccurred())
				Expect(rec).NotTo(BeNil())
				Expect(rec.Record.(*gen.Order).GetPrice()).To(Equal(int32(200)))
				Expect(rec.Version).NotTo(BeNil())
				Expect(rec.Version.IsComplete()).To(BeTrue())
				wantVer, _ := NewCompleteVersion(completeGlobalFor(1), 1)
				Expect(rec.Version.Equal(wantVer)).To(BeTrue())

				// RecordExists hits the bare key.
				exists, eErr := store.RecordExists(tuple.Tuple{int64(1)}, SerializableIsolation)
				Expect(eErr).NotTo(HaveOccurred())
				Expect(exists).To(BeTrue())
				missing, eErr := store.RecordExists(tuple.Tuple{int64(99)}, SerializableIsolation)
				Expect(eErr).NotTo(HaveOccurred())
				Expect(missing).To(BeFalse())

				// Full scan returns every record with its version attached.
				prices := map[int64]int32{}
				vers := map[int64]bool{}
				cur := store.ScanRecords(nil, ForwardScan())
				for r, sErr := range Seq2(cur, ctx) {
					Expect(sErr).NotTo(HaveOccurred())
					o := r.Record.(*gen.Order)
					prices[o.GetOrderId()] = o.GetPrice()
					vers[o.GetOrderId()] = r.Version != nil && r.Version.IsComplete()
				}
				Expect(prices).To(Equal(map[int64]int32{1: 100, 2: 200, 3: 300}))
				Expect(vers).To(Equal(map[int64]bool{1: true, 2: true, 3: true}))

				// PK-only scan: bare keys, no dedup, every PK once.
				var pks []int64
				kc := store.ScanRecordKeys(nil, ForwardScan())
				for k, sErr := range Seq2(kc, ctx) {
					Expect(sErr).NotTo(HaveOccurred())
					pks = append(pks, k[0].(int64))
				}
				Expect(pks).To(ConsistOf(int64(1), int64(2), int64(3)))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reads suffixed records + subspace-8 versions (format 5, split, omit=false)", func() {
			ss := specSubspace()
			md := legacyMetaData(true, true)
			orders := []*gen.Order{order(10, 1000), order(20, 2000)}

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				layDownLegacy(rtx, ss, md, 5, false, orders)
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store := openLegacy(rtx, ss, md)
				// format 5 < 6 -> old version format, but suffix is NOT omitted.
				Expect(store.useOldVersionFormat()).To(BeTrue())
				Expect(store.omitUnsplitRecordSuffix()).To(BeFalse())

				rec, lErr := store.LoadRecord(tuple.Tuple{int64(10)})
				Expect(lErr).NotTo(HaveOccurred())
				Expect(rec).NotTo(BeNil())
				Expect(rec.Record.(*gen.Order).GetPrice()).To(Equal(int32(1000)))
				Expect(rec.Version).NotTo(BeNil())
				wantVer, _ := NewCompleteVersion(completeGlobalFor(0), 0)
				Expect(rec.Version.Equal(wantVer)).To(BeTrue())

				count := 0
				cur := store.ScanRecords(nil, ForwardScan())
				for r, sErr := range Seq2(cur, ctx) {
					Expect(sErr).NotTo(HaveOccurred())
					Expect(r.Version).NotTo(BeNil())
					count++
				}
				Expect(count).To(Equal(2))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("skips version I/O when the metadata stores no versions (old format)", func() {
			ss := specSubspace()
			mdNoVer := legacyMetaData(false, false)
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				layDownLegacy(rtx, ss, mdNoVer, 4, true, []*gen.Order{order(1, 100)})
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store := openLegacy(rtx, ss, mdNoVer)
				ver, vErr := store.LoadRecordVersion(tuple.Tuple{int64(1)}, false)
				Expect(vErr).NotTo(HaveOccurred())
				Expect(ver).To(BeNil())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("writing to a legacy store (no migration)", func() {
		It("save/update/delete keep the bare-key + subspace-8 layout (format 4, omit)", func() {
			ss := specSubspace()
			md := legacyMetaData(false, true)
			recSub := ss.Sub(RecordKey)
			verSub := ss.Sub(RecordVersionKey)
			pk1 := tuple.Tuple{int64(1)}
			pk2 := tuple.Tuple{int64(2)}

			// Seed one existing record, then insert a second through the store.
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				layDownLegacy(rtx, ss, md, 4, true, []*gen.Order{order(1, 100)})
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store := openLegacy(rtx, ss, md)
				_, sErr := store.SaveRecord(order(2, 222))
				return nil, sErr
			})
			Expect(err).NotTo(HaveOccurred())

			// The new record must land at the bare key, NOT at pk+0, and its version
			// must be in subspace 8 (a committed 12-byte value), not inline.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				Expect(rawGet(rtx, recSub.Pack(pk2))).NotTo(BeNil())
				Expect(rawGet(rtx, recSub.Pack(appendToTuple(pk2, unsplitRecord)))).To(BeNil())
				Expect(rawGet(rtx, recSub.Pack(appendToTuple(pk2, recordVersionSuffix)))).To(BeNil())
				legacyVer := rawGet(rtx, verSub.Pack(pk2))
				Expect(legacyVer).To(HaveLen(VersionBytes))

				// Read back through the store.
				store := openLegacy(rtx, ss, md)
				rec, lErr := store.LoadRecord(pk2)
				Expect(lErr).NotTo(HaveOccurred())
				Expect(rec.Record.(*gen.Order).GetPrice()).To(Equal(int32(222)))
				Expect(rec.Version).NotTo(BeNil())
				Expect(rec.Version.IsComplete()).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Update record 1 in place; delete record 2.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store := openLegacy(rtx, ss, md)
				if _, sErr := store.SaveRecord(order(1, 111)); sErr != nil {
					return nil, sErr
				}
				deleted, dErr := store.DeleteRecord(pk2)
				Expect(dErr).NotTo(HaveOccurred())
				Expect(deleted).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				// Update overwrote the bare key in place.
				updated := rawGet(rtx, recSub.Pack(pk1))
				Expect(updated).NotTo(BeNil())
				// Delete cleared both the bare record key and the subspace-8 version.
				Expect(rawGet(rtx, recSub.Pack(pk2))).To(BeNil())
				Expect(rawGet(rtx, verSub.Pack(pk2))).To(BeNil())

				store := openLegacy(rtx, ss, md)
				rec, lErr := store.LoadRecord(pk1)
				Expect(lErr).NotTo(HaveOccurred())
				Expect(rec.Record.(*gen.Order).GetPrice()).To(Equal(int32(111)))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("DeleteRecordsWhere clears the legacy version subspace", func() {
			ss := specSubspace()
			// Composite PK (type-prefixed) so deleteRecordsWhere has a prefix to target.
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Concat(RecordTypeKey(), Field("order_id")))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.SetSplitLongRecords(false)
			builder.SetStoreRecordVersions(true)
			md, mErr := builder.Build()
			Expect(mErr).NotTo(HaveOccurred())

			orderTypeKey := md.GetRecordType("Order").GetRecordTypeKey()
			recSub := ss.Sub(RecordKey)
			verSub := ss.Sub(RecordVersionKey)

			// Lay down records with composite PKs (typeKey, id) in the legacy bare-key
			// layout and versions in subspace 8.
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				tx := rtx.Transaction()
				fv, mdv, uv := int32(4), int32(md.Version()), int32(0)
				hdr := &gen.DataStoreInfo{FormatVersion: &fv, MetaDataversion: &mdv, UserVersion: &uv, OmitUnsplitRecordSuffix: proto.Bool(true)}
				hb, _ := hdr.MarshalVT()
				tx.Set(fdb.Key(ss.Pack(tuple.Tuple{StoreInfoKey})), hb)
				rt := md.GetRecordType("Order")
				for i, id := range []int64{1, 2} {
					o := order(id, int32(id*10))
					data, _ := serializeUnion(o, rt)
					pk := tuple.Tuple{orderTypeKey, id}
					tx.Set(fdb.Key(recSub.Pack(pk)), data)
					ver, _ := NewCompleteVersion(completeGlobalFor(byte(i)), i)
					tx.Set(fdb.Key(verSub.Pack(pk)), ver.ToBytes())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store := openLegacy(rtx, ss, md)
				return nil, store.DeleteRecordsWhere(tuple.Tuple{orderTypeKey})
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				for _, id := range []int64{1, 2} {
					pk := tuple.Tuple{orderTypeKey, id}
					Expect(rawGet(rtx, recSub.Pack(pk))).To(BeNil())
					Expect(rawGet(rtx, verSub.Pack(pk))).To(BeNil())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("on-open format migration", func() {
		It("moves subspace-8 versions inline when upgrading a split store past format 6", func() {
			ss := specSubspace()
			md := legacyMetaData(true, true) // split => omit stays false => versions convert
			recSub := ss.Sub(RecordKey)
			verSub := ss.Sub(RecordVersionKey)
			orders := []*gen.Order{order(1, 100), order(2, 200)}

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				layDownLegacy(rtx, ss, md, 4, false, orders)
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Open WITHOUT skip -> migration runs in this transaction.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, oErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				Expect(oErr).NotTo(HaveOccurred())
				Expect(store.GetFormatVersion()).To(Equal(int32(formatVersionCurrent)))
				Expect(store.useOldVersionFormat()).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				// Legacy subspace cleared; inline version keys now present.
				for _, id := range []int64{1, 2} {
					pk := tuple.Tuple{id}
					Expect(rawGet(rtx, verSub.Pack(pk))).To(BeNil())
					Expect(rawGet(rtx, recSub.Pack(appendToTuple(pk, recordVersionSuffix)))).NotTo(BeNil())
				}
				// Records still readable with versions, now in the modern layout.
				store, oErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				Expect(oErr).NotTo(HaveOccurred())
				Expect(store.useOldVersionFormat()).To(BeFalse())
				rec, lErr := store.LoadRecord(tuple.Tuple{int64(1)})
				Expect(lErr).NotTo(HaveOccurred())
				Expect(rec.Version).NotTo(BeNil())
				Expect(rec.Version.IsComplete()).To(BeTrue())
				wantVer, _ := NewCompleteVersion(completeGlobalFor(0), 0)
				Expect(rec.Version.Equal(wantVer)).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("sets omit_unsplit_record_suffix and keeps versions in subspace 8 for a non-split store", func() {
			ss := specSubspace()
			md := legacyMetaData(false, true) // !split, format<5 => omit set on upgrade, no conversion
			recSub := ss.Sub(RecordKey)
			verSub := ss.Sub(RecordVersionKey)

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				layDownLegacy(rtx, ss, md, 4, true, []*gen.Order{order(7, 700)})
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, oErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				Expect(oErr).NotTo(HaveOccurred())
				Expect(store.GetFormatVersion()).To(Equal(int32(formatVersionCurrent)))
				// !split store upgraded from <5 keeps the legacy layout forever.
				Expect(store.omitUnsplitRecordSuffix()).To(BeTrue())
				Expect(store.useOldVersionFormat()).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				pk := tuple.Tuple{int64(7)}
				// Record still at the bare key; version still in subspace 8 (NOT inline).
				Expect(rawGet(rtx, recSub.Pack(pk))).NotTo(BeNil())
				Expect(rawGet(rtx, verSub.Pack(pk))).To(HaveLen(VersionBytes))
				Expect(rawGet(rtx, recSub.Pack(appendToTuple(pk, recordVersionSuffix)))).To(BeNil())

				store, oErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				Expect(oErr).NotTo(HaveOccurred())
				rec, lErr := store.LoadRecord(pk)
				Expect(lErr).NotTo(HaveOccurred())
				Expect(rec.Record.(*gen.Order).GetPrice()).To(Equal(int32(700)))
				Expect(rec.Version).NotTo(BeNil())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
