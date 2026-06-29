package recordlayer

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// Ground-truth recall monitoring (RFC-156 §3.1 invariant 5 / Tier-1 operability).
// A vector index corrupts SILENTLY — wrong results, not a crash — so the one
// signal that catches drift or quiet corruption in production is recall against
// brute-force ground truth. This is that monitor: sample query vectors from the
// index's own records, compute the TRUE k-nearest by a full metric scan of the
// corpus, compare to the index's k-nearest, and report recall@k.
//
// It is an opt-in diagnostic (like SPFreshDebugIntegrity), O(querySamples ×
// corpus) — run it off the serving path on a cadence. Wire MeanRecall to an
// alert: a drop below your measured baseline means maintenance is behind, an
// ingest-rate recall trade is in effect, or (with the integrity check also
// failing) real corruption.

// spfreshMaxRecallCorpus bounds the in-memory ground-truth corpus. The whole
// measurement runs inside the caller's SINGLE transaction (the corpus scan plus
// every query search), so it is bounded by FDB's 5s / size transaction limits —
// this cap keeps a typical-record index inside that envelope. For larger indexes
// the corpus scan alone blows the 5s limit (the failure spfreshScanRecordBatches
// exists to avoid); measure a sampled subset, or use a future db-based variant
// that batches the corpus scan across transactions and runs each query in its
// own snapshot tx (RFC-156 §4 follow-up). Exceeding the cap errors rather than
// silently degrading to a partial corpus.
const spfreshMaxRecallCorpus = 50_000

// SPFreshRecallReport summarizes a recall measurement.
type SPFreshRecallReport struct {
	K               int     // neighbors per query
	CorpusSize      int     // indexed records scanned for ground truth
	QueriesRun      int     // query samples evaluated
	MeanRecall      float64 // average recall@k across queries (the headline signal)
	MinRecall       float64 // worst single-query recall@k
	PerfectFraction float64 // fraction of queries with recall@k == 1.0
}

