package vectorindex

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"time"

	"fdb.dev/pkg/recordlayer"

	. "github.com/onsi/ginkgo/v2"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/vectorcodec"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

type countingListener struct{ enqueued, executed int }

func (l *countingListener) onTaskEnqueued() { l.enqueued++ }
func (l *countingListener) onTaskExecuted() { l.executed++ }

var _ = Describe("GuardiANN structure", func() {
	ctx := context.Background()
	It("splits, reassigns, collapses and merges while every vector stays findable", func() {
		cfg := defaultGuardiannConfig(4)
		cfg.primaryClusterMin, cfg.primaryClusterMax, cfg.primaryClusterHardMax = 3, 12, 40
		cfg.collapseMinDuplicates = 6
		cfg.replicatedClusterTarget, cfg.replicatedClusterMaxWrites = 4, 8
		cfg.underreplicatedPrimaryClusterMax = 4
		cfg.deterministicRandomness = true
		cfg.minChildFraction = 0.05
		Expect(cfg.validate()).To(Succeed())
		ss := specSubspace().Sub("guardiann")
		listener := &countingListener{}
		g := newGuardiann(ss, cfg, nil, listener)
		run := func(f func(tx fdb.WritableTransaction) error) {
			_, err := sharedDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				g = newGuardiann(ss, cfg, nil, listener)
				return nil, f(rtx.Transaction())
			})
			Expect(err).NotTo(HaveOccurred())
		}
		rnd := rand.New(rand.NewSource(7))
		vectors := map[int64]gVector{}
		for i := int64(0); i < 160; i++ {
			v := make([]float64, 4)
			if i < 20 {
				v = []float64{5, 5, 5, 5} // duplicates, collapsed once a cluster holds enough
			} else {
				c := float64(i % 4 * 10)
				for d := range v {
					v[d] = c + rnd.NormFloat64()
				}
			}
			vectors[i] = gVector{data: v, typ: vectorcodec.TypeDouble}
		}
		for lo := int64(0); lo < 160; lo += 10 {
			run(func(tx fdb.WritableTransaction) error {
				for i := lo; i < lo+10; i++ {
					if err := g.insert(tx, tuple.Tuple{i}, vectors[i], nil, true); err != nil {
						return err
					}
				}
				return nil
			})
		}
		drain := func() {
			for round := 0; round < 200; round++ {
				var n int
				run(func(tx fdb.WritableTransaction) error {
					var err error
					n, err = g.executeDeferredTasks(tx, 5, g.env.Now().Add(1<<40))
					return err
				})
				if n == 0 {
					return
				}
			}
			Fail("deferred tasks did not drain")
		}
		check := func(live map[int64]gVector) {
			var snap guardiannSnapshot
			run(func(tx fdb.WritableTransaction) error {
				snap = readGuardiann(tx, g)
				return nil
			})
			Expect(snap.tasks).To(Equal(0))
			Expect(listener.enqueued-listener.executed).To(Equal(snap.tasks), "the task counts track the queue")
			Expect(snap.centroids).To(Equal(len(snap.clusters)), "one centroid per cluster")
			Expect(snap.vectors).To(Equal(len(live)))
			total := 0
			for id, m := range snap.clusters {
				Expect(snap.primaries[id]).To(Equal(m.numPrimary()), "cluster %s primary count", id)
				Expect(m.numUnderrep).To(Equal(snap.underrep[id]), "cluster %s underreplicated count", id)
				// A REASSIGN whose task found the cluster also SPLIT_MERGE is
				// consumed, and a split that then finds the cluster in bounds
				// clears only its own state (Java's ReassignTask.runTask and
				// SplitMergeTask.runTask), so REASSIGN may outlive its task.
				Expect(m.states&^clusterStateReassign).To(Equal(0), "cluster %s states", id)
				total += m.numPrimary()
			}
			Expect(total + snap.collapsed).To(BeNumerically(">=", len(live)))
			found := 0
			run(func(tx fdb.WritableTransaction) error {
				for pk, v := range live {
					res, err := g.search(tx, 5, defaultGuardiannSearchConfig(), v)
					if err != nil {
						return err
					}
					for _, r := range res {
						if r.primaryKey[0].(int64) == pk || r.distance == 0 {
							found++
							break
						}
					}
				}
				return nil
			})
			Expect(found).To(Equal(len(live)), "every live vector is its own nearest neighbour")
		}
		drain()
		var snap guardiannSnapshot
		run(func(tx fdb.WritableTransaction) error {
			snap = readGuardiann(tx, g)
			return nil
		})
		Expect(len(snap.clusters)).To(BeNumerically(">", 4), "clusters were split")
		Expect(snap.collapsed).To(BeNumerically(">", 0), "duplicates were collapsed")
		check(vectors)
		// Deleting most vectors merges clusters back.
		live := map[int64]gVector{}
		for pk, v := range vectors {
			live[pk] = v
		}
		for lo := int64(0); lo < 150; lo += 10 {
			run(func(tx fdb.WritableTransaction) error {
				for i := lo; i < lo+10; i++ {
					if err := g.delete(tx, tuple.Tuple{i}, vectors[i], true); err != nil {
						return fmt.Errorf("delete %d: %w", i, err)
					}
					delete(live, i)
				}
				return nil
			})
		}
		drain()
		run(func(tx fdb.WritableTransaction) error {
			snap = readGuardiann(tx, g)
			return nil
		})
		Expect(len(snap.clusters)).To(BeNumerically("<", 5), "clusters were merged")
		check(live)
	})
})

