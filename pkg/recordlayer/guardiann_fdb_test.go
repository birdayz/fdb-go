package recordlayer

import (
	"context"
	"errors"
	"fmt"
	"math/rand"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/vectorcodec"
)

type countingListener struct{ enqueued, executed int }

func (l *countingListener) onTaskEnqueued() { l.enqueued++ }
func (l *countingListener) onTaskExecuted() { l.executed++ }

// guardiannSnapshot reads the structure's bookkeeping back.
type guardiannSnapshot struct {
	clusters  map[tuple.UUID]guardiannClusterMetadata
	primaries map[tuple.UUID]int // primary references per cluster
	tasks     int
	centroids int
	vectors   int
	collapsed int
}

func readGuardiann(tx fdb.ReadTransaction, g *guardiann) guardiannSnapshot {
	s := guardiannSnapshot{clusters: map[tuple.UUID]guardiannClusterMetadata{}, primaries: map[tuple.UUID]int{}}
	count := func(ss subspace.Subspace) []fdb.KeyValue {
		r, err := fdb.PrefixRange(ss.Bytes())
		Expect(err).NotTo(HaveOccurred())
		kvs, err := tx.GetRange(r, fdb.RangeOptions{}).GetSliceWithError()
		Expect(err).NotTo(HaveOccurred())
		return kvs
	}
	for _, kv := range count(g.sub(gSubClusterMetadata)) {
		k, err := g.sub(gSubClusterMetadata).Unpack(kv.Key)
		Expect(err).NotTo(HaveOccurred())
		m, err := g.fetchClusterMetadata(tx, k[0].(tuple.UUID))
		Expect(err).NotTo(HaveOccurred())
		s.clusters[m.id] = *m
	}
	for _, kv := range count(g.sub(gSubVectorRefs)) {
		k, err := g.sub(gSubVectorRefs).Unpack(kv.Key)
		Expect(err).NotTo(HaveOccurred())
		ref, err := vectorRefFromValue(k[1].(tuple.Tuple), kv.Value)
		Expect(err).NotTo(HaveOccurred())
		if ref.primary {
			s.primaries[k[0].(tuple.UUID)]++
		}
	}
	s.tasks = len(count(g.sub(gSubTasks)))
	s.centroids = len(count(g.centroids.storage.dataSubspace.Sub(int64(0))))
	s.vectors = len(count(g.sub(gSubVectorMetadata)))
	s.collapsed = len(count(g.sub(gSubCollapsed)))
	return s
}

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
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
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

var _ = Describe("GuardiANN scan options", func() {
	ctx := context.Background()
	DescribeTable("applies per-scan search options and fans out over a partial prefix", func(useRaBitQ bool) {
		ks := specSubspace()
		vecIdx := NewVectorIndex("vec_guardiann_opts", KeyWithValue(Concat(Field("quantity"), Field("price")), 1), 1)
		vecIdx.Options[IndexOptionVectorEngine] = "GUARDIANN"
		vecIdx.Options[IndexOptionHNSWUseRaBitQ] = fmt.Sprint(useRaBitQ)
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.AddIndex("Order", vecIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			for _, o := range []struct{ id, price, qty int64 }{{1, 10, 1}, {2, 20, 1}, {3, 50, 2}} {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(o.id), Price: proto.Int32(int32(o.price)), Quantity: proto.Int32(int32(o.qty))})
				Expect(err).NotTo(HaveOccurred())
			}
			ids := func(prefix tuple.Tuple, opts VectorIndexScanOptions) ([]int64, error) {
				cursor := store.ScanVectorIndexWithOptions(vecIdx, prefix, []float64{15}, 10, opts, nil, ForwardScan())
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
			got, err := ids(tuple.Tuple{int64(1)}, VectorIndexScanOptions{GuardiannSearchMaxClusters: &one})
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(ConsistOf(int64(1), int64(2)))
			got, err = ids(nil, VectorIndexScanOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(ConsistOf(int64(1), int64(2), int64(3)), "every partition of a partial prefix")
			for _, include := range []bool{true, false} {
				for _, prefix := range []tuple.Tuple{nil, {int64(1)}} {
					got, err := ids(prefix, VectorIndexScanOptions{ReturnVectors: &include})
					Expect(err).NotTo(HaveOccurred())
					if prefix == nil {
						Expect(got).To(ConsistOf(int64(1), int64(2), int64(3)))
					} else {
						Expect(got).To(ConsistOf(int64(1), int64(2)))
					}
				}
			}
			half := 0.5
			_, err = ids(tuple.Tuple{int64(1)}, VectorIndexScanOptions{GuardiannCandidatePoolFactor: &half})
			var iae *IllegalArgumentError
			Expect(errors.As(err, &iae)).To(BeTrue(), "%v", err)
			Expect(iae.Message).To(Equal("candidatePoolFactor must be >= 1.0"))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	}, Entry("without RaBitQ", false), Entry("with RaBitQ configured before training", true))
})
