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

// The GuardiANN knob consumers (RFC-257 WS-D section 1). A concurrency knob
// below 1 is refused where Java hands it to MoreAsyncUtil.forEach, with
// forEach's IllegalArgumentException (parity). A neighbour fetch whose width
// or pipeline is below 1 is where Java writes a task back with the same empty
// neighbour list forever; Go raises a typed VectorCapabilityError there
// instead (declared (c)).
var _ = Describe("GuardiANN knob consumers", func() {
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
	// Thirteen vectors over a maximum of twelve queue one split, deferred.
	queueSplit := func(ss subspace.Subspace, cfg guardiannConfig) {
		Expect(run(ss, cfg, func(g *guardiann, tx fdb.WritableTransaction) error {
			for i := int64(1); i <= 13; i++ {
				if err := g.insert(tx, tuple.Tuple{i}, vector(i), nil, false); err != nil {
					return err
				}
			}
			return nil
		})).To(Succeed())
	}
	drainOne := func(g *guardiann, tx fdb.WritableTransaction) error {
		_, err := g.executeDeferredTasks(tx, 1, g.env.Now().Add(1<<40))
		return err
	}

	It("refuses a split whose neighbour fetch is empty, and forEach's parallelism once neighbours are in", func() {
		ss := specSubspace().Sub("split")
		cfg := base()
		queueSplit(ss, cfg)

		var capability *VectorCapabilityError
		zeroWidth := cfg
		zeroWidth.splitNumNearestClusters = 0
		err := run(ss, zeroWidth, drainOne)
		Expect(errors.As(err, &capability)).To(BeTrue(), "%v", err)
		Expect(capability.Option).To(Equal(recordlayer.IndexOptionGuardiannSplitNumNearestClusters))
		Expect(capability.Operation).To(Equal("split"))

		zeroPipeline := cfg
		zeroPipeline.splitMergeConcurrency = 0
		err = run(ss, zeroPipeline, drainOne)
		Expect(errors.As(err, &capability)).To(BeTrue(), "%v", err)
		Expect(capability.Option).To(Equal(recordlayer.IndexOptionGuardiannSplitMergeConcurrency))

		// The healthy phase 1 writes the task back with its neighbours; a
		// zero splitMergeConcurrency then fails at forEach, as in Java.
		Expect(run(ss, cfg, drainOne)).To(Succeed())
		err = run(ss, zeroPipeline, drainOne)
		var illegal *recordlayer.IllegalArgumentError
		Expect(errors.As(err, &illegal)).To(BeTrue(), "%v", err)
		Expect(illegal.Message).To(Equal("parallelism must be at least 1, got 0"))
		Expect(run(ss, cfg, drainOne)).To(Succeed(), "the task is still queued and runs under a healthy config")
	})

	It("refuses a reassign's empty neighbour fetch and forEach's parallelism", func() {
		ss := specSubspace().Sub("reassign")
		cfg := base()
		target := guardiannClusterMetadata{}
		task := &guardiannTask{kind: taskReassign, centroid: vector(1)}

		negative := cfg
		negative.reassignNumNeighboringClusters = -1
		var capability *VectorCapabilityError
		err := run(ss, negative, func(g *guardiann, tx fdb.WritableTransaction) error { return g.reassign(tx, task, target) })
		Expect(errors.As(err, &capability)).To(BeTrue(), "%v", err)
		Expect(capability.Option).To(Equal(recordlayer.IndexOptionGuardiannReassignNumNeighboringClusters))
		Expect(capability.Value).To(Equal(-1))

		zero := cfg
		zero.reassignConcurrency = 0
		err = run(ss, zero, func(g *guardiann, tx fdb.WritableTransaction) error { return g.reassign(tx, task, target) })
		Expect(errors.As(err, &capability)).To(BeTrue(), "%v", err)
		Expect(capability.Option).To(Equal(recordlayer.IndexOptionGuardiannReassignConcurrency))

		withNeighbours := *task
		withNeighbours.nearest = []guardiannClusterRef{{centroid: vector(1)}}
		err = run(ss, zero, func(g *guardiann, tx fdb.WritableTransaction) error {
			return g.reassign(tx, &withNeighbours, target)
		})
		var illegal *recordlayer.IllegalArgumentError
		Expect(errors.As(err, &illegal)).To(BeTrue(), "%v", err)
	})

	It("refuses collapse and bounce concurrency below 1 at their forEach", func() {
		ss := specSubspace().Sub("collapse_bounce")
		cfg := base()
		var illegal *recordlayer.IllegalArgumentError
		collapse := cfg
		collapse.collapseConcurrency = 0
		err := run(ss, collapse, func(g *guardiann, tx fdb.WritableTransaction) error {
			return g.collapse(tx, &guardiannTask{kind: taskCollapse, centroid: vector(1)}, guardiannClusterMetadata{})
		})
		Expect(errors.As(err, &illegal)).To(BeTrue(), "%v", err)

		bounce := cfg
		bounce.bounceConcurrency = 0
		err = run(ss, bounce, func(g *guardiann, tx fdb.WritableTransaction) error {
			return g.runTask(tx, &guardiannTask{kind: taskBounce})
		})
		Expect(errors.As(err, &illegal)).To(BeTrue(), "%v", err)
		Expect(illegal.Message).To(Equal("parallelism must be at least 1, got 0"))
	})

	It("refuses a delete at deleteConcurrency below 1 once the vector is found", func() {
		ss := specSubspace().Sub("delete")
		cfg := base()
		Expect(run(ss, cfg, func(g *guardiann, tx fdb.WritableTransaction) error {
			return g.insert(tx, tuple.Tuple{int64(1)}, vector(1), nil, false)
		})).To(Succeed())
		zero := cfg
		zero.deleteConcurrency = 0
		Expect(run(ss, zero, func(g *guardiann, tx fdb.WritableTransaction) error {
			return g.delete(tx, tuple.Tuple{int64(99)}, vector(99), false)
		})).To(Succeed(), "deleting a never-indexed key succeeds, as in Java")
		err := run(ss, zero, func(g *guardiann, tx fdb.WritableTransaction) error {
			return g.delete(tx, tuple.Tuple{int64(1)}, vector(1), false)
		})
		var illegal *recordlayer.IllegalArgumentError
		Expect(errors.As(err, &illegal)).To(BeTrue(), "%v", err)
	})
})
