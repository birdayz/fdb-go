package recordlayer

import (
	"container/heap"
	"encoding/binary"
	"fmt"
	"math"
	"sort"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
)

// HNSWConfig configures an HNSW graph.
// Matches Java's com.apple.foundationdb.async.hnsw.Config.
type HNSWConfig struct {
	NumDimensions  int
	M              int          // Connectivity factor (default 16)
	MMax           int          // Max connections for non-zero layers (default M)
	MMax0          int          // Max connections for layer 0 (default 2*M)
	EfConstruction int          // Insertion search factor (default 200)
	Metric         VectorMetric // Distance metric
}

// ValidateHNSWConfig validates the HNSW configuration.
// Matches Java's Config validation.
func ValidateHNSWConfig(c HNSWConfig) error {
	if c.NumDimensions < 1 {
		return fmt.Errorf("hnsw: numDimensions must be >= 1, got %d", c.NumDimensions)
	}
	if c.M < 4 || c.M > 200 {
		return fmt.Errorf("hnsw: m must be in [4, 200], got %d", c.M)
	}
	if c.MMax < 4 || c.MMax > 200 {
		return fmt.Errorf("hnsw: mMax must be in [4, 200], got %d", c.MMax)
	}
	if c.MMax0 < 4 || c.MMax0 > 300 {
		return fmt.Errorf("hnsw: mMax0 must be in [4, 300], got %d", c.MMax0)
	}
	if c.EfConstruction < 100 || c.EfConstruction > 400 {
		return fmt.Errorf("hnsw: efConstruction must be in [100, 400], got %d", c.EfConstruction)
	}
	return nil
}

// DefaultHNSWConfig returns a default HNSW configuration.
func DefaultHNSWConfig(numDimensions int) HNSWConfig {
	return HNSWConfig{
		NumDimensions:  numDimensions,
		M:              16,
		MMax:           16,
		MMax0:          32,
		EfConstruction: 200,
		Metric:         VectorMetricEuclidean,
	}
}

// hnswGraph is an HNSW (Hierarchical Navigable Small World) graph stored in FDB.
// Wire-compatible with Java's com.apple.foundationdb.async.hnsw.HNSW.
type hnswGraph struct {
	storage *hnswStorage
	config  HNSWConfig
}

// NewHNSWGraph creates a new HNSW graph.
func NewHNSWGraph(storage *hnswStorage, config HNSWConfig) *hnswGraph {
	return &hnswGraph{
		storage: storage,
		config:  config,
	}
}

