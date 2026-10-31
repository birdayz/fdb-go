package recordlayer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

const (
	largeContextChunkBytes = 256 << 10
	largeContextOverhead   = 32 << 10
	largeContextPriceIndex = "large_context_price"
	largeContextIDIndex    = "large_context_id"
)

type largeContextStoredSizes struct {
	keys, keyBytes, valueBytes int
}

// A full-context dry run is deliberately rolled back even though it does not
// write the source record. Only the separately guarded chunks are committed.
type largeContextPreviewOnly struct{}

func (*largeContextPreviewOnly) Error() string { return "large context preview only" }

func largeContextStoreMetadata() (*RecordMetaData, error) {
	b := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto).SetSplitLongRecords(true)
	b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	b.AddIndex("TypedRecord", NewIndex(largeContextPriceIndex, Field("price")))
	b.AddIndex("TypedRecord", NewIndex(largeContextIDIndex, Field("id")))
	return b.Build()
}

func largeContextChunk(id int64, content []byte) *demopb.TypedRecord {
	return &demopb.TypedRecord{Id: proto.Int64(id), Price: proto.Int32(7), ValBytes: content}
}

func largeContextRun(ctx context.Context, ks subspace.Subspace, md *RecordMetaData, serializer *TransformedRecordSerializer, body func(*FDBRecordStore) error) error {
	_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
		if err := rc.Transaction().Options().SetTimeout(1500); err != nil {
			return nil, err
		}
		if err := rc.Transaction().Options().SetRetryLimit(10); err != nil {
			return nil, err
		}
		store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(ks).SetSerializer(serializer).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		return nil, body(store)
	})
	return err
}

func largeContextEstimate(estimate int) error {
	if estimate > 300000 {
		return fmt.Errorf("large context conservative transaction estimate %d exceeds 300000", estimate)
	}
	return nil
}

func largeContextGuardSize(store *FDBRecordStore, started time.Time) (int64, error) {
	n, err := store.context.Transaction().GetApproximateSize().Get()
	if err != nil {
		return 0, err
	}
	elapsed := time.Since(started)
	if n > 500000 || elapsed > time.Second {
		return n, fmt.Errorf("large context transaction guard exceeded: bytes=%d elapsed=%s", n, elapsed)
	}
	return n, nil
}

func largeContextGuard(store *FDBRecordStore, started time.Time) error {
	_, err := largeContextGuardSize(store, started)
	return err
}

func largeContextSnapshot(ctx context.Context, ks subspace.Subspace, md *RecordMetaData, serializer *TransformedRecordSerializer, target subspace.Subspace) ([]fdb.KeyValue, error) {
	var rows []fdb.KeyValue
	err := largeContextRun(ctx, ks, md, serializer, func(store *FDBRecordStore) error {
		var err error
		rows, err = store.context.Transaction().GetRange(target, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).GetSliceWithError()
		return err
	})
	return rows, err
}

func largeContextCheckPhysical(ks subspace.Subspace, id int64, rows []fdb.KeyValue, saved *FDBStoredRecord[proto.Message]) (largeContextStoredSizes, error) {
	var sizes largeContextStoredSizes
	for i, kv := range rows {
		suffix := int64(0)
		if saved.Split {
			suffix = int64(i + 1)
		}
		wantKey := ks.Sub(RecordKey).Pack(tuple.Tuple{id, suffix})
		if !bytes.Equal(kv.Key, wantKey) || len(kv.Value) == 0 || len(kv.Value) > splitRecordSize {
			return sizes, fmt.Errorf("chunk %d has unexpected split key/value at position %d", id, i)
		}
		if saved.Split && i < len(rows)-1 && len(kv.Value) != splitRecordSize {
			return sizes, fmt.Errorf("chunk %d has short nonfinal split at position %d", id, i)
		}
		sizes.keys++
		sizes.keyBytes += len(kv.Key)
		sizes.valueBytes += len(kv.Value)
	}
	if sizes.keys != saved.KeyCount || sizes.keyBytes != saved.KeySize || sizes.valueBytes != saved.ValueSize {
		return sizes, fmt.Errorf("chunk %d physical sizes %+v differ from store sizes keys=%d keybytes=%d valuebytes=%d", id, sizes, saved.KeyCount, saved.KeySize, saved.ValueSize)
	}
	return sizes, nil
}

