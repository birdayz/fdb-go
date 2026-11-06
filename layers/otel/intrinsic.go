package otel

import (
	"context"
	"encoding/hex"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

const (
	spanIndexName = "span_id_v2"
	timeIndexName = "span_time_v2"
)

// SpanMatch is a span-level identity, deliberately distinct from trace-level Hit.
// Standalone span-ID lookup returns all matches, not a trace-ID-hinted lookup.
type SpanMatch struct {
	Trace string `json:"trace_id"`
	ID    string `json:"span_id"`
	Start int64  `json:"start_ns"`
}

// TimeRange selects intrinsic span start timestamps in [Start, End).
type TimeRange struct {
	Start int64 `json:"start_ns"`
	End   int64 `json:"end_ns"`
}

func (r TimeRange) Validate() error {
	if r.End < r.Start {
		return fail("config", "time range", "end must not precede start")
	}
	return nil
}

// IntrinsicBackend preserves Backend while adding complete span and top-K time
// lookup; README.md specifies ordering, matching witnesses and work budgets.
type IntrinsicBackend interface {
	Span(context.Context, string) ([]SpanMatch, error)
	Time(context.Context, TimeRange) (QueryResult, error)
}

func validIdentity(s string, size int) bool {
	wire, err := hex.DecodeString(s)
	if err != nil || len(wire) != size || hex.EncodeToString(wire) != s {
		return false
	}
	var nonzero byte
	for _, b := range wire {
		nonzero |= b
	}
	return nonzero != 0
}

// intrinsicMatch uses metadata to reconstruct deduplicated primary keys;
// validate covering shape rather than guessing appended PK positions.
func intrinsicMatch(entry *recordlayer.IndexEntry, name string, tenant int) (SpanMatch, error) {
	bad := func() (SpanMatch, error) {
		return SpanMatch{}, fail("corruption", "intrinsic index", "malformed covering index entry")
	}
	if entry == nil || entry.Index == nil || entry.Index.Name != name || len(entry.Key) != 4 || len(entry.Value) != 0 {
		return bad()
	}
	values, pk := entry.IndexValues(), entry.PrimaryKey()
	if len(values) != 3 || len(pk) != 3 {
		return bad()
	}
	indexedTenant, ok := values[0].(int64)
	if !ok || indexedTenant != int64(tenant) {
		return bad()
	}
	pkTenant, tenantOK := pk[0].(int64)
	trace, traceOK := pk[1].(string)
	span, spanOK := pk[2].(string)
	if !tenantOK || pkTenant != indexedTenant || !traceOK || !spanOK || !validIdentity(trace, 16) || !validIdentity(span, 8) {
		return bad()
	}
	var start int64
	if name == spanIndexName {
		indexedSpan, idOK := values[1].(string)
		var timeOK bool
		start, timeOK = values[2].(int64)
		if !idOK || indexedSpan != span || !timeOK {
			return bad()
		}
	} else if name == timeIndexName {
		var timeOK bool
		start, timeOK = values[1].(int64)
		indexedTrace, idOK := values[2].(string)
		if !timeOK || !idOK || indexedTrace != trace {
			return bad()
		}
	} else {
		return bad()
	}
	return SpanMatch{Trace: trace, ID: span, Start: start}, nil
}

// scanIntrinsic checks actual post-read bytes: a soft cursor limit misses the
// final oversized entry. Every retry gets fresh output and scan state.
func (b *FDBBackend) scanIntrinsic(ctx context.Context, name string, rng recordlayer.TupleRange, reverse bool, empty bool, visit func(SpanMatch) (bool, error), reset func()) error {
	_, err := b.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		reset()
		b.lastWork = QueryResult{}
		if err := b.readManifest(rc, true); err != nil {
			return nil, err
		}
		store, err := b.open(rc)
		if err != nil {
			return nil, err
		}
		idx := b.md.GetIndex(name)
		if idx == nil {
			return nil, fail("config", "intrinsic index", "index metadata missing")
		}
		indexState, err := store.ReadIndexState(name)
		if err != nil {
			return nil, err
		}
		if !indexState.IsScannable() {
			return nil, fail("ownership", "intrinsic query", "index is not readable")
		}
		if empty {
			return nil, nil
		}
		props := recordlayer.ForwardScan()
		if reverse {
			props = recordlayer.ReverseScan()
		}
		state := recordlayer.NewScanLimiterState()
		props.ExecuteProperties.ScanState = state
		props.ExecuteProperties.ReturnedRowLimit = b.cfg.MaxCandidates + 1
		cur := store.ScanIndex(idx, rng, nil, props)
		defer cur.Close()
		for {
			r, err := cur.OnNext(ctx)
			if err != nil {
				return nil, err
			}
			if !r.HasNext() {
				if r.GetNoNextReason() != recordlayer.SourceExhausted {
					return nil, failf("budget", "intrinsic query", "scan stopped: %v", r.GetNoNextReason())
				}
				return nil, nil
			}
			b.lastWork.Candidates++
			b.lastWork.Bytes = state.BytesScanned()
			if b.lastWork.Candidates > b.cfg.MaxCandidates || b.lastWork.Bytes > b.cfg.MaxBytes {
				return nil, fail("budget", "intrinsic query", "consumed index candidates or logical bytes exceed budget")
			}
			match, err := intrinsicMatch(r.GetValue(), name, b.cfg.Tenant)
			if err != nil {
				return nil, err
			}
			done, err := visit(match)
			if err != nil || done {
				return nil, err
			}
		}
	})
	return err
}

func (b *FDBBackend) Span(ctx context.Context, span string) ([]SpanMatch, error) {
	b.lastWork = QueryResult{}
	out := []SpanMatch{}
	err := b.scanIntrinsic(ctx, spanIndexName, recordlayer.TupleRangeAllOf(tuple.Tuple{int64(b.cfg.Tenant), span}), false, false, func(match SpanMatch) (bool, error) {
		if match.ID != span {
			return false, fail("corruption", "span lookup", "index returned another span identity")
		}
		out = append(out, match)
		return false, nil
	}, func() { out = []SpanMatch{} })
	return out, err
}

func (b *FDBBackend) Time(ctx context.Context, interval TimeRange) (QueryResult, error) {
	b.lastWork = QueryResult{}
	out := QueryResult{Hits: []Hit{}}
	if err := interval.Validate(); err != nil {
		return out, err
	}
	seen := map[string]bool{}
	err := b.scanIntrinsic(ctx, timeIndexName, recordlayer.TupleRangeBetween(tuple.Tuple{int64(b.cfg.Tenant), interval.Start}, tuple.Tuple{int64(b.cfg.Tenant), interval.End}), true, interval.Start == interval.End, func(match SpanMatch) (bool, error) {
		if match.Start < interval.Start || match.Start >= interval.End {
			return false, fail("corruption", "time lookup", "index returned timestamp outside requested range")
		}
		if !seen[match.Trace] {
			seen[match.Trace] = true
			out.Hits = append(out.Hits, Hit{Trace: match.Trace, Latest: match.Start})
		}
		return len(out.Hits) == b.cfg.K, nil
	}, func() { out = QueryResult{Hits: []Hit{}}; seen = map[string]bool{} })
	out.Candidates, out.Bytes = b.lastWork.Candidates, b.lastWork.Bytes
	return out, err
}