// Insert adds a vector to the HNSW graph.
// primaryKey identifies the record. vector is the float64 vector to index.
// Wire-compatible with Java's HNSW insert (COMPACT node format, deterministic layer assignment).
func (g *hnswGraph) Insert(tx fdb.Transaction, primaryKey tuple.Tuple, vector []float64) error {
	vecBytes := serializeVector(vector)

	// Check if node already exists — if so, delete first (update semantics).
	_, _, err := g.storage.loadNodeLayer(tx, 0, primaryKey)
	if err == nil {
		// Node exists — delete and re-insert.
		if delErr := g.Delete(tx, primaryKey, vector); delErr != nil {
			return delErr
		}
	}

	// Determine insertion layer (deterministic per PK).
	insertLayer := topLayer(primaryKey, g.config.M)

	// Load access info (entry point).
	epLayer, epPK, epVecBytes, epErr := g.storage.loadAccessInfo(tx)

	if epErr != nil {
		// No entry point — this is the first node.
		// Save node at all layers from 0 to insertLayer.
		for layer := 0; layer <= insertLayer; layer++ {
			g.storage.saveNodeLayer(tx, layer, primaryKey, vecBytes, nil)
		}
		g.storage.saveAccessInfo(tx, insertLayer, primaryKey, vecBytes)
		return nil
	}

	epVec, _ := deserializeVector(epVecBytes)

	// Greedy search from top to insertion layer.
	currentPK := epPK
	currentVec := epVec
	for layer := epLayer; layer > insertLayer; layer-- {
		currentPK, currentVec, err = g.searchLayerGreedy(tx, vector, currentPK, currentVec, layer)
		if err != nil {
			return err
		}
	}

	// Insert at each layer from min(insertLayer, epLayer) down to 0.
	for layer := min(insertLayer, epLayer); layer >= 0; layer-- {
		// Find ef_construction nearest neighbors at this layer.
		neighbors, err := g.searchLayerMulti(tx, vector, currentPK, currentVec, g.config.EfConstruction, layer)
		if err != nil {
			return err
		}

		// Select M best neighbors.
		maxConn := g.config.MMax
		if layer == 0 {
			maxConn = g.config.MMax0
		}
		selected := g.selectNeighbors(neighbors, maxConn)

		// Build neighbor list for the new node at this layer.
		newNodeNeighbors := make([]tuple.Tuple, len(selected))
		for i, nb := range selected {
			newNodeNeighbors[i] = nb.pk
		}

		// Save new node at this layer.
		g.storage.saveNodeLayer(tx, layer, primaryKey, vecBytes, newNodeNeighbors)

		// Add reverse connections to each neighbor.
		for _, nb := range selected {
			nbVecBytes, nbNeighbors, loadErr := g.storage.loadNodeLayer(tx, layer, nb.pk)
			if loadErr != nil {
				continue
			}

			// Add the new node as a neighbor.
			nbNeighbors = append(nbNeighbors, primaryKey)

			// Prune if over limit.
			if len(nbNeighbors) > maxConn {
				nbVec, _ := deserializeVector(nbVecBytes)
				nbNeighbors, err = g.pruneNeighbors(tx, nbVec, nbNeighbors, maxConn, layer)
				if err != nil {
					return err
				}
			}

			g.storage.saveNodeLayer(tx, layer, nb.pk, nbVecBytes, nbNeighbors)
		}

		if len(neighbors) > 0 {
			// Use nearest neighbor as entry point for next layer down.
			currentPK = neighbors[0].pk
			currentVec = neighbors[0].vec
		}
	}

	// If new node has layers above the entry point, save those layers too.
	if insertLayer > epLayer {
		for layer := epLayer + 1; layer <= insertLayer; layer++ {
			g.storage.saveNodeLayer(tx, layer, primaryKey, vecBytes, nil)
		}
		g.storage.saveAccessInfo(tx, insertLayer, primaryKey, vecBytes)
	}

	return nil
}

// Delete removes a node from the HNSW graph and repairs neighbor connections.
// Wire-compatible with Java's HNSW delete (graph repair, entry point update).
func (g *hnswGraph) Delete(tx fdb.Transaction, primaryKey tuple.Tuple, vector []float64) error {
	topLvl := topLayer(primaryKey, g.config.M)

	// Check existence at layer 0.
	_, _, err := g.storage.loadNodeLayer(tx, 0, primaryKey)
	if err != nil {
		return nil // already deleted or doesn't exist
	}

	// Delete from all layers and repair.
	for layer := 0; layer <= topLvl; layer++ {
		_, neighbors, loadErr := g.storage.loadNodeLayer(tx, layer, primaryKey)
		if loadErr != nil {
			continue // not present at this layer
		}

		g.storage.deleteNodeLayer(tx, layer, primaryKey)

		// Repair: for each neighbor, reconnect.
		for _, nbPK := range neighbors {
			if repairErr := g.repairNeighbor(tx, layer, nbPK, primaryKey); repairErr != nil {
				return repairErr
			}
		}
	}

	// Update entry point if needed.
	epLayer, epPK, _, epErr := g.storage.loadAccessInfo(tx)
	if epErr != nil {
		return nil // no entry point
	}
	if tupleEqual(epPK, primaryKey) {
		// Find a replacement entry point by scanning for any remaining node,
		// starting from the highest layer and working down.
		var newEntryPK tuple.Tuple
		var newEntryVecBytes []byte
		newEntryLayer := 0

		for layer := epLayer; layer >= 0 && newEntryPK == nil; layer-- {
			// Find any node at this layer.
			foundPK, foundVec, scanErr := g.storage.findAnyNodeAtLayer(tx, layer)
			if scanErr == nil && foundPK != nil {
				newEntryPK = foundPK
				newEntryVecBytes = foundVec
				newEntryLayer = layer
			}
		}

		if newEntryPK != nil {
			g.storage.saveAccessInfo(tx, newEntryLayer, newEntryPK, newEntryVecBytes)
		} else {
			g.storage.clearAccessInfo(tx)
		}
	}

	return nil
}