var _ = Describe("Vector scan options", func() {
	ctx := context.Background()
	DescribeTable("applies per-scan search options and fans out over a partial prefix", func(engine string, useRaBitQ bool) {
		ks := specSubspace()
		vecIdx := recordlayer.NewVectorIndex("vec_guardiann_opts", recordlayer.KeyWithValue(recordlayer.Concat(recordlayer.Field("quantity"), recordlayer.Field("price")), 1), 1)
		vecIdx.Options[recordlayer.IndexOptionVectorEngine] = engine
		vecIdx.Options[recordlayer.IndexOptionHNSWUseRaBitQ] = fmt.Sprint(useRaBitQ)
		builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
		builder.AddIndex("Order", vecIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			for _, o := range []struct{ id, price, qty int64 }{{1, 10, 1}, {2, 20, 1}, {3, 50, 2}} {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(o.id), Price: proto.Int32(int32(o.price)), Quantity: proto.Int32(int32(o.qty))})
				Expect(err).NotTo(HaveOccurred())
			}
			ids := func(prefix tuple.Tuple, opts recordlayer.VectorIndexScanOptions) ([]int64, error) {
				// Exercise the wire boundary before the real index reads,
				// including ReturnVectors, cluster limits, and invalid knobs.
				wire, err := opts.ToProto()
				if err != nil {
					return nil, err
				}
				opts, err = recordlayer.VectorIndexScanOptionsFromProto(wire)
				if err != nil {
					return nil, err
				}
				cursor := store.ScanVectorIndexWithOptions(vecIdx, prefix, []float64{15}, 10, opts, nil, recordlayer.ForwardScan())
				var out []int64
				for {
					r, err := cursor.OnNext(ctx)
					if err != nil {
						return nil, err
					}
					if !r.HasNext() {
						return out, nil
					}
					id := r.GetValue().Key[1].(int64)
					price := map[int64]float64{1: 10, 2: 20, 3: 50}[id]
					if opts.ReturnVectors != nil && !*opts.ReturnVectors || opts.ReturnVectors == nil && useRaBitQ {
						Expect(r.GetValue().Value).To(Equal(tuple.Tuple{nil}))
					} else {
						Expect(r.GetValue().Value).To(Equal(tuple.Tuple{vectorcodec.Serialize([]float64{price})}))
					}
					out = append(out, id)
				}
			}
			one := 1
			got, err := ids(tuple.Tuple{int64(1)}, recordlayer.VectorIndexScanOptions{GuardiannSearchMaxClusters: &one})
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(ConsistOf(int64(1), int64(2)))
			got, err = ids(nil, recordlayer.VectorIndexScanOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(ConsistOf(int64(1), int64(2), int64(3)), "every partition of a partial prefix")
			for _, include := range []bool{true, false} {
				for _, prefix := range []tuple.Tuple{nil, {int64(1)}} {
					got, err := ids(prefix, recordlayer.VectorIndexScanOptions{ReturnVectors: &include})
					Expect(err).NotTo(HaveOccurred())
					if prefix == nil {
						Expect(got).To(ConsistOf(int64(1), int64(2), int64(3)))
					} else {
						Expect(got).To(ConsistOf(int64(1), int64(2)))
					}
				}
			}
			if engine == "HNSW" {
				return nil, nil
			}
			half := 0.5
			_, err = ids(tuple.Tuple{int64(1)}, recordlayer.VectorIndexScanOptions{GuardiannCandidatePoolFactor: &half})
			var iae *recordlayer.IllegalArgumentError
			Expect(errors.As(err, &iae)).To(BeTrue(), "%v", err)
			Expect(iae.Message).To(Equal("candidatePoolFactor must be >= 1.0"))
			nan := math.NaN()
			_, err = ids(tuple.Tuple{int64(1)}, recordlayer.VectorIndexScanOptions{GuardiannCandidatePoolFactor: &nan})
			Expect(errors.As(err, &iae)).To(BeTrue(), "%v", err)
			Expect(iae.Message).To(Equal("candidatePoolFactor must be >= 1.0"))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	}, Entry("GuardiANN without RaBitQ", "GUARDIANN", false), Entry("GuardiANN before training", "GUARDIANN", true), Entry("HNSW without RaBitQ", "HNSW", false), Entry("HNSW before training", "HNSW", true))
})

