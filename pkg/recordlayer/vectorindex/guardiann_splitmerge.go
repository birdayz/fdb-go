package vectorindex

import (
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

// SplitMergeTask: an overfull cluster is split (1→2 or 2→3), an underfull one
// merged (2→1 or 3→2), by k-means over the core clusters' primaries, scored
// against the current partition.

type repartitioningCandidate struct {
	cls       *clusterClassification
	primaries []guardiannVectorRef
	kMeans    kMeansResult
	// nk marks a candidate with fewer cleaned primaries than its k, which the
	// Go n<k rule scores INVALID without calling KMeans (Java throws
	// "vectors.size() must be >= k" at KMeans.java:136); kMeans is empty.
	nk bool
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
	// split()/merge() construct the quantizer on entry (SplitMergeTask.java:227, :430).
	if err := g.codec.requireQuantizer(); err != nil {
		return err
	}
	random := newSplittableRandomForUUID(t.id)
	num, numOption, operation := g.config.mergeNumNearestClusters, recordlayer.IndexOptionGuardiannMergeNumNearestClusters, "merge"
	if split {
		num, numOption, operation = g.config.splitNumNearestClusters, recordlayer.IndexOptionGuardiannSplitNumNearestClusters, "split"
	}
	if len(t.nearest) == 0 {
		// Java's phase-1 re-enqueue (SplitMergeTask.java:232-236/:435-437).
		if err := g.neighbourFetchRefusal(operation, num, numOption, num,
			g.config.splitMergeConcurrency, recordlayer.IndexOptionGuardiannSplitMergeConcurrency); err != nil {
			return err
		}
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
		return g.writeTask(tx, &next)
	}
	// fetchClusterMetadataForReferences at splitMergeConcurrency
	// (SplitMergeTask.java:239-240/:442-443).
	if g.config.splitMergeConcurrency < 1 {
		return parallelismError(g.config.splitMergeConcurrency)
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
func (g *guardiann) kMeansCandidate(cls *clusterClassification, refs []guardiannVectorRef, random *splittableRandom, k int) (*repartitioningCandidate, error) {
	var primaries []guardiannVectorRef
	var vectors []gVector
	for _, r := range refs {
		if r.primary {
			primaries = append(primaries, r)
			vectors = append(vectors, r.vector)
		}
	}
	if k >= 1 && len(vectors) < k {
		return &repartitioningCandidate{cls: cls, primaries: primaries, nk: true}, nil
	}
	result, err := kMeansFit(random, g.codec, vectors, k, g.config.kMeansMaxIterations, g.config.kMeansMaxRestarts)
	if err != nil {
		return nil, err
	}
	return &repartitioningCandidate{cls: cls, primaries: primaries, kMeans: result}, nil
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
		out[i], err = g.kMeansCandidate(cls, refs, nested, k(cls))
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (g *guardiann) selectSplitCandidate(tx fdb.WritableTransaction, t *guardiannTask, random *splittableRandom, num int,
	target guardiannClusterMetadata, nearest []guardiannClusterWithDistance,
) (*repartitioningCandidate, error) {
	c12 := classifyClusters(nearest, target, t.centroid, 1, num-1)
	if c12 == nil {
		return nil, &recordlayer.RecordCoreError{Message: "split found no target cluster"}
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
	result, err := g.scoreCandidate(inner[:1], cands[0])
	if err != nil {
		return nil, err
	}
	scored := []scoredCandidate{{cands[0], result}}
	if cands[1] != nil {
		result, err := g.scoreCandidate(inner, cands[1])
		if err != nil {
			return nil, err
		}
		scored = append(scored, scoredCandidate{cands[1], result})
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
	// Java throws here (orElseThrow, SplitMergeTask.java:397, or KMeans.java:136
	// for an n<k candidate) and fails the same way forever.
	return g.unsplittable(tx, random, target, t.centroid, inner[0], cands[0])
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
	result, err := g.scoreCandidate(inner[:len(c21.core)], cands[0])
	if err != nil {
		return nil, err
	}
	scored := []scoredCandidate{{cands[0], result}}
	if cands[1] != nil {
		result, err := g.scoreCandidate(inner, cands[1])
		if err != nil {
			return nil, err
		}
		scored = append(scored, scoredCandidate{cands[1], result})
	}
	if best := selectBestCandidate(scored); best != nil {
		return best, nil
	}
	if cands[0].nk {
		// The 2->1 fallback's core holds no live primary: Java calls KMeans
		// with k = 1 on no vectors and throws (KMeans.java:136) forever.
		return nil, g.mergeEmptyCore(tx, random, c21)
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

// scoreCandidate is SplitMergeTask.scoreCandidate; an n<k candidate is
// INVALID.
func (g *guardiann) scoreCandidate(current []guardiannCluster, cand *repartitioningCandidate) (evaluationResult, error) {
	if cand.nk {
		return evaluationResult{decision: decisionInvalidCandidate}, nil
	}
	var curVectors []gVector
	var assignment []int
	centroids := make([]gVector, len(current))
	for c, cl := range current {
		centroids[c] = cl.centroid
		for _, r := range cl.refs {
			if r.primary {
				curVectors = append(curVectors, r.vector)
				assignment = append(assignment, c)
			}
		}
	}
	candVectors := make([]gVector, len(cand.primaries))
	for i, r := range cand.primaries {
		candVectors[i] = r.vector
	}
	candCentroids := make([]gVector, len(cand.kMeans.centroids))
	for i, c := range cand.kMeans.centroids {
		candCentroids[i] = c
	}
	params := defaultPartitionParameters(g.config.metric)
	params.codec = g.codec
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
	var newIDs []tuple.UUID
	for range cand.kMeans.centroids {
		id, err := g.randomUUID(random)
		if err != nil {
			return err
		}
		newIDs = append(newIDs, id)
	}
	// nearestOver is assignPrimaryVectorReferences' map of the new clusters
	// and the neighbours, its statistics from the clusters' own, and the
	// primaries' nearest clusters over it.
	nearestOver := func(drop map[tuple.UUID]bool) (*orderedClusters, map[tuple.UUID]guardiannRunningStats, map[string][]guardiannClusterWithDistance, error) {
		clusters := newOrderedClusters()
		for i, c := range cand.kMeans.centroids {
			if !drop[newIDs[i]] {
				clusters.put(guardiannClusterWithDistance{meta: guardiannClusterMetadata{id: newIDs[i], stats: runningStatsIdentity()}, centroid: c})
			}
		}
		for _, c := range cls.neighboring {
			clusters.put(c)
		}
		stats := map[tuple.UUID]guardiannRunningStats{}
		for _, k := range clusters.keys {
			stats[k] = clusters.values[k].meta.stats
		}
		inverted, updates, err := g.computeNearestClusters(cand.primaries, clusters.list())
		if err != nil {
			return nil, nil, nil, err
		}
		mergeStatsUpdates(stats, updates)
		return clusters, stats, inverted, nil
	}
	clusters, stats, inverted, err := nearestOver(nil)
	if err != nil {
		return err
	}
	// Java asserts every written cluster holds a primary (SplitMergeTask.java:
	// 909), so a new cluster that final ownership against the new AND the
	// neighbouring centroids leaves without one fails the task forever. Go
	// drops such a cluster before any write (no centroid, metadata or
	// references) and recomputes ownership and replicas without it, so it can
	// neither receive nor occlude a replica; homes do not change, because a
	// dropped cluster was nobody's nearest. RFC-257 WS-D declared (h).
	homes := map[tuple.UUID]bool{}
	for _, r := range cand.primaries {
		homes[inverted[r.id.key()][0].meta.id] = true
	}
	drop := map[tuple.UUID]bool{}
	var surviving []tuple.UUID
	for _, id := range newIDs {
		if homes[id] {
			surviving = append(surviving, id)
		} else {
			drop[id] = true
		}
	}
	if len(drop) > 0 {
		if clusters, stats, inverted, err = nearestOver(drop); err != nil {
			return err
		}
	}
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
		selected, err := g.selectReplicas(r, p.distance, near[1:], stats, nil)
		if err != nil {
			return err
		}
		for _, s := range selected {
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
	for _, id := range surviving {
		c := g.codec.toClientCoordinates(clusters.values[id].centroid)
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
	for _, k := range assignment.keys {
		for _, v := range assignment.values[k] {
			if err := g.writeVectorRef(tx, k, v); err != nil {
				return err
			}
		}
	}
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
	// The cause set stays every minted id, so neighbours are force-reassigned
	// as after any split; the bounce reassigns the surviving new clusters.
	if len(dependents) > 0 && len(surviving) > 0 {
		id, err := g.normalPriorityTaskID(random)
		if err != nil {
			return err
		}
		return g.writeTask(tx, &guardiannTask{kind: taskBounce, id: id, targets: surviving, dependents: dependents, finalKind: taskReassign})
	}
	return nil
}
