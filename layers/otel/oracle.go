package otel

import (
	"container/heap"
	"context"
)

// oracleStats bounds retained answers by unique predicates * K; scratch
// witnesses contain only the current adjacent source pair.
type oracleStats struct {
	Predicates       int
	ScannedSpans     int
	PeakRetainedHits int
}

func retainOracleHit(h *oracleHeap, hit Hit, k int) bool {
	if h.Len() < k {
		heap.Push(h, hit)
		return true
	}
	worst := (*h)[0]
	if hit.Latest > worst.Latest || (hit.Latest == worst.Latest && hit.Trace > worst.Trace) {
		heap.Pop(h)
		heap.Push(h, hit)
	}
	return false
}

func finishOracleHeap(ctx context.Context, h *oracleHeap) ([]Hit, error) {
	out := make([]Hit, h.Len())
	for i := len(out) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return nil, wrap("canceled", "oracle topK", err)
		}
		out[i] = heap.Pop(h).(Hit)
	}
	return out, nil
}

// scanOraclePredicates combines the only two source witnesses before top-K
// retention, without backend encodings or a whole-matching-trace population map.
func scanOraclePredicates(ctx context.Context, c Config, attributes []Attribute, intervals []TimeRange) (map[Attribute][]Hit, map[TimeRange][]Hit, oracleStats, error) {
	stats := oracleStats{}
	attrs := map[Attribute]*oracleHeap{}
	times := map[TimeRange]*oracleHeap{}
	if c.K < 1 {
		return nil, nil, stats, fail("config", "oracle topK", "positive K required")
	}
	for _, a := range attributes {
		if _, exists := attrs[a]; !exists {
			attrs[a] = &oracleHeap{}
		}
	}
	for _, interval := range intervals {
		if err := interval.Validate(); err != nil {
			return nil, nil, stats, err
		}
		if _, exists := times[interval]; !exists {
			times[interval] = &oracleHeap{}
		}
	}
	stats.Predicates = len(attrs) + len(times)
	if err := ctx.Err(); err != nil {
		return nil, nil, stats, wrap("canceled", "oracle scan", err)
	}
	retained := 0
	// Reused bounded scratch state: at most the requested attributes found on
	// this one trace, not matching trace identities from earlier source pairs.
	pairMatches := map[Attribute]int64{}
	for lo := 0; stats.Predicates > 0 && lo < c.Count; lo += 2 {
		if err := ctx.Err(); err != nil {
			return nil, nil, stats, wrap("canceled", "oracle scan", err)
		}
		first := Generate(c, lo)
		pair := [2]Span{first}
		count := 1
		if lo+1 < c.Count {
			pair[1] = Generate(c, lo+1)
			count++
		}
		stats.ScannedSpans += count
		if first.Tenant != c.Tenant {
			continue
		}
		clear(pairMatches)
		for i := 0; i < count; i++ {
			for _, a := range pair[i].Attributes {
				if _, requested := attrs[a]; requested {
					if previous, ok := pairMatches[a]; !ok || pair[i].Start > previous {
						pairMatches[a] = pair[i].Start
					}
				}
			}
		}
		for a, latest := range pairMatches {
			if retainOracleHit(attrs[a], Hit{Trace: first.Trace, Latest: latest}, c.K) {
				retained++
			}
		}
		for interval, h := range times {
			matched := false
			var latest int64
			for i := 0; i < count; i++ {
				s := pair[i]
				if s.Start >= interval.Start && s.Start < interval.End && (!matched || s.Start > latest) {
					matched, latest = true, s.Start
				}
			}
			if matched && retainOracleHit(h, Hit{Trace: first.Trace, Latest: latest}, c.K) {
				retained++
			}
		}
		if retained > stats.PeakRetainedHits {
			stats.PeakRetainedHits = retained
		}
	}
	outAttrs := make(map[Attribute][]Hit, len(attrs))
	outTimes := make(map[TimeRange][]Hit, len(times))
	for a, h := range attrs {
		out, err := finishOracleHeap(ctx, h)
		if err != nil {
			return nil, nil, stats, err
		}
		outAttrs[a] = out
	}
	for interval, h := range times {
		out, err := finishOracleHeap(ctx, h)
		if err != nil {
			return nil, nil, stats, err
		}
		outTimes[interval] = out
	}
	return outAttrs, outTimes, stats, nil
}

func ExpectedTimeContext(ctx context.Context, c Config, interval TimeRange) ([]Hit, error) {
	_, results, _, err := scanOraclePredicates(ctx, c, nil, []TimeRange{interval})
	return results[interval], err
}

func ExpectedSpanContext(ctx context.Context, c Config, id string) ([]SpanMatch, error) {
	out := []SpanMatch{}
	for i := 0; i < c.Count; i++ {
		if err := ctx.Err(); err != nil {
			return nil, wrap("canceled", "span oracle scan", err)
		}
		s := Generate(c, i)
		if s.Tenant == c.Tenant && s.ID == id {
			out = append(out, SpanMatch{Trace: s.Trace, ID: s.ID, Start: s.Start})
		}
	}
	return out, nil
}

// prepareQueryOracles bounds equality/time answers and uses known source
// witnesses for point needles, entirely outside timed backend calls.
func prepareQueryOracles(ctx context.Context, c Config, cases []queryCase) (oracleStats, error) {
	var attrs []Attribute
	var intervals []TimeRange
	for i := range cases {
		q := &cases[i]
		if err := ctx.Err(); err != nil {
			return oracleStats{}, wrap("canceled", "prepare queries", err)
		}
		switch q.kind {
		case "trace":
			q.expectedTrace = []SpanHit{}
			lo := q.sourceOrdinal / 2 * 2
			for ordinal := lo; ordinal < lo+2 && ordinal < c.Count; ordinal++ {
				s := Generate(c, ordinal)
				if s.Tenant == c.Tenant && s.Trace == q.trace {
					q.expectedTrace = append(q.expectedTrace, SpanHit{ID: s.ID, Start: s.Start})
				}
			}
			oracleTraceSort(q.expectedTrace)
		case "span":
			q.expectedSpans = []SpanMatch{}
			s := Generate(c, q.sourceOrdinal)
			if s.Tenant == c.Tenant && s.ID == q.span {
				q.expectedSpans = append(q.expectedSpans, SpanMatch{Trace: s.Trace, ID: s.ID, Start: s.Start})
			}
		case "time":
			intervals = append(intervals, q.interval)
		default:
			attrs = append(attrs, q.attribute)
		}
	}
	attributeResults, timeResults, stats, err := scanOraclePredicates(ctx, c, attrs, intervals)
	if err != nil {
		return stats, err
	}
	for i := range cases {
		if err := ctx.Err(); err != nil {
			return stats, wrap("canceled", "prepare queries", err)
		}
		switch cases[i].kind {
		case "trace", "span":
		case "time":
			cases[i].expected = timeResults[cases[i].interval]
		default:
			cases[i].expected = attributeResults[cases[i].attribute]
		}
	}
	return stats, nil
}