var _ = Describe("GuardiANN training", func() {
	It("accumulates samples across transactions and clears them when the centroid is trained", func() {
		ctx := context.Background()
		ss := specSubspace().Sub("training")
		cfg := defaultGuardiannConfig(3)
		cfg.useRaBitQ, cfg.deterministicRandomness = true, true
		cfg.sampleVectorStatsProbability, cfg.maintainStatsProbability = 1, 1
		cfg.sampleBatchSize, cfg.statsThreshold = 2, 3
		for i, data := range [][]float64{{1, 2, 3}, {4, 5, 6}, {7, 8, 9}} {
			_, err := sharedDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				g := newGuardiann(ss, cfg, nil, nil)
				tx := rtx.Transaction()
				info, err := g.fetchAccessInfo(tx)
				Expect(err).NotTo(HaveOccurred())
				if info == nil {
					info = &guardiannAccessInfoValue{rotatorSeed: -1}
					g.writeAccessInfo(tx, info)
				}
				err = g.addToStatsIfNecessary(tx, newSplittableRandomForKey(tuple.Tuple{int64(i)}), info, gVector{data: data, typ: 2})
				Expect(err).NotTo(HaveOccurred())
				info, err = g.fetchAccessInfo(tx)
				Expect(err).NotTo(HaveOccurred())
				r, err := fdb.PrefixRange(g.sub(gSubSamples).Bytes())
				Expect(err).NotTo(HaveOccurred())
				samples, err := tx.GetRange(r, fdb.RangeOptions{}).GetSliceWithError()
				Expect(err).NotTo(HaveOccurred())
				if i < 2 {
					Expect(info.negatedCentroid).To(BeNil())
					Expect(samples).To(HaveLen(1))
					key, err := g.sub(gSubSamples).Unpack(samples[0].Key)
					Expect(err).NotTo(HaveOccurred())
					Expect(key[0]).To(Equal(int64(i + 1)))
					Expect(key[1]).To(BeAssignableToTypeOf(tuple.UUID{}))
				} else {
					Expect(samples).To(BeEmpty())
					Expect(info.negatedCentroid).To(HaveLen(3))
					original := newFhtKacRotator(info.rotatorSeed, 3, 10).transposedApply(info.negatedCentroid)
					for j, want := range []float64{-4, -5, -6} {
						Expect(original[j]).To(BeNumerically("~", want, 1e-12))
					}
					err = g.addToStatsIfNecessary(tx, newSplittableRandomForKey(tuple.Tuple{int64(9)}), info, gVector{data: []float64{99, 99, 99}, typ: 2})
					Expect(err).NotTo(HaveOccurred())
					samples, err = tx.GetRange(r, fdb.RangeOptions{}).GetSliceWithError()
					Expect(err).NotTo(HaveOccurred())
					Expect(samples).To(BeEmpty())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		}
	})
})

var _ = Describe("GuardiANN trained search", func() {
	It("reads mixed pretraining HALF and Java RaBitQ references in one cluster", func() {
		cfg := defaultGuardiannConfig(3)
		cfg.useRaBitQ, cfg.deterministicRandomness = true, true
		cfg.metric = VectorMetricEuclideanSquare
		ss := specSubspace().Sub("mixed-trained")
		_, err := sharedDB.Run(context.Background(), func(rtx *recordlayer.FDBRecordContext) (any, error) {
			g := newGuardiann(ss, cfg, nil, nil)
			tx := rtx.Transaction()
			v, err := decodeGVector(vectorcodec.SerializeHalf([]float64{1, 2, 3}))
			Expect(err).NotTo(HaveOccurred())
			for _, pk := range []int64{1, 2} {
				Expect(g.insert(tx, tuple.Tuple{pk}, v, nil, false)).To(Succeed())
			}
			g.writeAccessInfo(tx, &guardiannAccessInfoValue{rotatorSeed: 42, negatedCentroid: []float64{-0.25, 0.5, -0.75}})
			r, err := fdb.PrefixRange(g.sub(gSubVectorRefs).Bytes())
			Expect(err).NotTo(HaveOccurred())
			refs, err := tx.GetRange(r, fdb.RangeOptions{}).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			Expect(refs).To(HaveLen(2))
			for _, kv := range refs {
				key, err := g.sub(gSubVectorRefs).Unpack(kv.Key)
				Expect(err).NotTo(HaveOccurred())
				if key[1].(tuple.Tuple)[0] == int64(1) {
					value, err := tuple.Unpack(kv.Value)
					Expect(err).NotTo(HaveOccurred())
					value[3], err = hex.DecodeString("0340218a6f8ff36398bfd24f6f0e0ad5b63fc34edb3de0b770fb7a")
					Expect(err).NotTo(HaveOccurred())
					tx.Set(kv.Key, value.Pack())
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(context.Background(), func(rtx *recordlayer.FDBRecordContext) (any, error) {
			g := newGuardiann(ss, cfg, nil, nil)
			results, err := g.search(rtx.Transaction(), 2, defaultGuardiannSearchConfig(), gVector{data: []float64{1.5, 2.5, 3.5}, typ: 2})
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(2))
			Expect(results[0].primaryKey).To(Equal(tuple.Tuple{int64(2)}))
			Expect(results[0].distance).To(BeNumerically("~", 0.75, 1e-14))
			Expect(results[1].primaryKey).To(Equal(tuple.Tuple{int64(1)}))
			Expect(results[1].distance).To(BeNumerically("~", 0.7715969549182908, 1e-14))
			Expect(hex.EncodeToString(results[1].vector.encode())).To(Equal("023fee714ee8af20254000392c4f941c064007fca06b941fd5"))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

var _ = Describe("GuardiANN automatic training", func() {
	It("trains during inserts and maintains mixed plain and quantized references", func() {
		cfg := defaultGuardiannConfig(3)
		cfg.useRaBitQ, cfg.deterministicRandomness = true, true
		cfg.sampleVectorStatsProbability, cfg.maintainStatsProbability = 1, 1
		cfg.sampleBatchSize, cfg.statsThreshold = 2, 8
		cfg.primaryClusterMin, cfg.primaryClusterMax, cfg.primaryClusterHardMax = 2, 6, 40
		ss := specSubspace().Sub("automatic-training")
		g := newGuardiann(ss, cfg, nil, nil)
		run := func(f func(fdb.WritableTransaction) error) {
			_, err := sharedDB.Run(context.Background(), func(rtx *recordlayer.FDBRecordContext) (any, error) { return nil, f(rtx.Transaction()) })
			Expect(err).NotTo(HaveOccurred())
		}
		for i := int64(0); i < 16; i++ {
			run(func(tx fdb.WritableTransaction) error {
				v := gVector{data: []float64{float64(i / 4 * 10), float64(i % 4), 1}, typ: 2}
				if err := g.insert(tx, tuple.Tuple{i}, v, nil, false); err != nil {
					return err
				}
				info, err := g.fetchAccessInfo(tx)
				Expect(err).NotTo(HaveOccurred())
				if i >= 7 {
					Expect(info.negatedCentroid).To(HaveLen(3))
				} else {
					Expect(info.negatedCentroid).To(BeNil())
				}
				if i == 6 {
					tasks, err := g.fetchSomeTasks(tx, 10)
					Expect(err).NotTo(HaveOccurred())
					Expect(tasks).NotTo(BeEmpty(), "pretraining tasks must survive the coordinate transition")
				}
				return nil
			})
		}
		run(func(tx fdb.WritableTransaction) error {
			r, err := fdb.PrefixRange(g.sub(gSubVectorRefs).Bytes())
			Expect(err).NotTo(HaveOccurred())
			refs, err := tx.GetRange(r, fdb.RangeOptions{}).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			plain, encoded := 0, 0
			for _, kv := range refs {
				v, err := tuple.Unpack(kv.Value)
				Expect(err).NotTo(HaveOccurred())
				if v[3].([]byte)[0] == 3 {
					encoded++
				} else {
					plain++
				}
			}
			Expect(plain).To(BeNumerically(">", 0))
			Expect(encoded).To(BeNumerically(">", 0))
			return nil
		})
		for n := 0; n < 100; n++ {
			count := 0
			run(func(tx fdb.WritableTransaction) error {
				var err error
				count, err = g.executeDeferredTasks(tx, 5, time.Time{})
				return err
			})
			if count == 0 {
				break
			}
			Expect(n).To(BeNumerically("<", 99), "maintenance must quiesce")
		}
		run(func(tx fdb.WritableTransaction) error {
			multiple, err := g.centroidCardinalityMultiple(tx)
			Expect(err).NotTo(HaveOccurred())
			Expect(multiple).To(BeTrue(), "maintenance must split the overfull cluster")
			sc := defaultGuardiannSearchConfig()
			sc.searchMinClustersBeforePruning, sc.searchMaxClusters = 100, 100
			results, err := g.search(tx, 16, sc, gVector{data: []float64{10, 1, 1}, typ: 2})
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(16))
			seen := map[int64]bool{}
			for _, r := range results {
				seen[r.primaryKey[0].(int64)] = true
			}
			Expect(seen).To(HaveLen(16))
			for _, i := range []int64{0, 12} {
				Expect(g.delete(tx, tuple.Tuple{i}, gVector{data: []float64{float64(i / 4 * 10), float64(i % 4), 1}, typ: 2}, true)).To(Succeed())
			}
			results, err = g.search(tx, 16, sc, gVector{data: []float64{10, 1, 1}, typ: 2})
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(14))
			for _, r := range results {
				Expect(r.primaryKey[0]).NotTo(BeElementOf(int64(0), int64(12)))
			}
			return nil
		})
	})
})