// Search finds the k nearest neighbors to the query vector.
// Returns results sorted by distance (closest first).
func (g *hnswGraph) Search(tx fdb.ReadTransaction, query []float64, k, efSearch int) ([]hnswSearchResult, error) {
	epLayer, epPK, epVecBytes, err := g.storage.loadAccessInfo(tx)
	if err != nil {
		return nil, nil // empty graph
	}

	epVec, _ := deserializeVector(epVecBytes)

	// Greedy descent from top layer to layer 1.
	currentPK := epPK
	currentVec := epVec
	for layer := epLayer; layer > 0; layer-- {
		currentPK, currentVec, err = g.searchLayerGreedy(tx, query, currentPK, currentVec, layer)
		if err != nil {
			return nil, err
		}
	}

	// Search at layer 0 with efSearch.
	candidates, err := g.searchLayerMulti(tx, query, currentPK, currentVec, max(efSearch, k), 0)
	if err != nil {
		return nil, err
	}

	// Return top-k.
	if len(candidates) > k {
		candidates = candidates[:k]
	}

	results := make([]hnswSearchResult, len(candidates))
	for i, c := range candidates {
		results[i] = hnswSearchResult{
			PrimaryKey: c.pk,
			Distance:   c.dist,
		}
	}
	return results, nil
}

// searchLayerGreedy finds the single nearest neighbor at a given layer (greedy descent).
func (g *hnswGraph) searchLayerGreedy(tx fdb.ReadTransaction, query []float64, epPK tuple.Tuple, epVec []float64, layer int) (tuple.Tuple, []float64, error) {
	bestPK := epPK
	bestVec := epVec
	bestDist := vectorDistance(query, epVec, g.config.Metric)
	changed := true

	for changed {
		changed = false
		_, neighbors, err := g.storage.loadNodeLayer(tx, layer, bestPK)
		if err != nil {
			break // no data at this layer
		}
		for _, nbPK := range neighbors {
			nbVecBytes, _, loadErr := g.storage.loadNodeLayer(tx, layer, nbPK)
			if loadErr != nil {
				continue
			}
			nbVec, _ := deserializeVector(nbVecBytes)
			dist := vectorDistance(query, nbVec, g.config.Metric)
			if dist < bestDist {
				bestDist = dist
				bestPK = nbPK
				bestVec = nbVec
				changed = true
			}
		}
	}

	return bestPK, bestVec, nil
}

// hnswCandidate represents a search candidate with its primary key, vector, and distance.
type hnswCandidate struct {
	pk   tuple.Tuple
	vec  []float64
	dist float64
}

// searchLayerMulti finds the ef nearest neighbors at a given layer.
func (g *hnswGraph) searchLayerMulti(tx fdb.ReadTransaction, query []float64, epPK tuple.Tuple, epVec []float64, ef, layer int) ([]hnswCandidate, error) {
	if epPK == nil {
		return nil, nil
	}

	epDist := vectorDistance(query, epVec, g.config.Metric)

	// Candidates (min-heap by distance) and visited set.
	candidates := &distHeap{}
	heap.Push(candidates, distItem{pk: epPK, dist: epDist})

	visited := map[string]bool{string(epPK.Pack()): true}

	var results []hnswCandidate
	results = append(results, hnswCandidate{pk: epPK, vec: epVec, dist: epDist})

	for candidates.Len() > 0 {
		closest := heap.Pop(candidates).(distItem)

		// Check if we've explored enough.
		if len(results) >= ef {
			farthestResult := results[len(results)-1].dist
			if closest.dist > farthestResult {
				break
			}
		}

		// Load the node's neighbors at this layer.
		_, neighbors, err := g.storage.loadNodeLayer(tx, layer, closest.pk)
		if err != nil {
			continue
		}

		// Explore neighbors.
		for _, nbPK := range neighbors {
			key := string(nbPK.Pack())
			if visited[key] {
				continue
			}
			visited[key] = true

			nbVecBytes, _, loadErr := g.storage.loadNodeLayer(tx, layer, nbPK)
			if loadErr != nil {
				continue
			}
			nbVec, _ := deserializeVector(nbVecBytes)
			dist := vectorDistance(query, nbVec, g.config.Metric)

			if len(results) < ef || dist < results[len(results)-1].dist {
				heap.Push(candidates, distItem{pk: nbPK, dist: dist})
				results = append(results, hnswCandidate{pk: nbPK, vec: nbVec, dist: dist})
				sort.Slice(results, func(i, j int) bool {
					return results[i].dist < results[j].dist
				})
				if len(results) > ef {
					results = results[:ef]
				}
			}
		}
	}

	return results, nil
}

