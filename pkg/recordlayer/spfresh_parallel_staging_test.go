package recordlayer

import (
	"context"
	"fmt"
	"sort"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
)

// RFC-103: the parallel staging scan must produce a SHARD-COUNT-INVARIANT staged
// set. These tests pin that directly (byte-identical staging keyspace S=1 vs
// S=8) and end-to-end (identical kNN results), plus the prefix-safety fallback.
var _ = Describe("SPFresh parallel staging scan (RFC-103)", func() {
	ctx := context.Background()

	// Record-type-prefixed PKs ⇒ shard-safe (PrimaryKeyHasRecordTypePrefix), so
	// the fan-out actually runs S>1 rather than degrading to the serial floor.
	buildMeta := func(idx *Index) *RecordMetaData {
		b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		b.GetRecordType("Order").SetPrimaryKey(Concat(RecordTypeKey(), Field("order_id")))
		b.GetRecordType("Customer").SetPrimaryKey(Concat(RecordTypeKey(), Field("customer_id")))
		b.GetRecordType("TypedRecord").SetPrimaryKey(Concat(RecordTypeKey(), Field("id")))
		b.AddIndex("Order", idx)
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}

	newVecIndex := func(name string) *Index {
		idx := NewIndex(name, Concat(Field("price"), Field("quantity")))
		idx.Type = IndexTypeVectorSPFresh
		idx.Options = map[string]string{
			IndexOptionSPFreshNumDimensions: "2",
			IndexOptionSPFreshLmax:          "32",
			IndexOptionSPFreshCellTarget:    "4",
			IndexOptionSPFreshCellMax:       "8",
		}
		return idx
	}

	const nRecords = 160
	saveRecords := func(storeBuilder func(*FDBRecordContext) (*FDBRecordStore, error), indexName string) {
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			if serr != nil {
				return nil, serr
			}
			_, serr = store.MarkIndexDisabled(indexName)
			return nil, serr
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			if serr != nil {
				return nil, serr
			}
			for i := 0; i < nRecords; i++ {
				if _, serr = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(int64(i)),
					Price:    proto.Int32(int32((i * 13) % 50)),
					Quantity: proto.Int32(int32((i*7)%40 + 1)),
				}); serr != nil {
					return nil, serr
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	}

	// stageAndDump runs coarsePass + the (sharded) staging scan over a store of
	// nRecords and returns the staging keyspace relative to the staging-subspace
	// prefix, plus the shard count actually used. It deliberately stops BEFORE
	// finalize (which clears staging) so the staged set itself can be compared —
	// fine IDs are assigned later in wave A and are not shard-count-invariant.
	stageAndDump := func(ks subspace.Subspace, shards int) (map[string][]byte, int) {
		idx := newVecIndex("spf_det")
		md := buildMeta(idx)
		storeBuilder := func(rtx *FDBRecordContext) (*FDBRecordStore, error) {
			return NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		}
		saveRecords(storeBuilder, "spf_det")

		var index *Index
		var config SPFreshConfig
		var indexSubspace subspace.Subspace
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			if serr != nil {
				return nil, serr
			}
			index = store.GetMetaData().GetIndex("spf_det")
			config = parseSPFreshConfig(index)
			indexSubspace = store.indexSubspace(index)
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		storage := newSPFreshStorage(indexSubspace, 1)
		builder := newSPFreshBuilder(sharedDB, storage, config, "test-det")
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			r, rerr := storage.generationRange()
			if rerr != nil {
				return nil, rerr
			}
			rtx.Transaction().ClearRange(r)
			spfreshTakeBuilderToken(rtx.Transaction(), storage, builder.token)
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		var sample [][]float64
		totalN := 0
		sampler := newSPFreshPKSampler(spfreshBoundarySampleCap)
		Expect(spfreshScanRecordBatches(ctx, sharedDB, storeBuilder, index, indexSubspace, spfreshScanBatchSize, nil, func(batch []spfreshBuildInput) error {
			for _, in := range batch {
				sampler.observe(in.fullPK)
				sample = append(sample, in.vec)
				totalN++
			}
			return nil
		})).To(Succeed())
		Expect(builder.coarsePass(ctx, sample, totalN, 42)).To(Succeed())

		var ranges []spfreshShardRange
		if shards == 1 {
			ranges = spfreshShardRanges(nil)
		} else {
			ranges = spfreshShardRanges(sampler.boundaries(shards))
		}
		Expect(spfreshStageRecordsSharded(ctx, sharedDB, storeBuilder, index, indexSubspace, config.stagingScanBatch(), builder.stageInTx, ranges)).To(Succeed())

		dump := map[string][]byte{}
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			pr, perr := fdb.PrefixRange(storage.staging.Bytes())
			if perr != nil {
				return nil, perr
			}
			kvs, kerr := rtx.Transaction().GetRange(pr, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).GetSliceWithError()
			if kerr != nil {
				return nil, kerr
			}
			prefix := storage.staging.Bytes()
			for _, kv := range kvs {
				dump[string([]byte(kv.Key)[len(prefix):])] = append([]byte(nil), kv.Value...)
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		return dump, len(ranges)
	}

	It("S=1 and S=8 stage a byte-identical staging keyspace (the staged set is shard-count-invariant)", func() {
		// Distinct, both-fresh subspaces: cell-ID allocation starts at 1 in each,
		// so identical clustering ⇒ identical cell IDs ⇒ comparable staging keys.
		// (A shared subspace would let the second build inherit the first's
		// allocator block and clear its generation.)
		base := specSubspace()
		d1, n1 := stageAndDump(base.Sub("s1"), 1)
		d8, n8 := stageAndDump(base.Sub("s8"), 8)

		// Prove the test exercised what it claims: S=1 ran one shard, S=8
		// actually fanned out (else "byte-identical" would be trivially true).
		Expect(n1).To(Equal(1), "S=1 must be a single serial shard")
		Expect(n8).To(Equal(8), "S=8 must fan out into 8 disjoint shards")

		// Every record staged exactly once into its routed cell.
		Expect(d1).To(HaveLen(nRecords))
		Expect(d8).To(HaveLen(nRecords))
		// The headline invariant: byte-identical staged set regardless of S.
		Expect(d8).To(Equal(d1), "S=8 staging keyspace must be byte-identical to S=1")
	})

	// buildAndQuery runs the FULL parallel build (coarse + sharded staging +
	// finalize + flip) at the given shard count and returns the kNN result keys
	// — the user-visible recall surface.
	buildAndQuery := func(ks subspace.Subspace, shards int, q []float64, k int) []string {
		idx := newVecIndex("spf_recall")
		md := buildMeta(idx)
		storeBuilder := func(rtx *FDBRecordContext) (*FDBRecordStore, error) {
			return NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		}
		saveRecords(storeBuilder, "spf_recall")
		Expect(buildSPFreshIndex(ctx, sharedDB, storeBuilder, "spf_recall", 42, shards)).To(Succeed())
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			if serr != nil {
				return nil, serr
			}
			_, serr = store.MarkIndexReadable("spf_recall")
			return nil, serr
		})
		Expect(err).NotTo(HaveOccurred())

		var keys []string
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			if serr != nil {
				return nil, serr
			}
			maintainer, merr := store.getIndexMaintainer(idx)
			if merr != nil {
				return nil, merr
			}
			sbd := maintainer.(interface {
				ScanByDistance(TupleRange, []byte, ScanProperties) RecordCursor[*IndexEntry]
			})
			cursor := sbd.ScanByDistance(TupleRange{
				Low:  tuple.Tuple{SerializeVector(q)},
				High: tuple.Tuple{int64(k)},
			}, nil, ScanProperties{})
			for {
				res, cerr := cursor.OnNext(ctx)
				if cerr != nil {
					return nil, cerr
				}
				if !res.HasNext() {
					break
				}
				keys = append(keys, fmt.Sprint(res.GetValue().Key))
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		return keys
	}

	It("S=1 and S=8 full builds return the identical kNN result set (recall unaffected)", func() {
		q := []float64{15, 15}
		base := specSubspace()
		r1 := buildAndQuery(base.Sub("s1"), 1, q, 10)
		r8 := buildAndQuery(base.Sub("s8"), 8, q, 10)
		Expect(r1).NotTo(BeEmpty())
		sort.Strings(r1)
		sort.Strings(r8)
		Expect(r8).To(Equal(r1), "identical staged set ⇒ identical clustering ⇒ identical recall")
	})

	It("an unsafe (bare-PK, multi-type) store falls back to S=1 and builds + queries correctly", func() {
		ks := specSubspace()
		idx := newVecIndex("spf_unsafe")
		// Bare PKs (no RecordTypeKey prefix) ⇒ shard-UNSAFE ⇒ S=1 fallback.
		b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		b.AddIndex("Order", idx)
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		storeBuilder := func(rtx *FDBRecordContext) (*FDBRecordStore, error) {
			return NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		}
		saveRecords(storeBuilder, "spf_unsafe")
		// Default fan-out (S=8 requested) must silently degrade to S=1 and build.
		Expect(buildSPFreshIndex(ctx, sharedDB, storeBuilder, "spf_unsafe", 42, 8)).To(Succeed())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			if serr != nil {
				return nil, serr
			}
			_, serr = store.MarkIndexReadable("spf_unsafe")
			return nil, serr
		})
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			if serr != nil {
				return nil, serr
			}
			maintainer, merr := store.getIndexMaintainer(idx)
			if merr != nil {
				return nil, merr
			}
			sbd := maintainer.(interface {
				ScanByDistance(TupleRange, []byte, ScanProperties) RecordCursor[*IndexEntry]
			})
			cursor := sbd.ScanByDistance(TupleRange{
				Low:  tuple.Tuple{SerializeVector([]float64{15, 15})},
				High: tuple.Tuple{int64(5)},
			}, nil, ScanProperties{})
			n := 0
			for {
				res, cerr := cursor.OnNext(ctx)
				if cerr != nil {
					return nil, cerr
				}
				if !res.HasNext() {
					break
				}
				n++
			}
			Expect(n).To(Equal(5), "S=1 fallback build must produce a queryable index")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
