package recordlayer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// A store over records written through an encrypting TransformedRecordSerializer:
// every write path stores the transformation, and a store that cannot read an
// existing record (no key, or another key) refuses to overwrite or delete it.
var _ = Describe("A store writing through TransformedRecordSerializer", func() {
	ctx := context.Background()

	newKey := func() []byte {
		k := make([]byte, 16)
		_, err := rand.Read(k)
		Expect(err).NotTo(HaveOccurred())
		return k
	}
	encrypting := func(key []byte) *TransformedRecordSerializer {
		s, err := NewTransformedRecordSerializerJCEBuilder().SetEncryptWhenSerializing(true).SetEncryptionKey(key).Build()
		Expect(err).NotTo(HaveOccurred())
		return s
	}
	metaData := func() *RecordMetaData {
		b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto).SetSplitLongRecords(true)
		b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}
	withStore := func(ks subspace.Subspace, md *RecordMetaData, s *TransformedRecordSerializer, f func(*FDBRecordStore) error) error {
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			b := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks)
			if s != nil {
				b = b.SetSerializer(s)
			}
			store, err := b.CreateOrOpen()
			if err != nil {
				return nil, err
			}
			return nil, f(store)
		})
		return err
	}
	rawValues := func(ks subspace.Subspace, id int64) [][]byte {
		var out [][]byte
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			kvs, err := rtx.Transaction().GetRange(ks.Sub(RecordKey, id), fdb.RangeOptions{}).GetSliceWithError()
			if err != nil {
				return nil, err
			}
			for _, kv := range kvs {
				out = append(out, kv.Value)
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		return out
	}
	order := func(id int64, tags ...string) *gen.Order {
		return &gen.Order{OrderId: proto.Int64(id), Price: proto.Int32(int32(id)), Tags: tags} //nolint:gosec
	}

	It("refuses to overwrite or delete a record it cannot read, on every write path", func() {
		ks, md, key := specSubspace(), metaData(), newKey()
		original := order(1, "original")
		Expect(withStore(ks, md, encrypting(key), func(s *FDBRecordStore) error {
			_, err := s.SaveRecord(original)
			return err
		})).To(Succeed())

		replacement := order(1, "replacement")
		// Another key, checked to decrypt the stored record to bad padding
		// (a random one reads garbage about one time in 255).
		wrong, err := keyWithBadPadding(rawValues(ks, 1)[0], 16)
		Expect(err).NotTo(HaveOccurred())
		for _, reader := range []struct {
			name string
			s    *TransformedRecordSerializer
			want string
		}{
			{"no serializer", nil, "this serializer cannot decrypt"},
			{"another key", encrypting(wrong), "decryption error"},
		} {
			for _, op := range []struct {
				name string
				run  func(*FDBRecordStore) error
			}{
				{"SaveRecord", func(s *FDBRecordStore) error { _, err := s.SaveRecord(replacement); return err }},
				// Java loads the existing record before its existence check, so a
				// record it cannot read is RecordDeserializationException, not
				// RecordAlreadyExistsException.
				{"InsertRecord", func(s *FDBRecordStore) error { _, err := s.InsertRecord(replacement); return err }},
				{"DeleteRecord", func(s *FDBRecordStore) error { _, err := s.DeleteRecord(tuple.Tuple{int64(1)}); return err }},
				{"DryRunSaveRecord", func(s *FDBRecordStore) error {
					_, err := s.DryRunSaveRecord(replacement, RecordExistenceCheckNone)
					return err
				}},
				{"DryRunDeleteRecord", func(s *FDBRecordStore) error { _, err := s.DryRunDeleteRecord(tuple.Tuple{int64(1)}); return err }},
				{"SaveRecordBatch", func(s *FDBRecordStore) error {
					_, err := s.SaveRecordBatch([]proto.Message{replacement})
					return err
				}},
			} {
				err := withStore(ks, md, reader.s, op.run)
				var deser *RecordDeserializationError
				Expect(errors.As(err, &deser)).To(BeTrue(), "%s with %s: %v", op.name, reader.name, err)
				Expect(err.Error()).To(ContainSubstring(reader.want), "%s with %s", op.name, reader.name)
			}
		}
		// Under a record-update lock the decode still comes first, as Java's
		// saveTypedRecord and deleteTypedRecord load the old record before
		// validateRecordUpdateAllowed: a store that cannot read the record
		// fails the decode, one that can fails the lock.
		Expect(withStore(ks, md, encrypting(key), func(s *FDBRecordStore) error {
			return s.SetStoreLockState(gen.DataStoreInfo_StoreLockState_FORBID_RECORD_UPDATE, "serializer spec")
		})).To(Succeed())
		for _, op := range []struct {
			name string
			run  func(*FDBRecordStore) error
		}{
			{"SaveRecord", func(s *FDBRecordStore) error { _, err := s.SaveRecord(replacement); return err }},
			{"DeleteRecord", func(s *FDBRecordStore) error { _, err := s.DeleteRecord(tuple.Tuple{int64(1)}); return err }},
			{"SaveRecordBatch", func(s *FDBRecordStore) error {
				_, err := s.SaveRecordBatch([]proto.Message{replacement})
				return err
			}},
		} {
			err := withStore(ks, md, nil, op.run)
			var deser *RecordDeserializationError
			Expect(errors.As(err, &deser)).To(BeTrue(), "%s, locked, without the key: %v", op.name, err)
			err = withStore(ks, md, encrypting(key), op.run)
			var locked *StoreIsLockedForRecordUpdatesError
			Expect(errors.As(err, &locked)).To(BeTrue(), "%s, locked, with the key: %v", op.name, err)
		}
		Expect(withStore(ks, md, encrypting(key), func(s *FDBRecordStore) error {
			return s.ClearStoreLockState()
		})).To(Succeed())

		// Nothing overwrote or removed it.
		Expect(withStore(ks, md, encrypting(key), func(s *FDBRecordStore) error {
			rec, err := s.LoadRecord(tuple.Tuple{int64(1)})
			if err != nil {
				return err
			}
			Expect(rec).NotTo(BeNil())
			Expect(proto.Equal(rec.Record, original)).To(BeTrue(), "the record was changed: %v", rec.Record)
			return nil
		})).To(Succeed())
	})

	It("keeps the serializer on a copied builder and builds an index through the store's", func() {
		ss, key := specSubspace(), newKey()
		mb := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		mb.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		mb.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		mb.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		mb.AddIndex("Order", NewIndex("order_price", Field("price")))
		md, err := mb.Build()
		Expect(err).NotTo(HaveOccurred())
		enc := encrypting(key)

		// A store re-opened from AsBuilder / CopyBuilder writes through the
		// serializer too (Java's Builder.copyFrom copies it).
		var template *FDBRecordStore
		Expect(withStore(ss, md, enc, func(s *FDBRecordStore) error {
			template = s
			if _, err := s.SaveRecord(order(20, "a")); err != nil {
				return err
			}
			copied, err := s.AsBuilder().Open()
			if err != nil {
				return err
			}
			_, err = copied.SaveRecord(order(21, "b"))
			return err
		})).To(Succeed())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			copied, err := template.CopyBuilder(rtx).Open()
			if err != nil {
				return nil, err
			}
			if _, err := copied.SaveRecord(order(22, "c")); err != nil {
				return nil, err
			}
			_, err = copied.MarkIndexDisabled("order_price")
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		for _, id := range []int64{20, 21, 22} {
			Expect(rawValues(ss, id)[0][0]).To(Equal(byte(0x01)), "record %d written through the serializer", id)
		}

		// The indexer reads the encrypted records to rebuild the index. Opening
		// its stores through a fresh builder, it cannot decrypt them; given the
		// store as its template (Java's setRecordStore), it can.
		idx := md.GetIndex("order_price")
		bare, err := NewOnlineIndexerBuilder().SetDatabase(sharedDB).SetMetaData(md).SetSubspace(ss).SetIndex(idx).Build()
		Expect(err).NotTo(HaveOccurred())
		_, err = bare.BuildIndex(ctx)
		Expect(err).To(MatchError(ContainSubstring("this serializer cannot decrypt")))
		templated, err := NewOnlineIndexerBuilder().SetDatabase(sharedDB).SetRecordStore(template).SetIndex(idx).Build()
		Expect(err).NotTo(HaveOccurred())
		_, err = templated.BuildIndex(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(withStore(ss, md, enc, func(s *FDBRecordStore) error {
			Expect(s.GetIndexState("order_price")).To(Equal(IndexStateReadable))
			return nil
		})).To(Succeed())
	})

	// Java's setRecordStore takes the store as a template (asBuilder, which
	// copies the store's format version, FDBRecordStore.java:5746) and the
	// store's context's database as its runner. And a template builder is a
	// copy of the configuration, so a later subspace is the one every part of
	// the store is opened in.
	It("opens the indexer's stores as its template: the format version, the database, a later subspace", func() {
		mb := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		mb.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		mb.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		mb.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		mb.AddIndex("Order", NewIndex("order_price", Field("price")))
		md, err := mb.Build()
		Expect(err).NotTo(HaveOccurred())
		idx := md.GetIndex("order_price")
		const pinned = 10 // below the default a store opens at
		seed := func(ss subspace.Subspace, ids ...int64) *FDBRecordStore {
			var store *FDBRecordStore
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				s, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).SetFormatVersion(pinned).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				for _, id := range ids {
					if _, err := s.SaveRecord(order(id, "x")); err != nil {
						return nil, err
					}
				}
				_, err = s.MarkIndexDisabled("order_price")
				store = s
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
			return store
		}
		headerVersion := func(ss subspace.Subspace) int32 {
			var v int32
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				// Opened at the pin, which neither upgrades nor downgrades.
				s, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).SetFormatVersion(pinned).Open()
				if err != nil {
					return nil, err
				}
				v = s.GetFormatVersion()
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			return v
		}
		entries := func(ss subspace.Subspace) int {
			var n int
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				kvs, err := rtx.Transaction().GetRange(ss.Sub(IndexKey, "order_price"), fdb.RangeOptions{}).GetSliceWithError()
				n = len(kvs)
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
			return n
		}

		// The store as the template, and no database given: the indexer takes
		// the store's, opens at the store's format version, and leaves the
		// pinned header where it was.
		ssA := specSubspace().Sub("A")
		template := seed(ssA, 40, 41)
		Expect(template.AsBuilder().formatVersion).NotTo(BeNil())
		Expect(*template.AsBuilder().formatVersion).To(Equal(int32(pinned)), "AsBuilder carries the store's format version")
		indexer, err := NewOnlineIndexerBuilder().SetRecordStore(template).SetIndex(idx).Build()
		Expect(err).NotTo(HaveOccurred(), "SetRecordStore supplies the store's database")
		_, err = indexer.BuildIndex(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(headerVersion(ssA)).To(Equal(int32(pinned)), "the indexer kept the pinned header")
		Expect(entries(ssA)).To(Equal(2))

		// A header above the pin: the store's format version is the header's,
		// as Java's checkPossiblyRebuild raises it, and so is its builder's.
		ssC := specSubspace().Sub("C")
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			_, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ssC).SetFormatVersion(12).CreateOrOpen()
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			s, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ssC).SetFormatVersion(pinned).Open()
			if err != nil {
				return nil, err
			}
			Expect(*s.AsBuilder().formatVersion).To(Equal(int32(12)), "a header above the pin")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// A builder that has already opened a store in A, as a template whose
		// subspace is then set to B: the index is built over B's records, in B.
		// B's prefix packs to another length than A's, so records read through
		// a stale records subspace would not decode as B's. No database is
		// given: the indexer takes the one of the builder's context.
		ssB := specSubspace().Sub("BBBBBBBB")
		seed(ssB, 50, 51, 52)
		var usedA *StoreBuilder
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			usedA = NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ssA).SetFormatVersion(pinned)
			_, err := usedA.Open()
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		indexer, err = NewOnlineIndexerBuilder().SetRecordStoreBuilder(usedA).SetSubspace(ssB).SetIndex(idx).Build()
		Expect(err).NotTo(HaveOccurred(), "SetRecordStoreBuilder supplies the database of the builder's context")
		_, err = indexer.BuildIndex(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(entries(ssB)).To(Equal(3), "B's index holds B's three records")
		Expect(entries(ssA)).To(Equal(2), "A's index is untouched")
	})

	// The write-time serialization validation deserializes what it reads back
	// through the store's reader, as Java's validateSerialization deserializes
	// through its inner serializer: read back under another key whose padding
	// holds, the bytes are garbage, and the store's reader refuses them
	// ("cannot deserialize record", its cause the reader's, not a cipher's).
	It("validates a write by reading it back as the store reads a record", func() {
		ks, md := specSubspace(), metaData()
		o := order(60, "validated")
		rt := md.GetRecordType("Order")
		_, write := rt.asJavaForSave(o)
		union, err := serializeUnionOver(write, rt, nil)
		Expect(err).NotTo(HaveOccurred())
		key := newKey()
		_, goodPad, err := wrongKeysFor(key, union)
		Expect(err).NotTo(HaveOccurred())
		km := &validationKeyManager{keys: map[int32][]byte{0: key}, later: goodPad}
		s, err := NewTransformedRecordSerializerJCEBuilder().SetEncryptWhenSerializing(true).
			SetKeyManager(km).SetWriteValidationRatio(1).Build()
		Expect(err).NotTo(HaveOccurred())
		err = withStore(ks, md, s, func(st *FDBRecordStore) error { _, err := st.SaveRecord(o); return err })
		var ve *RecordSerializationValidationError
		Expect(errors.As(err, &ve)).To(BeTrue(), "%v", err)
		Expect(ve.Message).To(Equal("cannot deserialize record"))
		Expect(ve.Cause).NotTo(BeNil())
		Expect(ve.Cause.Error()).NotTo(ContainSubstring("decryption error"), "the read-back decrypted; the reader refused it")
		Expect(rawValues(ks, 60)).To(BeEmpty(), "nothing is written")
	})

	It("names the record in a failed write-time validation, on every write path", func() {
		ks, md := specSubspace(), metaData()
		for i, path := range []struct {
			name  string
			write func(*FDBRecordStore, *gen.Order) error
		}{
			{"SaveRecord", func(s *FDBRecordStore, o *gen.Order) error { _, err := s.SaveRecord(o); return err }},
			{"DryRunSaveRecord", func(s *FDBRecordStore, o *gen.Order) error {
				_, err := s.DryRunSaveRecord(o, RecordExistenceCheckNone)
				return err
			}},
			{"SaveRecordBatch", func(s *FDBRecordStore, o *gen.Order) error {
				_, err := s.SaveRecordBatch([]proto.Message{o})
				return err
			}},
		} {
			// The validation decrypts under another key than the write
			// encrypted with, so it fails whatever the record's bytes.
			km := &validationKeyManager{keys: map[int32][]byte{0: newKey()}, later: newKey()}
			s, err := NewTransformedRecordSerializerJCEBuilder().SetEncryptWhenSerializing(true).
				SetKeyManager(km).SetWriteEncryptionValidationRatio(1).Build()
			Expect(err).NotTo(HaveOccurred())
			id := int64(30 + i)
			err = withStore(ks, md, s, func(st *FDBRecordStore) error { return path.write(st, order(id, "v")) })
			var ve *RecordSerializationValidationError
			Expect(errors.As(err, &ve)).To(BeTrue(), "%s: %v", path.name, err)
			Expect(ve.Message).To(HavePrefix("encryption validation error: "), path.name)
			Expect(ve.RecordType).To(Equal("Order"), path.name)
			Expect(ve.PrimaryKey).To(Equal(tuple.Tuple{id}), path.name)
			Expect(rawValues(ks, id)).To(BeEmpty(), "%s: nothing is written", path.name)
		}
	})

	It("stores the transformation on the batch, dry-run and split paths", func() {
		ks, md, key := specSubspace(), metaData(), newKey()
		enc := encrypting(key)

		// The batch path: each record stored encrypted (prefix 01, key 0).
		Expect(withStore(ks, md, enc, func(s *FDBRecordStore) error {
			_, err := s.SaveRecordBatch([]proto.Message{order(10, "a"), order(11, "b")})
			return err
		})).To(Succeed())
		for _, id := range []int64{10, 11} {
			raw := rawValues(ks, id)
			Expect(raw).To(HaveLen(1), "record %d unsplit", id)
			Expect(raw[0][0]).To(Equal(byte(0x01)), "record %d's stored prefix", id)
		}

		// The dry run reports the size the save then stores: the encrypted
		// length, not the union message's.
		var dryRunSize int
		Expect(withStore(ks, md, enc, func(s *FDBRecordStore) error {
			rec, err := s.DryRunSaveRecord(order(12, "c"), RecordExistenceCheckNone)
			if err != nil {
				return err
			}
			dryRunSize = rec.ValueSize
			_, err = s.SaveRecord(order(12, "c"))
			return err
		})).To(Succeed())
		raw := rawValues(ks, 12)
		Expect(raw).To(HaveLen(1))
		Expect(dryRunSize).To(Equal(len(raw[0])), "the dry run's value size is the stored length")

		// A record past the split size after its transformation: chunked,
		// the first chunk carrying the prefix, and read back whole.
		big := make([]byte, 90_000)
		_, err := rand.Read(big)
		Expect(err).NotTo(HaveOccurred())
		large := order(13, hex.EncodeToString(big[:45_000]), hex.EncodeToString(big[45_000:]))
		Expect(withStore(ks, md, enc, func(s *FDBRecordStore) error {
			_, err := s.SaveRecord(large)
			return err
		})).To(Succeed())
		raw = rawValues(ks, 13)
		Expect(len(raw)).To(BeNumerically(">", 1), "the record is split")
		Expect(raw[0][0]).To(Equal(byte(0x01)), "the first chunk carries the prefix")
		Expect(withStore(ks, md, enc, func(s *FDBRecordStore) error {
			rec, err := s.LoadRecord(tuple.Tuple{int64(13)})
			if err != nil {
				return err
			}
			Expect(proto.Equal(rec.Record, large)).To(BeTrue(), "the split record reads back whole")
			return nil
		})).To(Succeed())
		err = withStore(ks, md, nil, func(s *FDBRecordStore) error {
			_, err := s.LoadRecord(tuple.Tuple{int64(13)})
			return err
		})
		Expect(err).To(MatchError(ContainSubstring("this serializer cannot decrypt")), "a split record is decoded after it is joined")
	})
})
