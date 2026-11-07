package otel

import (
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
)

// SameSpanQuery intersects predicates on one (trace_id, span_id), in posting order.
// Predicates[0] drives the scan; put the most selective predicate first.
type SameSpanQuery struct {
	Predicates   []Attribute `json:"predicates"`
	TimeWindow   *TimeRange  `json:"time_window,omitempty"`
	Limit        int         `json:"limit,omitempty"`
	Continuation []byte      `json:"continuation,omitempty"`
}

// SpanQueryPage is one snapshot_page: every row was verified at ReadVersion.
// A full page always carries a continuation; the following page may be empty.
type SpanQueryPage struct {
	Spans        []SpanMatch `json:"spans"`
	Continuation []byte      `json:"continuation,omitempty"`
	ReadVersion  int64       `json:"read_version"`
	Candidates   int         `json:"candidates"`
	Bytes        int64       `json:"logical_read_bytes"`
}

// TraceExistentialQuery allows different span/log/message witnesses within one trace (RFC-283 §6.1).
// Logs use LOG_TRACE correlation; message clauses take neither Limit nor Continuation.
type TraceExistentialQuery struct {
	Clauses        []Attribute            `json:"clauses"`
	LogClauses     []Attribute            `json:"log_clauses,omitempty"`
	MessageClauses []MessageFullTextQuery `json:"message_clauses,omitempty"`
	TimeWindow     *TimeRange             `json:"time_window,omitempty"`
	Limit          int                    `json:"limit,omitempty"`
}

// TraceExistentialHit keeps newest-first witnesses per matching span/log/message clause.
// Latest is the newest timestamp across all witnesses.
type TraceExistentialHit struct {
	Trace            string             `json:"trace_id"`
	Latest           int64              `json:"latest"`
	Witnesses        [][]SpanMatch      `json:"witnesses"`
	LogWitnesses     [][]LogMatch       `json:"log_witnesses,omitempty"`
	MessageWitnesses [][]MessageSpanHit `json:"message_witnesses,omitempty"`
}

type TraceExistentialResult struct {
	Hits        []TraceExistentialHit `json:"hits"`
	ReadVersion int64                 `json:"read_version"`
	Candidates  int                   `json:"candidates"`
	Bytes       int64                 `json:"logical_read_bytes"`
}

// SpanLink is one stored OTLP link; Resolved reports that its target is a
// span stored in the same trace. Other targets stay unresolved, never absent.
type SpanLink struct {
	TraceID    string               `json:"trace_id"`
	SpanID     string               `json:"span_id"`
	TraceState string               `json:"trace_state,omitempty"`
	Flags      uint32               `json:"flags,omitempty"`
	Attributes []*commonpb.KeyValue `json:"attributes,omitempty"`
	Resolved   bool                 `json:"resolved"`
}

// TraceGraph is the observed parent adjacency of one trace. A span whose
// parent is not stored is an orphan, not a root; its ancestry is unknown.
type TraceGraph struct {
	Trace          string                `json:"trace_id"`
	Spans          []SpanMatch           `json:"spans"`
	Roots          []string              `json:"roots"`
	Children       map[string][]string   `json:"children"`
	Parent         map[string]string     `json:"parent"`
	MissingParents []string              `json:"missing_parents"`
	Orphans        []string              `json:"orphans"`
	Cycles         []string              `json:"cycles"`
	Depth          map[string]int        `json:"depth"`
	MaxDepth       int                   `json:"max_depth"`
	Links          map[string][]SpanLink `json:"links"`
}
