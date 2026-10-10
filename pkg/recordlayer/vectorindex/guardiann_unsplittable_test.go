package vectorindex

import (
	"context"
	"errors"
	"math/rand"
	"testing"

	"fdb.dev/pkg/recordlayer"

	. "github.com/onsi/ginkgo/v2"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/vectorcodec"
	. "github.com/onsi/gomega"
)

// The peel's admission edges: W = floor(log2(n - 1)) * max(n, 128) * max(d, 128) *
// max(I(R+1)/32, (R+1)(2I+2)/72, 1) against B = 1.96e7. Lowering B, dropping
// the check, either knob term, the knob floor or either size floor reddens a row.
func TestGuardiannPeelAdmission(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		n, d, iterations, restarts int
		want                       bool
	}{
		{2000, 980, 8, 3, true},   // W = B exactly
		{2000, 981, 8, 3, false},  // one dimension past it
		{1001, 2175, 8, 3, true},  // the first over-max size: 19,594,575
		{1001, 2176, 8, 3, false}, // refused
		{1001, 2048, 8, 3, true},  // the d = 2048 fixture shape
		{2000, 768, 8, 3, true},   // 768-dimensional embeddings at the hard cap
		{2000, 768, 16, 3, false}, // twice the iterations doubles W: the knob factor
		{2000, 490, 16, 3, true},  // W = B at twice the iterations
		{2000, 500, 16, 3, false}, // the iteration term refuses what the pass term admits
		{2000, 4096, 1, 0, false}, // smaller knobs never enlarge admission: the floor
		{2000, 551, 1, 31, true},  // 32 single-iteration restarts: 128 passes, f = 16/9
		{2000, 552, 1, 31, false}, // the pass term refuses what the iteration term admits
		{2000, 1, 1, 136, true},   // d is priced at no less than 128: the most restarts at I = 1
		{2000, 1, 1, 137, false},  // without the floor, d = 1 would admit R = 17639
		{3, 1, 1, 21532, true},    // n is priced at no less than 128 too
		{3, 1, 1, 21533, false},   // without that floor, n = 3 would admit nearly a million
		{2000, 4096, 8, 3, false},
		{2, 1 << 20, 8, 3, true}, // log2(1) = 0: one pair always peels
		{1, 4, 8, 3, false},
	} {
		if got := peelAdmitted(c.n, c.d, c.iterations, c.restarts); got != c.want {
			t.Errorf("peelAdmitted(n=%d, d=%d, I=%d, R=%d) = %v, want %v", c.n, c.d, c.iterations, c.restarts, got, c.want)
		}
	}
}

