package recordlayer

import (
	"fmt"
	"math"
	"sort"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// GuardiANN operations: Primitives' maintenance bookkeeping, Insert, Delete
// and Search (kNearestNeighborsSearch).

// guardiannClusterCapacityError is Java's ClusterCapacityExceededException,
// surfaced by the record layer as VectorIndexClusterTooLargeException.
type guardiannClusterCapacityError struct {
	clusterID tuple.UUID
	size      int
	hardMax   int
}

// Error is ClusterCapacityExceededException's message; the record layer
// translates it to VectorIndexClusterTooLargeError.
func (e *guardiannClusterCapacityError) Error() string {
	return "primary cluster reached its hard cap while the deferred split backlog is not being drained"
}

// updateClusterMetadataAndEnqueueSplitOrReassignTaskMaybe is the Primitives
// method of that name: an overfull cluster gets a SPLIT_MERGE, otherwise a
// REASSIGN when a bound is violated or it neighbours a fresh split.
func (g *guardiann) updateAndEnqueueSplitOrReassign(tx fdb.WritableTransaction, random *splittableRandom,
	m guardiannClusterMetadata, centroid gVector, primaryAdded, underrepAdded, replicatedAdded int,
	stats guardiannRunningStats, causes []tuple.UUID,
) (*tuple.UUID, error) {
	total := m.numPrimary() + primaryAdded
	if !m.has(clusterStateSplitMerge) && !m.has(clusterStateCollapse) && primaryAdded > 0 && total > g.config.primaryClusterMax {
		id, err := g.updateAndEnqueueSplitMerge(tx, random, m, centroid, underrepAdded, replicatedAdded, stats)
		return &id, err
	}
	return g.updateAndEnqueueReassign(tx, random, m, centroid, primaryAdded, underrepAdded, replicatedAdded, stats, causes)
}

func (g *guardiann) updateAndEnqueueReassign(tx fdb.WritableTransaction, random *splittableRandom,
	m guardiannClusterMetadata, centroid gVector, primaryAdded, underrepAdded, replicatedAdded int,
	stats guardiannRunningStats, causes []tuple.UUID,
) (*tuple.UUID, error) {
	underrep := m.numUnderrep + underrepAdded
	replicated := m.numReplicated + replicatedAdded
	if !m.has(clusterStateReassign) && !containsUUID(causes, m.id) &&
		(len(causes) > 0 || replicated > g.config.replicatedClusterMaxWrites || underrep > g.config.underreplicatedPrimaryClusterMax) {
		id, err := g.normalPriorityTaskID(random)
		if err != nil {
			return nil, err
		}
		g.writeTask(tx, &guardiannTask{kind: taskReassign, id: id, targets: []tuple.UUID{m.id}, centroid: centroid, causes: causes})
		g.writeClusterMetadata(tx, m.withAdditionalVectorsAndStates(underrepAdded, replicatedAdded, stats, clusterStateReassign))
		return &id, nil
	}
	// Java persists only a primary or replica delta (Primitives.java:1254), so an
	// underreplication-only change is lost; Go persists it (RFC-257 WS-D
	// declared (h)).
	if primaryAdded != 0 || replicatedAdded != 0 || underrepAdded != 0 {
		g.writeClusterMetadata(tx, m.withAdditionalVectorsAndStates(underrepAdded, replicatedAdded, stats, 0))
	}
	return nil, nil
}

func (g *guardiann) updateAndEnqueueSplitMerge(tx fdb.WritableTransaction, random *splittableRandom,
	m guardiannClusterMetadata, centroid gVector, underrepAdded, replicatedAdded int, stats guardiannRunningStats,
) (tuple.UUID, error) {
	id, err := g.normalPriorityTaskID(random)
	if err != nil {
		return id, err
	}
	g.writeTask(tx, &guardiannTask{kind: taskSplitMerge, id: id, targets: []tuple.UUID{m.id}, centroid: centroid})
	g.writeClusterMetadata(tx, m.withAdditionalVectorsAndStates(underrepAdded, replicatedAdded, stats, clusterStateSplitMerge))
	return id, nil
}

// updateAndEnqueueMergeOrReassign follows a primary delete. underrepAdded is
// -1 when the deleted primary was underreplicated: Java never decrements the
// count for it, so a cluster's underreplicated count can exceed its primaries;
// Go keeps it the number of physical underreplicated primaries (RFC-257 WS-D
// declared (h)).
func (g *guardiann) updateAndEnqueueMergeOrReassign(tx fdb.WritableTransaction, random *splittableRandom,
	m guardiannClusterMetadata, centroid gVector, stats guardiannRunningStats, underrepAdded int,
) error {
	lowered := m
	lowered.numUnderrep += underrepAdded
	merged, err := g.enqueueMergeIfUndersized(tx, random, lowered, centroid, stats, m.numPrimary()-1)
	if err != nil || merged {
		return err
	}
	_, err = g.updateAndEnqueueReassign(tx, random, m, centroid, -1, underrepAdded, 0, stats, nil)
	return err
}

func (g *guardiann) enqueueMergeIfUndersized(tx fdb.WritableTransaction, random *splittableRandom,
	m guardiannClusterMetadata, centroid gVector, stats guardiannRunningStats, total int,
) (bool, error) {
	if m.has(clusterStateSplitMerge) || m.has(clusterStateCollapse) || total >= m.mergeThreshold(&g.config) {
		return false, nil
	}
	multiple, err := g.centroidCardinalityMultiple(tx)
	if err != nil || !multiple {
		return false, err
	}
	_, err = g.updateAndEnqueueSplitMerge(tx, random, m, centroid, 0, 0, stats)
	return err == nil, err
}

// findNearestClustersMetadata walks the centroids around the target cluster.
func (g *guardiann) findNearestClustersMetadata(tx fdb.ReadTransaction, target guardiannClusterMetadata, centroid gVector, num int) ([]guardiannClusterWithDistance, error) {
	sc := g.config.constructionSearchConfig
	walk, err := g.centroidsOrderedByDistance(tx, g.codec.toClientCoordinates(centroid), sc.centroidEfRingSearch, sc.centroidEfOutwardSearch)
	if err != nil {
		return nil, err
	}
	entries, err := walk.take(num)
	if err != nil {
		return nil, err
	}
	out := make([]guardiannClusterWithDistance, 0, len(entries))
	for _, e := range entries {
		if e.clusterID == target.id {
			out = append(out, guardiannClusterWithDistance{meta: target, centroid: e.vector})
			continue
		}
		m, err := g.requireClusterMetadata(tx, e.clusterID)
		if err != nil {
			return nil, err
		}
		out = append(out, guardiannClusterWithDistance{meta: m, centroid: e.vector})
	}
	return out, nil
}

func (g *guardiann) fetchClusterMetadataForRefs(tx fdb.ReadTransaction, refs []guardiannClusterRef) ([]guardiannClusterWithDistance, error) {
	out := make([]guardiannClusterWithDistance, 0, len(refs))
	for _, r := range refs {
		m, err := g.fetchClusterMetadata(tx, r.clusterID)
		if err != nil {
			return nil, err
		}
		if m != nil {
			out = append(out, guardiannClusterWithDistance{meta: *m, centroid: r.centroid})
		}
	}
	return out, nil
}

func (g *guardiann) fetchCluster(tx fdb.ReadTransaction, id tuple.UUID, centroid gVector) (guardiannCluster, error) {
	m, err := g.fetchClusterMetadata(tx, id)
	if err != nil {
		return guardiannCluster{}, err
	}
	if m == nil {
		return guardiannCluster{}, &RecordCoreError{Message: "guardiann cluster metadata is missing"}
	}
	refs, err := g.fetchVectorRefs(tx, id, g.codec.decode)
	return guardiannCluster{meta: *m, centroid: centroid, refs: refs}, err
}

func (g *guardiann) fetchCoreClusters(tx fdb.ReadTransaction, core []guardiannClusterWithDistance) ([]guardiannCluster, error) {
	out := make([]guardiannCluster, len(core))
	for i, c := range core {
		var err error
		if out[i], err = g.fetchCluster(tx, c.meta.id, c.centroid); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// cleanUpVectorReferences merges the clusters' references per vector id and
// drops those whose vector metadata no longer names them.
func (g *guardiann) cleanUpVectorReferences(tx fdb.ReadTransaction, clusters []guardiannCluster, discardReplicated bool) ([]guardiannVectorRef, error) {
	byID := map[string]int{}
	var merged []guardiannVectorRef
	for _, c := range clusters {
		for _, r := range c.refs {
			if discardReplicated && !r.primary {
				continue
			}
			k := r.id.key()
			i, ok := byID[k]
			if !ok {
				byID[k] = len(merged)
				merged = append(merged, r)
				continue
			}
			old := merged[i]
			// A primary is kept whichever copy comes first: Java's incoming-
			// replica branch (Primitives.mergeVectorReference) replaces an earlier
			// primary with a replica, so a repartitioning loses the primary
			// (RFC-257 WS-D declared (h)). Two replicas keep Java's priority rule.
			switch {
			case old.primary:
			case r.primary:
				merged[i] = r
			case old.replicationPriority() <= r.replicationPriority():
				merged[i] = r
			}
		}
	}
	// Java's vectorsByIdMap is a HashMap<VectorId, VectorReference>, and its
	// values() order is what KMeans receives (it seeds and sums in that
	// order, so it reaches the persisted centroids' last bits).
	ids := make([]guardiannVectorID, len(merged))
	for i, r := range merged {
		ids[i] = r.id
	}
	out := merged[:0:0]
	for _, i := range javaHashMapOrder(ids) {
		r := merged[i]
		if r.collapsed {
			out = append(out, r)
			continue
		}
		md, err := g.fetchVectorMetadata(tx, r.id.pk)
		if err != nil {
			return nil, err
		}
		if md != nil && md.id.uuid == r.id.uuid {
			out = append(out, r)
		}
	}
	return out, nil
}

// executeDeferredTasks is Primitives.executeDeferredTasks: run up to numTasks
// queued tasks in key order (high priority first), the first unconditionally
// and the rest until the deadline (none when zero).
func (g *guardiann) executeDeferredTasks(tx fdb.WritableTransaction, numTasks int, deadline time.Time) (int, error) {
	info, err := g.fetchAccessInfo(tx)
	if err != nil {
		return 0, err
	}
	g, err = g.withAccessInfo(info)
	if err != nil {
		return 0, err
	}
	tasks, err := g.fetchSomeTasks(tx, numTasks)
	if err != nil {
		return 0, err
	}
	executed := 0
	for i, t := range tasks {
		if i > 0 && !deadline.IsZero() && !g.env.Now().Before(deadline) {
			break
		}
		ran, err := g.executeSingleTask(tx, t)
		if err != nil {
			return executed, err
		}
		if ran {
			executed++
		}
	}
	return executed, nil
}

func (g *guardiann) executeSingleTask(tx fdb.WritableTransaction, t *guardiannTask) (bool, error) {
	key := fdb.Key(g.sub(gSubTasks).Pack(tuple.Tuple{t.id}))
	existing, err := tx.Get(key).Get()
	if err != nil || existing == nil {
		return false, err
	}
	tx.Clear(key)
	if err := g.runTask(tx, t); err != nil {
		// The removal is buffered and the work is not: whatever the error, a
		// commit would drop the task (Java leaves that to its caller).
		if g.poison != nil {
			g.poison(err)
		}
		return false, err
	}
	if g.listener != nil {
		g.listener.onTaskExecuted()
	}
	return true, nil
}

// insert is Insert.insert.
func (g *guardiann) insert(tx fdb.WritableTransaction, pk tuple.Tuple, vector gVector, additional tuple.Tuple, maintainInTransaction bool) error {
	random := newSplittableRandomForKey(pk)
	info, err := g.fetchAccessInfo(tx)
	if err != nil {
		return err
	}
	existing, err := g.fetchVectorMetadata(tx, pk)
	if err != nil {
		return err
	}
	if info == nil {
		if info, err = g.initialAccessInfoAndFirstCluster(tx, random, vector); err != nil {
			return err
		}
		existing = nil
	} else if maintainInTransaction {
		if _, err := g.executeDeferredTasks(tx, 1, time.Time{}); err != nil {
			return err
		}
	}
	if existing != nil {
		return nil
	}
	g, err = g.withAccessInfo(info)
	if err != nil {
		return err
	}
	clientVector := vector
	vector, err = g.codec.toStoredCoordinates(vector)
	if err != nil {
		return err
	}
	if err = g.insertIntoClusters(tx, random, pk, clientVector, vector, additional, maintainInTransaction); err != nil {
		return err
	}
	return g.addToStatsIfNecessary(tx, random, info, vector)
}

func (g *guardiann) insertIntoClusters(tx fdb.WritableTransaction, random *splittableRandom, pk tuple.Tuple, clientVector, vector gVector, additional tuple.Tuple, maintainInTransaction bool) error {
	sc := g.config.constructionSearchConfig
	walk, err := g.centroidsOrderedByDistance(tx, clientVector, sc.centroidEfRingSearch, sc.centroidEfOutwardSearch)
	if err != nil {
		return err
	}
	var candidates []guardiannClusterWithDistance
	primaryDistance := math.NaN()
	var primaryID tuple.UUID
	for len(candidates) < g.config.insertMaxCandidateClusters {
		e, ok, err := walk.next()
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		m, err := g.requireClusterMetadata(tx, e.clusterID)
		if err != nil {
			return err
		}
		c := guardiannClusterWithDistance{meta: m, centroid: e.vector, distance: e.distance}
		if len(candidates) == 0 {
			primaryID, primaryDistance = m.id, e.distance
		} else if g.config.replicationPriority(e.distance, primaryDistance, m.numPrimary(), m.stats.meanOrNaN(),
			m.stats.populationStdDev()) < g.config.replicationPriorityMin {
			break
		}
		candidates = append(candidates, c)
	}
	// Java writes the identity before this check (Insert.java:293, :330-338),
	// leaving an identity without references behind a caught refusal; Go
	// checks first. Nothing between the two positions reads identity rows.
	if len(candidates) > 0 && !maintainInTransaction {
		if m := candidates[0].meta; m.numPrimary()+1 > g.config.primaryClusterHardMax {
			return &guardiannClusterCapacityError{clusterID: m.id, size: m.numPrimary() + 1, hardMax: g.config.primaryClusterHardMax}
		}
	}
	vectorUUID, err := guardiannVectorUUID(pk, g.config.deterministicRandomness, g)
	if err != nil {
		return err
	}
	md := guardiannVectorMetadata{id: guardiannVectorID{pk: pk, uuid: vectorUUID}, additionalValues: additional}
	g.writeVectorMetadata(tx, md)
	var selected []guardiannClusterWithDistance
	for _, c := range candidates {
		m := c.meta
		isPrimary := m.id == primaryID
		stats := m.stats
		if isPrimary {
			g.writeVectorRef(tx, m.id, guardiannVectorRef{id: md.id, vector: vector, primary: true})
			stats = stats.add(c.distance)
		} else {
			occluded, err := g.isOccluded(c, selected)
			if err != nil {
				return err
			}
			if occluded {
				continue
			}
			priority := g.config.replicationPriority(c.distance, primaryDistance, m.numPrimary(), m.stats.meanOrNaN(), m.stats.populationStdDev())
			g.writeVectorRef(tx, m.id, guardiannVectorRef{id: md.id, vector: vector, priority: priority})
			selected = append(selected, c)
		}
		primaryAdded, replicatedAdded := 0, 1
		if isPrimary {
			primaryAdded, replicatedAdded = 1, 0
		}
		if _, err := g.updateAndEnqueueSplitOrReassign(tx, random, m, c.centroid, primaryAdded, 0, replicatedAdded, stats, nil); err != nil {
			return err
		}
	}
	return nil
}

// guardiannVectorUUID is RandomHelpers.randomUuid(primaryKey, deterministic):
// a name-based (v3) UUID of the packed key, or a random one.
func guardiannVectorUUID(pk tuple.Tuple, deterministic bool, g *guardiann) (tuple.UUID, error) {
	if deterministic {
		return javaNameUUID(pk.Pack()), nil
	}
	return guardiannRandomUUID(nil, false, g.env)
}

func (g *guardiann) initialAccessInfoAndFirstCluster(tx fdb.WritableTransaction, random *splittableRandom, vector gVector) (*guardiannAccessInfoValue, error) {
	info := &guardiannAccessInfoValue{rotatorSeed: -1}
	if g.config.useRaBitQ && !g.config.metric.satisfiesPreservedUnderTranslation() {
		info.rotatorSeed = random.nextLong()
		info.negatedCentroid = make([]float64, g.config.numDimensions)
	}
	g.writeAccessInfo(tx, info)
	clusterID, err := g.randomUUID(random)
	if err != nil {
		return nil, err
	}
	g.writeClusterMetadata(tx, guardiannClusterMetadata{id: clusterID, stats: runningStatsIdentity()})
	return info, g.centroids.insertTyped(tx, tuple.Tuple{clusterID}, vector.data, vector.typ)
}

// delete is Delete.delete.
func (g *guardiann) delete(tx fdb.WritableTransaction, pk tuple.Tuple, vector gVector, maintainInTransaction bool) error {
	random := newSplittableRandomForKey(pk)
	info, err := g.fetchAccessInfo(tx)
	if err != nil || info == nil {
		return err
	}
	md, err := g.fetchVectorMetadata(tx, pk)
	if err != nil || md == nil {
		return err
	}
	if maintainInTransaction {
		// Declared (d): no inline task when a Go consumer would refuse the
		// head task, decided at snapshot so the skip adds no read conflict; a
		// delete that does not skip re-reads the head serializably, as Java's.
		skip, err := g.headTaskRefused(tx)
		if err != nil {
			return err
		}
		if !skip {
			if _, err := g.executeDeferredTasks(tx, 1, time.Time{}); err != nil {
				return err
			}
		}
	}
	g, err = g.withAccessInfo(info)
	if err != nil {
		return err
	}
	clientVector := vector
	vector, err = g.codec.toStoredCoordinates(vector)
	if err != nil {
		return err
	}
	sc := g.config.constructionSearchConfig
	walk, err := g.centroidsOrderedByDistance(tx, clientVector, sc.centroidEfRingSearch, sc.centroidEfOutwardSearch)
	if err != nil {
		return err
	}
	entries, err := walk.take(g.config.deleteMaxCandidateClusters)
	if err != nil {
		return err
	}
	clusters := make([]guardiannClusterWithDistance, len(entries))
	for i, e := range entries {
		m, err := g.requireClusterMetadata(tx, e.clusterID)
		if err != nil {
			return err
		}
		clusters[i] = guardiannClusterWithDistance{meta: m, centroid: e.vector, distance: e.distance}
	}
	// The candidates' references are fetched at deleteConcurrency
	// (Delete.java:170-176), once their metadata is in, whatever their number.
	if g.config.deleteConcurrency < 1 {
		return parallelismError(g.config.deleteConcurrency)
	}
	refs := make([]*guardiannVectorRef, len(entries))
	for i, e := range entries {
		if refs[i], err = g.fetchVectorRef(tx, e.clusterID, pk); err != nil {
			return err
		}
	}
	foundPrimary := false
	for i, ref := range refs {
		if ref == nil {
			continue
		}
		c := clusters[i]
		g.deleteVectorRef(tx, c.meta.id, pk)
		if ref.primary {
			foundPrimary = true
			stats, err := c.meta.stats.remove(c.distance)
			if err != nil {
				return err
			}
			underrepAdded := 0
			if ref.underrep {
				underrepAdded = -1
			}
			if err := g.updateAndEnqueueMergeOrReassign(tx, random.split(), c.meta, c.centroid, stats, underrepAdded); err != nil {
				return err
			}
		} else if _, err := g.updateAndEnqueueReassign(tx, random.split(), c.meta, c.centroid, 0, 0, -1, c.meta.stats, nil); err != nil {
			return err
		}
	}
	if !foundPrimary {
		g.deleteCollapsedID(tx, signatureUUID(vector), pk)
	}
	g.deleteVectorMetadata(tx, pk)
	return nil
}

// guardiannResult is a kNearestNeighborsSearch ResultEntry.
type guardiannResult struct {
	primaryKey       tuple.Tuple
	vector           gVector
	additionalValues tuple.Tuple
	distance         float64
}

// search is Search.kNearestNeighborsSearch.
func (g *guardiann) search(tx fdb.ReadTransaction, k int, sc guardiannSearchConfig, query gVector) ([]guardiannResult, error) {
	info, err := g.fetchAccessInfo(tx)
	if err != nil || info == nil || k <= 0 {
		return nil, err
	}
	g, err = g.withAccessInfo(info)
	if err != nil {
		return nil, err
	}
	codec := g.codec
	transformedQuery, err := codec.toStoredCoordinates(query)
	if err != nil {
		return nil, err
	}
	walk, err := g.centroidsOrderedByDistance(tx, query, sc.centroidEfRingSearch, sc.centroidEfOutwardSearch)
	if err != nil {
		return nil, err
	}
	entries, err := walk.take(sc.searchMaxClusters)
	if err != nil {
		return nil, err
	}
	clusters := make([]guardiannClusterWithDistance, 0, len(entries))
	for _, e := range entries {
		m, err := g.fetchClusterMetadata(tx, e.clusterID)
		if err != nil {
			return nil, err
		}
		if m == nil {
			return nil, &RecordCoreError{Message: "guardiann cluster metadata is missing"}
		}
		clusters = append(clusters, guardiannClusterWithDistance{meta: *m, centroid: e.vector, distance: e.distance})
	}
	clusters = pruneClusters(clusters, sc)
	var candidates []guardiannRefAndDistance
	for _, c := range clusters {
		refs, err := g.fetchVectorRefs(tx, c.meta.id, codec.decode)
		if err != nil {
			return nil, err
		}
		for _, r := range refs {
			distance, err := codec.distance(transformedQuery, r.vector)
			if err != nil {
				return nil, err
			}
			candidates = append(candidates, guardiannRefAndDistance{ref: r, distance: distance})
		}
	}
	pool := sc.candidatePoolSize(k)
	top := distinctTopKMin(candidates, pool)
	collapsed := false
	for _, t := range top {
		collapsed = collapsed || t.ref.collapsed
	}
	if collapsed {
		var expanded []guardiannRefAndDistance
		for _, t := range top {
			if !t.ref.collapsed {
				expanded = append(expanded, t)
				continue
			}
			ids, err := g.fetchCollapsedIDs(tx, signatureUUID(t.ref.vector))
			if err != nil {
				return nil, err
			}
			for _, id := range ids {
				r := t.ref
				r.id = id
				expanded = append(expanded, guardiannRefAndDistance{ref: r, distance: t.distance})
			}
		}
		top = distinctTopKMin(expanded, pool)
	}
	metadata := map[string]*guardiannVectorMetadata{}
	var results []guardiannResult
	for _, t := range top {
		key := string(t.ref.id.pk.Pack())
		md, ok := metadata[key]
		if !ok {
			if md, err = g.fetchVectorMetadata(tx, t.ref.id.pk); err != nil {
				return nil, err
			}
			metadata[key] = md
		}
		if len(results) >= k || md == nil || md.id.uuid != t.ref.id.uuid {
			continue
		}
		results = append(results, guardiannResult{primaryKey: md.id.pk, vector: codec.toClientCoordinates(t.ref.vector), additionalValues: md.additionalValues, distance: t.distance})
	}
	return results, nil
}

// pruneClusters is Search.pruneClusters: past the minimum, stop at the first
// cluster whose distance ratio to the nearest exceeds the cutoff.
func pruneClusters(clusters []guardiannClusterWithDistance, sc guardiannSearchConfig) []guardiannClusterWithDistance {
	if len(clusters) <= sc.searchMinClustersBeforePruning {
		return clusters
	}
	nearest := clusters[0].distance
	i := sc.searchMinClustersBeforePruning
	for ; i < len(clusters); i++ {
		if clusters[i].distance/nearest > sc.searchDistanceRatioCutoff {
			break
		}
	}
	return clusters[:i]
}

// Task kinds (TaskKind).
const (
	taskSplitMerge = 0
	taskReassign   = 1
	taskBounce     = 2
	taskCollapse   = 3
)

var taskKindNames = []string{"SPLIT_MERGE", "REASSIGN", "BOUNCE", "COLLAPSE"}

// guardiannTask is one deferred task (AbstractDeferredTask and subclasses).
type guardiannTask struct {
	kind       int
	id         tuple.UUID
	targets    []tuple.UUID
	centroid   gVector               // split/merge, reassign, collapse
	nearest    []guardiannClusterRef // split/merge, reassign
	causes     []tuple.UUID          // reassign
	dependents []tuple.UUID          // bounce
	finalKind  int                   // bounce
}

func (t *guardiannTask) target() tuple.UUID { return t.targets[0] }

// valueTuple is each task's valueTuple().
func (t *guardiannTask) valueTuple(encode func(gVector) []byte) tuple.Tuple {
	nearest := func() tuple.Tuple {
		out := make(tuple.Tuple, len(t.nearest))
		for i, r := range t.nearest {
			out[i] = clusterRefTuple(r, encode)
		}
		return out
	}
	switch t.kind {
	case taskSplitMerge:
		return tuple.Tuple{int64(t.kind), t.target(), encode(t.centroid), nearest()}
	case taskReassign:
		return tuple.Tuple{int64(t.kind), t.target(), encode(t.centroid), uuidSetTuple(t.causes), nearest()}
	case taskBounce:
		return tuple.Tuple{int64(t.kind), uuidSetTuple(t.targets), uuidSetTuple(t.dependents), taskKindNames[t.finalKind]}
	default:
		return tuple.Tuple{int64(t.kind), t.target(), encode(t.centroid)}
	}
}

func taskFromTuples(key, value tuple.Tuple, decode func([]byte) (gVector, error)) (*guardiannTask, error) {
	const what = "task"
	kind, err := guardiannInt(value, 0, what)
	if err != nil {
		return nil, err
	}
	id, err := guardiannElem[tuple.UUID](key, 0, "task key")
	if err != nil {
		return nil, err
	}
	t := &guardiannTask{kind: kind, id: id}
	nearest := func(i int) error {
		nt, err := guardiannElem[tuple.Tuple](value, i, what)
		if err != nil {
			return err
		}
		for j := range nt {
			e, err := guardiannElem[tuple.Tuple](nt, j, "task nearest clusters")
			if err != nil {
				return err
			}
			r, err := clusterRefFromTuple(e, decode)
			if err != nil {
				return err
			}
			t.nearest = append(t.nearest, r)
		}
		return nil
	}
	uuids := func(i int) ([]tuple.UUID, error) {
		set, err := guardiannElem[tuple.Tuple](value, i, what)
		if err != nil {
			return nil, err
		}
		return uuidSetFromTuple(set)
	}
	switch t.kind {
	case taskBounce:
		if t.targets, err = uuids(1); err != nil {
			return nil, err
		}
		if t.dependents, err = uuids(2); err != nil {
			return nil, err
		}
		name, err := guardiannElem[string](value, 3, what)
		if err != nil {
			return nil, err
		}
		t.finalKind = -1
		for i, n := range taskKindNames {
			if n == name {
				t.finalKind = i
			}
		}
		if t.finalKind < 0 {
			// Java's Kind.valueOf throws on an unknown name.
			return nil, &RecordCoreError{Message: fmt.Sprintf("guardiann bounce task: unknown final kind %q", name)}
		}
		return t, nil
	case taskSplitMerge, taskReassign, taskCollapse:
		target, err := guardiannElem[tuple.UUID](value, 1, what)
		if err != nil {
			return nil, err
		}
		t.targets = []tuple.UUID{target}
		raw, err := guardiannElem[[]byte](value, 2, what)
		if err != nil {
			return nil, err
		}
		if t.centroid, err = decode(raw); err != nil {
			return nil, err
		}
		switch t.kind {
		case taskSplitMerge:
			err = nearest(3)
		case taskReassign:
			if t.causes, err = uuids(3); err != nil {
				return nil, err
			}
			err = nearest(4)
		}
		return t, err
	}
	return nil, &RecordCoreError{Message: "unknown guardiann task kind"}
}

func (g *guardiann) writeTask(tx fdb.WritableTransaction, t *guardiannTask) {
	tx.Set(fdb.Key(g.sub(gSubTasks).Pack(tuple.Tuple{t.id})), t.valueTuple(g.codec.encode).Pack())
	if g.listener != nil {
		g.listener.onTaskEnqueued()
	}
}

func (g *guardiann) fetchSomeTasks(tx fdb.ReadTransaction, n int) ([]*guardiannTask, error) {
	ts := g.sub(gSubTasks)
	r, err := fdb.PrefixRange(ts.Bytes())
	if err != nil {
		return nil, err
	}
	kvs, err := tx.GetRange(r, fdb.RangeOptions{Limit: n, Mode: fdb.StreamingModeWantAll}).GetSliceWithError()
	if err != nil {
		return nil, err
	}
	out := make([]*guardiannTask, 0, len(kvs))
	for _, kv := range kvs {
		key, err := ts.Unpack(kv.Key)
		if err != nil {
			return nil, err
		}
		value, err := tuple.Unpack(kv.Value)
		if err != nil {
			return nil, err
		}
		t, err := taskFromTuples(key, value, g.codec.decode)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

func (g *guardiann) fetchTask(tx fdb.ReadTransaction, id tuple.UUID) (*guardiannTask, error) {
	b, err := tx.Get(fdb.Key(g.sub(gSubTasks).Pack(tuple.Tuple{id}))).Get()
	if err != nil || b == nil {
		return nil, err
	}
	value, err := tuple.Unpack(b)
	if err != nil {
		return nil, err
	}
	return taskFromTuples(tuple.Tuple{id}, value, g.codec.decode)
}

// Task ids: the top bit orders normal-priority tasks after high-priority ones.
func (g *guardiann) normalPriorityTaskID(random *splittableRandom) (tuple.UUID, error) {
	u, err := g.randomUUID(random)
	u[0] |= 0x80
	return u, err
}

func (g *guardiann) highPriorityTaskID(random *splittableRandom) (tuple.UUID, error) {
	u, err := g.randomUUID(random)
	u[0] &= 0x7f
	return u, err
}

// sortClustersByDistance is Comparator.comparing(distance), stable.
func sortClustersByDistance(cs []guardiannClusterWithDistance) {
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].distance < cs[j].distance })
}
