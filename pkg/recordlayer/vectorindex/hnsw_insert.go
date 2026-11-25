// Portions derived from FoundationDB Record Layer (InliningStorageAdapter.java,
// Insert.java, NodeReferenceWithVector.java, Config.java, and others),
// Copyright 2015-2025 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2026 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package vectorindex

import (
	"fmt"

	"fdb.dev/internal/recordlayer/vectorcodec"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// Insert adds a vector to the HNSW graph.
// primaryKey identifies the record. vector is the float64 vector to index.
// Wire-compatible with Java's HNSW insert (compact + inlining node formats,
// deterministic layer assignment, FHT-KAC rotation for RaBitQ).
// Insert adds a DOUBLE vector; see insertTyped.
func (g *hnswGraph) Insert(tx fdb.WritableTransaction, primaryKey tuple.Tuple, vector []float64) error {
	return g.insertTyped(tx, primaryKey, vector, vectorcodec.TypeDouble)
}

// insertTyped adds primaryKey's vector. vectorType is the VectorType ordinal
// the vector was encoded with: without a storage transform Java's no-op
// quantizer stores the vector at that precision (a HALF column stays HALF).
func (g *hnswGraph) insertTyped(tx fdb.WritableTransaction, primaryKey tuple.Tuple, vector []float64, vectorType byte) error {
	// Fire both existence check and access info read as parallel futures.
	// Existence check uses layer 0 (always compact format).
	existKey := g.storage.dataSubspace.Pack(tuple.Tuple{int64(0), primaryKey})
	existFuture := tx.Get(fdb.Key(existKey))
	accessKey := g.storage.accessSubspace.Pack(tuple.Tuple{})
	accessFuture := tx.Get(fdb.Key(accessKey))

	// Resolve both reads before acting on either, as Java combines them
	// (Insert.java:184-194); an unresolved future must not outlive the call.
	existData, existErr := existFuture.Get()
	accessData, accessErr := accessFuture.Get()
	if existErr != nil {
		return fmt.Errorf("hnsw insert: existence check: %w", existErr)
	}
	if accessErr != nil {
		return fmt.Errorf("hnsw insert: access info read: %w", accessErr)
	}
	if existData != nil {
		// A node already in the graph is left as it is (Insert.java:195-197):
		// the maintainer removes a record's old entry before inserting its new
		// one, so an insert finds its key present only when the entry is
		// already indexed, as when an index build reaches a record a
		// concurrent save indexed. Deleting and re-inserting it rewired the
		// graph Java leaves untouched.
		return nil
	}

	// Determine insertion layer (deterministic per PK).
	insertLayer := topLayer(primaryKey, g.config.M)

	accessInfo, epErr := g.storage.parseAccessInfo(accessData)

	if epErr != nil {
		// No entry point — first node in the graph.
		return g.firstInsert(tx, primaryKey, vector, vectorType, insertLayer)
	}
	if err := g.raBitQuantizerAdmits(accessInfo); err != nil {
		return err
	}

	// Build transform from access info (nil if no rotation configured).
	transform := g.buildTransform(accessInfo)
	g.opXform = transform

	// Transform vector for storage and search.
	// All vectors in the graph are stored in the current coordinate system: when a
	// transform is active the vector is rotated/translated and quantized; otherwise it
	// is stored plain (and lifted at read once a centroid exists), matching Java's
	// noOp-until-centroid quantizer choice.
	queryVec := vector
	vectorForWrite := hnswVector{data: vectorcodec.SerializeAs(vectorType, vector), transformed: true}
	if transform != nil {
		queryVec = transform.apply(vector)
		vectorForWrite.data = serializeVector(queryVec)
	}

	epLayer := accessInfo.layer
	epPK := accessInfo.pk
	epVector := hnswVector{data: accessInfo.vectorBytes, transformed: true}

	// Preload upper layers used in greedy descent (few nodes each).
	for layer := epLayer; layer > insertLayer && layer > 0; layer-- {
		if preloadErr := g.storage.preloadLayerDispatch(tx, layer); preloadErr != nil {
			return preloadErr
		}
	}

	// Greedy search from top to insertion layer.
	// Search uses the transformed query vector.
	var err error
	currentPK := epPK
	currentVector := epVector
	for layer := epLayer; layer > insertLayer; layer-- {
		currentPK, currentVector, err = g.searchLayerGreedy(tx, queryVec, currentPK, currentVector, layer)
		if err != nil {
			return err
		}
	}

	// Insert at each layer from min(insertLayer, epLayer) down to 0.
	for layer := min(insertLayer, epLayer); layer >= 0; layer-- {
		currentPK, currentVector, err = g.insertIntoLayer(tx, primaryKey, vectorForWrite, queryVec, currentPK, currentVector, layer)
		if err != nil {
			return err
		}
	}

	// If new node has layers above the entry point, save those layers too
	// and update the entry point.
	if insertLayer > epLayer {
		for layer := epLayer + 1; layer <= insertLayer; layer++ {
			if saveErr := g.saveNodeLayer(tx, layer, primaryKey, vectorForWrite, nil, nil); saveErr != nil {
				return fmt.Errorf("hnsw insert: save lonely node at layer %d: %w", layer, saveErr)
			}
		}
		accessInfo.layer = insertLayer
		accessInfo.pk = primaryKey
		accessInfo.vectorBytes = vectorForWrite.data
		g.storage.saveAccessInfo(tx, accessInfo)
	}

	// RaBitQ centroid bootstrap: sample this vector and, once StatsThreshold have accumulated,
	// establish the rotated centroid (Java Insert.addToStatsIfNecessary). The random is
	// PK-seeded per insert (Primitives.random(primaryKey)); queryVec is the vector in the
	// current (identity, pre-centroid) transform space, which is what Java samples.
	if err := g.addToStatsIfNecessary(tx, accessInfo, queryVec, newSplittableRandomForKey(primaryKey)); err != nil {
		return err
	}

	return nil
}

// insertIntoLayer connects primaryKey into one layer and updates reverse edges,
// returning the nearest neighbor found to seed the next layer down.
func (g *hnswGraph) insertIntoLayer(tx fdb.WritableTransaction, primaryKey tuple.Tuple, vectorForWrite hnswVector, queryVec []float64, currentPK tuple.Tuple, currentVector hnswVector, layer int) (tuple.Tuple, hnswVector, error) {
	// Find ef_construction nearest neighbors at this layer.
	neighbors, err := g.searchLayerMulti(tx, queryVec, currentPK, currentVector, g.config.EfConstruction, layer)
	if err != nil {
		return nil, hnswVector{}, err
	}

	// The NEW node connects to up to M neighbors (Java Insert.insertIntoLayer:
	// selectCandidates(..., getConfig().getM(), ...) — getM() is layer-independent).
	// maxConn (MMax, or MMax0 at layer 0) is the cap for PRUNING existing neighbors
	// when a reverse edge overflows them, NOT for the new node's own selection.
	// Selecting maxConn here was a Go-only divergence: with the default MMax0=32 > M=16
	// it gave the new node up to 32 layer-0 edges vs Java's 16 — a denser graph.
	maxConn := g.config.MMax
	if layer == 0 {
		maxConn = g.config.MMax0
	}
	var selected []hnswCandidate
	if g.config.ExtendCandidates {
		selected, err = g.selectNeighborsHeuristic(tx, queryVec, neighbors, g.config.M, layer)
		if err != nil {
			return nil, hnswVector{}, err
		}
	} else {
		selected = g.selectNeighbors(neighbors, g.config.M)
	}

	// Decode the ≤M selected neighbor spans to tuple.Tuple once. The graph-write
	// path (save/prune) operates on tuple.Tuple; only this bounded set is decoded,
	// so the search traversal stays boxing-free.
	selectedPKs := make([]tuple.Tuple, len(selected))
	for i, nb := range selected {
		selectedPKs[i], err = decodeNestedPK(nb.pkSpan)
		if err != nil {
			return nil, hnswVector{}, fmt.Errorf("hnsw insert: decode selected neighbor span at layer %d: %w", layer, err)
		}
	}

	// Build neighbor list for the new node at this layer, threading each selected
	// candidate's vector with it (Java's NodeReferenceWithVector): inlining edge
	// values need the neighbor's vector, and on a cold tx the cache may not have it.
	newNodeNeighbors := make([]tuple.Tuple, len(selectedPKs))
	copy(newNodeNeighbors, selectedPKs)
	newNodeNeighborVecs := make([]hnswVector, len(selected))
	for i, nb := range selected {
		newNodeNeighborVecs[i] = nb.vector
	}

	// Save new node at this layer.
	if saveErr := g.saveNodeLayer(tx, layer, primaryKey, vectorForWrite, newNodeNeighbors, newNodeNeighborVecs); saveErr != nil {
		return nil, hnswVector{}, fmt.Errorf("hnsw insert: save new node at layer %d: %w", layer, saveErr)
	}

	// Add reverse connections to each neighbor.
	for _, nbPK := range selectedPKs {
		if revErr := g.addReverseConnection(tx, layer, nbPK, primaryKey, vectorForWrite, maxConn); revErr != nil {
			return nil, hnswVector{}, revErr
		}
	}

	if len(neighbors) > 0 {
		// Use nearest neighbor as entry point for next layer down.
		currentPK, err = decodeNestedPK(neighbors[0].pkSpan)
		if err != nil {
			return nil, hnswVector{}, fmt.Errorf("hnsw insert: decode next-layer entry point: %w", err)
		}
		currentVector = neighbors[0].vector
	}
	return currentPK, currentVector, nil
}

// addReverseConnection adds primaryKey to nbPK's neighbor list at layer, pruning if needed.
func (g *hnswGraph) addReverseConnection(tx fdb.WritableTransaction, layer int, nbPK, primaryKey tuple.Tuple, vectorForWrite hnswVector, maxConn int) error {
	nbVecBytes, nbNeighborSpans, loadErr := g.storage.loadNodeLayerDispatch(tx, layer, nbPK)
	if loadErr != nil {
		return fmt.Errorf("hnsw insert: load neighbor %v at layer %d for reverse connection: %w", nbPK, layer, loadErr)
	}

	// Decode this neighbor's own edge list (bounded by maxConn) back to
	// tuple.Tuple for the write path, then add the new node as a neighbor.
	nbNeighbors := make([]tuple.Tuple, 0, len(nbNeighborSpans)+1)
	for _, sp := range nbNeighborSpans {
		pk, derr := decodeNestedPK(sp)
		if derr != nil {
			return fmt.Errorf("hnsw insert: decode reverse neighbor span at layer %d: %w", layer, derr)
		}
		nbNeighbors = append(nbNeighbors, pk)
	}
	nbNeighbors = append(nbNeighbors, primaryKey)

	// Prune at the limit (Primitives.pruneNeighborsIfNecessary skips
	// only below mMax: a full list is re-selected too).
	if len(nbNeighbors) >= maxConn {
		// Resolve vector bytes (at inlining layers, own vector is at layer 0).
		resolvedVec := g.storage.resolveVectorBytes(layer, nbPK, nbVecBytes)
		nbVec, decErr := g.decodeStoredVector(resolvedVec)
		if decErr != nil {
			return fmt.Errorf("hnsw insert: decode neighbor %v vector at layer %d for pruning: %w", nbPK, layer, decErr)
		}
		var err error
		nbNeighbors, err = g.pruneNeighbors(tx, nbVec, hnswVector{data: resolvedVec}, nbNeighbors, maxConn, layer)
		if err != nil {
			return err
		}
	}

	// Thread the new node's vector for its entry in the reverse list (it is not
	// in the cold cache yet — its layer-0 record is saved after the upper
	// layers); existing neighbors resolve from the cache (their vectors were
	// cached by the edge read that surfaced them).
	nbNeighborVecs := make([]hnswVector, len(nbNeighbors))
	for i, pk := range nbNeighbors {
		if tupleEqual(pk, primaryKey) {
			nbNeighborVecs[i] = vectorForWrite
		}
	}
	if saveErr := g.saveNodeLayer(tx, layer, nbPK, hnswVector{data: nbVecBytes}, nbNeighbors, nbNeighborVecs); saveErr != nil {
		return fmt.Errorf("hnsw insert: save reverse connection for %v at layer %d: %w", nbPK, layer, saveErr)
	}
	return nil
}

// firstInsert handles the first-ever insertion into the graph.
// When a quantizer is enabled and the metric doesn't preserve translation (Cosine/DotProduct),
// initializes the FHT-KAC rotator immediately with a zero centroid.
// Matches Java's Insert.firstInsert().
func (g *hnswGraph) firstInsert(tx fdb.WritableTransaction, primaryKey tuple.Tuple, vector []float64, vectorType byte, insertLayer int) error {
	info := &hnswAccessInfo{
		layer:       insertLayer,
		pk:          primaryKey,
		rotatorSeed: -1,
	}

	queryVec := vector

	if g.config.Quantizer != nil && !g.config.Metric.satisfiesPreservedUnderTranslation() {
		// Cosine/DotProduct: activate rotation immediately, seeded by the
		// insert's RandomHelpers.random(pk) (Insert.firstInsert).
		info.rotatorSeed = newSplittableRandomForKey(primaryKey).nextLong()

		// Zero centroid = no translation, rotation only.
		info.centroid = make([]float64, g.config.NumDimensions)
		if err := g.raBitQuantizerAdmits(info); err != nil {
			return err
		}

		// Apply transform before encoding.
		transform := g.buildTransform(info)
		if transform != nil {
			queryVec = transform.apply(vector)
		}
	}

	// Quantize only when a transform is active (Cosine/DotProduct: immediate rotation).
	// For a translation-preserving metric (Euclidean) the first node is pre-centroid, so
	// it is stored plain — matching Java's noOp quantizer until a centroid is sampled.
	g.opXform = g.buildTransform(info)
	vectorForWrite := hnswVector{data: vectorcodec.SerializeAs(vectorType, queryVec), transformed: true}
	if g.opXform != nil {
		vectorForWrite.data = serializeVector(queryVec)
	}
	info.vectorBytes = vectorForWrite.data

	for layer := 0; layer <= insertLayer; layer++ {
		if saveErr := g.saveNodeLayer(tx, layer, primaryKey, vectorForWrite, nil, nil); saveErr != nil {
			return fmt.Errorf("hnsw first insert: save layer %d: %w", layer, saveErr)
		}
	}
	g.storage.saveAccessInfo(tx, info)
	return nil
}

// --- RaBitQ centroid bootstrap (Java Insert.addToStatsIfNecessary + StorageAdapter samples) ---

// aggregatedVector is one SAMPLES entry: a partial sum of `count` raw vectors. Java AggregatedVector.
type aggregatedVector struct {
	count int
	vec   []float64
}

// addToStatsIfNecessary samples the inserted vector toward establishing a RaBitQ centroid,
// matching Java Insert.addToStatsIfNecessary. Active only when RaBitQ is enabled and no centroid
// is yet established. transformedVec is the new vector in the current (identity, pre-centroid)
// transform space. The random is the insert's PK-seeded SplittableRandom, consumed in Java's
// order (sample, maintain, then the rotator seed at the transition).
func (g *hnswGraph) addToStatsIfNecessary(tx fdb.WritableTransaction, info *hnswAccessInfo, transformedVec []float64, random *splittableRandom) error {
	if g.config.Quantizer == nil || info == nil || info.hasTransform() {
		return nil // not RaBitQ, or centroid already established
	}
	if random.nextDouble() < g.config.SampleVectorStatsProbability {
		if err := g.storage.appendSampledVector(tx, 1, transformedVec); err != nil {
			return err
		}
	}
	if random.nextDouble() >= g.config.MaintainStatsProbability {
		return nil
	}
	// Roll up: consume up to 50 samples, aggregate, append the partial back.
	samples, err := g.storage.consumeSampledVectors(tx, 50)
	if err != nil {
		return err
	}
	agg, aggErr := aggregateVectors(samples)
	if aggErr != nil {
		return aggErr
	}
	if agg.count == 0 {
		return nil
	}
	if err := g.storage.appendSampledVector(tx, agg.count, agg.vec); err != nil {
		return err
	}
	if agg.count < g.config.StatsThreshold {
		return nil
	}
	// Establish the centroid: rotatorSeed = random.nextLong(); centroid = -mean; rotate; transition.
	rotatorSeed := random.nextLong()
	rotator := newFhtKacRotator(rotatorSeed, g.config.NumDimensions, 10)
	centroid := make([]float64, len(agg.vec))
	for i, v := range agg.vec {
		centroid[i] = v * (-1.0 / float64(agg.count))
	}
	rotatedCentroid := rotator.apply(centroid)

	// Re-express the entry node vector in the new (rotated + translated) coordinate system, so the
	// entry node is always in the internal system while data vectors may be a mix (Java comment).
	normalize := g.config.Metric == VectorMetricCosine
	transform := newHNSWTransform(rotatorSeed, rotatedCentroid, g.config.NumDimensions, normalize)
	entryVec, derr := g.decodeVector(hnswVector{data: info.vectorBytes, transformed: true})
	if derr != nil {
		return fmt.Errorf("hnsw stats: decode entry vector: %w", derr)
	}
	info.rotatorSeed = rotatorSeed
	info.centroid = rotatedCentroid
	// Access-info keeps the transformed, unquantized vector. Existing plain
	// data nodes remain raw and are lifted independently when fetched.
	info.vectorBytes = serializeVector(transform.apply(entryVec))
	g.storage.saveAccessInfo(tx, info)
	return g.storage.deleteAllSampledVectors(tx)
}

// aggregateVectors sums the partial vectors and their counts. Java Insert.aggregateVectors.
// All samples come from one index, so their dimensions must agree; a mismatch means
// corrupt SAMPLES data and must surface, not be silently truncated into a wrong centroid.
func aggregateVectors(samples []aggregatedVector) (aggregatedVector, error) {
	var sum []float64
	count := 0
	for _, s := range samples {
		if sum == nil {
			sum = make([]float64, len(s.vec))
		}
		if len(s.vec) != len(sum) {
			return aggregatedVector{}, fmt.Errorf("hnsw stats: sampled vector dimension mismatch: %d vs %d", len(s.vec), len(sum))
		}
		for i, v := range s.vec {
			sum[i] += v
		}
		count += s.count
	}
	return aggregatedVector{count: count, vec: sum}, nil
}

// appendSampledVector writes one SAMPLES entry. Key: samplesSubspace.Pack(count, uuid);
// value: Tuple{serializeVector(vec)}. Matches Java StorageHelpers.appendSampledVector — the
// per-entry count is in the key, the (raw) vector in the value, and the unique element is a tuple
// UUID, Java's UUID.randomUUID (version 4, IETF variant). It is ignored on read, so entries an
// older Go wrote with a byte-string element stay consumable.
func (s *hnswStorage) appendSampledVector(tx fdb.WritableTransaction, count int, vec []float64) error {
	var uniq tuple.UUID
	// Draw the unique key element through the DST randomness seam (crypto/rand
	// in production, the seeded source in simulation) so a run is reproducible.
	if _, err := s.env.Read(uniq[:]); err != nil {
		return fmt.Errorf("hnsw stats: sample key entropy: %w", err)
	}
	uniq[6] = uniq[6]&0x0f | 0x40 // version 4
	uniq[8] = uniq[8]&0x3f | 0x80 // IETF variant
	key := s.samplesSubspace.Pack(tuple.Tuple{int64(count), uniq})
	value := tuple.Tuple{serializeVector(vec)}.Pack()
	tx.Set(fdb.Key(key), value)
	return nil
}

// consumeSampledVectors reads up to numMax SAMPLES entries (snapshot, reverse) and CLEARS each
// (only the consumed keys take a read-conflict, not the whole range). Java consumeSampledVectors.
func (s *hnswStorage) consumeSampledVectors(tx fdb.WritableTransaction, numMax int) ([]aggregatedVector, error) {
	r, err := fdb.PrefixRange(s.samplesSubspace.Bytes())
	if err != nil {
		return nil, fmt.Errorf("hnsw stats: samples range: %w", err)
	}
	kvs, err := tx.Snapshot().GetRange(r, fdb.RangeOptions{Limit: numMax, Reverse: true, Mode: fdb.StreamingModeIterator}).GetSliceWithError()
	if err != nil {
		return nil, fmt.Errorf("hnsw stats: read samples: %w", err)
	}
	out := make([]aggregatedVector, 0, len(kvs))
	for _, kv := range kvs {
		av, perr := s.aggregatedVectorFromRaw(kv.Key, kv.Value)
		if perr != nil {
			continue
		}
		if cerr := tx.AddReadConflictKey(fdb.Key(kv.Key)); cerr != nil {
			return nil, cerr
		}
		tx.Clear(fdb.Key(kv.Key))
		out = append(out, av)
	}
	return out, nil
}

// deleteAllSampledVectors clears the whole SAMPLES subspace. Java deleteAllSampledVectors.
func (s *hnswStorage) deleteAllSampledVectors(tx fdb.WritableTransaction) error {
	r, err := fdb.PrefixRange(s.samplesSubspace.Bytes())
	if err != nil {
		return fmt.Errorf("hnsw stats: samples range: %w", err)
	}
	tx.ClearRange(r)
	return nil
}

// aggregatedVectorFromRaw decodes one SAMPLES entry: count from key[0], vector from the value.
func (s *hnswStorage) aggregatedVectorFromRaw(key, value []byte) (aggregatedVector, error) {
	keyTuple, err := s.samplesSubspace.Unpack(fdb.Key(key))
	if err != nil || len(keyTuple) < 1 {
		return aggregatedVector{}, fmt.Errorf("hnsw stats: unpack sample key")
	}
	count, ok := keyTuple[0].(int64)
	if !ok {
		return aggregatedVector{}, fmt.Errorf("hnsw stats: sample count not int64")
	}
	valTuple, err := tuple.Unpack(value)
	if err != nil || len(valTuple) < 1 {
		return aggregatedVector{}, fmt.Errorf("hnsw stats: unpack sample value")
	}
	vb, ok := valTuple[0].([]byte)
	if !ok {
		return aggregatedVector{}, fmt.Errorf("hnsw stats: sample vector not bytes")
	}
	vec, derr := deserializeVector(vb)
	if derr != nil {
		return aggregatedVector{}, derr
	}
	return aggregatedVector{count: int(count), vec: vec}, nil
}