// MeasureSPFreshRecall measures recall@k of a readable SPFresh index against
// brute-force ground truth over `querySamples` query vectors drawn from the
// index's own records (deterministic by seed). Returns a zero-query report when
// the index is empty. The store must be bound to a transaction (call inside
// db.Run); the whole scan+queries run in that one tx, so keep the corpus under
// spfreshMaxRecallCorpus (see its doc for the scale limit). Mostly read-only,
// but it can inherit SearchSPFreshIndex's read-path split-repair write for any
// over-envelope posting (none on a maintained index). Distances use the index's
// configured metric, so the
// ground-truth ranking matches what the index optimizes, and recall is scored
// with the SPANN §4.2.1 tie correction (a result within the k-th true distance
// counts, so equidistant/duplicate vectors don't under-report).
//
// Queries are drawn from the corpus itself (not a held-out set as in the SPANN
// SIFT benchmark), so absolute numbers run slightly optimistic — the self-match
// is a guaranteed hit. That is fine for the intended use as a relative drift
// signal: alert on MeanRecall falling below YOUR measured baseline, not on an
// absolute target.
func MeasureSPFreshRecall(ctx context.Context, store *FDBRecordStore, indexName string, k, querySamples int, seed int64) (SPFreshRecallReport, error) {
	report := SPFreshRecallReport{K: k}
	if k <= 0 {
		return report, fmt.Errorf("spfresh recall: k must be > 0, got %d", k)
	}
	idx := store.GetMetaData().GetIndex(indexName)
	if idx == nil {
		return report, fmt.Errorf("spfresh recall: index %q not found", indexName)
	}
	if idx.Type != IndexTypeVectorSPFresh {
		return report, fmt.Errorf("spfresh recall: index %q has type %q, not %q", indexName, idx.Type, IndexTypeVectorSPFresh)
	}
	config := parseSPFreshConfig(idx)

	// Which record types this index covers.
	allowed := map[string]bool{}
	for _, rt := range store.GetMetaData().RecordTypesForIndex(idx) {
		allowed[rt.Name] = true
	}

	// Build the ground-truth corpus: every indexed record's (pk, vector).
	type corpusEntry struct {
		pk  tuple.Tuple
		vec []float64
	}
	var corpus []corpusEntry
	cursor := store.ScanRecords(nil, ForwardScan())
	defer func() { _ = cursor.Close() }()
	for {
		res, err := cursor.OnNext(ctx)
		if err != nil {
			return report, fmt.Errorf("spfresh recall: scan records: %w", err)
		}
		if !res.HasNext() {
			break
		}
		rec := res.GetValue()
		if rec.RecordType != nil && !allowed[rec.RecordType.Name] {
			continue
		}
		tuples, err := idx.RootExpression.Evaluate(rec, rec.Record)
		if err != nil {
			continue // expression doesn't apply to this record — skip
		}
		for _, values := range tuples {
			// tupleToVector (the maintainer's decoder) handles BOTH numeric
			// tuple elements AND a serialized-vector []byte (the
			// KeyWithValue(Field("vector_data"), 0) shape the SPFresh benchmarks
			// and real deployments use). A plain numeric-only conversion would
			// drop the []byte and silently report an empty corpus on the common
			// index shape (codex P1).
			t := make(tuple.Tuple, len(values))
			for i, v := range values {
				t[i] = v
			}
			vec, verr := tupleToVector(t)
			if verr != nil || len(vec) != config.NumDimensions {
				continue
			}
			corpus = append(corpus, corpusEntry{pk: rec.PrimaryKey, vec: vec})
		}
		if len(corpus) > spfreshMaxRecallCorpus {
			return report, fmt.Errorf("spfresh recall: corpus exceeds %d vectors — too large for full ground truth; sample the corpus upstream", spfreshMaxRecallCorpus)
		}
	}

	report.CorpusSize = len(corpus)
	if len(corpus) == 0 {
		return report, nil // empty index: vacuously fine, no queries
	}
	if k > len(corpus) {
		k = len(corpus)
		report.K = k
	}

	// Deterministic query sample (without replacement) from the corpus.
	rng := rand.New(rand.NewPCG(uint64(seed), 0x5eed))
	q := querySamples
	if q <= 0 || q > len(corpus) {
		q = len(corpus)
	}
	perm := rng.Perm(len(corpus))[:q]

	// pk -> true (corpus) vector. An index result is scored by its TRUE fp64
	// corpus distance, NOT the index's r.Distance: r.Distance is re-ranked from
	// the fp16 sidecar and carries half-precision error (~1e-3 relative for real
	// embeddings), which would corrupt the boundary comparison below (e.g. a
	// self-query's fp16 distance ~1e-3 > the fp64 k-th distance 0). Scoring both
	// sides from the fp64 corpus keeps the tie band meaningful.
	corpusVec := make(map[string][]float64, len(corpus))
	for i := range corpus {
		corpusVec[string(corpus[i].pk.Pack())] = corpus[i].vec
	}

	report.MinRecall = 1.0
	sumRecall := 0.0
	perfect := 0
	dists := make([]float64, len(corpus)) // reused per-query distance buffer

	for _, qi := range perm {
		query := corpus[qi].vec

		// True k-th distance: compute every corpus distance ONCE (the index's
		// metric, smaller = nearer), then select the k-th smallest by sorting
		// the float slice — no vectorDistance call inside the sort comparator.
		for i := range corpus {
			dists[i] = vectorDistance(query, corpus[i].vec, config.Metric)
		}
		sort.Float64s(dists)
		kthDist := dists[k-1]
		// SPANN §4.2.1 tie correction on TRUE (fp64) distances: credit a
		// returned id if its true distance is within the k-th true distance,
		// not only if it matches an arbitrarily-chosen true top-k id. Both sides
		// are fp64 from the corpus, so ties (duplicate / equidistant vectors)
		// count and the fp16 sidecar error never reaches the comparison.
		tol := 1e-9 + 1e-9*math.Abs(kthDist)

		// Index top-k.
		results, err := SearchSPFreshIndex(store, indexName, query, k)
		if err != nil {
			return report, fmt.Errorf("spfresh recall: search: %w", err)
		}
		hits := 0
		for _, r := range results {
			v, ok := corpusVec[string(r.PrimaryKey.Pack())]
			if !ok {
				continue // result is not a live indexed record (orphan) — not a true hit
			}
			if vectorDistance(query, v, config.Metric) <= kthDist+tol {
				hits++
			}
		}
		recall := float64(hits) / float64(k)
		sumRecall += recall
		if recall < report.MinRecall {
			report.MinRecall = recall
		}
		if recall >= 1.0 {
			perfect++
		}
	}

	report.QueriesRun = q
	report.MeanRecall = sumRecall / float64(q)
	report.PerfectFraction = float64(perfect) / float64(q)
	return report, nil
}
