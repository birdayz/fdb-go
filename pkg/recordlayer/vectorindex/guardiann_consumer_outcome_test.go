package vectorindex

import (
	"context"
	"errors"

	"fdb.dev/pkg/recordlayer"

	. "github.com/onsi/ginkgo/v2"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/vectorcodec"
	. "github.com/onsi/gomega"
)

// Declared (d): an inline delete runs no task whose head task a Go consumer
// would refuse (consumerOutcome), and only then; the task stays queued and
// counted and the next drain meets the refusal. Java's delete fails there.
var _ = Describe("GuardiANN inline delete and the head task's consumer outcome", func() {
	ctx := context.Background()
	base := func() guardiannConfig {
		cfg := defaultGuardiannConfig(2)
		cfg.primaryClusterMin, cfg.primaryClusterMax, cfg.primaryClusterHardMax = 3, 12, 40
		cfg.collapseMinDuplicates = 6
		cfg.deterministicRandomness = true
		Expect(cfg.validate()).To(Succeed())
		return cfg
	}
	run := func(ss subspace.Subspace, cfg guardiannConfig, f func(g *guardiann, tx fdb.WritableTransaction) error) error {
		_, err := sharedDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			return nil, f(newGuardiann(ss, cfg, nil, nil), rtx.Transaction())
		})
		return err
	}
	vector := func(i int64) gVector {
		return gVector{data: []float64{float64(i), float64(i % 3)}, typ: vectorcodec.TypeDouble}
	}
	// Fourteen vectors over a maximum of twelve queue one split that stays due
	// after one delete.
	queueSplit := func(ss subspace.Subspace, cfg guardiannConfig) {
		Expect(run(ss, cfg, func(g *guardiann, tx fdb.WritableTransaction) error {
			for i := int64(1); i <= 14; i++ {
				if err := g.insert(tx, tuple.Tuple{i}, vector(i), nil, false); err != nil {
					return err
				}
			}
			return nil
		})).To(Succeed())
	}
	head := func(ss subspace.Subspace, cfg guardiannConfig) (consumerOutcome, int) {
		var out consumerOutcome
		var n int
		Expect(run(ss, cfg, func(g *guardiann, tx fdb.WritableTransaction) error {
			tasks, err := g.fetchSomeTasks(tx.Snapshot(), 100)
			if err != nil || len(tasks) == 0 {
				return err
			}
			n = len(tasks)
			out, err = g.consumerOutcome(tx.Snapshot(), tasks[0])
			return err
		})).To(Succeed())
		return out, n
	}
	drainOne := func(g *guardiann, tx fdb.WritableTransaction) error {
		_, err := g.executeDeferredTasks(tx, 1, g.env.Now().Add(1<<40))
		return err
	}

	It("skips a REFUSED head split, keeps it queued, and the next drain meets the refusal", func() {
		ss := specSubspace().Sub("refused")
		cfg := base()
		queueSplit(ss, cfg)
		zero := cfg
		zero.splitNumNearestClusters = 0
		out, queued := head(ss, zero)
		Expect(out.kind).To(Equal(outcomeRefused))
		var capability *VectorCapabilityError
		Expect(errors.As(out.err, &capability)).To(BeTrue())

		Expect(run(ss, zero, func(g *guardiann, tx fdb.WritableTransaction) error {
			return g.delete(tx, tuple.Tuple{int64(1)}, vector(1), true)
		})).To(Succeed(), "the inline delete skips the refused task (Java's fails)")
		_, after := head(ss, zero)
		Expect(after).To(Equal(queued), "the skipped task stays queued")
		Expect(run(ss, zero, func(g *guardiann, tx fdb.WritableTransaction) error {
			md, err := g.fetchVectorMetadata(tx, tuple.Tuple{int64(1)})
			Expect(md).To(BeNil(), "the vector is deleted")
			return err
		})).To(Succeed())
		err := run(ss, zero, drainOne)
		Expect(errors.As(err, &capability)).To(BeTrue(), "the next drain refuses it: %v", err)
	})

	It("runs a head task that RUNS or is CONSUMED, exactly as Java's delete does", func() {
		ss := specSubspace().Sub("runs")
		cfg := base()
		queueSplit(ss, cfg)
		out, queued := head(ss, cfg)
		Expect(out.kind).To(Equal(outcomeRuns), "a phase-1 split runs (its re-enqueue)")
		Expect(run(ss, cfg, func(g *guardiann, tx fdb.WritableTransaction) error {
			return g.delete(tx, tuple.Tuple{int64(1)}, vector(1), true)
		})).To(Succeed())
		out, _ = head(ss, cfg)
		Expect(out.kind).To(Equal(outcomeRuns), "the delete ran the phase-1 task; its re-enqueued copy heads the queue")

		// Deleting below the maximum makes the queued split a false alarm.
		Expect(run(ss, cfg, func(g *guardiann, tx fdb.WritableTransaction) error {
			for i := int64(2); i <= 4; i++ {
				if err := g.delete(tx, tuple.Tuple{i}, vector(i), false); err != nil {
					return err
				}
			}
			return nil
		})).To(Succeed())
		out, _ = head(ss, cfg)
		Expect(out.kind).To(Equal(outcomeConsumed), "a split of a cluster back in bounds is a false alarm")
		_ = queued
	})

	It("skips conservatively when only the KMeans knobs would refuse", func() {
		ss := specSubspace().Sub("nk")
		cfg := base()
		queueSplit(ss, cfg)
		Expect(run(ss, cfg, drainOne)).To(Succeed(), "phase 1 attaches the neighbours")
		bad := cfg
		bad.kMeansMaxIterations = 0
		out, _ := head(ss, bad)
		Expect(out.kind).To(Equal(outcomeRefusedUnlessAllNK))
		Expect(out.refused()).To(BeTrue())
	})

	It("follows a bounce to the dependency it would run", func() {
		ss := specSubspace().Sub("bounce")
		cfg := base()
		zero := cfg
		zero.bounceConcurrency = 0
		Expect(run(ss, zero, func(g *guardiann, tx fdb.WritableTransaction) error {
			out, err := g.consumerOutcome(tx.Snapshot(), &guardiannTask{kind: taskBounce, id: tuple.UUID{1}})
			Expect(out.kind).To(Equal(outcomeRefused), "the bounce's own knob")
			return err
		})).To(Succeed())

		collapse := &guardiannTask{kind: taskCollapse, id: tuple.UUID{2}, targets: []tuple.UUID{{9}}, centroid: vector(1)}
		Expect(run(ss, cfg, func(g *guardiann, tx fdb.WritableTransaction) error {
			g.writeClusterMetadata(tx, guardiannClusterMetadata{id: tuple.UUID{9}, stats: runningStatsIdentity(), states: clusterStateCollapse})
			return g.writeTask(tx, collapse)
		})).To(Succeed())
		bounce := &guardiannTask{kind: taskBounce, id: tuple.UUID{3}, targets: []tuple.UUID{{9}}, dependents: []tuple.UUID{collapse.id}, finalKind: taskSplitMerge}
		noCollapse := cfg
		noCollapse.collapseConcurrency = 0
		Expect(run(ss, noCollapse, func(g *guardiann, tx fdb.WritableTransaction) error {
			out, err := g.consumerOutcome(tx.Snapshot(), bounce)
			Expect(out.kind).To(Equal(outcomeRefused), "its live collapse dependency is refused")
			return err
		})).To(Succeed())
		Expect(run(ss, cfg, func(g *guardiann, tx fdb.WritableTransaction) error {
			out, err := g.consumerOutcome(tx.Snapshot(), bounce)
			Expect(out.kind).To(Equal(outcomeRuns))
			return err
		})).To(Succeed())
	})

	// The race fixtures (ws-d-design.md, declared (d)): A runs in a
	// transaction left open while B commits; A's commit is then decided by
	// what A read serializably.
	begin := func() fdb.Transaction {
		tx, err := sharedDB.CreateTransaction()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(tx.Cancel)
		return tx
	}
	isNotCommitted := func(err error) bool {
		var fe fdb.Error
		return errors.As(err, &fe) && fe.Code == 1020
	}

	It("a skipping delete reads the head task at snapshot: a concurrent drain of it does not conflict", func() {
		ss := specSubspace().Sub("race-skip")
		cfg := base()
		queueSplit(ss, cfg)
		zero := cfg
		zero.splitNumNearestClusters = 0
		txA := begin()
		Expect(newGuardiann(ss, zero, nil, nil).delete(txA, tuple.Tuple{int64(1)}, vector(1), true)).To(Succeed())
		// B consumes the head task A skipped (phase 1 under the healthy config).
		Expect(run(ss, cfg, drainOne)).To(Succeed())
		Expect(txA.Commit().Get()).To(Succeed(), "the skip added no read conflict on the task B consumed")
	})

	It("a false-alarm clear read the cluster serializably: a concurrent insert into it fails the clear with 1020", func() {
		ss := specSubspace().Sub("race-clear")
		cfg := base()
		queueSplit(ss, cfg)
		Expect(run(ss, cfg, func(g *guardiann, tx fdb.WritableTransaction) error {
			for i := int64(2); i <= 4; i++ {
				if err := g.delete(tx, tuple.Tuple{i}, vector(i), false); err != nil {
					return err
				}
			}
			return nil
		})).To(Succeed())
		out, _ := head(ss, cfg)
		Expect(out.kind).To(Equal(outcomeConsumed), "the queued split is a false alarm")
		txA := begin()
		Expect(drainOne(newGuardiann(ss, cfg, nil, nil), txA)).To(Succeed())
		// B inserts into the SPLIT_MERGE cluster without re-arming it.
		Expect(run(ss, cfg, func(g *guardiann, tx fdb.WritableTransaction) error {
			return g.insert(tx, tuple.Tuple{int64(20)}, vector(20), nil, false)
		})).To(Succeed())
		_, queued := head(ss, cfg)
		Expect(queued).To(Equal(1), "B enqueued nothing")
		err := txA.Commit().Get()
		Expect(isNotCommitted(err)).To(BeTrue(), "the clear must fail with not_committed: %v", err)
	})

	It("a non-skipping delete read the queue head serializably: a higher-priority enqueue fails it with 1020", func() {
		ss := specSubspace().Sub("race-enqueue")
		cfg := base()
		queueSplit(ss, cfg)
		out, _ := head(ss, cfg)
		Expect(out.kind).To(Equal(outcomeRuns))
		txA := begin()
		Expect(newGuardiann(ss, cfg, nil, nil).delete(txA, tuple.Tuple{int64(1)}, vector(1), true)).To(Succeed())
		// B enqueues a task ahead of the head A ran.
		Expect(run(ss, cfg, func(g *guardiann, tx fdb.WritableTransaction) error {
			id, err := g.highPriorityTaskID(newSplittableRandomForUUID(tuple.UUID{7}))
			if err != nil {
				return err
			}
			return g.writeTask(tx, &guardiannTask{kind: taskBounce, id: id, targets: []tuple.UUID{{9}}, finalKind: taskSplitMerge})
		})).To(Succeed())
		err := txA.Commit().Get()
		Expect(isNotCommitted(err)).To(BeTrue(), "the inline delete must fail with not_committed: %v", err)
	})
})