// selectNeighbors returns the best maxConn neighbors by distance.
func (g *hnswGraph) selectNeighbors(neighbors []hnswCandidate, maxConn int) []hnswCandidate {
	if len(neighbors) <= maxConn {
		return neighbors
	}
	sort.Slice(neighbors, func(i, j int) bool {
		return neighbors[i].dist < neighbors[j].dist
	})
	return neighbors[:maxConn]
}

// pruneNeighbors re-selects the best maxConn neighbors for a node by computing distances.
func (g *hnswGraph) pruneNeighbors(tx fdb.ReadTransaction, nodeVec []float64, neighborPKs []tuple.Tuple, maxConn, layer int) ([]tuple.Tuple, error) {
	type entry struct {
		pk   tuple.Tuple
		dist float64
	}
	var entries []entry
	for _, pk := range neighborPKs {
		vecBytes, _, err := g.storage.loadNodeLayer(tx, layer, pk)
		if err != nil {
			continue
		}
		vec, _ := deserializeVector(vecBytes)
		dist := vectorDistance(nodeVec, vec, g.config.Metric)
		entries = append(entries, entry{pk: pk, dist: dist})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].dist < entries[j].dist
	})
	if len(entries) > maxConn {
		entries = entries[:maxConn]
	}
	result := make([]tuple.Tuple, len(entries))
	for i, e := range entries {
		result[i] = e.pk
	}
	return result, nil
}

// repairNeighbor removes a deleted node from a neighbor's list and finds replacement connections.
func (g *hnswGraph) repairNeighbor(tx fdb.Transaction, layer int, neighborPK, deletedPK tuple.Tuple) error {
	nbVecBytes, nbNeighbors, err := g.storage.loadNodeLayer(tx, layer, neighborPK)
	if err != nil {
		return nil // neighbor doesn't exist
	}

	// Remove deleted node from neighbor's list.
	filtered := make([]tuple.Tuple, 0, len(nbNeighbors))
	for _, pk := range nbNeighbors {
		if !tupleEqual(pk, deletedPK) {
			filtered = append(filtered, pk)
		}
	}

	// Find candidates from neighbors-of-neighbors.
	candidateMap := make(map[string]tuple.Tuple)
	for _, pk := range filtered {
		_, nbn, loadErr := g.storage.loadNodeLayer(tx, layer, pk)
		if loadErr != nil {
			continue
		}
		for _, candidate := range nbn {
			candidateKey := string(candidate.Pack())
			if !tupleEqual(candidate, neighborPK) && !tupleEqual(candidate, deletedPK) {
				candidateMap[candidateKey] = candidate
			}
		}
	}

	// Build candidate list with distances.
	nbVec, _ := deserializeVector(nbVecBytes)

	type candidateEntry struct {
		pk   tuple.Tuple
		dist float64
	}
	// Start with existing (filtered) neighbors.
	seen := make(map[string]bool)
	var allCandidates []candidateEntry
	for _, pk := range filtered {
		vecBytes, _, loadErr := g.storage.loadNodeLayer(tx, layer, pk)
		if loadErr != nil {
			continue
		}
		vec, _ := deserializeVector(vecBytes)
		dist := vectorDistance(nbVec, vec, g.config.Metric)
		allCandidates = append(allCandidates, candidateEntry{pk: pk, dist: dist})
		seen[string(pk.Pack())] = true
	}
	// Add new candidates from neighbors-of-neighbors.
	for key, pk := range candidateMap {
		if seen[key] {
			continue
		}
		vecBytes, _, loadErr := g.storage.loadNodeLayer(tx, layer, pk)
		if loadErr != nil {
			continue
		}
		vec, _ := deserializeVector(vecBytes)
		dist := vectorDistance(nbVec, vec, g.config.Metric)
		allCandidates = append(allCandidates, candidateEntry{pk: pk, dist: dist})
	}

	// Select best connections.
	maxConn := g.config.MMax
	if layer == 0 {
		maxConn = g.config.MMax0
	}

	sort.Slice(allCandidates, func(i, j int) bool {
		return allCandidates[i].dist < allCandidates[j].dist
	})
	if len(allCandidates) > maxConn {
		allCandidates = allCandidates[:maxConn]
	}

	newNeighbors := make([]tuple.Tuple, len(allCandidates))
	for i, c := range allCandidates {
		newNeighbors[i] = c.pk
	}

	g.storage.saveNodeLayer(tx, layer, neighborPK, nbVecBytes, newNeighbors)
	return nil
}

