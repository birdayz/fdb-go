package recordlayer

import (
	"context"
	"fmt"
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

// spfreshMaxRecallCorpus bounds the in-memory ground-truth corpus. Beyond this,
// full brute-force ground truth is too expensive to be a periodic check; sample
// the corpus upstream (e.g. a dedicated sampling index) instead of silently
// degrading to approximate ground truth.
const spfreshMaxRecallCorpus = 500_000

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
// the index is empty. The store must be bound to a read transaction (call
// inside db.Run). Distances use the index's configured metric, so the
// ground-truth ranking matches what the index optimizes.
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
			vec := spfreshValuesToFloat64(values)
			if len(vec) != config.NumDimensions {
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

	report.MinRecall = 1.0
	sumRecall := 0.0
	perfect := 0
	scratch := make([]int, len(corpus)) // reused index buffer for the sort

	for _, qi := range perm {
		query := corpus[qi].vec

		// True top-k: rank the whole corpus by the index's metric (ascending =
		// nearest), take the first k pks.
		for i := range scratch {
			scratch[i] = i
		}
		sort.Slice(scratch, func(a, b int) bool {
			return vectorDistance(query, corpus[scratch[a]].vec, config.Metric) <
				vectorDistance(query, corpus[scratch[b]].vec, config.Metric)
		})
		truth := make(map[string]bool, k)
		for i := 0; i < k; i++ {
			truth[string(corpus[scratch[i]].pk.Pack())] = true
		}

		// Index top-k.
		results, err := SearchSPFreshIndex(store, indexName, query, k)
		if err != nil {
			return report, fmt.Errorf("spfresh recall: search: %w", err)
		}
		hits := 0
		for _, r := range results {
			if truth[string(r.PrimaryKey.Pack())] {
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

// spfreshValuesToFloat64 converts an evaluated index key tuple to a float64
// vector, returning nil if any component is non-numeric. Mirrors the chaos
// harness's valuesToFloat64 (kept package-local to avoid a test-only import).
func spfreshValuesToFloat64(values []any) []float64 {
	vec := make([]float64, 0, len(values))
	for _, v := range values {
		switch n := v.(type) {
		case float64:
			vec = append(vec, n)
		case float32:
			vec = append(vec, float64(n))
		case int64:
			vec = append(vec, float64(n))
		case int32:
			vec = append(vec, float64(n))
		case int:
			vec = append(vec, float64(n))
		default:
			return nil
		}
	}
	return vec
}
