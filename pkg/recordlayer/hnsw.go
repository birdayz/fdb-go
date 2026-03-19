package recordlayer

import (
	"container/heap"
	"fmt"
	"math"
	"math/rand"
	"sort"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
)

// HNSWConfig configures an HNSW graph.
// Matches Java's com.apple.foundationdb.async.hnsw.Config (subset).
type HNSWConfig struct {
	NumDimensions   int
	M               int          // Connectivity factor (default 16)
	MMax            int          // Max connections for non-zero layers (default M)
	MMax0           int          // Max connections for layer 0 (default 2*M)
	EfConstruction  int          // Insertion search factor (default 200)
	Metric          VectorMetric // Distance metric
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
// Matches Java's com.apple.foundationdb.async.hnsw.HNSW (simplified).
type hnswGraph struct {
	storage *hnswStorage
	config  HNSWConfig
	rng     *rand.Rand
}

// NewHNSWGraph creates a new HNSW graph.
func NewHNSWGraph(storage *hnswStorage, config HNSWConfig) *hnswGraph {
	return &hnswGraph{
		storage: storage,
		config:  config,
		rng:     rand.New(rand.NewSource(42)),
	}
}

// Insert adds a vector to the HNSW graph.
// nodeID identifies the record (e.g., primary key bytes).
// vector is the float64 vector to index.
func (g *hnswGraph) Insert(tx fdb.Transaction, nodeID []byte, vector []float64) error {
	// Determine insertion layer (probabilistic).
	insertLayer := g.randomLevel()

	// Load graph metadata.
	meta, err := g.storage.loadMeta(tx)
	if err != nil {
		return err
	}

	// Save the node data.
	node := &hnswNode{
		ID:         nodeID,
		Vector:     vector,
		MaxLayer:   insertLayer,
		Neighbors:  make([][]hnswNeighbor, insertLayer+1),
	}
	for i := range node.Neighbors {
		node.Neighbors[i] = nil
	}

	if meta.EntryNodeID == nil {
		// First node — just save and set as entry.
		if err := g.storage.saveNode(tx, node); err != nil {
			return err
		}
		meta.EntryNodeID = nodeID
		meta.MaxLayer = insertLayer
		return g.storage.saveMeta(tx, meta)
	}

	// Load entry node.
	entryNode, err := g.storage.loadNode(tx, meta.EntryNodeID)
	if err != nil {
		return fmt.Errorf("hnsw insert: load entry: %w", err)
	}
	if entryNode == nil {
		// Corrupt state — entry node missing. Reset.
		if err := g.storage.saveNode(tx, node); err != nil {
			return err
		}
		meta.EntryNodeID = nodeID
		meta.MaxLayer = insertLayer
		return g.storage.saveMeta(tx, meta)
	}

	// Greedy search from top to insertion layer.
	ep := entryNode
	for layer := meta.MaxLayer; layer > insertLayer; layer-- {
		ep, err = g.searchLayer(tx, vector, ep, 1, layer)
		if err != nil {
			return err
		}
	}

	// Insert at each layer from insertLayer down to 0.
	for layer := min(insertLayer, meta.MaxLayer); layer >= 0; layer-- {
		// Find ef_construction nearest neighbors at this layer.
		neighbors, err := g.searchLayerMulti(tx, vector, ep, g.config.EfConstruction, layer)
		if err != nil {
			return err
		}

		// Select M best neighbors.
		maxConn := g.config.MMax
		if layer == 0 {
			maxConn = g.config.MMax0
		}
		selectedNeighbors := g.selectNeighbors(neighbors, maxConn)

		// Set bidirectional connections.
		node.Neighbors[layer] = selectedNeighbors
		for _, nb := range selectedNeighbors {
			nbNode, err := g.storage.loadNode(tx, nb.NodeID)
			if err != nil {
				return err
			}
			if nbNode == nil {
				continue
			}
			// Add reverse connection.
			g.addConnection(nbNode, layer, nodeID, nb.Distance)
			// Prune if over limit.
			if len(nbNode.Neighbors[layer]) > maxConn {
				nbNode.Neighbors[layer] = g.selectNeighbors(nbNode.Neighbors[layer], maxConn)
			}
			if err := g.storage.saveNode(tx, nbNode); err != nil {
				return err
			}
		}

		if len(neighbors) > 0 {
			// Set ep to nearest neighbor for next layer.
			ep, err = g.storage.loadNode(tx, neighbors[0].NodeID)
			if err != nil {
				return err
			}
		}
	}

	if err := g.storage.saveNode(tx, node); err != nil {
		return err
	}

	// Update entry point if new node has higher layer.
	if insertLayer > meta.MaxLayer {
		meta.EntryNodeID = nodeID
		meta.MaxLayer = insertLayer
		return g.storage.saveMeta(tx, meta)
	}

	return nil
}

// Delete removes a vector from the HNSW graph.
func (g *hnswGraph) Delete(tx fdb.Transaction, nodeID []byte) error {
	node, err := g.storage.loadNode(tx, nodeID)
	if err != nil {
		return err
	}
	if node == nil {
		return nil // already deleted
	}

	// Remove all connections from neighbors pointing to this node.
	for layer := 0; layer <= node.MaxLayer; layer++ {
		if layer >= len(node.Neighbors) {
			continue
		}
		for _, nb := range node.Neighbors[layer] {
			nbNode, err := g.storage.loadNode(tx, nb.NodeID)
			if err != nil {
				return err
			}
			if nbNode == nil {
				continue
			}
			g.removeConnection(nbNode, layer, nodeID)
			if err := g.storage.saveNode(tx, nbNode); err != nil {
				return err
			}
		}
	}

	g.storage.deleteNode(tx, nodeID)

	// If this was the entry node, find a new one.
	meta, err := g.storage.loadMeta(tx)
	if err != nil {
		return err
	}
	if meta.EntryNodeID != nil && hnswBytesEqual(meta.EntryNodeID, nodeID) {
		// Pick a neighbor from the highest layer as new entry.
		var newEntry []byte
		for layer := node.MaxLayer; layer >= 0 && newEntry == nil; layer-- {
			if layer < len(node.Neighbors) {
				for _, nb := range node.Neighbors[layer] {
					newEntry = nb.NodeID
					break
				}
			}
		}
		meta.EntryNodeID = newEntry
		if newEntry == nil {
			meta.MaxLayer = 0
		}
		return g.storage.saveMeta(tx, meta)
	}

	return nil
}

// Search finds the k nearest neighbors to the query vector.
// Returns results sorted by distance (closest first).
func (g *hnswGraph) Search(tx fdb.ReadTransaction, query []float64, k, efSearch int) ([]hnswSearchResult, error) {
	meta, err := g.storage.loadMeta(tx)
	if err != nil {
		return nil, err
	}
	if meta.EntryNodeID == nil {
		return nil, nil
	}

	entryNode, err := g.storage.loadNode(tx, meta.EntryNodeID)
	if err != nil {
		return nil, err
	}
	if entryNode == nil {
		return nil, nil
	}

	// Greedy descent from top layer to layer 1.
	ep := entryNode
	for layer := meta.MaxLayer; layer > 0; layer-- {
		ep, err = g.searchLayer(tx, query, ep, 1, layer)
		if err != nil {
			return nil, err
		}
	}

	// Search at layer 0 with efSearch.
	candidates, err := g.searchLayerMulti(tx, query, ep, max(efSearch, k), 0)
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
			NodeID:   c.NodeID,
			Distance: c.Distance,
		}
	}
	return results, nil
}