// hnswSearchResult is a search result.
type hnswSearchResult struct {
	PrimaryKey tuple.Tuple
	Distance   float64
}

// hnswStorage handles FDB storage of HNSW graph nodes.
// Wire-compatible with Java's COMPACT node format.
//
// Subspace layout:
//   Sub(0) — data: (layer, primaryKey) -> (nodeKind, vectorTuple, neighborsTuple)
//   Sub(1) — access info: () -> (entryPointLayer, entryPointPK, entryPointVectorTuple)
type hnswStorage struct {
	dataSubspace   subspace.Subspace
	accessSubspace subspace.Subspace
	config         HNSWConfig
}

func newHNSWStorage(ss subspace.Subspace, config HNSWConfig) *hnswStorage {
	return &hnswStorage{
		dataSubspace:   ss.Sub(int64(0)),
		accessSubspace: ss.Sub(int64(1)),
		config:         config,
	}
}

// saveNodeLayer writes one layer's data for a node in COMPACT format.
// Key: dataSubspace.Pack(layer, primaryKey...)
// Value: Tuple.Pack(nodeKind, vectorTuple, neighborsTuple)
func (s *hnswStorage) saveNodeLayer(tx fdb.Transaction, layer int, primaryKey tuple.Tuple, vectorBytes []byte, neighbors []tuple.Tuple) {
	keyTuple := tuple.Tuple{int64(layer)}
	keyTuple = append(keyTuple, primaryKey...)
	key := s.dataSubspace.Pack(keyTuple)

	neighborList := make(tuple.Tuple, len(neighbors))
	for i, pk := range neighbors {
		neighborList[i] = pk
	}

	value := tuple.Tuple{
		int64(0),                  // COMPACT node kind
		tuple.Tuple{vectorBytes},  // vector bytes wrapped in tuple
		neighborList,              // neighbor PKs as nested tuples
	}
	tx.Set(fdb.Key(key), value.Pack())
}

// loadNodeLayer reads one layer's data for a node.
// Returns vector bytes, neighbor PKs, and error (non-nil if not found).
func (s *hnswStorage) loadNodeLayer(tx fdb.ReadTransaction, layer int, primaryKey tuple.Tuple) (vectorBytes []byte, neighbors []tuple.Tuple, err error) {
	keyTuple := tuple.Tuple{int64(layer)}
	keyTuple = append(keyTuple, primaryKey...)
	key := s.dataSubspace.Pack(keyTuple)

	data, err := tx.Get(fdb.Key(key)).Get()
	if err != nil {
		return nil, nil, fmt.Errorf("hnsw: get node layer %d: %w", layer, err)
	}
	if data == nil {
		return nil, nil, fmt.Errorf("hnsw: node not found at layer %d", layer)
	}

	t, err := tuple.Unpack(data)
	if err != nil {
		return nil, nil, fmt.Errorf("hnsw: unpack node layer: %w", err)
	}
	if len(t) < 3 {
		return nil, nil, fmt.Errorf("hnsw: node tuple too short: %d elements", len(t))
	}

	// t[0] = nodeKind (int64, 0 = COMPACT)
	// t[1] = vectorTuple (tuple containing vector bytes)
	// t[2] = neighborsTuple (tuple of neighbor PK tuples)

	switch vt := t[1].(type) {
	case tuple.Tuple:
		if len(vt) > 0 {
			if vb, ok := vt[0].([]byte); ok {
				vectorBytes = vb
			}
		}
	}

	switch nt := t[2].(type) {
	case tuple.Tuple:
		neighbors = make([]tuple.Tuple, 0, len(nt))
		for _, elem := range nt {
			if pkTuple, ok := elem.(tuple.Tuple); ok {
				neighbors = append(neighbors, pkTuple)
			}
		}
	}

	return vectorBytes, neighbors, nil
}

