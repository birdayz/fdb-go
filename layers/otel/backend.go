package otel

// Benchmark backend interfaces and the result types every backend shares.

import "context"

type Size struct {
	Keys          int64 `json:"keys"`
	KeyBytes      int64 `json:"key_bytes"`
	ValueBytes    int64 `json:"value_bytes"`
	PhysicalBytes int64 `json:"physical_bytes,omitempty"`
	// PrefixBytes is each key less its common prefix with the previous key in
	// key order, plus the value: a proxy for an engine's key prefix
	// compression (Redwood), not a measured on-disk size.
	PrefixBytes int64 `json:"prefix_bytes,omitempty"`
}

// AddKV counts one key-value pair read in key order after prev.
func (s *Size) AddKV(prev, key, value []byte) {
	s.Keys++
	s.KeyBytes += int64(len(key))
	s.ValueBytes += int64(len(value))
	n := 0
	for n < len(prev) && n < len(key) && prev[n] == key[n] {
		n++
	}
	s.PrefixBytes += int64(len(key)-n) + int64(len(value))
}

type Statistics struct {
	Rows                int64             `json:"rows"`
	Attributes          int64             `json:"attributes"`
	LogicalPayloadBytes int64             `json:"logical_payload_bytes"`
	Structures          map[string]Size   `json:"structures"`
	Details             map[string]string `json:"details,omitempty"`
}

type Backend interface {
	Ingest(context.Context) (Statistics, error)
	Query(context.Context, Attribute) (QueryResult, error)
	Trace(context.Context, string) ([]SpanHit, error)
	Audit(context.Context) (Statistics, error)
	Drop(context.Context) error
	Close()
}

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