// Java throws where a split has no usable candidate (SplitMergeTask.java:397)
// or a candidate holds fewer cleaned vectors than k (KMeans.java:136), and the
// task fails the same way forever. Go peels outliers off an admitted cluster,
// else reconciles it in place, and fails with ClusterUnsplittableError only
// when the reconciled cluster is above the hard cap (DIVERGENCES.md, "GuardiANN
// splits what Java cannot").
var _ = Describe("GuardiANN unsplittable clusters", func() {
	ctx := context.Background()
	config := func() guardiannConfig {
		cfg := defaultGuardiannConfig(4)
		cfg.primaryClusterMin, cfg.primaryClusterMax, cfg.primaryClusterHardMax = 2, 10, 14
		cfg.collapseMinDuplicates = 6
		cfg.minChildFraction = 0.1
		cfg.deterministicRandomness = true
		Expect(cfg.validate()).To(Succeed())
		return cfg
	}
	// A tight core of n points and one far outlier: KMeans k=2 isolates the
	// outlier, a child of 1/(n+1) < minChildFraction, so every candidate of a
	// lone cluster is INVALID.
	outlierShape := func(n int) map[int64]gVector {
		rnd := rand.New(rand.NewSource(11))
		out := map[int64]gVector{}
		for i := 0; i < n; i++ {
			v := make([]float64, 4)
			for d := range v {
				v[d] = rnd.NormFloat64() * 0.01
			}
			out[int64(i)] = gVector{data: v, typ: vectorcodec.TypeDouble}
		}
		out[int64(n)] = gVector{data: []float64{100, 100, 100, 100}, typ: vectorcodec.TypeDouble}
		return out
	}
	runner := func(ss []byte, cfg guardiannConfig) func(func(g *guardiann, tx fdb.WritableTransaction) error) error {
		return func(f func(g *guardiann, tx fdb.WritableTransaction) error) error {
			_, err := sharedDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				g := newGuardiann(specSubspace().Sub(string(ss)), cfg, nil, nil)
				return nil, f(g, rtx.Transaction())
			})
			return err
		}
	}
	findsAll := func(run func(func(g *guardiann, tx fdb.WritableTransaction) error) error, vectors map[int64]gVector) {
		Expect(run(func(g *guardiann, tx fdb.WritableTransaction) error {
			for i, v := range vectors {
				res, err := g.search(tx, 1, defaultGuardiannSearchConfig(), v)
				Expect(err).NotTo(HaveOccurred())
				Expect(res).NotTo(BeEmpty())
				Expect(res[0].primaryKey).To(Equal(tuple.Tuple{i}), "vector %d", i)
			}
			return nil
		})).To(Succeed())
	}

	drain := func(run func(func(g *guardiann, tx fdb.WritableTransaction) error) error) {
		for round := 0; round < 50; round++ {
			var n int
			Expect(run(func(g *guardiann, tx fdb.WritableTransaction) error {
				var err error
				n, err = g.executeDeferredTasks(tx, 5, g.env.Now().Add(1<<40))
				return err
			})).To(Succeed())
			if n == 0 {
				return
			}
		}
		Fail("deferred tasks did not drain")
	}

	for _, inline := range []bool{false, true} {
		mode := map[bool]string{false: "deferred", true: "inline"}[inline]
		It("peels the outlier off a lone over-max cluster and splits it ("+mode+")", func() {
			cfg := config()
			run := runner([]byte("peel-"+mode), cfg)
			vectors := outlierShape(10)
			Expect(run(func(g *guardiann, tx fdb.WritableTransaction) error {
				for i := int64(0); i <= 10; i++ {
					if err := g.insert(tx, tuple.Tuple{i}, vectors[i], nil, inline); err != nil {
						return err
					}
				}
				return nil
			})).To(Succeed())
			drain(run)
			var snap guardiannSnapshot
			Expect(run(func(g *guardiann, tx fdb.WritableTransaction) error {
				snap = readGuardiann(tx, g)
				return nil
			})).To(Succeed())
			Expect(snap.tasks).To(Equal(0))
			Expect(len(snap.clusters)).To(BeNumerically(">=", 2), "the peel split the cluster")
			for id, m := range snap.clusters {
				Expect(m.numPrimary()).To(Equal(snap.primaries[id]))
				Expect(m.numPrimary()).To(BeNumerically("<=", cfg.primaryClusterMax))
				Expect(m.has(clusterStateSplitMerge)).To(BeFalse())
			}
			findsAll(run, vectors)
		})
	}

	It("selects the peel's first usable refit and pins its exit", func() {
		cfg := config()
		run := runner([]byte("peel-exits"), cfg)
		vectors := outlierShape(10)
		Expect(run(func(g *guardiann, tx fdb.WritableTransaction) error {
			primaries := make([]guardiannVectorRef, 0, len(vectors))
			current := guardiannCluster{centroid: gVector{data: make([]float64, 4), typ: vectorcodec.TypeDouble}}
			for i := int64(0); i <= 10; i++ {
				r := guardiannVectorRef{id: guardiannVectorID{pk: tuple.Tuple{i}}, vector: vectors[i], primary: true}
				primaries = append(primaries, r)
				current.refs = append(current.refs, r)
			}
			c12, err := g.kMeansCandidate(&clusterClassification{}, primaries, newSplittableRandomForUUID(tuple.UUID{1}), 2)
			Expect(err).NotTo(HaveOccurred())
			result, err := g.scoreCandidate([]guardiannCluster{current}, c12)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.decision).To(Equal(decisionInvalidCandidate), "the target's own candidate isolates the outlier")
			cand, exit, err := g.peelSplit(newSplittableRandomForUUID(tuple.UUID{2}), current, c12)
			Expect(err).NotTo(HaveOccurred())
			Expect(exit).To(Equal(peelSelected))
			sizes := [2]int{}
			for _, a := range cand.kMeans.assignment {
				sizes[a]++
			}
			Expect(min(sizes[0], sizes[1])).To(BeNumerically(">=", 2), "both children hold at least minChildFraction of n")

			// A coordinate-identical mass: KMeans cannot separate it, so the
			// undersized child holds none of the mass and the peel stops.
			same := make([]guardiannVectorRef, 3)
			for i := range same {
				same[i] = guardiannVectorRef{
					id: guardiannVectorID{pk: tuple.Tuple{int64(i)}}, primary: true,
					vector: gVector{data: []float64{1, 1, 1, 1}, typ: vectorcodec.TypeDouble},
				}
			}
			stuck := &repartitioningCandidate{primaries: same, kMeans: kMeansResult{
				centroids: []gVector{same[0].vector, same[0].vector}, assignment: []int{0, 0, 0},
			}}
			_, exit, err = g.peelSplit(newSplittableRandomForUUID(tuple.UUID{3}), current, stuck)
			Expect(err).NotTo(HaveOccurred())
			Expect(exit).To(Equal(peelUndersizedChildHoldsNoMass))

			// Two members: removing the undersized one leaves fewer than two.
			pair := &repartitioningCandidate{primaries: primaries[9:], kMeans: kMeansResult{
				centroids: []gVector{vectors[9], vectors[10]}, assignment: []int{0, 1},
			}}
			_, exit, err = g.peelSplit(newSplittableRandomForUUID(tuple.UUID{4}), current, pair)
			Expect(err).NotTo(HaveOccurred())
			Expect(exit).To(Equal(peelMassBelowTwo))
			return nil
		})).To(Succeed())
	})

	It("reconciles a stale-inflated cluster whose survivors are n<k instead of failing forever", func() {
		cfg := config()
		run := runner([]byte("reconcile-nk"), cfg)
		vectors := outlierShape(10)
		Expect(run(func(g *guardiann, tx fdb.WritableTransaction) error {
			for i := int64(0); i <= 10; i++ {
				if err := g.insert(tx, tuple.Tuple{i}, vectors[i], nil, false); err != nil {
					return err
				}
			}
			// Ten identities vanish without their references: only one live
			// primary is left, fewer than the 1->2 candidate's k.
			for i := int64(0); i < 10; i++ {
				tx.Clear(fdb.Key(g.sub(gSubVectorMetadata).Pack(tuple.Tuple{i})))
			}
			return nil
		})).To(Succeed())
		drain(run)
		Expect(run(func(g *guardiann, tx fdb.WritableTransaction) error {
			snap := readGuardiann(tx, g)
			Expect(snap.tasks).To(Equal(0))
			Expect(snap.clusters).To(HaveLen(1))
			for id, m := range snap.clusters {
				Expect(m.numPrimary()).To(Equal(1))
				Expect(snap.primaries[id]).To(Equal(1), "the stale references were removed")
				Expect(m.has(clusterStateSplitMerge)).To(BeFalse())
				Expect(m.maxEverPrimary).To(Equal(11), "the lifetime peak is unchanged")
			}
			return nil
		})).To(Succeed())
		findsAll(run, map[int64]gVector{10: vectors[10]})
	})

	It("drops a new cluster that final ownership leaves without a primary", func() {
		cfg := config()
		run := runner([]byte("drop-empty-child"), cfg)
		vectors := outlierShape(8)
		delete(vectors, 8)
		Expect(run(func(g *guardiann, tx fdb.WritableTransaction) error {
			for i := int64(0); i < 8; i++ {
				if err := g.insert(tx, tuple.Tuple{i}, vectors[i], nil, false); err != nil {
					return err
				}
			}
			return nil
		})).To(Succeed())
		var survivor tuple.UUID
		Expect(run(func(g *guardiann, tx fdb.WritableTransaction) error {
			snap := readGuardiann(tx, g)
			Expect(snap.clusters).To(HaveLen(1))
			var target guardiannClusterMetadata
			for _, m := range snap.clusters {
				target = m
			}
			cluster, err := g.fetchCluster(tx, target.id, gVector{data: make([]float64, 4), typ: vectorcodec.TypeDouble})
			Expect(err).NotTo(HaveOccurred())
			// A KMeans child whose centroid is far from every primary: Java
			// asserts it holds a primary (SplitMergeTask.java:909) and fails.
			cand := &repartitioningCandidate{
				cls:       &clusterClassification{core: []guardiannClusterWithDistance{{meta: target, centroid: cluster.centroid}}},
				primaries: cluster.refs,
				kMeans: kMeansResult{centroids: []gVector{
					{data: make([]float64, 4), typ: vectorcodec.TypeDouble},
					{data: []float64{500, 500, 500, 500}, typ: vectorcodec.TypeDouble},
				}, assignment: make([]int, len(cluster.refs))},
			}
			return g.applyRepartitioning(tx, newSplittableRandomForUUID(tuple.UUID{6}), cand)
		})).To(Succeed())
		Expect(run(func(g *guardiann, tx fdb.WritableTransaction) error {
			snap := readGuardiann(tx, g)
			Expect(snap.clusters).To(HaveLen(1), "the empty child was not written")
			Expect(snap.centroids).To(Equal(1))
			for id, m := range snap.clusters {
				survivor = id
				Expect(m.numPrimary()).To(Equal(8))
				Expect(snap.primaries[id]).To(Equal(8))
			}
			tasks, err := g.fetchSomeTasks(tx, 100)
			Expect(err).NotTo(HaveOccurred())
			for _, t := range tasks {
				if t.kind == taskBounce {
					Expect(t.targets).To(Equal([]tuple.UUID{survivor}), "the bounce names only the surviving new cluster")
				}
			}
			return nil
		})).To(Succeed())
		drain(run)
		findsAll(run, vectors)
	})

	It("merges empty cores down to one retained cluster when every vector is deleted, then reinserts", func() {
		cfg := config()
		cfg.primaryClusterHardMax = 20
		run := runner([]byte("empty-core"), cfg)
		rnd := rand.New(rand.NewSource(5))
		vectors := map[int64]gVector{}
		for i := int64(0); i < 40; i++ {
			c := float64(i % 4 * 20)
			v := make([]float64, 4)
			for d := range v {
				v[d] = c + rnd.NormFloat64()
			}
			vectors[i] = gVector{data: v, typ: vectorcodec.TypeDouble}
		}
		for lo := int64(0); lo < 40; lo += 5 {
			Expect(run(func(g *guardiann, tx fdb.WritableTransaction) error {
				for i := lo; i < lo+5; i++ {
					if err := g.insert(tx, tuple.Tuple{i}, vectors[i], nil, false); err != nil {
						return err
					}
				}
				return nil
			})).To(Succeed())
			drain(run)
		}
		Expect(run(func(g *guardiann, tx fdb.WritableTransaction) error {
			Expect(len(readGuardiann(tx, g).clusters)).To(BeNumerically(">=", 4))
			return nil
		})).To(Succeed())
		for lo := int64(0); lo < 40; lo += 10 {
			Expect(run(func(g *guardiann, tx fdb.WritableTransaction) error {
				for i := lo; i < lo+10; i++ {
					if err := g.delete(tx, tuple.Tuple{i}, vectors[i], false); err != nil {
						return err
					}
				}
				return nil
			})).To(Succeed())
		}
		drain(run)
		Expect(run(func(g *guardiann, tx fdb.WritableTransaction) error {
			snap := readGuardiann(tx, g)
			Expect(snap.tasks).To(Equal(0))
			Expect(snap.clusters).To(HaveLen(1), "repeated empty merges end at one retained cluster")
			Expect(snap.centroids).To(Equal(1))
			for _, m := range snap.clusters {
				Expect(m.numPrimary()).To(Equal(0))
				Expect(m.states).To(Equal(0))
			}
			return nil
		})).To(Succeed())
		for lo := int64(0); lo < 40; lo += 5 {
			Expect(run(func(g *guardiann, tx fdb.WritableTransaction) error {
				for i := lo; i < lo+5; i++ {
					if err := g.insert(tx, tuple.Tuple{i}, vectors[i], nil, false); err != nil {
						return err
					}
				}
				return nil
			})).To(Succeed())
			drain(run)
		}
		findsAll(run, vectors)
	})

	It("fails a reconcile above the hard cap with ClusterUnsplittableError, not the capacity error", func() {
		cfg := config()
		run := runner([]byte("reconcile-hardcap"), cfg)
		vectors := outlierShape(15)
		Expect(run(func(g *guardiann, tx fdb.WritableTransaction) error {
			for i := int64(0); i <= 15; i++ {
				if err := g.insert(tx, tuple.Tuple{i}, vectors[i], nil, false); err != nil {
					// The deferred cap refuses the 15th primary.
					var capacity *guardiannClusterCapacityError
					Expect(errors.As(err, &capacity)).To(BeTrue(), "error: %v", err)
					break
				}
			}
			return nil
		})).To(Succeed())
		var target guardiannClusterMetadata
		var centroid gVector
		Expect(run(func(g *guardiann, tx fdb.WritableTransaction) error {
			snap := readGuardiann(tx, g)
			Expect(snap.clusters).To(HaveLen(1))
			for _, m := range snap.clusters {
				target = m
			}
			tasks, err := g.fetchSomeTasks(tx, 10)
			Expect(err).NotTo(HaveOccurred())
			Expect(tasks).NotTo(BeEmpty())
			centroid = tasks[0].centroid
			return nil
		})).To(Succeed())
		// One primary more than the hard cap reaches the cluster without the
		// cap check, as inline inserts and neighbour re-homing do.
		err := run(func(g *guardiann, tx fdb.WritableTransaction) error {
			extra := guardiannVectorRef{id: guardiannVectorID{pk: tuple.Tuple{int64(99)}, uuid: tuple.UUID{9}}, vector: vectors[0], primary: true}
			if err := g.writeVectorRef(tx, target.id, extra); err != nil {
				return err
			}
			g.writeVectorMetadata(tx, guardiannVectorMetadata{id: extra.id})
			return g.reconcileUnsplittable(tx, newSplittableRandomForUUID(tuple.UUID{5}), target, centroid, UnsplittableNoUsablePartition)
		})
		var unsplittable *ClusterUnsplittableError
		Expect(errors.As(err, &unsplittable)).To(BeTrue(), "error: %v", err)
		Expect(unsplittable.Cluster).To(Equal(target.id))
		Expect(unsplittable.Count).To(Equal(15))
		Expect(unsplittable.Limit).To(Equal(14))
		Expect(unsplittable.Cause).To(Equal(UnsplittableNoUsablePartition))
		var capacity *guardiannClusterCapacityError
		Expect(errors.As(err, &capacity)).To(BeFalse(), "the unsplittable error is not the insert cap's")
	})
})
