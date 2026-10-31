package recordlayer

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	mathrand "math/rand/v2"
	"reflect"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

var _ = Describe("Go-only Brotli record serialization", func() {
	metadata := func() *RecordMetaData {
		b := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto).SetSplitLongRecords(true)
		b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		b.AddIndex("Order", NewIndex("brotli_price", Field("price")))
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}
	serializer := func() *TransformedRecordSerializer {
		s, err := NewTransformedRecordSerializerBuilder().SetGoOnlyBrotliCompression(1).SetWriteValidationRatio(1).Build()
		Expect(err).NotTo(HaveOccurred())
		return s
	}
	withStore := func(ctx context.Context, ks subspace.Subspace, md *RecordMetaData, codec *TransformedRecordSerializer, fn func(*FDBRecordStore) error) error {
		_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(ks).SetSerializer(codec).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			return nil, fn(store)
		})
		return err
	}
	snapshot := func(ctx context.Context, ks subspace.Subspace) []fdb.KeyValue {
		var rows []fdb.KeyValue
		_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			var err error
			rows, err = rc.Transaction().GetRange(ks, fdb.RangeOptions{}).GetSliceWithError()
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		return rows
	}
	order := func(id int64, price int32) *demopb.Order {
		o := &demopb.Order{OrderId: proto.Int64(id), Price: proto.Int32(price), Tags: []string{strings.Repeat("Brotli record bytes ", 100)}}
		o.ProtoReflect().SetUnknown(protowire.AppendBytes(protowire.AppendTag(nil, 19000, protowire.BytesType), []byte{0, 0xff, 0x80, 1, 2, 3}))
		return o
	}
	assertRecord := func(ctx context.Context, ks subspace.Subspace, md *RecordMetaData, codec *TransformedRecordSerializer, want *demopb.Order) {
		Expect(withStore(ctx, ks, md, codec, func(store *FDBRecordStore) error {
			record, err := store.LoadRecord(tuple.Tuple{want.GetOrderId()})
			if err != nil {
				return err
			}
			Expect(record).NotTo(BeNil())
			Expect(proto.Equal(record.Record, want)).To(BeTrue())
			Expect(record.Record.ProtoReflect().GetUnknown()).To(Equal(want.ProtoReflect().GetUnknown()))
			return nil
		})).To(Succeed())
	}

	It("uses the store serializer on every save path and accounts dry runs using stored bytes", func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		ks, md, codec := specSubspace(), metadata(), serializer()
		paths := []struct {
			name string
			save func(*FDBRecordStore, *demopb.Order) error
		}{
			{"save", func(s *FDBRecordStore, o *demopb.Order) error { _, err := s.SaveRecord(o); return err }},
			{"insert", func(s *FDBRecordStore, o *demopb.Order) error { _, err := s.InsertRecord(o); return err }},
			{"pipelined", func(s *FDBRecordStore, o *demopb.Order) error {
				_, err := s.SaveRecordsPipelined([]proto.Message{o}, RecordExistenceCheckNone)
				return err
			}},
			{"batch", func(s *FDBRecordStore, o *demopb.Order) error {
				_, err := s.SaveRecordBatch([]proto.Message{o})
				return err
			}},
			{"typed", func(s *FDBRecordStore, o *demopb.Order) error {
				typed, err := NewTypedRecordStore[*demopb.Order](s, md.GetRecordType("Order"))
				if err != nil {
					return err
				}
				_, err = typed.SaveRecord(o)
				return err
			}},
			{"update", func(s *FDBRecordStore, o *demopb.Order) error {
				if _, err := s.InsertRecord(order(o.GetOrderId(), -1)); err != nil {
					return err
				}
				_, err := s.UpdateRecord(o)
				return err
			}},
		}
		for i, path := range paths {
			id := int64(i + 1)
			want := order(id, int32(i+1)) //nolint:gosec
			Expect(withStore(ctx, ks, md, codec, func(s *FDBRecordStore) error { return path.save(s, want) })).To(Succeed(), path.name)
			assertRecord(ctx, ks, md, codec, want)
			raw := snapshot(ctx, ks.Sub(RecordKey, id))
			Expect(raw).To(HaveLen(1), path.name)
			Expect(raw[0].Value[:2]).To(Equal([]byte{transformPrefixCompressed, brotliCompressionVersion}), path.name)
		}
		var drySize int
		want := order(20, 20)
		Expect(withStore(ctx, ks, md, codec, func(s *FDBRecordStore) error {
			dry, err := s.DryRunSaveRecord(want, RecordExistenceCheckNone)
			if err != nil {
				return err
			}
			drySize = dry.ValueSize
			return nil
		})).To(Succeed())
		Expect(snapshot(ctx, ks.Sub(RecordKey, int64(20)))).To(BeEmpty())
		Expect(withStore(ctx, ks, md, codec, func(s *FDBRecordStore) error { _, err := s.SaveRecord(want); return err })).To(Succeed())
		raw := snapshot(ctx, ks.Sub(RecordKey, int64(20)))
		Expect(raw).To(HaveLen(1))
		Expect(drySize).To(Equal(len(raw[0].Value)))
		Expect(withStore(ctx, ks, md, codec, func(s *FDBRecordStore) error {
			entries, err := AsList(ctx, s.ScanIndex(md.GetIndex("brotli_price"), TupleRangeAll, nil, ForwardScan()))
			if err != nil {
				return err
			}
			Expect(entries).To(HaveLen(len(paths) + 1))
			for _, entry := range entries {
				Expect(entry.Key[0]).NotTo(Equal(int64(-1)), "update left a stale old index value")
			}
			return nil
		})).To(Succeed())
	})

	It("preserves authoritative old records and indexes through duplicate batches, refusal and deletion", func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		ks, md, codec := specSubspace(), metadata(), serializer()
		first, last := order(1, 11), order(1, 22)
		Expect(withStore(ctx, ks, md, codec, func(s *FDBRecordStore) error {
			_, err := s.SaveRecordBatch([]proto.Message{first, last})
			return err
		})).To(Succeed())
		assertRecord(ctx, ks, md, codec, last)
		Expect(withStore(ctx, ks, md, codec, func(s *FDBRecordStore) error {
			entries, err := AsList(ctx, s.ScanIndex(md.GetIndex("brotli_price"), TupleRangeAll, nil, ForwardScan()))
			if err != nil {
				return err
			}
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Key[0]).To(Equal(int64(22)))
			return nil
		})).To(Succeed())
		before := snapshot(ctx, ks)
		for _, op := range []struct {
			name string
			run  func(*FDBRecordStore) error
		}{
			{"load", func(s *FDBRecordStore) error { _, err := s.LoadRecord(tuple.Tuple{int64(1)}); return err }},
			{"save", func(s *FDBRecordStore) error { _, err := s.SaveRecord(first); return err }},
			{"insert", func(s *FDBRecordStore) error { _, err := s.InsertRecord(first); return err }},
			{"delete", func(s *FDBRecordStore) error { _, err := s.DeleteRecord(tuple.Tuple{int64(1)}); return err }},
			{"batch", func(s *FDBRecordStore) error { _, err := s.SaveRecordBatch([]proto.Message{first}); return err }},
			{"pipelined", func(s *FDBRecordStore) error {
				_, err := s.SaveRecordsPipelined([]proto.Message{first}, RecordExistenceCheckNone)
				return err
			}},
			{"dry save", func(s *FDBRecordStore) error {
				_, err := s.DryRunSaveRecord(first, RecordExistenceCheckNone)
				return err
			}},
			{"dry delete", func(s *FDBRecordStore) error { _, err := s.DryRunDeleteRecord(tuple.Tuple{int64(1)}); return err }},
		} {
			err := withStore(ctx, ks, md, nil, op.run)
			var deser *RecordDeserializationError
			Expect(errors.As(err, &deser)).To(BeTrue(), "%s: %v", op.name, err)
			Expect(err.Error()).To(ContainSubstring("unknown compression version"))
			Expect(reflect.DeepEqual(snapshot(ctx, ks), before)).To(BeTrue(), op.name+" changed durable source/index bytes")
		}
		// A checksum-invalid old image must also remain authoritative to an
		// opted-in reader; neither overwrite nor deletion may discard it.
		Expect(withStore(ctx, ks, md, codec, func(s *FDBRecordStore) error {
			key := ks.Sub(RecordKey).Pack(tuple.Tuple{int64(1), int64(0)})
			raw, err := s.context.Transaction().Get(key).Get()
			if err != nil {
				return err
			}
			raw = bytes.Clone(raw)
			raw[6] ^= 0xff
			s.context.Transaction().Set(key, raw)
			return nil
		})).To(Succeed())
		corrupt := snapshot(ctx, ks)
		for _, delete := range []bool{false, true} {
			err := withStore(ctx, ks, md, codec, func(s *FDBRecordStore) error {
				if delete {
					_, err := s.DeleteRecord(tuple.Tuple{int64(1)})
					return err
				}
				_, err := s.SaveRecordBatch([]proto.Message{first})
				return err
			})
			Expect(err).To(MatchError(ContainSubstring("decompression checksum mismatch")))
			Expect(reflect.DeepEqual(snapshot(ctx, ks), corrupt)).To(BeTrue())
		}
		// Restore the exact durable image, then delete through the serializer.
		Expect(withStore(ctx, ks, md, codec, func(s *FDBRecordStore) error {
			for _, kv := range before {
				s.context.Transaction().Set(kv.Key, kv.Value)
			}
			return nil
		})).To(Succeed())
		Expect(withStore(ctx, ks, md, codec, func(s *FDBRecordStore) error {
			deleted, err := s.DeleteRecord(tuple.Tuple{int64(1)})
			if err != nil {
				return err
			}
			Expect(deleted).To(BeTrue())
			entries, err := AsList(ctx, s.ScanIndex(md.GetIndex("brotli_price"), TupleRangeAll, nil, ForwardScan()))
			Expect(entries).To(BeEmpty())
			return err
		})).To(Succeed())
		Expect(snapshot(ctx, ks.Sub(RecordKey))).To(BeEmpty())
	})

	It("joins transformed split records and clears their old chunks on replacement", func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		ks, md, codec := specSubspace(), metadata(), serializer()
		rng := mathrand.New(mathrand.NewPCG(283, 19))
		data := make([]byte, 220_000)
		for i := range data {
			data[i] = byte(rng.Uint64())
		}
		large := order(1, 1)
		large.Tags = []string{hex.EncodeToString(data)}
		Expect(withStore(ctx, ks, md, codec, func(s *FDBRecordStore) error {
			saved, err := s.SaveRecord(large)
			if err == nil && !saved.Split {
				return fmt.Errorf("fixture did not remain split after Brotli: %d bytes", saved.ValueSize)
			}
			return err
		})).To(Succeed())
		raw := snapshot(ctx, ks.Sub(RecordKey, int64(1)))
		Expect(len(raw)).To(BeNumerically(">", 1))
		Expect(raw[0].Value[:2]).To(Equal([]byte{transformPrefixCompressed, brotliCompressionVersion}))
		assertRecord(ctx, ks, md, codec, large)
		small := order(1, 2)
		Expect(withStore(ctx, ks, md, codec, func(s *FDBRecordStore) error { _, err := s.SaveRecordBatch([]proto.Message{small}); return err })).To(Succeed())
		assertRecord(ctx, ks, md, codec, small)
		Expect(snapshot(ctx, ks.Sub(RecordKey, int64(1)))).To(HaveLen(1))
		Expect(withStore(ctx, ks, md, codec, func(s *FDBRecordStore) error {
			entries, err := AsList(ctx, s.ScanIndex(md.GetIndex("brotli_price"), TupleRangeAll, nil, ForwardScan()))
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Key[0]).To(Equal(int64(2)))
			return err
		})).To(Succeed())
	})
})
