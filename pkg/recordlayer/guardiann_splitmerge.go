package recordlayer

import (
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// SplitMergeTask: an overfull cluster is split (1→2 or 2→3), an underfull one
// merged (2→1 or 3→2), by k-means over the core clusters' primaries, scored
// against the current partition.

type repartitioningCandidate struct {
	cls       *clusterClassification
	primaries []guardiannVectorRef
	kMeans    kMeansResult
}

func (g *guardiann) runSplitMerge(tx fdb.WritableTransaction, t *guardiannTask) error {
	m, err := g.fetchClusterMetadata(tx, t.target())
	if err != nil || m == nil || !m.has(clusterStateSplitMerge) || m.has(clusterStateCollapse) {
		return err
	}
	threshold := m.mergeThreshold(&g.config)
	if m.numPrimary() >= threshold && m.numPrimary() <= g.config.primaryClusterMax {
		g.writeClusterMetadata(tx, m.withStates(m.states&^clusterStateSplitMerge))
		return nil
	}
	split := m.numPrimary() > g.config.primaryClusterMax
	random := newSplittableRandomForUUID(t.id)
	num := g.config.mergeNumNearestClusters
	if split {
		num = g.config.splitNumNearestClusters
	}
	if len(t.nearest) == 0 {
		nearest, err := g.findNearestClustersMetadata(tx, *m, t.centroid, num)
		if err != nil {
			return err
		}
		id, err := g.highPriorityTaskID(random)
		if err != nil {
			return err
		}
		next := *t
		next.id = id
		next.nearest = clusterRefsOf(nearest)
		g.writeTask(tx, &next)
		return nil
	}
	nearest, err := g.fetchClusterMetadataForRefs(tx, t.nearest)
	if err != nil {
		return err
	}
	var cand *repartitioningCandidate
	if split {
		cand, err = g.selectSplitCandidate(tx, t, random, num, *m, nearest)
	} else {
		cand, err = g.selectMergeCandidate(tx, t, random, num, *m, nearest)
	}
	if err != nil || cand == nil {
		return err
	}
	return g.applyRepartitioning(tx, random, cand)
}

// kMeansCandidate is SplitMergeTask.kMeans over a classification's cleaned
// references.
func (g *guardiann) kMeansCandidate(cls *clusterClassification, refs []guardiannVectorRef, random *splittableRandom, k int) *repartitioningCandidate {
	var primaries []guardiannVectorRef
	var vectors [][]float64
	for _, r := range refs {
		if r.primary {
			primaries = append(primaries, r)
			vectors = append(vectors, r.vector.data)
		}
	}
	return &repartitioningCandidate{
		cls: cls, primaries: primaries,
		kMeans: kMeansFit(random, g.config.metric, vectors, k, g.config.kMeansMaxIterations, g.config.kMeansMaxRestarts),
	}
}

func largestCoreClusters(a, b *clusterClassification) []guardiannClusterWithDistance {
	if a == nil {
		return b.core
	}
	if b == nil || len(a.core) >= len(b.core) {
		return a.core
	}
	return b.core
}

// candidatesFor runs k-means for each non-nil classification with a split
// random per classification (RandomHelpers.forEach).
func (g *guardiann) candidatesFor(tx fdb.ReadTransaction, random *splittableRandom, classes []*clusterClassification,
	inner []guardiannCluster, k func(*clusterClassification) int, skip func(*clusterClassification) bool,
) ([]*repartitioningCandidate, error) {
	out := make([]*repartitioningCandidate, len(classes))
	for i, cls := range classes {
		nested := random.split()
		if cls == nil || skip(cls) {
			continue
		}
		clamped := inner
		if len(cls.core) != len(inner) {
			clamped = inner[:len(cls.core)]
		}
		refs, err := g.cleanUpVectorReferences(tx, clamped, true)
		if err != nil {
			return nil, err
		}
		out[i] = g.kMeansCandidate(cls, refs, nested, k(cls))
	}
	return out, nil
}

func (g *guardiann) selectSplitCandidate(tx fdb.WritableTransaction, t *guardiannTask, random *splittableRandom, num int,
	target guardiannClusterMetadata, nearest []guardiannClusterWithDistance,
) (*repartitioningCandidate, error) {
	c12 := classifyClusters(nearest, target, t.centroid, 1, num-1)
	if c12 == nil {
		return nil, &RecordCoreError{Message: "split found no target cluster"}
	}
	c23 := classifyClusters(nearest, target, t.centroid, 2, num-2)
	inner, err := g.fetchCoreClusters(tx, largestCoreClusters(c12, c23))
	if err != nil {
		return nil, err
	}
	cands, err := g.candidatesFor(tx, random, []*clusterClassification{c12, c23}, inner,
		func(c *clusterClassification) int { return len(c.core) + 1 },
		func(*clusterClassification) bool { return false })
	if err != nil {
		return nil, err
	}
	scored := []scoredCandidate{{cands[0], g.scoreCandidate(inner[:1], cands[0])}}
	if cands[1] != nil {
		scored = append(scored, scoredCandidate{cands[1], g.scoreCandidate(inner, cands[1])})
	}
	if best := selectBestCandidate(scored); best != nil {
		return best, nil
	}
	collapsing, err := g.enqueueCollapseIfNecessary(tx, random, cands[0].primaries, t.target(), t.centroid)
	if err != nil {
		return nil, err
	}
	if collapsing {
		g.writeClusterMetadata(tx, target.withStates(clusterStateCollapse))
		return nil, nil
	}
	return nil, &RecordCoreError{Message: "no valid split candidate"}
}

func (g *guardiann) selectMergeCandidate(tx fdb.WritableTransaction, t *guardiannTask, random *splittableRandom, num int,
	target guardiannClusterMetadata, nearest []guardiannClusterWithDistance,
) (*repartitioningCandidate, error) {
	c21 := classifyClusters(nearest, target, t.centroid, 2, num-2)
	if c21 == nil {
		g.writeClusterMetadata(tx, target.withStates(target.states&^clusterStateSplitMerge))
		return nil, nil
	}
	c32 := classifyClusters(nearest, target, t.centroid, 3, num-3)
	inner, err := g.fetchCoreClusters(tx, largestCoreClusters(c21, c32))
	if err != nil {
		return nil, err
	}
	cands, err := g.candidatesFor(tx, random, []*clusterClassification{c21, c32}, inner,
		func(c *clusterClassification) int { return len(c.core) - 1 },
		func(c *clusterClassification) bool { return len(c.core) < 2 })
	if err != nil {
		return nil, err
	}
	scored := []scoredCandidate{{cands[0], g.scoreCandidate(inner[:len(c21.core)], cands[0])}}
	if cands[1] != nil {
		scored = append(scored, scoredCandidate{cands[1], g.scoreCandidate(inner, cands[1])})
	}
	if best := selectBestCandidate(scored); best != nil {
		return best, nil
	}
	return cands[0], nil
}

type scoredCandidate struct {
	cand   *repartitioningCandidate
	result evaluationResult
}

// selectBestCandidate is selectBestCandidateMaybe over the candidates in
// order: an accepted candidate beats a kept one, then the higher score gain.
func selectBestCandidate(scored []scoredCandidate) *repartitioningCandidate {
	var best *scoredCandidate
	for i := range scored {
		s := &scored[i]
		if s.result.decision == decisionInvalidCandidate {
			continue
		}
		if best == nil || isBetterCandidate(s.result, best.result) {
			best = s
		}
	}
	if best == nil {
		return nil
	}
	return best.cand
}

func isBetterCandidate(r, incumbent evaluationResult) bool {
	a, b := r.decision == decisionAcceptCandidate, incumbent.decision == decisionAcceptCandidate
	if a != b {
		return a
	}
	return r.scoreGain > incumbent.scoreGain
}

// scoreCandidate is SplitMergeTask.scoreCandidate.
func (g *guardiann) scoreCandidate(current []guardiannCluster, cand *repartitioningCandidate) evaluationResult {
	var curVectors [][]float64
	var assignment []int
	centroids := make([][]float64, len(current))
	for c, cl := range current {
		centroids[c] = cl.centroid.data
		for _, r := range cl.refs {
			if r.primary {
				curVectors = append(curVectors, r.vector.data)
				assignment = append(assignment, c)
			}
		}
	}
	candVectors := make([][]float64, len(cand.primaries))
	for i, r := range cand.primaries {
		candVectors[i] = r.vector.data
	}
	candCentroids := make([][]float64, len(cand.kMeans.centroids))
	for i, c := range cand.kMeans.centroids {
		candCentroids[i] = c.data
	}
	params := defaultPartitionParameters(g.config.metric)
	params.minChildFraction = g.config.minChildFraction
	params.maxRelativeImbalance = g.config.maxRelativeImbalance
	if len(candCentroids) > len(centroids) {
		params.gammaImbalancePenalty = g.config.splitImbalancePenalty
	}
	return evaluatePartitions(curVectors, partition{centroids: centroids, assignments: assignment},
		candVectors, partition{centroids: candCentroids, assignments: cand.kMeans.assignment}, params)
}

func (g *guardiann) applyRepartitioning(tx fdb.WritableTransaction, random *splittableRandom, cand *repartitioningCandidate) error {
	cls := cand.cls
	clusters := newOrderedClusters()
	var newIDs []tuple.UUID
	for _, c := range cand.kMeans.centroids {
		id, err := g.randomUUID(random)
		if err != nil {
			return err
		}
		newIDs = append(newIDs, id)
		clusters.put(guardiannClusterWithDistance{meta: guardiannClusterMetadata{id: id, stats: runningStatsIdentity()}, centroid: c})
	}
	for _, c := range cls.neighboring {
		clusters.put(c)
	}
	stats := map[tuple.UUID]guardiannRunningStats{}
	for _, k := range clusters.keys {
		stats[k] = clusters.values[k].meta.stats
	}
	inverted, updates := g.computeNearestClusters(cand.primaries, clusters.list())
	mergeStatsUpdates(stats, updates)
	assignment := newOrderedAssignments()
	topKs := map[tuple.UUID]*guardiannTopK{}
	var topKOrder []tuple.UUID
	for _, r := range cand.primaries {
		near := inverted[r.id.key()]
		p := near[0]
		if !containsUUID(newIDs, p.meta.id) {
			assignment.put(p.meta.id, r.toPrimaryUnderreplicated())
			continue
		}
		assignment.put(p.meta.id, r.toPrimary())
		for _, s := range g.selectReplicas(r, p.distance, near[1:], stats, nil) {
			if containsUUID(newIDs, s.cluster) {
				tk, ok := topKs[s.cluster]
				if !ok {
					tk = newTopKMax(g.config.replicatedClusterTarget, compareByPriorityThenID)
					topKs[s.cluster] = tk
					topKOrder = append(topKOrder, s.cluster)
				}
				tk.add(s.ref)
			} else {
				assignment.put(s.cluster, s.ref)
			}
		}
	}
	for _, k := range topKOrder {
		assignment.put(k, topKs[k].unsorted()...)
	}
	// replaceCentroidsInHnsw
	for _, c := range cls.core {
		if err := g.centroids.Delete(tx, tuple.Tuple{c.meta.id}); err != nil {
			return err
		}
	}
	for _, id := range newIDs {
		c := clusters.values[id].centroid
		if err := g.centroids.insertTyped(tx, tuple.Tuple{id}, c.data, c.typ); err != nil {
			return err
		}
	}
	// persistRepartitioning
	for _, c := range cls.core {
		if err := g.deleteVectorRefsForCluster(tx, c.meta.id); err != nil {
			return err
		}
		g.deleteClusterMetadata(tx, c.meta.id)
	}
	counts := countAssignments(assignment)
	assignment.each(func(k tuple.UUID, v guardiannVectorRef) { g.writeVectorRef(tx, k, v) })
	var dependents []tuple.UUID
	for _, k := range clusters.keys {
		c := clusters.values[k]
		id, err := g.updateAndEnqueueSplitOrReassign(tx, random, c.meta, c.centroid, counts.primary[k], counts.underrep[k],
			counts.replicated[k], stats[k], newIDs)
		if err != nil {
			return err
		}
		if id != nil {
			dependents = appendUniqueUUID(dependents, *id)
		}
	}
	if len(dependents) > 0 {
		id, err := g.normalPriorityTaskID(random)
		if err != nil {
			return err
		}
		g.writeTask(tx, &guardiannTask{kind: taskBounce, id: id, targets: newIDs, dependents: dependents, finalKind: taskReassign})
	}
	return nil
}
