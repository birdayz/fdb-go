package otel

import (
	"context"
	"sort"
)

// These small-fixture references deliberately retain all matching traces. The
// production oracle must produce the same answers without that population map.
func bruteOracleHits(c Config, matches func(Span) bool) []Hit {
	latest := map[string]int64{}
	for ordinal := 0; ordinal < c.Count; ordinal++ {
		s := Generate(c, ordinal)
		if s.Tenant != c.Tenant || !matches(s) {
			continue
		}
		if ns, ok := latest[s.Trace]; !ok || s.Start > ns {
			latest[s.Trace] = s.Start
		}
	}
	out := []Hit{}
	for trace, start := range latest {
		out = append(out, Hit{Trace: trace, Latest: start})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Latest == out[j].Latest {
			return out[i].Trace > out[j].Trace
		}
		return out[i].Latest > out[j].Latest
	})
	if len(out) > c.K {
		out = out[:c.K]
	}
	return out
}

func bruteAttributeOracle(c Config, attribute Attribute) []Hit {
	return bruteOracleHits(c, func(s Span) bool {
		for _, a := range s.Attributes {
			if a == attribute {
				return true
			}
		}
		return false
	})
}

func bruteTimeOracle(c Config, interval TimeRange) []Hit {
	return bruteOracleHits(c, func(s Span) bool {
		return s.Start >= interval.Start && s.Start < interval.End
	})
}

func bruteSpanOracle(c Config, id string) []SpanMatch {
	out := []SpanMatch{}
	for ordinal := 0; ordinal < c.Count; ordinal++ {
		s := Generate(c, ordinal)
		if s.Tenant == c.Tenant && s.ID == id {
			out = append(out, SpanMatch{Trace: s.Trace, ID: s.ID, Start: s.Start})
		}
	}
	return out
}

func oracleTopKContext(ctx context.Context, m map[string]int64, k int) ([]Hit, error) {
	if err := ctx.Err(); err != nil {
		return nil, wrap("canceled", "oracle topK", err)
	}
	if k < 1 {
		return nil, fail("config", "oracle topK", "positive K required")
	}
	h := oracleHeap{}
	for id, ns := range m {
		if err := ctx.Err(); err != nil {
			return nil, wrap("canceled", "oracle topK", err)
		}
		retainOracleHit(&h, Hit{Trace: id, Latest: ns}, k)
	}
	return finishOracleHeap(ctx, &h)
}