func largeContextSaveChunk(ctx context.Context, ks subspace.Subspace, md *RecordMetaData, serializer *TransformedRecordSerializer, record *demopb.TypedRecord) (*FDBStoredRecord[proto.Message], int64, error) {
	var saved *FDBStoredRecord[proto.Message]
	var actualBytes int64
	err := largeContextRun(ctx, ks, md, serializer, func(store *FDBRecordStore) error {
		started := time.Now()
		dry, err := store.DryRunSaveRecord(record, RecordExistenceCheckNone)
		if err != nil {
			return err
		}
		if err := largeContextEstimate(dry.KeySize + dry.ValueSize + largeContextOverhead); err != nil {
			return err
		}
		results, err := store.SaveRecordBatch([]proto.Message{record})
		if err != nil {
			return err
		}
		if len(results) != 1 || results[0].KeyCount != dry.KeyCount || results[0].KeySize != dry.KeySize || results[0].ValueSize != dry.ValueSize || results[0].Split != dry.Split {
			return fmt.Errorf("chunk %d save did not match dry-run physical sizes", record.GetId())
		}
		actualBytes, err = largeContextGuardSize(store, started)
		if err != nil {
			return err
		}
		saved = results[0]
		return nil
	})
	return saved, actualBytes, err
}

func largeContextCheckIndexes(ctx context.Context, ks subspace.Subspace, md *RecordMetaData, serializer *TransformedRecordSerializer, chunks int, deletedFirst bool) error {
	return largeContextRun(ctx, ks, md, serializer, func(store *FDBRecordStore) error {
		want := chunks
		if deletedFirst {
			want--
		}
		for _, name := range []string{largeContextPriceIndex, largeContextIDIndex} {
			index := md.GetIndex(name)
			entries, err := AsList(ctx, store.ScanIndex(index, TupleRangeAll, nil, ForwardScan()))
			if err != nil {
				return err
			}
			if len(entries) != want {
				return fmt.Errorf("%s has %d entries, want %d", name, len(entries), want)
			}
			seen := make(map[int64]bool, want)
			for _, entry := range entries {
				pk := entry.PrimaryKey()
				if len(pk) != 1 {
					return fmt.Errorf("%s has an invalid primary key %v", name, pk)
				}
				id, ok := pk[0].(int64)
				if !ok || id < 1 || id > int64(chunks) || seen[id] || (deletedFirst && id == 1) || len(entry.Value) != 0 {
					return fmt.Errorf("%s lost native index identity or contains covering body data: %v", name, entry)
				}
				seen[id] = true
				if name == largeContextPriceIndex && (len(entry.Key) != 2 || entry.Key[0] != int64(7)) {
					return fmt.Errorf("price index has a stale or invalid key %v", entry.Key)
				}
				if name == largeContextIDIndex && (len(entry.Key) != 1 || entry.Key[0] != id) {
					return fmt.Errorf("id index has an invalid key %v", entry.Key)
				}
			}
			raw, err := store.context.Transaction().GetRange(store.IndexSubspace(index), fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).GetSliceWithError()
			if err != nil {
				return err
			}
			if len(raw) != want {
				return fmt.Errorf("%s physical entry count is %d, want %d", name, len(raw), want)
			}
			for _, kv := range raw {
				if len(kv.Value) != 0 || len(kv.Key) > 1000 {
					return fmt.Errorf("%s contains a nonempty value or oversized metadata key", name)
				}
			}
		}
		return nil
	})
}

