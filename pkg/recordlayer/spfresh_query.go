package recordlayer

import (
	"fmt"
	"sort"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/vectorcodec"
)

// SPFresh query path (RFC-094 §4): route on the cached two-level table →
// fetch k_c postings in one parallel burst (snapshot reads, fetch-capped) →
// RaBitQ residual distances → exact re-rank of the top C from the fp16
// sidecar (parallel point reads). Three network round trips happy path
// (GRV + postings + re-rank); +2 RT per forwarded posting (≤ depth 2, then
// the caller refreshes); all reads snapshot — a query never conflicts with
// anything.

// spfreshSearcher executes searches against one generation's storage using a
// shared routing cache.
type spfreshSearcher struct {
	storage *spfreshStorage
	config  SPFreshConfig
	cache   *spfreshRoutingCache
	quant   *spfreshQuantizer

	// runtime knobs (RFC-094 §4 defaults; never stored)
	w  int // coarse cells probed
	kc int // fine postings fetched
	c  int // re-rank candidates
}

func newSPFreshSearcher(storage *spfreshStorage, config SPFreshConfig, cache *spfreshRoutingCache) *spfreshSearcher {
	return &spfreshSearcher{
		storage: storage,
		config:  config,
		cache:   cache,
		quant:   newSPFreshQuantizer(config),
		w:       32,
		kc:      96,
		c:       400,
	}
}

// spfreshSearchResult is one search hit.
type spfreshSearchResult struct {
	PrimaryKey tuple.Tuple
	Distance   float64
}

// spfreshApproxHit is an approximate candidate before re-rank.
type spfreshApproxHit struct {
	pk    tuple.Tuple
	pkKey string
	est   float64
}

