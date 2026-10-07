package recordlayer

import (
	"context"
	"math/rand"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/vectorcodec"
)

// The peel's acceptance fixtures (ws-d-design.md, "Acceptance fixtures"): real
// FDB, default KMeans knobs, HALF precision, no RaBitQ, the tight core with 50
// outliers at the shapes the admission is chosen to cover. FDB's 5 s
// transaction window is a time limit, so each split asserts through the
// attempt observer that it commits in ONE attempt: a 1007 hidden behind a retry
// would otherwise pass. The peel's own duration at these shapes is gated by
// TestGuardiannPeelPerformanceCriterion.
var _ = Describe("GuardiANN peel acceptance fixtures", func() {
	ctx := context.Background()

	outlierCluster := func(n, d int, seed int64) []gVector {
		rnd := rand.New(rand.NewSource(seed))
		out := make([]gVector, n)
		for i := range out {
			v := make([]float64, d)
			for j := range v {
				v[j] = rnd.NormFloat64() * 0.01
			}
			if i >= n-50 {
				v[(i*7)%d] += 50
			}
			half, err := vectorcodec.Deserialize(vectorcodec.SerializeHalf(v))
			Expect(err).NotTo(HaveOccurred())
			out[i] = gVector{data: half, typ: vectorcodec.TypeHalf}
		}
		return out
	}

	type fixture struct {
		db       *FDBDatabase
		g        func() *guardiann
		attempts map[uint64]int // executions per attempt-loop call
		failed   map[uint64]bool
	}
	setup := func(name string, d, iterations int) *fixture {
		cfg := defaultGuardiannConfig(d)
		cfg.kMeansMaxIterations = iterations
		cfg.deterministicRandomness = true
		Expect(cfg.validate()).To(Succeed())
		ss := specSubspace().Sub(name)
		f := &fixture{
			db:       NewFDBDatabaseWithTransactor(sharedDB.transactor, sharedDB.db),
			g:        func() *guardiann { return newGuardiann(ss, cfg, nil, nil) },
			attempts: map[uint64]int{},
			failed:   map[uint64]bool{},
		}
		f.db.SetAttemptObserver(func(call AttemptCall, err error) {
			f.attempts[call.CallID]++
			if err != nil {
				f.failed[call.CallID] = true
			}
		})
		return f
	}
	run := func(f *fixture, body func(g *guardiann, tx fdb.WritableTransaction) error) {
		_, err := f.db.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			return nil, body(f.g(), rtx.Transaction())
		})
		Expect(err).NotTo(HaveOccurred())
	}
	// insertAll inserts the vectors deferred (the split tasks queue), or
	// inline on the last one only, which then splits the full cluster.
	insertAll := func(f *fixture, vectors []gVector, inlineLast bool) {
		const batch = 200
		for start := 0; start < len(vectors); start += batch {
			end := min(start+batch, len(vectors))
			run(f, func(g *guardiann, tx fdb.WritableTransaction) error {
				for i := start; i < end; i++ {
					inline := inlineLast && i == len(vectors)-1
					if err := g.insert(tx, tuple.Tuple{int64(i)}, vectors[i], nil, inline); err != nil {
						return err
					}
				}
				return nil
			})
		}
	}
	drain := func(f *fixture) {
		for round := 0; round < 200; round++ {
			var n int
			run(f, func(g *guardiann, tx fdb.WritableTransaction) error {
				var err error
				n, err = g.executeDeferredTasks(tx, 1, g.env.Now().Add(1<<40))
				return err
			})
			if n == 0 {
				return
			}
		}
		Fail("deferred tasks did not drain")
	}
	snapshot := func(f *fixture) guardiannSnapshot {
		var snap guardiannSnapshot
		run(f, func(g *guardiann, tx fdb.WritableTransaction) error {
			snap = readGuardiann(tx, g)
			return nil
		})
		return snap
	}
	expectOneAttemptEach := func(f *fixture) {
		for call, n := range f.attempts {
			Expect(f.failed[call]).To(BeFalse(), "an attempt of call %d failed: a split that misses the transaction window retries", call)
			Expect(n).To(Equal(1), "call %d took %d attempts", call, n)
		}
	}
	expectSplit := func(f *fixture, n int) {
		snap := snapshot(f)
		Expect(snap.tasks).To(Equal(0))
		Expect(len(snap.clusters)).To(BeNumerically(">=", 2), "the peel split the over-max cluster")
		total := 0
		for id, m := range snap.clusters {
			Expect(m.numPrimary()).To(Equal(snap.primaries[id]))
			Expect(m.numPrimary()).To(BeNumerically("<=", defaultGuardiannConfig(1).primaryClusterMax))
			total += m.numPrimary()
		}
		Expect(total).To(Equal(n))
	}

	It("splits the d = 768 tight cluster at the hard cap by a deferred drain, each transaction in one attempt", func() {
		const n, d = 2000, 768
		f := setup("d768-deferred", d, 8)
		insertAll(f, outlierCluster(n, d, 1), false)
		start := time.Now()
		drain(f)
		GinkgoWriter.Printf("d=768 n=2000 deferred drain: %v\n", time.Since(start))
		expectOneAttemptEach(f)
		expectSplit(f, n)
	})

	It("splits the d = 768 tight cluster at the hard cap by an inline insert, in one attempt", func() {
		const n, d = 2000, 768
		f := setup("d768-inline", d, 8)
		insertAll(f, outlierCluster(n, d, 2), true)
		drain(f)
		expectOneAttemptEach(f)
		expectSplit(f, n)
	})

	It("splits the d = 2048 first over-max cluster by a deferred drain, in one attempt", func() {
		const n, d = 1001, 2048
		f := setup("d2048-deferred", d, 8)
		insertAll(f, outlierCluster(n, d, 3), false)
		start := time.Now()
		drain(f)
		GinkgoWriter.Printf("d=2048 n=1001 deferred drain: %v\n", time.Since(start))
		expectOneAttemptEach(f)
		expectSplit(f, n)
	})

	It("refuses the peel at twice the KMeans iterations (the knob factor) and reconciles the cluster at the hard cap in place", func() {
		const n, d = 2000, 768
		f := setup("d768-i16", d, 16)
		insertAll(f, outlierCluster(n, d, 4), false)
		drain(f)
		expectOneAttemptEach(f)
		snap := snapshot(f)
		Expect(snap.tasks).To(Equal(0))
		Expect(snap.clusters).To(HaveLen(1), "admission refused the peel, so the cluster stays whole")
		for id, m := range snap.clusters {
			Expect(m.numPrimary()).To(Equal(n))
			Expect(snap.primaries[id]).To(Equal(n))
		}
	})
})