// deleteNodeLayer removes one layer's data for a node.
func (s *hnswStorage) deleteNodeLayer(tx fdb.Transaction, layer int, primaryKey tuple.Tuple) {
	keyTuple := tuple.Tuple{int64(layer)}
	keyTuple = append(keyTuple, primaryKey...)
	key := s.dataSubspace.Pack(keyTuple)
	tx.Clear(fdb.Key(key))
}

// loadAccessInfo reads the entry point metadata.
// Returns layer, primary key, vector bytes, and error (non-nil if no entry point).
func (s *hnswStorage) loadAccessInfo(tx fdb.ReadTransaction) (layer int, pk tuple.Tuple, vectorBytes []byte, err error) {
	key := s.accessSubspace.Pack(tuple.Tuple{})
	data, getErr := tx.Get(fdb.Key(key)).Get()
	if getErr != nil {
		return 0, nil, nil, fmt.Errorf("hnsw: get access info: %w", getErr)
	}
	if data == nil {
		return 0, nil, nil, fmt.Errorf("hnsw: no entry point")
	}

	t, unpackErr := tuple.Unpack(data)
	if unpackErr != nil {
		return 0, nil, nil, fmt.Errorf("hnsw: unpack access info: %w", unpackErr)
	}
	if len(t) < 3 {
		return 0, nil, nil, fmt.Errorf("hnsw: access info too short: %d elements", len(t))
	}

	// t[0] = entryPointLayer
	// t[1] = entryPointPK (tuple)
	// t[2] = entryPointVectorTuple (tuple containing vector bytes)

	if l, ok := asInt64(t[0]); ok {
		layer = int(l)
	}

	switch pkVal := t[1].(type) {
	case tuple.Tuple:
		pk = pkVal
	}

	switch vt := t[2].(type) {
	case tuple.Tuple:
		if len(vt) > 0 {
			if vb, ok := vt[0].([]byte); ok {
				vectorBytes = vb
			}
		}
	}

	if pk == nil {
		return 0, nil, nil, fmt.Errorf("hnsw: access info has nil PK")
	}

	return layer, pk, vectorBytes, nil
}

// saveAccessInfo writes the entry point metadata.
func (s *hnswStorage) saveAccessInfo(tx fdb.Transaction, layer int, pk tuple.Tuple, vectorBytes []byte) {
	key := s.accessSubspace.Pack(tuple.Tuple{})
	value := tuple.Tuple{int64(layer), pk, tuple.Tuple{vectorBytes}}
	tx.Set(fdb.Key(key), value.Pack())
}

// clearAccessInfo removes the entry point metadata.
func (s *hnswStorage) clearAccessInfo(tx fdb.Transaction) {
	key := s.accessSubspace.Pack(tuple.Tuple{})
	tx.Clear(fdb.Key(key))
}

// findAnyNodeAtLayer finds any node present at the given layer.
// Returns the first node found by scanning the layer prefix.
func (s *hnswStorage) findAnyNodeAtLayer(tx fdb.ReadTransaction, layer int) (pk tuple.Tuple, vectorBytes []byte, err error) {
	prefix := s.dataSubspace.Pack(tuple.Tuple{int64(layer)})
	r, rangeErr := fdb.PrefixRange(prefix)
	if rangeErr != nil {
		return nil, nil, rangeErr
	}

	ri := tx.GetRange(r, fdb.RangeOptions{Limit: 1}).Iterator()
	if ri.Advance() {
		kv, getErr := ri.Get()
		if getErr != nil {
			return nil, nil, getErr
		}

		// Extract primary key from the FDB key.
		// Key format: dataSubspace + (layer, pk...)
		keyTuple, unpackErr := s.dataSubspace.Unpack(kv.Key)
		if unpackErr != nil {
			return nil, nil, unpackErr
		}
		// keyTuple[0] = layer, keyTuple[1:] = primary key elements
		if len(keyTuple) > 1 {
			pk = make(tuple.Tuple, len(keyTuple)-1)
			copy(pk, keyTuple[1:])
		}

		// Parse the value for vector bytes.
		t, valErr := tuple.Unpack(kv.Value)
		if valErr != nil {
			return nil, nil, valErr
		}
		if len(t) >= 2 {
			if vt, ok := t[1].(tuple.Tuple); ok && len(vt) > 0 {
				if vb, ok := vt[0].([]byte); ok {
					vectorBytes = vb
				}
			}
		}

		return pk, vectorBytes, nil
	}

	return nil, nil, fmt.Errorf("hnsw: no nodes at layer %d", layer)
}