// search returns the k nearest neighbors of query. The routing cache must be
// loaded (the maintainer refreshes it off the query path).
func (s *spfreshSearcher) search(tx fdb.ReadTransaction, query []float64, k int) ([]spfreshSearchResult, error) {
	if k <= 0 {
		return nil, nil
	}
	routed, err := s.cache.route(tx, s.storage, query, s.w, s.kc)
	if err != nil {
		return nil, err
	}
	if len(routed) == 0 {
		return nil, nil
	}

	// One parallel burst: all posting range reads issued before any resolves.
	// The fetch cap (2×Lmax+1 rows) bounds an unmaintained posting's cost to
	// THIS query (metered, never unbounded — RFC-094 §4).
	limit := 2*s.config.Lmax + 1
	type postingFetch struct {
		routed spfreshRouted
		future fdb.RangeResult
	}
	fetches := make([]postingFetch, 0, len(routed))
	for _, rt := range routed {
		r, rerr := s.storage.postingRange(rt.fineID)
		if rerr != nil {
			return nil, rerr
		}
		fetches = append(fetches, postingFetch{
			routed: rt,
			future: tx.Snapshot().GetRange(r, fdb.RangeOptions{Limit: limit, Mode: fdb.StreamingModeWantAll}),
		})
	}

	// Residual distance estimation per posting; min-estimate dedup across
	// closure replicas (RFC-094 §4/§7).
	best := make(map[string]*spfreshApproxHit)
	residual := make([]float64, len(query))
	var forwards []spfreshRouted // stale-cache HDR redirects, resolved after the burst
	for _, f := range fetches {
		kvs, kerr := f.future.GetSliceWithError()
		if kerr != nil {
			return nil, fmt.Errorf("spfresh search: posting %d: %w", f.routed.fineID, kerr)
		}
		for d := range query {
			residual[d] = query[d] - f.routed.vec[d]
		}
		for _, kv := range kvs {
			pk, isEntry, perr := s.storage.postingPK(kv.Key)
			if perr != nil {
				return nil, perr
			}
			if !isEntry {
				// Forwarded posting (split landed inside our staleness
				// window): queue the children, resolved below (+2 RT bounded).
				cellID, childA, childB, herr := decodePostingHDR(kv.Value)
				if herr != nil {
					return nil, herr
				}
				fwd, ferr := s.resolveForward(tx, cellID, childA, childB)
				if ferr != nil {
					return nil, ferr
				}
				forwards = append(forwards, fwd...)
				continue
			}
			est, derr := s.quant.Distance(residual, kv.Value, s.config.NumDimensions)
			if derr != nil {
				return nil, derr
			}
			s.mergeHit(best, pk, est)
		}
	}

	// Resolve forwarded children: one more parallel burst (depth 1; deeper
	// chains are the caller's refresh signal — we treat children's own HDRs
	// as absent entries here and rely on the next refresh, bounded per spec).
	if len(forwards) > 0 {
		type fwdFetch struct {
			routed spfreshRouted
			future fdb.RangeResult
		}
		ff := make([]fwdFetch, 0, len(forwards))
		for _, rt := range forwards {
			r, rerr := s.storage.postingRange(rt.fineID)
			if rerr != nil {
				return nil, rerr
			}
			ff = append(ff, fwdFetch{routed: rt, future: tx.Snapshot().GetRange(r, fdb.RangeOptions{Limit: limit, Mode: fdb.StreamingModeWantAll})})
		}
		for _, f := range ff {
			kvs, kerr := f.future.GetSliceWithError()
			if kerr != nil {
				return nil, fmt.Errorf("spfresh search: forwarded posting %d: %w", f.routed.fineID, kerr)
			}
			for d := range query {
				residual[d] = query[d] - f.routed.vec[d]
			}
			for _, kv := range kvs {
				pk, isEntry, perr := s.storage.postingPK(kv.Key)
				if perr != nil || !isEntry {
					continue // chain depth 2: next refresh handles it
				}
				est, derr := s.quant.Distance(residual, kv.Value, s.config.NumDimensions)
				if derr != nil {
					return nil, derr
				}
				s.mergeHit(best, pk, est)
			}
		}
	}

	if len(best) == 0 {
		return nil, nil
	}

	// Top-C by estimate.
	hits := make([]*spfreshApproxHit, 0, len(best))
	for _, h := range best {
		hits = append(hits, h)
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].est != hits[j].est {
			return hits[i].est < hits[j].est
		}
		return hits[i].pkKey < hits[j].pkKey
	})
	cTop := s.c
	if cTop < k {
		cTop = k
	}
	if cTop < len(hits) {
		hits = hits[:cTop]
	}

	// Exact re-rank from the fp16 sidecar (parallel point reads). With the
	// sidecar disabled the estimates stand (the maintainer's record-read
	// fallback arrives with the maintainer slice).
	results := make([]spfreshSearchResult, 0, len(hits))
	if s.config.Sidecar {
		futures := make([]fdb.FutureByteSlice, len(hits))
		for i, h := range hits {
			futures[i] = tx.Snapshot().Get(s.storage.sidecarKey(h.pk))
		}
		for i, h := range hits {
			data, gerr := futures[i].Get()
			if gerr != nil {
				return nil, fmt.Errorf("spfresh search: sidecar read: %w", gerr)
			}
			if data == nil {
				continue // deleted between bursts: skip
			}
			vec, derr := vectorcodec.Deserialize(data)
			if derr != nil {
				return nil, derr
			}
			results = append(results, spfreshSearchResult{
				PrimaryKey: h.pk,
				Distance:   spfreshMetricDistance(s.config.Metric, query, vec),
			})
		}
	} else {
		for _, h := range hits {
			results = append(results, spfreshSearchResult{PrimaryKey: h.pk, Distance: h.est})
		}
	}

	sort.Slice(results, func(i, j int) bool {
		if results[i].Distance != results[j].Distance {
			return results[i].Distance < results[j].Distance
		}
		return string(results[i].PrimaryKey.Pack()) < string(results[j].PrimaryKey.Pack())
	})
	if k < len(results) {
		results = results[:k]
	}
	return results, nil
}

func (s *spfreshSearcher) mergeHit(best map[string]*spfreshApproxHit, pk tuple.Tuple, est float64) {
	key := string(pk.Pack())
	if h, ok := best[key]; ok {
		if est < h.est {
			h.est = est
		}
		return
	}
	best[key] = &spfreshApproxHit{pk: pk, pkKey: key, est: est}
}

// resolveForward point-reads the children's centroid rows using the cellID
// CARRIED IN THE HDR — never the cell the client routed through (the parent
// may itself have moved cells; RFC-094 §3/§4, codex r4 #3).
func (s *spfreshSearcher) resolveForward(tx fdb.ReadTransaction, cellID, childA, childB int64) ([]spfreshRouted, error) {
	var out []spfreshRouted
	for _, fineID := range []int64{childA, childB} {
		if fineID == 0 {
			continue
		}
		data, err := tx.Snapshot().Get(s.storage.centroidKey(cellID, fineID)).Get()
		if err != nil {
			return nil, fmt.Errorf("spfresh search: forward child (%d,%d): %w", cellID, fineID, err)
		}
		if data == nil {
			continue // deeper staleness: the next cache refresh re-routes
		}
		row, derr := decodeCentroidRow(data)
		if derr != nil {
			return nil, derr
		}
		vec, verr := row.vector()
		if verr != nil {
			return nil, verr
		}
		out = append(out, spfreshRouted{cellID: cellID, fineID: fineID, vec: vec})
	}
	return out, nil
}

// spfreshMetricDistance computes the exact metric distance for re-ranking.
func spfreshMetricDistance(metric VectorMetric, a, b []float64) float64 {
	return vectorDistance(a, b, metric)
}
