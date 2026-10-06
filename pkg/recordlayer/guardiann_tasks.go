package recordlayer

import (
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// GuardiANN's deferred tasks: SplitMergeTask, ReassignTask, CollapseTask and
// BounceTask.

func (g *guardiann) runTask(tx fdb.WritableTransaction, t *guardiannTask) error {
	switch t.kind {
	case taskSplitMerge:
		return g.runSplitMerge(tx, t)
	case taskReassign:
		m, err := g.fetchClusterMetadata(tx, t.target())
		if err != nil || m == nil || !m.has(clusterStateReassign) || m.has(clusterStateSplitMerge) || m.has(clusterStateCollapse) {
			return err
		}
		return g.reassign(tx, t, *m)
	case taskCollapse:
		m, err := g.fetchClusterMetadata(tx, t.target())
		if err != nil || m == nil || !m.has(clusterStateCollapse) {
			return err
		}
		return g.collapse(tx, t, *m)
	case taskBounce:
		return g.runBounce(tx, t)
	}
	return &RecordCoreError{Message: "unknown guardiann task kind"}
}

// orderedAssignments is an ImmutableListMultimap<UUID, VectorReference>:
// keys and their values in insertion order.
type orderedAssignments struct {
	keys   []tuple.UUID
	values map[tuple.UUID][]guardiannVectorRef
}

func newOrderedAssignments() *orderedAssignments {
	return &orderedAssignments{values: map[tuple.UUID][]guardiannVectorRef{}}
}

func (a *orderedAssignments) put(k tuple.UUID, v ...guardiannVectorRef) {
	if _, ok := a.values[k]; !ok {
		a.keys = append(a.keys, k)
		a.values[k] = nil
	}
	a.values[k] = append(a.values[k], v...)
}

func (a *orderedAssignments) each(f func(k tuple.UUID, v guardiannVectorRef)) {
	for _, k := range a.keys {
		for _, v := range a.values[k] {
			f(k, v)
		}
	}
}

// orderedClusters is an ImmutableMap<UUID, ClusterMetadataWithDistance>.
type orderedClusters struct {
	keys   []tuple.UUID
	values map[tuple.UUID]guardiannClusterWithDistance
}

func newOrderedClusters() *orderedClusters {
	return &orderedClusters{values: map[tuple.UUID]guardiannClusterWithDistance{}}
}

func (o *orderedClusters) put(c guardiannClusterWithDistance) {
	if _, ok := o.values[c.meta.id]; !ok {
		o.keys = append(o.keys, c.meta.id)
	}
	o.values[c.meta.id] = c
}

func (o *orderedClusters) list() []guardiannClusterWithDistance {
	out := make([]guardiannClusterWithDistance, len(o.keys))
	for i, k := range o.keys {
		out[i] = o.values[k]
	}
	return out
}

// computeNearestClusters is AbstractDeferredTask.computeNearestClusters: for
// each primary vector, the candidate clusters sorted by distance, and the
// stats each vector adds to its nearest cluster.
func (g *guardiann) computeNearestClusters(refs []guardiannVectorRef, candidates []guardiannClusterWithDistance) (map[string][]guardiannClusterWithDistance, map[tuple.UUID]guardiannRunningStats, error) {
	inverted := map[string][]guardiannClusterWithDistance{}
	updates := map[tuple.UUID]guardiannRunningStats{}
	for _, r := range refs {
		if !r.primary {
			continue
		}
		sorted := make([]guardiannClusterWithDistance, len(candidates))
		for i, c := range candidates {
			var err error
			c.distance, err = g.distance(r.vector, c.centroid)
			if err != nil {
				return nil, nil, err
			}
			sorted[i] = c
		}
		sortClustersByDistance(sorted)
		p := sorted[0]
		if s, ok := updates[p.meta.id]; ok {
			updates[p.meta.id] = s.add(p.distance)
		} else {
			updates[p.meta.id] = runningStatsOf(p.distance)
		}
		inverted[r.id.key()] = sorted
	}
	return inverted, updates, nil
}

func mergeStatsUpdates(target, updates map[tuple.UUID]guardiannRunningStats) {
	for k, v := range updates {
		if old, ok := target[k]; ok {
			target[k] = old.combine(v)
		} else {
			target[k] = v
		}
	}
}

// clusterClassification is AbstractDeferredTask.ClusterClassification.
type clusterClassification struct {
	core, neighboring []guardiannClusterWithDistance
}

// classifyClusters is AbstractDeferredTask.classifyClusters; nil when fewer
// than numCore core clusters exist.
func classifyClusters(clusters []guardiannClusterWithDistance, target guardiannClusterMetadata, centroid gVector, numCore, numNeighboring int) *clusterClassification {
	found := false
	for _, c := range clusters {
		if c.meta.id == target.id {
			found = true
			break
		}
	}
	var cls clusterClassification
	if found {
		core := min(numCore, len(clusters))
		neighboring := min(numNeighboring, len(clusters)-core)
		cls = clusterClassification{core: clusters[:core], neighboring: clusters[core : core+neighboring]}
	} else {
		core := min(numCore-1, len(clusters))
		cls.core = append([]guardiannClusterWithDistance{{meta: target, centroid: centroid}}, clusters[:core]...)
		neighboring := min(numNeighboring, len(clusters)-core)
		cls.neighboring = clusters[core : core+neighboring]
	}
	if len(cls.core) < numCore {
		return nil
	}
	return &cls
}

// computeTargetClusterDelta is AbstractDeferredTask.computeTargetClusterDelta.
func computeTargetClusterDelta(target guardiannCluster, assigned []guardiannVectorRef) (toWrite []guardiannVectorRef, toDelete []tuple.Tuple) {
	byPK := map[string]guardiannVectorRef{}
	var order []string
	for _, a := range assigned {
		k := string(a.id.pk.Pack())
		if _, ok := byPK[k]; !ok {
			order = append(order, k)
		}
		byPK[k] = a
	}
	old := map[string]bool{}
	for _, r := range target.refs {
		k := string(r.id.pk.Pack())
		old[k] = true
		a, ok := byPK[k]
		if !ok {
			toDelete = append(toDelete, r.id.pk)
			continue
		}
		if a.primary != r.primary || a.isUnderreplicated() != r.isUnderreplicated() || r.replicationPriorityChanged(a) {
			toWrite = append(toWrite, a)
		}
	}
	for _, k := range order {
		if !old[k] {
			toWrite = append(toWrite, byPK[k])
		}
	}
	return toWrite, toDelete
}

func (g *guardiann) persistTargetClusterDelta(tx fdb.WritableTransaction, target tuple.UUID, toWrite []guardiannVectorRef, toDelete []tuple.Tuple) {
	for _, pk := range toDelete {
		g.deleteVectorRef(tx, target, pk)
	}
	for _, r := range toWrite {
		g.writeVectorRef(tx, target, r)
	}
}

// enqueueCollapseIfNecessary is AbstractDeferredTask.enqueueCollapseIfNecessary.
func (g *guardiann) enqueueCollapseIfNecessary(tx fdb.WritableTransaction, random *splittableRandom, primaries []guardiannVectorRef, target tuple.UUID, centroid gVector) (bool, error) {
	counts := map[tuple.UUID]int{}
	maxCount := 0
	for _, r := range primaries {
		if r.primary {
			s := signatureUUID(r.vector)
			counts[s]++
			maxCount = max(maxCount, counts[s])
		}
	}
	if maxCount <= g.config.collapseMinDuplicates {
		return false, nil
	}
	collapseID, err := g.highPriorityTaskID(random)
	if err != nil {
		return false, err
	}
	bounceID, err := g.highPriorityTaskID(random)
	if err != nil {
		return false, err
	}
	g.writeTask(tx, &guardiannTask{kind: taskCollapse, id: collapseID, targets: []tuple.UUID{target}, centroid: centroid})
	g.writeTask(tx, &guardiannTask{
		kind: taskBounce, id: bounceID, targets: []tuple.UUID{target},
		dependents: []tuple.UUID{collapseID}, finalKind: taskSplitMerge,
	})
	return true, nil
}

// selectReplicas is the tasks' selectReplicationAssignments. requireEligible
// is ReassignTask's gate (underreplicated, or a cause cluster).
func (g *guardiann) selectReplicas(ref guardiannVectorRef, distanceToPrimary float64, candidates []guardiannClusterWithDistance,
	stats map[tuple.UUID]guardiannRunningStats, eligible func(guardiannClusterWithDistance) bool,
) ([]struct {
	cluster tuple.UUID
	ref     guardiannVectorRef
}, error,
) {
	var selected []guardiannClusterWithDistance
	var out []struct {
		cluster tuple.UUID
		ref     guardiannVectorRef
	}
	for _, c := range candidates {
		if eligible != nil && !eligible(c) {
			continue
		}
		s := stats[c.meta.id]
		priority := g.config.replicationPriority(c.distance, distanceToPrimary, int(s.n), s.meanOrNaN(), s.populationStdDev())
		if priority >= g.config.replicationPriorityMin {
			occluded, err := g.isOccluded(c, selected)
			if err != nil {
				return nil, err
			}
			if occluded {
				continue
			}
			out = append(out, struct {
				cluster tuple.UUID
				ref     guardiannVectorRef
			}{c.meta.id, ref.toReplicated(priority)})
			selected = append(selected, c)
		}
	}
	return out, nil
}

// countAssignments counts per-cluster additions of an assignment.
type assignmentCounts struct {
	primary, underrep, replicated map[tuple.UUID]int
}

func countAssignments(a *orderedAssignments) assignmentCounts {
	c := assignmentCounts{primary: map[tuple.UUID]int{}, underrep: map[tuple.UUID]int{}, replicated: map[tuple.UUID]int{}}
	a.each(func(k tuple.UUID, v guardiannVectorRef) {
		if v.primary {
			c.primary[k]++
			if v.underrep {
				c.underrep[k]++
			}
		} else {
			c.replicated[k]++
		}
	})
	return c
}

// ---- ReassignTask ----

func (g *guardiann) reassign(tx fdb.WritableTransaction, t *guardiannTask, target guardiannClusterMetadata) error {
	random := newSplittableRandomForUUID(t.id)
	numNeighboring := g.config.reassignNumNeighboringClusters
	if len(t.nearest) == 0 {
		// Java's phase-1 re-enqueue (ReassignTask.java:243-251, :322-327).
		if err := g.neighbourFetchRefusal("reassign", 1+numNeighboring, IndexOptionGuardiannReassignNumNeighboringClusters,
			numNeighboring, g.config.reassignConcurrency, IndexOptionGuardiannReassignConcurrency); err != nil {
			return err
		}
		nearest, err := g.findNearestClustersMetadata(tx, target, t.centroid, 1+numNeighboring)
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
	// fetchClusterMetadataForReferences at reassignConcurrency
	// (ReassignTask.java:254-257).
	if g.config.reassignConcurrency < 1 {
		return parallelismError(g.config.reassignConcurrency)
	}
	nearest, err := g.fetchClusterMetadataForRefs(tx, t.nearest)
	if err != nil {
		return err
	}
	cls := classifyClusters(nearest, target, t.centroid, 1, numNeighboring)
	if cls == nil {
		return &RecordCoreError{Message: "reassign found no target cluster"}
	}
	inner, err := g.fetchCoreClusters(tx, cls.core)
	if err != nil {
		return err
	}
	cleaned, err := g.cleanUpVectorReferences(tx, inner, false)
	if err != nil {
		return err
	}
	targetC := cls.core[0]
	clusters := newOrderedClusters()
	clusters.put(targetC)
	for _, c := range cls.neighboring {
		clusters.put(c)
	}
	stats := map[tuple.UUID]guardiannRunningStats{}
	for _, k := range clusters.keys {
		if k == target.id {
			stats[k] = runningStatsIdentity()
		} else {
			stats[k] = clusters.values[k].meta.stats
		}
	}
	inverted, updates, err := g.computeNearestClusters(cleaned, clusters.list())
	if err != nil {
		return err
	}
	mergeStatsUpdates(stats, updates)
	assignment := newOrderedAssignments()
	var replicas []guardiannVectorRef
	for _, r := range cleaned {
		if !r.primary {
			replicas = append(replicas, r)
			continue
		}
		near := inverted[r.id.key()]
		p := near[0]
		if p.meta.id != target.id {
			assignment.put(p.meta.id, r.toPrimaryUnderreplicated())
			continue
		}
		assignment.put(p.meta.id, r.toPrimary())
		eligible := func(c guardiannClusterWithDistance) bool {
			return r.isUnderreplicated() || containsUUID(t.causes, c.meta.id)
		}
		selected, err := g.selectReplicas(r, p.distance, near[1:], stats, eligible)
		if err != nil {
			return err
		}
		for _, s := range selected {
			assignment.put(s.cluster, s.ref)
		}
	}
	// ReassignTask.foldCollapsedReplicas over the replicas by descending
	// priority, keeping at most replicatedClusterTarget.
	sortRefsDesc(replicas)
	var kept []guardiannVectorRef
	seen := map[tuple.UUID]bool{}
	for _, r := range replicas {
		if len(kept) >= g.config.replicatedClusterTarget {
			break
		}
		sig := signatureUUID(r.vector)
		if r.collapsed {
			if !seen[sig] {
				seen[sig] = true
				kept = append(kept, r)
			}
			continue
		}
		stored, err := g.fetchCollapsedID(tx, sig, r.id.pk)
		if err != nil {
			return err
		}
		if stored == nil {
			kept = append(kept, r)
		} else if !seen[sig] {
			seen[sig] = true
			kept = append(kept, r.toCollapsed(sig, sig))
		}
	}
	assignment.put(target.id, kept...)
	toWrite, toDelete := computeTargetClusterDelta(inner[0], assignment.values[target.id])
	counts := countAssignments(assignment)
	assignment.each(func(k tuple.UUID, v guardiannVectorRef) {
		if k != target.id {
			g.writeVectorRef(tx, k, v)
		}
	})
	g.persistTargetClusterDelta(tx, target.id, toWrite, toDelete)
	var newTarget *guardiannClusterMetadata
	for _, k := range clusters.keys {
		c := clusters.values[k]
		s := stats[k]
		if k == target.id {
			m := c.meta.withNewVectors(0, counts.replicated[k], s, 0)
			newTarget = &m
			g.writeClusterMetadata(tx, m)
			continue
		}
		if _, err := g.updateAndEnqueueSplitOrReassign(tx, random, c.meta, c.centroid, counts.primary[k], counts.underrep[k],
			counts.replicated[k], s, nil); err != nil {
			return err
		}
	}
	if newTarget != nil {
		_, err := g.enqueueMergeIfUndersized(tx, random, *newTarget, t.centroid, newTarget.stats, newTarget.numPrimary())
		return err
	}
	return nil
}

// sortRefsDesc is a PriorityQueue drained by (priority, id) reversed.
func sortRefsDesc(refs []guardiannVectorRef) {
	for i := 1; i < len(refs); i++ {
		for j := i; j > 0 && compareByPriorityThenID(refs[j], refs[j-1]) > 0; j-- {
			refs[j], refs[j-1] = refs[j-1], refs[j]
		}
	}
}

func clusterRefsOf(cs []guardiannClusterWithDistance) []guardiannClusterRef {
	out := make([]guardiannClusterRef, len(cs))
	for i, c := range cs {
		out[i] = guardiannClusterRef{clusterID: c.meta.id, centroid: c.centroid}
	}
	return out
}

// ---- CollapseTask ----

func (g *guardiann) collapse(tx fdb.WritableTransaction, t *guardiannTask, target guardiannClusterMetadata) error {
	random := newSplittableRandomForUUID(t.id)
	// fetchCoreClusters at collapseConcurrency (CollapseTask.java:182), past
	// the task's no-op exits.
	if g.config.collapseConcurrency < 1 {
		return parallelismError(g.config.collapseConcurrency)
	}
	cluster, err := g.fetchCluster(tx, target.id, t.centroid)
	if err != nil {
		return err
	}
	sigs := map[string]tuple.UUID{}
	bySig := map[tuple.UUID]int{}
	for _, r := range cluster.refs {
		if r.primary {
			s := signatureUUID(r.vector)
			if _, ok := sigs[r.id.key()]; !ok {
				sigs[r.id.key()] = s
				bySig[s]++
			}
		}
	}
	blackHole := map[tuple.UUID]bool{}
	for _, r := range cluster.refs {
		if r.primary && r.collapsed {
			blackHole[sigs[r.id.key()]] = true
		}
	}
	var assignments []guardiannVectorRef
	var collapsedKeys []tuple.UUID
	collapsedIDs := map[tuple.UUID][]guardiannVectorID{}
	replicated := newTopKMax(g.config.replicatedClusterTarget, compareByPriorityThenID)
	stats := runningStatsIdentity()
	for _, r := range cluster.refs {
		if !r.primary {
			replicated.add(r)
			continue
		}
		d, err := g.distance(r.vector, t.centroid)
		if err != nil {
			return err
		}
		if !r.collapsed {
			sig := sigs[r.id.key()]
			if bySig[sig] > g.config.collapseMinDuplicates && !blackHole[sig] {
				u, err := g.randomUUID(random)
				if err != nil {
					return err
				}
				blackHole[sig] = true
				stats = stats.add(d)
				assignments = append(assignments, r.toCollapsed(sig, u))
				if _, ok := collapsedIDs[sig]; !ok {
					collapsedKeys = append(collapsedKeys, sig)
				}
				collapsedIDs[sig] = append(collapsedIDs[sig], r.id)
				continue
			}
			if blackHole[sig] {
				if _, ok := collapsedIDs[sig]; !ok {
					collapsedKeys = append(collapsedKeys, sig)
				}
				collapsedIDs[sig] = append(collapsedIDs[sig], r.id)
				continue
			}
		}
		stats = stats.add(d)
		assignments = append(assignments, r)
	}
	assignments = append(assignments, replicated.unsorted()...)
	toWrite, toDelete := computeTargetClusterDelta(cluster, assignments)
	underrep, repl := 0, 0
	for _, r := range assignments {
		if r.primary {
			if r.underrep {
				underrep++
			}
		} else {
			repl++
		}
	}
	g.persistTargetClusterDelta(tx, target.id, toWrite, toDelete)
	for _, sig := range collapsedKeys {
		for _, id := range collapsedIDs[sig] {
			g.writeCollapsedID(tx, sig, id)
		}
	}
	g.writeClusterMetadata(tx, target.withNewVectors(underrep, repl, stats, target.states&^clusterStateCollapse))
	return nil
}

// ---- BounceTask ----

func (g *guardiann) runBounce(tx fdb.WritableTransaction, t *guardiannTask) error {
	// The dependents' fetch at bounceConcurrency is the task's first
	// statement (BounceTask.java:130).
	if g.config.bounceConcurrency < 1 {
		return parallelismError(g.config.bounceConcurrency)
	}
	random := newSplittableRandomForUUID(t.id)
	var outstanding []*guardiannTask
	for _, id := range t.dependents {
		d, err := g.fetchTask(tx, id)
		if err != nil {
			return err
		}
		if d != nil {
			outstanding = append(outstanding, d)
		}
	}
	if len(outstanding) == 0 {
		return g.enqueueFollowUpTasks(tx, t, random)
	}
	pick := random.nextInt(len(outstanding))
	if _, err := g.executeSingleTask(tx, outstanding[pick]); err != nil {
		return err
	}
	if len(outstanding) > 1 {
		var rest []tuple.UUID
		for i, o := range outstanding {
			if i != pick {
				rest = append(rest, o.id)
			}
		}
		id, err := g.randomUUID(random)
		if err != nil {
			return err
		}
		g.writeTask(tx, &guardiannTask{kind: taskBounce, id: id, targets: t.targets, dependents: rest, finalKind: t.finalKind})
		return nil
	}
	return g.enqueueFollowUpTasks(tx, t, random)
}

func (g *guardiann) enqueueFollowUpTasks(tx fdb.WritableTransaction, t *guardiannTask, random *splittableRandom) error {
	for _, target := range t.targets {
		nested := random.split()
		centroid, err := g.fetchCentroid(tx, target)
		if err != nil {
			return err
		}
		m, err := g.fetchClusterMetadata(tx, target)
		if err != nil {
			return err
		}
		if centroid == nil || m == nil {
			continue
		}
		id, err := g.normalPriorityTaskID(nested)
		if err != nil {
			return err
		}
		if m.has(clusterStateReassign) || m.has(clusterStateSplitMerge) || m.has(clusterStateCollapse) {
			continue
		}
		final := &guardiannTask{kind: t.finalKind, id: id, targets: []tuple.UUID{target}, centroid: *centroid}
		state := clusterStateSplitMerge
		if t.finalKind == taskReassign {
			state = clusterStateReassign
		} else if t.finalKind != taskSplitMerge {
			return &RecordCoreError{Message: "unsupported kind for final task"}
		}
		g.writeClusterMetadata(tx, m.withStates(state))
		g.writeTask(tx, final)
	}
	return nil
}