// clearAll removes all HNSW graph data (data + access info).
func (s *hnswStorage) clearAll(tx fdb.Transaction) {
	for _, ss := range []subspace.Subspace{s.dataSubspace, s.accessSubspace} {
		r, err := fdb.PrefixRange(ss.Bytes())
		if err != nil {
			continue
		}
		tx.ClearRange(r)
	}
}

// --- Vector serialization ---
// Wire-compatible with Java's VectorType.DOUBLE.

// serializeVector serializes a float64 vector to bytes.
// Format: byte 0 = type ordinal (0 for DOUBLE), bytes 1+ = big-endian IEEE 754 float64 values.
func serializeVector(vec []float64) []byte {
	buf := make([]byte, 1+8*len(vec))
	buf[0] = 0 // DOUBLE type ordinal
	for i, v := range vec {
		binary.BigEndian.PutUint64(buf[1+i*8:], math.Float64bits(v))
	}
	return buf
}

// deserializeVector deserializes a float64 vector from bytes.
func deserializeVector(data []byte) ([]float64, error) {
	if len(data) < 1 {
		return nil, fmt.Errorf("hnsw: empty vector data")
	}
	// data[0] = type byte, rest is float64 values
	numFloats := (len(data) - 1) / 8
	vec := make([]float64, numFloats)
	for i := 0; i < numFloats; i++ {
		vec[i] = math.Float64frombits(binary.BigEndian.Uint64(data[1+i*8:]))
	}
	return vec, nil
}

// --- Layer assignment ---
// Wire-compatible with Java's deterministic layer assignment using SplitMix64 hash of PK.

// topLayer computes the deterministic layer assignment for a given primary key.
// Uses the HNSW level generation formula with SplitMix64 hash of the PK's Java-compatible hashCode.
func topLayer(primaryKey tuple.Tuple, m int) int {
	packed := primaryKey.Pack()
	h := javaHashCode(packed)
	u := 1.0 - splitMixDouble(int64(h))
	lambda := 1.0 / math.Log(float64(m))
	level := int(math.Floor(-math.Log(u) * lambda))
	if level < 0 {
		level = 0
	}
	return level
}

// splitMixLong applies the SplitMix64 hash function.
// Matches Java's SplittableRandom.mix64.
func splitMixLong(x int64) int64 {
	x += -0x61C8864680B583EB // 0x9e3779b97f4a7c15 as signed int64
	x = (x ^ int64(uint64(x)>>30)) * -0x40A7B892E31B1A47 // 0xbf58476d1ce4e5b9
	x = (x ^ int64(uint64(x)>>27)) * -0x6B2FB644ECCEEE15 // 0x94d049bb133111eb
	x = x ^ int64(uint64(x)>>31)
	return x
}

// splitMixDouble converts a SplitMix64 hash to a double in [0, 1).
func splitMixDouble(x int64) float64 {
	return float64(uint64(splitMixLong(x))>>11) * (1.0 / float64(int64(1)<<53))
}

// javaHashCode computes a Java-compatible hash code for byte array.
// Matches: int hash = 1; for (byte b : data) hash = 31 * hash + b;
// where b is treated as signed (int8).
func javaHashCode(data []byte) int32 {
	var hash int32 = 1
	for _, b := range data {
		hash = 31*hash + int32(int8(b))
	}
	return hash
}

// --- Heap for search candidates ---

// distHeap is a min-heap for search candidates.
type distHeap []distItem

type distItem struct {
	pk   tuple.Tuple
	dist float64
}

func (h distHeap) Len() int            { return len(h) }
func (h distHeap) Less(i, j int) bool  { return h[i].dist < h[j].dist }
func (h distHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *distHeap) Push(x any)         { *h = append(*h, x.(distItem)) }
func (h *distHeap) Pop() any           { old := *h; n := len(old); x := old[n-1]; *h = old[:n-1]; return x }
