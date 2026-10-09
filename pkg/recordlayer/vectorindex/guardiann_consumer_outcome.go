package vectorindex

import (
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
)

// RFC-257 WS-D declared (d). Java's inline delete runs the partition's head
// task first and fails whenever that task throws. Go runs no inline task for a
// delete whose head task a Go consumer would refuse, and nowhere else: the
// task stays queued and counted and runs at the next drain (where the refusal
// surfaces). consumerOutcome is that decision, made by walking the task kind's
// prologue in Java's statement order with the same predicates the task itself
// evaluates, reading at the isolation the caller passes (a skipping delete
// reads at snapshot and so adds no read conflict).

type consumerOutcomeKind int

const (
	// outcomeConsumed: a no-op exit decides the task (missing or obsolete
	// cluster, a false alarm, a merge with no mergeable neighbour).
	outcomeConsumed consumerOutcomeKind = iota
	// outcomeRuns: every statement up to the task's real work passes.
	outcomeRuns
	// outcomeRefused: a Go consumer raises err at one of those statements.
	outcomeRefused
	// outcomeRefusedUnlessAllNK: the KMeans knobs would raise err, but only
	// for a candidate with at least k cleaned vectors, which the prologue
	// cannot know; a delete skips it conservatively.
	outcomeRefusedUnlessAllNK
)

type consumerOutcome struct {
	kind consumerOutcomeKind
	err  error
}

func (o consumerOutcome) refused() bool {
	return o.kind == outcomeRefused || o.kind == outcomeRefusedUnlessAllNK
}

func refusedBy(err error) consumerOutcome { return consumerOutcome{kind: outcomeRefused, err: err} }

// consumerOutcome walks t's prologue at the given read transaction.
func (g *guardiann) consumerOutcome(rtx fdb.ReadTransaction, t *guardiannTask) (consumerOutcome, error) {
	switch t.kind {
	case taskSplitMerge:
		return g.splitMergeOutcome(rtx, t)
	case taskReassign:
		m, err := g.fetchClusterMetadata(rtx, t.target())
		if err != nil {
			return consumerOutcome{}, err
		}
		if m == nil || !m.has(clusterStateReassign) || m.has(clusterStateSplitMerge) || m.has(clusterStateCollapse) {
			return consumerOutcome{kind: outcomeConsumed}, nil
		}
		if err := g.codec.requireQuantizer(); err != nil {
			return refusedBy(err), nil
		}
		numNeighboring := g.config.reassignNumNeighboringClusters
		if len(t.nearest) == 0 {
			if err := g.neighbourFetchRefusal("reassign", 1+numNeighboring, recordlayer.IndexOptionGuardiannReassignNumNeighboringClusters,
				numNeighboring, g.config.reassignConcurrency, recordlayer.IndexOptionGuardiannReassignConcurrency); err != nil {
				return refusedBy(err), nil
			}
			return consumerOutcome{kind: outcomeRuns}, nil
		}
		if g.config.reassignConcurrency < 1 {
			return refusedBy(parallelismError(g.config.reassignConcurrency)), nil
		}
		return consumerOutcome{kind: outcomeRuns}, nil
	case taskCollapse:
		m, err := g.fetchClusterMetadata(rtx, t.target())
		if err != nil {
			return consumerOutcome{}, err
		}
		if m == nil || !m.has(clusterStateCollapse) {
			return consumerOutcome{kind: outcomeConsumed}, nil
		}
		if err := g.codec.requireQuantizer(); err != nil {
			return refusedBy(err), nil
		}
		if g.config.collapseConcurrency < 1 {
			return refusedBy(parallelismError(g.config.collapseConcurrency)), nil
		}
		return consumerOutcome{kind: outcomeRuns}, nil
	case taskBounce:
		if g.config.bounceConcurrency < 1 {
			return refusedBy(parallelismError(g.config.bounceConcurrency)), nil
		}
		// The dependency the bounce would execute: outstanding ones in stored
		// order, picked by the task's own RNG (BounceTask.java:123, :161).
		var outstanding []*guardiannTask
		for _, id := range t.dependents {
			d, err := g.fetchTask(rtx, id)
			if err != nil {
				return consumerOutcome{}, err
			}
			if d != nil {
				outstanding = append(outstanding, d)
			}
		}
		if len(outstanding) == 0 {
			return consumerOutcome{kind: outcomeRuns}, nil
		}
		pick := newSplittableRandomForUUID(t.id).nextInt(len(outstanding))
		dep, err := g.consumerOutcome(rtx, outstanding[pick])
		if err != nil || dep.refused() {
			return dep, err
		}
		return consumerOutcome{kind: outcomeRuns}, nil
	}
	return refusedBy(&recordlayer.RecordCoreError{Message: "unknown guardiann task kind"}), nil
}

