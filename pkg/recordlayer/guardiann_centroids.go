package recordlayer

import (
	"bytes"
	"container/heap"
	"errors"
	"math"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// The centroid index is a plain HNSW (no inlining, no RaBitQ) keyed by
// cluster id. These are the HNSW operations GuardiANN needs beyond insert and
// delete: fetch, cardinality and HNSW.orderByDistance at radius 0.

// centroidNode is NodeReferenceWithDistance: a layer node's span, its stored
// vector bytes and its distance to the walk's center.
type centroidNode struct {
	span     []byte
	vec      []byte
	distance float64
}

// lessCentroidNode is NodeReferenceWithDistance.comparator: distance by
// Double.compare (NaN last, -0.0 before 0.0), then primary key.
func lessCentroidNode(a, b centroidNode) bool {
	if c := compareFloat64Java(a.distance, b.distance); c != 0 {
		return c < 0
	}
	return bytes.Compare(a.span, b.span) < 0
}

type centroidMinHeap []centroidNode

func (h centroidMinHeap) Len() int           { return len(h) }
func (h centroidMinHeap) Less(i, j int) bool { return lessCentroidNode(h[i], h[j]) }
func (h centroidMinHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *centroidMinHeap) Push(x any)        { *h = append(*h, x.(centroidNode)) }
func (h *centroidMinHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

type centroidMaxHeap struct{ centroidMinHeap }

func (h centroidMaxHeap) Less(i, j int) bool {
	return lessCentroidNode(h.centroidMinHeap[j], h.centroidMinHeap[i])
}

// centroidEntry is a ResultEntry of the centroid walk.
type centroidEntry struct {
	clusterID tuple.UUID
	vector    gVector
	distance  float64
}

func (g *guardiann) centroidEntryOf(n centroidNode) (centroidEntry, error) {
	pk, err := decodeNestedPK(n.span)
	if err != nil {
		return centroidEntry{}, err
	}
	v, err := g.codec.decode(n.vec)
	if err != nil {
		return centroidEntry{}, err
	}
	id, err := guardiannElem[tuple.UUID](pk, 0, "centroid key")
	if err != nil {
		return centroidEntry{}, err
	}
	return centroidEntry{clusterID: id, vector: v, distance: n.distance}, nil
}

// fetchCentroid is HNSW.fetch: the cluster's centroid on layer 0, or nil.
func (g *guardiann) fetchCentroid(tx fdb.ReadTransaction, clusterID tuple.UUID) (*gVector, error) {
	vec, _, err := g.centroids.storage.loadNodeLayer(tx, 0, tuple.Tuple{clusterID})
	if err != nil {
		if errors.Is(err, errHNSWNotPresent) {
			return nil, nil
		}
		return nil, err
	}
	v, err := g.codec.decode(vec)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// centroidCardinalityMultiple is HNSW.cardinality() == MULTIPLE: layer 0
// holds at least two nodes.
func (g *guardiann) centroidCardinalityMultiple(tx fdb.ReadTransaction) (bool, error) {
	r, err := fdb.PrefixRange(g.centroids.storage.dataSubspace.Pack(tuple.Tuple{int64(0)}))
	if err != nil {
		return false, err
	}
	kvs, err := tx.GetRange(r, fdb.RangeOptions{Limit: 2}).GetSliceWithError()
	return len(kvs) >= 2, err
}

// centroidsOrderedByDistance is Primitives.centroidsOrderedByDistance: the
// centroid HNSW's orderByDistance at radius 0 with quick start, yielding
// clusters in (approximately) ascending distance from center.
func (g *guardiann) centroidsOrderedByDistance(tx fdb.ReadTransaction, center gVector, efRing, efOutward int) (*centroidWalk, error) {
	w := &centroidWalk{g: g, tx: tx, center: center.data, efOutward: efOutward, visited: map[string]bool{}}
	info, err := g.centroids.storage.loadAccessInfo(tx)
	if err != nil {
		if errors.Is(err, errHNSWNotPresent) {
			return w, nil
		}
		return nil, err
	}
	if info == nil {
		return w, nil
	}
	upper := min(max(int(math.Floor(math.Sqrt(float64(efRing)))), 8), 32)
	current := []centroidNode{{span: nestPK(info.pk), vec: info.vectorBytes}}
	for layer := info.layer; layer >= 0; layer-- {
		ef := upper
		if layer == 0 {
			ef = efRing
		}
		if current, err = g.beamSearchLayer(tx, layer, current, ef, center.data); err != nil {
			return nil, err
		}
	}
	// OutwardTraversalIterator.initialTravelState: every zoom-in node is
	// visited, a candidate and (quick start) served first.
	for _, n := range current {
		w.visited[string(n.span)] = true
		heap.Push(&w.candidates, n)
		heap.Push(&w.quickStart, n)
		w.quickStartKeys = append(w.quickStartKeys, string(n.span))
	}
	return w, nil
}

// beamSearchLayer is Search.beamSearchLayer: re-read the start nodes on this
// layer, then a best-first search keeping the ef nearest.
func (g *guardiann) beamSearchLayer(tx fdb.ReadTransaction, layer int, start []centroidNode, ef int, center []float64) ([]centroidNode, error) {
	spans := make([][]byte, len(start))
	for i, n := range start {
		spans[i] = n.span
	}
	visited := map[string]bool{}
	var candidates centroidMinHeap
	nearest := centroidMaxHeap{}
	for _, r := range g.centroids.storage.loadNodeLayerBatchDispatch(tx, layer, spans) {
		if r.err != nil {
			if e := hnswFatal(r.err); e != nil {
				return nil, e
			}
			continue
		}
		n := centroidNode{span: r.span, vec: r.vecBytes}
		if n.distance, r.err = g.rawDistance(center, r.vecBytes); r.err != nil {
			return nil, r.err
		}
		visited[r.spanStr] = true
		heap.Push(&candidates, n)
		heap.Push(&nearest, n)
	}
	for candidates.Len() > 0 {
		c := heap.Pop(&candidates).(centroidNode)
		if nearest.Len() > 0 && c.distance > nearest.centroidMinHeap[0].distance {
			break
		}
		_, neighbors, err := g.centroids.storage.loadNodeLayerDispatch(tx, layer, mustDecodeSpan(c.span))
		if err != nil {
			if e := hnswFatal(err); e != nil {
				return nil, e
			}
			continue
		}
		var fresh [][]byte
		for _, nb := range neighbors {
			if !visited[string(nb)] {
				fresh = append(fresh, nb)
			}
		}
		for _, r := range g.centroids.storage.loadNodeLayerBatchDispatch(tx, layer, fresh) {
			if r.err != nil {
				if e := hnswFatal(r.err); e != nil {
					return nil, e
				}
				continue
			}
			visited[r.spanStr] = true
			d, err := g.rawDistance(center, r.vecBytes)
			if err != nil {
				return nil, err
			}
			if d < nearest.centroidMinHeap[0].distance || nearest.Len() < ef {
				n := centroidNode{span: r.span, vec: r.vecBytes, distance: d}
				heap.Push(&candidates, n)
				heap.Push(&nearest, n)
				if nearest.Len() > ef {
					heap.Pop(&nearest)
				}
			}
		}
	}
	return nearest.centroidMinHeap, nil
}

func (g *guardiann) rawDistance(center []float64, vecBytes []byte) (float64, error) {
	v, err := deserializeVector(vecBytes)
	if err != nil {
		return 0, err
	}
	return javaMetricDistance(center, v, g.config.metric), nil
}

func mustDecodeSpan(span []byte) tuple.Tuple {
	pk, err := decodeNestedPK(span)
	if err != nil {
		return nil
	}
	return pk
}

// centroidWalk is OutwardTraversalIterator at radius 0.
type centroidWalk struct {
	g              *guardiann
	tx             fdb.ReadTransaction
	center         []float64
	efOutward      int
	candidates     centroidMinHeap
	out            centroidMinHeap
	quickStart     centroidMinHeap
	quickStartKeys []string
	visited        map[string]bool
}

func (w *centroidWalk) isQuickStart(span []byte) bool {
	for _, k := range w.quickStartKeys {
		if k == string(span) {
			return true
		}
	}
	return false
}

// next returns the next centroid, or ok=false once the walk is exhausted.
func (w *centroidWalk) next() (centroidEntry, bool, error) {
	if w.quickStart.Len() == 0 {
		for w.candidates.Len() > 0 && w.out.Len() < w.efOutward {
			c := heap.Pop(&w.candidates).(centroidNode)
			if !w.isQuickStart(c.span) {
				heap.Push(&w.out, c)
			}
			_, neighbors, err := w.g.centroids.storage.loadNodeLayerDispatch(w.tx, 0, mustDecodeSpan(c.span))
			if err != nil {
				if e := hnswFatal(err); e != nil {
					return centroidEntry{}, false, e
				}
				continue
			}
			var fresh [][]byte
			for _, nb := range neighbors {
				if !w.visited[string(nb)] {
					fresh = append(fresh, nb)
				}
			}
			for _, r := range w.g.centroids.storage.loadNodeLayerBatchDispatch(w.tx, 0, fresh) {
				if r.err != nil {
					if e := hnswFatal(r.err); e != nil {
						return centroidEntry{}, false, e
					}
					continue
				}
				d, err := w.g.rawDistance(w.center, r.vecBytes)
				if err != nil {
					return centroidEntry{}, false, err
				}
				w.visited[r.spanStr] = true
				heap.Push(&w.candidates, centroidNode{span: r.span, vec: r.vecBytes, distance: d})
			}
		}
	}
	var n centroidNode
	switch {
	case w.quickStart.Len() > 0:
		n = heap.Pop(&w.quickStart).(centroidNode)
	case w.out.Len() > 0:
		n = heap.Pop(&w.out).(centroidNode)
	default:
		return centroidEntry{}, false, nil
	}
	e, err := w.g.centroidEntryOf(n)
	return e, err == nil, err
}

// take returns up to limit centroids of the walk.
func (w *centroidWalk) take(limit int) ([]centroidEntry, error) {
	var out []centroidEntry
	for len(out) < limit {
		e, ok, err := w.next()
		if err != nil || !ok {
			return out, err
		}
		out = append(out, e)
	}
	return out, nil
}