// searchLayer finds the single nearest neighbor at a given layer (greedy).
func (g *hnswGraph) searchLayer(tx fdb.ReadTransaction, query []float64, ep *hnswNode, ef, layer int) (*hnswNode, error) {
	if ep == nil {
		return nil, nil
	}

	best := ep
	bestDist := vectorDistance(query, ep.Vector, g.config.Metric)
	changed := true

	for changed {
		changed = false
		if layer >= len(best.Neighbors) {
			break
		}
		for _, nb := range best.Neighbors[layer] {
			nbNode, err := g.storage.loadNode(tx, nb.NodeID)
			if err != nil {
				return nil, err
			}
			if nbNode == nil {
				continue
			}
			dist := vectorDistance(query, nbNode.Vector, g.config.Metric)
			if dist < bestDist {
				bestDist = dist
				best = nbNode
				changed = true
			}
		}
	}
	return best, nil
}

// searchLayerMulti finds the ef nearest neighbors at a given layer.
func (g *hnswGraph) searchLayerMulti(tx fdb.ReadTransaction, query []float64, ep *hnswNode, ef, layer int) ([]hnswNeighbor, error) {
	if ep == nil {
		return nil, nil
	}

	epDist := vectorDistance(query, ep.Vector, g.config.Metric)

	// Candidates (min-heap by distance) and visited set.
	candidates := &distHeap{}
	heap.Push(candidates, distItem{nodeID: ep.ID, dist: epDist})

	visited := map[string]bool{string(ep.ID): true}

	var results []hnswNeighbor
	results = append(results, hnswNeighbor{NodeID: ep.ID, Distance: epDist})

	for candidates.Len() > 0 {
		closest := heap.Pop(candidates).(distItem)

		// Check if we've explored enough.
		if len(results) >= ef {
			farthestResult := results[len(results)-1].Distance
			if closest.dist > farthestResult {
				break
			}
		}

		// Load the node.
		node, err := g.storage.loadNode(tx, closest.nodeID)
		if err != nil {
			return nil, err
		}
		if node == nil || layer >= len(node.Neighbors) {
			continue
		}

		// Explore neighbors.
		for _, nb := range node.Neighbors[layer] {
			key := string(nb.NodeID)
			if visited[key] {
				continue
			}
			visited[key] = true

			nbNode, err := g.storage.loadNode(tx, nb.NodeID)
			if err != nil {
				return nil, err
			}
			if nbNode == nil {
				continue
			}

			dist := vectorDistance(query, nbNode.Vector, g.config.Metric)

			if len(results) < ef || dist < results[len(results)-1].Distance {
				heap.Push(candidates, distItem{nodeID: nb.NodeID, dist: dist})
				results = append(results, hnswNeighbor{NodeID: nb.NodeID, Distance: dist})
				sort.Slice(results, func(i, j int) bool {
					return results[i].Distance < results[j].Distance
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
func (g *hnswGraph) selectNeighbors(neighbors []hnswNeighbor, maxConn int) []hnswNeighbor {
	if len(neighbors) <= maxConn {
		return neighbors
	}
	sort.Slice(neighbors, func(i, j int) bool {
		return neighbors[i].Distance < neighbors[j].Distance
	})
	return neighbors[:maxConn]
}

// addConnection adds a neighbor connection to a node at a given layer.
func (g *hnswGraph) addConnection(node *hnswNode, layer int, neighborID []byte, dist float64) {
	for layer >= len(node.Neighbors) {
		node.Neighbors = append(node.Neighbors, nil)
	}
	node.Neighbors[layer] = append(node.Neighbors[layer], hnswNeighbor{NodeID: neighborID, Distance: dist})
}

// removeConnection removes a neighbor from a node at a given layer.
func (g *hnswGraph) removeConnection(node *hnswNode, layer int, neighborID []byte) {
	if layer >= len(node.Neighbors) {
		return
	}
	neighbors := node.Neighbors[layer]
	for i, nb := range neighbors {
		if hnswBytesEqual(nb.NodeID, neighborID) {
			node.Neighbors[layer] = append(neighbors[:i], neighbors[i+1:]...)
			return
		}
	}
}

// randomLevel assigns a random layer using the HNSW level generation formula.
// Returns level >= 0 where probability of level L = (1/M)^L.
func (g *hnswGraph) randomLevel() int {
	ml := 1.0 / math.Log(float64(g.config.M))
	level := int(-math.Log(g.rng.Float64()) * ml)
	return level
}

// hnswBytesEqual compares two byte slices.
func hnswBytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// hnswNode is a node in the HNSW graph.
type hnswNode struct {
	ID        []byte
	Vector    []float64
	MaxLayer  int
	Neighbors [][]hnswNeighbor // per layer
}

// hnswNeighbor is a reference to a neighbor node.
type hnswNeighbor struct {
	NodeID   []byte
	Distance float64
}

// hnswSearchResult is a search result.
type hnswSearchResult struct {
	NodeID   []byte
	Distance float64
}

// hnswMeta is the graph-level metadata.
type hnswMeta struct {
	EntryNodeID []byte
	MaxLayer    int
}

// hnswStorage handles FDB storage of HNSW graph nodes.
type hnswStorage struct {
	subspace subspace.Subspace
}

func newHNSWStorage(ss subspace.Subspace) *hnswStorage {
	return &hnswStorage{subspace: ss}
}

var metaKey = []byte("__meta__")

func (s *hnswStorage) loadMeta(tx fdb.ReadTransaction) (*hnswMeta, error) {
	data, err := tx.Get(fdb.Key(s.subspace.Pack(tuple.Tuple{metaKey}))).Get()
	if err != nil {
		return nil, err
	}
	if data == nil {
		return &hnswMeta{}, nil
	}
	t, err := tuple.Unpack(data)
	if err != nil {
		return nil, err
	}
	meta := &hnswMeta{}
	if len(t) > 0 {
		if id, ok := t[0].([]byte); ok {
			meta.EntryNodeID = id
		}
	}
	if len(t) > 1 {
		if layer, ok := asInt64(t[1]); ok {
			meta.MaxLayer = int(layer)
		}
	}
	return meta, nil
}

func (s *hnswStorage) saveMeta(tx fdb.Transaction, meta *hnswMeta) error {
	t := tuple.Tuple{meta.EntryNodeID, int64(meta.MaxLayer)}
	tx.Set(fdb.Key(s.subspace.Pack(tuple.Tuple{metaKey})), t.Pack())
	return nil
}

func (s *hnswStorage) loadNode(tx fdb.ReadTransaction, nodeID []byte) (*hnswNode, error) {
	data, err := tx.Get(fdb.Key(s.subspace.Pack(tuple.Tuple{nodeID}))).Get()
	if err != nil {
		return nil, err
	}
	if data == nil {
		return nil, nil
	}
	return deserializeHNSWNode(nodeID, data)
}

func (s *hnswStorage) saveNode(tx fdb.Transaction, node *hnswNode) error {
	data := serializeHNSWNode(node)
	tx.Set(fdb.Key(s.subspace.Pack(tuple.Tuple{node.ID})), data)
	return nil
}

func (s *hnswStorage) deleteNode(tx fdb.Transaction, nodeID []byte) {
	tx.Clear(fdb.Key(s.subspace.Pack(tuple.Tuple{nodeID})))
}

func (s *hnswStorage) clearAll(tx fdb.Transaction) {
	r, err := fdb.PrefixRange(s.subspace.Bytes())
	if err != nil {
		return
	}
	tx.ClearRange(r)
}

// serializeHNSWNode serializes a node to bytes.
// Format: tuple(maxLayer, vectorLen, vec[0], vec[1], ..., numLayers, [layerLen, neighborID, dist, ...], ...)
func serializeHNSWNode(node *hnswNode) []byte {
	t := make(tuple.Tuple, 0, 3+len(node.Vector)+4)
	t = append(t, int64(node.MaxLayer))
	t = append(t, int64(len(node.Vector)))
	for _, v := range node.Vector {
		t = append(t, math.Float64bits(v))
	}
	t = append(t, int64(len(node.Neighbors)))
	for _, layer := range node.Neighbors {
		t = append(t, int64(len(layer)))
		for _, nb := range layer {
			t = append(t, nb.NodeID)
			t = append(t, math.Float64bits(nb.Distance))
		}
	}
	return t.Pack()
}

// deserializeHNSWNode deserializes a node from bytes.
func deserializeHNSWNode(nodeID []byte, data []byte) (*hnswNode, error) {
	t, err := tuple.Unpack(data)
	if err != nil {
		return nil, fmt.Errorf("hnsw: unpack node: %w", err)
	}
	if len(t) < 3 {
		return nil, fmt.Errorf("hnsw: node tuple too short")
	}

	node := &hnswNode{ID: nodeID}
	idx := 0

	maxLayer, _ := asInt64(t[idx])
	node.MaxLayer = int(maxLayer)
	idx++

	vecLen, _ := asInt64(t[idx])
	idx++

	node.Vector = make([]float64, vecLen)
	for i := 0; i < int(vecLen) && idx < len(t); i++ {
		switch v := t[idx].(type) {
		case uint64:
			node.Vector[i] = math.Float64frombits(v)
		case int64:
			node.Vector[i] = math.Float64frombits(uint64(v))
		}
		idx++
	}

	if idx < len(t) {
		numLayers, _ := asInt64(t[idx])
		idx++
		node.Neighbors = make([][]hnswNeighbor, numLayers)
		for layer := 0; layer < int(numLayers) && idx < len(t); layer++ {
			layerLen, _ := asInt64(t[idx])
			idx++
			node.Neighbors[layer] = make([]hnswNeighbor, layerLen)
			for j := 0; j < int(layerLen) && idx+1 < len(t); j++ {
				var nbID []byte
				if id, ok := t[idx].([]byte); ok {
					nbID = id
				}
				idx++
				var dist float64
				switch v := t[idx].(type) {
				case uint64:
					dist = math.Float64frombits(v)
				case int64:
					dist = math.Float64frombits(uint64(v))
				}
				idx++
				node.Neighbors[layer][j] = hnswNeighbor{NodeID: nbID, Distance: dist}
			}
		}
	}

	return node, nil
}

// distHeap is a min-heap for search candidates.
type distHeap []distItem

type distItem struct {
	nodeID []byte
	dist   float64
}

func (h distHeap) Len() int            { return len(h) }
func (h distHeap) Less(i, j int) bool  { return h[i].dist < h[j].dist }
func (h distHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *distHeap) Push(x any)         { *h = append(*h, x.(distItem)) }
func (h *distHeap) Pop() any           { old := *h; n := len(old); x := old[n-1]; *h = old[:n-1]; return x }

