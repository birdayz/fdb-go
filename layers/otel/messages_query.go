package otel

// The message full-text query API; messages_query_rel.go serves it from
// MESSAGES_TEXT, MESSAGE_TRACES markers and the spans' ref_chunks.

const (
	MessageMatchAll    = "all"
	MessageMatchAny    = "any"
	MessageMatchPhrase = "phrase"
)

// MessageFullTextQuery matches message references: all (default), any, or consecutive-position phrase.
// Role/attribute/span predicates filter references; FirstTurnOnly selects the introducing span, independent of arrival.
type MessageFullTextQuery struct {
	Query         string      `json:"query"`
	Mode          string      `json:"mode,omitempty"`
	AttributeKey  string      `json:"attribute_key,omitempty"`
	Role          string      `json:"role,omitempty"`
	Predicates    []Attribute `json:"predicates,omitempty"`
	TimeWindow    *TimeRange  `json:"time_window,omitempty"`
	FirstTurnOnly bool        `json:"first_turn_only,omitempty"`
	Limit         int         `json:"limit,omitempty"`
	Continuation  []byte      `json:"continuation,omitempty"`
}

// MessageSpanHit identifies a message ("writer.seq") reference with 0-based manifest TurnOrdinals.
type MessageSpanHit struct {
	Trace        string `json:"trace_id"`
	Span         string `json:"span_id"`
	Start        int64  `json:"start_ns"`
	AttributeKey string `json:"attribute_key"`
	Role         string `json:"role,omitempty"`
	MessageID    string `json:"message_id"`
	TurnOrdinals []int  `json:"turn_ordinals"`
}

// MessageFullTextPage is one snapshot page of hits in sort-key order (start,
// trace and span descending, D3, then attribute key and writer/sequence
// message ID). A full page carries a continuation.
type MessageFullTextPage struct {
	Hits         []MessageSpanHit `json:"hits"`
	Continuation []byte           `json:"continuation,omitempty"`
	ReadVersion  int64            `json:"read_version"`
	Candidates   int              `json:"candidates"`
	Bytes        int64            `json:"logical_read_bytes"`
}

// messageRefReads bounds the range reads in flight at once.
const messageRefReads = 256