// splitMergeOutcome is runSplitMerge's prologue (SplitMergeTask.java:169-194,
// split()/merge() entry, the neighbour fetch, classifyClusters, KMeans).
func (g *guardiann) splitMergeOutcome(rtx fdb.ReadTransaction, t *guardiannTask) (consumerOutcome, error) {
	m, err := g.fetchClusterMetadata(rtx, t.target())
	if err != nil {
		return consumerOutcome{}, err
	}
	if m == nil || !m.has(clusterStateSplitMerge) || m.has(clusterStateCollapse) {
		return consumerOutcome{kind: outcomeConsumed}, nil
	}
	if m.numPrimary() >= m.mergeThreshold(&g.config) && m.numPrimary() <= g.config.primaryClusterMax {
		return consumerOutcome{kind: outcomeConsumed}, nil // the false-alarm clear
	}
	split := m.numPrimary() > g.config.primaryClusterMax
	if err := g.codec.requireQuantizer(); err != nil {
		return refusedBy(err), nil
	}
	num, numOption, operation := g.config.mergeNumNearestClusters, recordlayer.IndexOptionGuardiannMergeNumNearestClusters, "merge"
	if split {
		num, numOption, operation = g.config.splitNumNearestClusters, recordlayer.IndexOptionGuardiannSplitNumNearestClusters, "split"
	}
	if len(t.nearest) == 0 {
		if err := g.neighbourFetchRefusal(operation, num, numOption, num,
			g.config.splitMergeConcurrency, recordlayer.IndexOptionGuardiannSplitMergeConcurrency); err != nil {
			return refusedBy(err), nil
		}
		return consumerOutcome{kind: outcomeRuns}, nil // the phase-1 re-enqueue
	}
	if g.config.splitMergeConcurrency < 1 {
		return refusedBy(parallelismError(g.config.splitMergeConcurrency)), nil
	}
	if !split {
		nearest, err := g.fetchClusterMetadataForRefs(rtx, t.nearest)
		if err != nil {
			return consumerOutcome{}, err
		}
		if classifyClusters(nearest, *m, t.centroid, 2, num-2) == nil {
			return consumerOutcome{kind: outcomeConsumed}, nil // no mergeable neighbour
		}
	}
	switch {
	case g.config.kMeansMaxIterations < 1:
		return consumerOutcome{
			kind: outcomeRefusedUnlessAllNK,
			err:  &recordlayer.IllegalArgumentError{Message: "maxIterations must be >= 1"},
		}, nil
	case g.config.kMeansMaxRestarts < 0:
		return consumerOutcome{
			kind: outcomeRefusedUnlessAllNK,
			err:  &recordlayer.IllegalArgumentError{Message: "maxRestarts must be >= 0"},
		}, nil
	}
	return consumerOutcome{kind: outcomeRuns}, nil
}

// headTaskRefused reports whether the partition's head task would be refused
// by a Go consumer, read at snapshot.
func (g *guardiann) headTaskRefused(tx fdb.WritableTransaction) (bool, error) {
	snap := tx.Snapshot()
	info, err := g.fetchAccessInfo(snap)
	if err != nil {
		return false, err
	}
	h := g.withAccessInfo(info)
	tasks, err := h.fetchSomeTasks(snap, 1)
	if err != nil || len(tasks) == 0 {
		return false, err
	}
	out, err := h.consumerOutcome(snap, tasks[0])
	if err != nil {
		return false, err
	}
	return out.refused(), nil
}