var _ = Describe("Large LLM context through Record Layer serializers", func() {
	for _, name := range []string{"plain", "zlib1", "zlib6", "brotli1", "brotli4"} {
		It("stores and reconstructs guarded chunks with "+name, func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			codecs, err := largeContextCodecs()
			Expect(err).NotTo(HaveOccurred())
			Expect(codecs).To(HaveLen(5))
			var codec *largeContextCodec
			for i := range codecs {
				if codecs[i].name == name {
					Expect(codec).To(BeNil(), "duplicate codec name")
					codec = &codecs[i]
				}
			}
			Expect(codec).NotTo(BeNil(), "missing codec %s", name)
			corpus, err := loadLargeContextCorpus()
			Expect(err).NotTo(HaveOccurred())
			Expect(len(corpus)).To(BeNumerically(">", 300000))
			Expect(len(corpus)).To(BeNumerically("<", maxBrotliRecordBytes))
			corpusDigest := sha256.Sum256(corpus)
			ks := specSubspace()
			md, err := largeContextStoreMetadata()
			Expect(err).NotTo(HaveOccurred())
			Expect(md.GetRecordType("TypedRecord").GetIndexes()).To(HaveLen(2))

			var fullPreview *FDBStoredRecord[proto.Message]
			full := largeContextChunk(1000000, corpus)
			err = largeContextRun(ctx, ks, md, codec.serializer, func(store *FDBRecordStore) error {
				fullPreview, err = store.DryRunSaveRecord(full, RecordExistenceCheckNone)
				if err != nil {
					return err
				}
				return &largeContextPreviewOnly{}
			})
			var previewOnly *largeContextPreviewOnly
			Expect(errors.As(err, &previewOnly)).To(BeTrue(), "full-context dry run must not commit")
			Expect(fullPreview).NotTo(BeNil())
			previewKeys := 1
			if fullPreview.ValueSize > splitRecordSize {
				previewKeys = (fullPreview.ValueSize + splitRecordSize - 1) / splitRecordSize
			}
			Expect(fullPreview.KeyCount).To(Equal(previewKeys))
			Expect(fullPreview.Split).To(Equal(fullPreview.ValueSize > splitRecordSize))
			previewKeyBytes := 0
			for i := 0; i < previewKeys; i++ {
				suffix := int64(0)
				if fullPreview.Split {
					suffix = int64(i + 1)
				}
				previewKeyBytes += len(ks.Sub(RecordKey).Pack(tuple.Tuple{full.GetId(), suffix}))
			}
			Expect(fullPreview.KeySize).To(Equal(previewKeyBytes))
			rows, err := largeContextSnapshot(ctx, ks, md, codec.serializer, ks.Sub(RecordKey))
			Expect(err).NotTo(HaveOccurred())
			Expect(rows).To(BeEmpty(), "full-context preview leaked durable records")
			fullUnion, err := proto.Marshal(&demopb.UnionDescriptor{XTypedRecord: full})
			Expect(err).NotTo(HaveOccurred())
			if codec.serializer == nil {
				Expect(fullPreview.ValueSize).To(Equal(len(fullUnion)))
			}
			fmt.Printf("large-context full-record DRY-RUN ONLY: codec=%s corpus_bytes=%d sha256=%x union_bytes=%d transformed_value_bytes=%d split_keys=%d record_key_bytes=%d value_ratio=%.6f; no full-record commit\n", codec.name, len(corpus), corpusDigest, len(fullUnion), fullPreview.ValueSize, fullPreview.KeyCount, fullPreview.KeySize, float64(fullPreview.ValueSize)/float64(len(fullUnion)))

			chunks := (len(corpus) + largeContextChunkBytes - 1) / largeContextChunkBytes
			var physical largeContextStoredSizes
			var plainUnionBytes int
			var maxSourceTransactionBytes int64
			var firstSaved *FDBStoredRecord[proto.Message]
			for i := 0; i < chunks; i++ {
				lo, hi := i*largeContextChunkBytes, min((i+1)*largeContextChunkBytes, len(corpus))
				record := largeContextChunk(int64(i+1), corpus[lo:hi])
				union, err := proto.Marshal(&demopb.UnionDescriptor{XTypedRecord: record})
				Expect(err).NotTo(HaveOccurred())
				plainUnionBytes += len(union)
				saved, actualBytes, err := largeContextSaveChunk(ctx, ks, md, codec.serializer, record)
				Expect(err).NotTo(HaveOccurred(), "codec=%s chunk=%d", codec.name, i)
				maxSourceTransactionBytes = max(maxSourceTransactionBytes, actualBytes)
				if i == 0 {
					firstSaved = saved
				}
				rows, err := largeContextSnapshot(ctx, ks, md, codec.serializer, ks.Sub(RecordKey, record.GetId()))
				Expect(err).NotTo(HaveOccurred())
				sizes, err := largeContextCheckPhysical(ks, record.GetId(), rows, saved)
				Expect(err).NotTo(HaveOccurred())
				physical.keys += sizes.keys
				physical.keyBytes += sizes.keyBytes
				physical.valueBytes += sizes.valueBytes
				fmt.Printf("large-context chunk: codec=%s chunk=%d raw_bytes=%d plain_union_bytes=%d value_bytes=%d split_keys=%d key_bytes=%d actual_transaction_bytes=%d\n", codec.name, i, hi-lo, len(union), sizes.valueBytes, sizes.keys, sizes.keyBytes, actualBytes)
			}
			Expect(largeContextCheckIndexes(ctx, ks, md, codec.serializer, chunks, false)).To(Succeed())
			if codec.serializer == nil {
				Expect(physical.keys).To(BeNumerically(">", chunks), "plain source must exercise split records")
				Expect(physical.valueBytes).To(Equal(plainUnionBytes))
			}
			var reassembled bytes.Buffer
			reassembled.Grow(len(corpus))
			for i := 0; i < chunks; i++ {
				var chunkBytes []byte
				Expect(largeContextRun(ctx, ks, md, codec.serializer, func(store *FDBRecordStore) error {
					record, err := store.LoadRecord(tuple.Tuple{int64(i + 1)})
					if err != nil {
						return err
					}
					if record == nil {
						return fmt.Errorf("missing context chunk %d", i)
					}
					message, ok := record.Record.(*demopb.TypedRecord)
					if !ok || message.GetId() != int64(i+1) || message.GetPrice() != 7 {
						return fmt.Errorf("invalid context chunk %d metadata", i)
					}
					lo, hi := i*largeContextChunkBytes, min((i+1)*largeContextChunkBytes, len(corpus))
					if !bytes.Equal(message.ValBytes, corpus[lo:hi]) {
						return fmt.Errorf("context chunk %d changed bytes", i)
					}
					chunkBytes = bytes.Clone(message.ValBytes)
					return nil
				})).To(Succeed())
				_, err = reassembled.Write(chunkBytes)
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(reassembled.Bytes()).To(Equal(corpus))
			Expect(sha256.Sum256(reassembled.Bytes())).To(Equal(corpusDigest))
			fmt.Printf("large-context GUARDED CHUNKED persistence: codec=%s raw_chunk_cap=%d chunks=%d corpus_bytes=%d sha256=%x plain_union_bytes=%d persisted_value_bytes=%d record_keys=%d record_key_bytes=%d max_source_transaction_bytes=%d value_ratio=%.6f; two tiny VALUE indexes, body unindexed; not an OTel eight-posting workload or throughput claim\n", codec.name, largeContextChunkBytes, chunks, len(corpus), corpusDigest, plainUnionBytes, physical.valueBytes, physical.keys, physical.keyBytes, maxSourceTransactionBytes, float64(physical.valueBytes)/float64(plainUnionBytes))

			// Corrupt just one physical split value under the unchanged guards.
			original, err := largeContextSnapshot(ctx, ks, md, codec.serializer, ks.Sub(RecordKey, int64(1)))
			Expect(err).NotTo(HaveOccurred())
			Expect(original).NotTo(BeEmpty())
			Expect(largeContextRun(ctx, ks, md, codec.serializer, func(store *FDBRecordStore) error {
				started := time.Now()
				value := bytes.Clone(original[0].Value)
				value[0] = 0
				if err := largeContextEstimate(len(original[0].Key) + len(value) + largeContextOverhead); err != nil {
					return err
				}
				store.context.Transaction().Set(original[0].Key, value)
				return largeContextGuard(store, started)
			})).To(Succeed())
			corrupt, err := largeContextSnapshot(ctx, ks, md, codec.serializer, ks)
			Expect(err).NotTo(HaveOccurred())
			for _, operation := range []string{"load", "overwrite", "delete"} {
				err := largeContextRun(ctx, ks, md, codec.serializer, func(store *FDBRecordStore) error {
					started := time.Now()
					if err := largeContextEstimate(firstSaved.KeySize + firstSaved.ValueSize + largeContextOverhead); err != nil {
						return err
					}
					var err error
					switch operation {
					case "load":
						_, err = store.LoadRecord(tuple.Tuple{int64(1)})
					case "overwrite":
						_, err = store.SaveRecordBatch([]proto.Message{largeContextChunk(1, []byte("must not replace unreadable content"))})
					default:
						_, err = store.DeleteRecord(tuple.Tuple{int64(1)})
					}
					if err != nil {
						return err
					}
					return largeContextGuard(store, started)
				})
				var deser *RecordDeserializationError
				Expect(errors.As(err, &deser)).To(BeTrue(), "corrupt chunk %s: %v", operation, err)
				after, err := largeContextSnapshot(ctx, ks, md, codec.serializer, ks)
				Expect(err).NotTo(HaveOccurred())
				Expect(reflect.DeepEqual(after, corrupt)).To(BeTrue(), "rejected %s changed committed source/index bytes", operation)
			}
			Expect(largeContextRun(ctx, ks, md, codec.serializer, func(store *FDBRecordStore) error {
				started := time.Now()
				if err := largeContextEstimate(len(original[0].Key) + len(original[0].Value) + largeContextOverhead); err != nil {
					return err
				}
				store.context.Transaction().Set(original[0].Key, original[0].Value)
				return largeContextGuard(store, started)
			})).To(Succeed())

			small := largeContextChunk(1, []byte("small replacement context"))
			savedSmall, _, err := largeContextSaveChunk(ctx, ks, md, codec.serializer, small)
			Expect(err).NotTo(HaveOccurred())
			Expect(savedSmall.Split).To(BeFalse())
			rows, err = largeContextSnapshot(ctx, ks, md, codec.serializer, ks.Sub(RecordKey, int64(1)))
			Expect(err).NotTo(HaveOccurred())
			_, err = largeContextCheckPhysical(ks, 1, rows, savedSmall)
			Expect(err).NotTo(HaveOccurred())
			Expect(rows).To(HaveLen(1), "overwrite retained old split chunks")
			Expect(largeContextCheckIndexes(ctx, ks, md, codec.serializer, chunks, false)).To(Succeed())
			Expect(largeContextRun(ctx, ks, md, codec.serializer, func(store *FDBRecordStore) error {
				started := time.Now()
				record, err := store.LoadRecord(tuple.Tuple{int64(1)})
				if err != nil {
					return err
				}
				if record == nil || !proto.Equal(record.Record, small) {
					return fmt.Errorf("small replacement did not load byte-exactly")
				}
				if err := largeContextEstimate(record.KeySize + record.ValueSize + largeContextOverhead); err != nil {
					return err
				}
				deleted, err := store.DeleteRecord(tuple.Tuple{int64(1)})
				if err != nil {
					return err
				}
				if !deleted {
					return fmt.Errorf("small replacement was not deleted")
				}
				return largeContextGuard(store, started)
			})).To(Succeed())
			rows, err = largeContextSnapshot(ctx, ks, md, codec.serializer, ks.Sub(RecordKey, int64(1)))
			Expect(err).NotTo(HaveOccurred())
			Expect(rows).To(BeEmpty())
			Expect(largeContextCheckIndexes(ctx, ks, md, codec.serializer, chunks, true)).To(Succeed())
		})
	}
})
