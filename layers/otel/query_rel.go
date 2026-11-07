package otel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"strconv"

	"fdb.dev/layers/otel/otelsql"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Span reads on a tenant store (rfcs/relational-exporter-migration.md §3.2):
// each is one transaction over a standard OTEL_V1 index, newest first where
// it is a top-K, charged against the query budget and never truncated.

// OTEL_V1 span indexes and the key columns the readers use.
const (
	spansTimeIndex       = "SPANS_TIME"          // start_ns, trace_id, span_id
	spansSpanIDIndex     = "SPANS_SPAN_ID"       // span_id, start_ns, trace_id
	spansStrAttrIndex    = "SPANS_STR_ATTR"      // scope, key, value, start_ns, trace_id, span_id
	spansLongStrIndex    = "SPANS_LONG_STR_ATTR" // scope, key, digest, start_ns, trace_id, span_id
	spansIntAttrIndex    = "SPANS_INT_ATTR"
	spansDoubleAttrIndex = "SPANS_DOUBLE_ATTR"
	spansBoolAttrIndex   = "SPANS_BOOL_ATTR"
	spansRecordType      = "SPANS"
)

// SpanReader serves Query, Time, Span and Trace from a tenant store. The
// Exporter reads through it, and so does otelbench's sql backend with
// --sql-query direct, so benchmarks measure the production read path. Its
// methods are safe for concurrent use.
type SpanReader struct {
	store *tenantStore
	cfg   Config
}

// NewSpanReader reads ts on db within cfg's K and budgets.
func NewSpanReader(db *recordlayer.FDBDatabase, ts *otelsql.TenantStore, cfg Config) *SpanReader {
	return &SpanReader{store: &tenantStore{db: db, ts: ts}, cfg: cfg}
}

// attrQuery is the index scan an attribute equality reads, or none when no
// stored value can equal the attribute.
type attrQuery struct {
	index string
	value any
	// long is the verbatim value of a string over the indexed bound: the
	// index holds its digest, so every candidate's record is checked.
	long *string
}

// spanAttrQuery maps a typed attribute (D1) to its index scan.
func spanAttrQuery(a Attribute) (attrQuery, bool) {
	switch a.Type {
	case "string":
		if len(a.Value) <= otelsql.MaxIndexedStringBytes {
			return attrQuery{index: spansStrAttrIndex, value: a.Value}, true
		}
		sum := sha256.Sum256([]byte(a.Value))
		return attrQuery{index: spansLongStrIndex, value: sum[:otelsql.LongStringDigestBytes], long: &a.Value}, true
	case "int":
		n, err := strconv.ParseInt(a.Value, 10, 64)
		if err != nil {
			return attrQuery{}, false
		}
		return attrQuery{index: spansIntAttrIndex, value: n}, true
	case "double":
		f, err := parseCanonicalDouble(a.Value)
		if err != nil {
			return attrQuery{}, false
		}
		return attrQuery{index: spansDoubleAttrIndex, value: f}, true
	case "bool":
		b, err := strconv.ParseBool(a.Value)
		if err != nil {
			return attrQuery{}, false
		}
		return attrQuery{index: spansBoolAttrIndex, value: b}, true
	}
	return attrQuery{}, false
}

// parseCanonicalDouble reads formatDouble's text.
func parseCanonicalDouble(s string) (float64, error) {
	switch s {
	case "NaN":
		return math.NaN(), nil
	case "+Inf":
		return math.Inf(1), nil
	case "-Inf":
		return math.Inf(-1), nil
	}
	return strconv.ParseFloat(s, 64)
}

// Query returns the newest K distinct traces with a span carrying a.
func (r *SpanReader) Query(ctx context.Context, a Attribute) (QueryResult, error) {
	q, ok := spanAttrQuery(a)
	if !ok || len(a.Key) > otelsql.MaxIndexedStringBytes {
		return QueryResult{Hits: []Hit{}}, nil
	}
	rng := recordlayer.TupleRangeAllOf(tuple.Tuple{a.Scope, a.Key, q.value})
	if q.long == nil {
		return r.top(ctx, "query", q.index, rng, 3, 4, nil)
	}
	return r.top(ctx, "query", q.index, rng, 3, 4, func(store *recordlayer.FDBRecordStore, e *recordlayer.IndexEntry, work *queryWork) (bool, error) {
		return r.holdsLongString(store, e, work, a.Scope, a.Key, *q.long)
	})
}

// Time returns the newest K distinct traces with a span starting in tr.
func (r *SpanReader) Time(ctx context.Context, tr TimeRange) (QueryResult, error) {
	if err := tr.Validate(); err != nil {
		return QueryResult{Hits: []Hit{}}, err
	}
	if tr.Start == tr.End {
		return QueryResult{Hits: []Hit{}}, nil
	}
	return r.top(ctx, "time", spansTimeIndex, recordlayer.TupleRangeBetween(tuple.Tuple{tr.Start}, tuple.Tuple{tr.End}), 0, 1, nil)
}

// top reads index newest first and keeps each trace's first entry (its
// latest matching start) until K distinct traces: a reverse scan yields (start
// desc, trace desc), so the first K distinct traces are the top K by (latest
// desc, trace desc). verify, when set, admits a candidate.
func (r *SpanReader) top(ctx context.Context, op, index string, rng recordlayer.TupleRange, startCol, traceCol int, verify func(*recordlayer.FDBRecordStore, *recordlayer.IndexEntry, *queryWork) (bool, error)) (QueryResult, error) {
	var out QueryResult
	err := r.store.read(ctx, op, func(store *recordlayer.FDBRecordStore) error {
		out = QueryResult{Hits: []Hit{}}
		work := newQueryWork(r.cfg)
		defer work.result(&out)
		seen := map[string]struct{}{}
		return scanIndex(ctx, store, op, index, rng, true, work, func(e *recordlayer.IndexEntry) (bool, error) {
			start, err := entryInt64(op, e, startCol)
			if err != nil {
				return false, err
			}
			trace, err := entryBytes(op, e, traceCol)
			if err != nil {
				return false, err
			}
			if _, dup := seen[string(trace)]; dup {
				return false, nil
			}
			if verify != nil {
				ok, err := verify(store, e, work)
				if err != nil || !ok {
					return false, err
				}
			}
			seen[string(trace)] = struct{}{}
			out.Hits = append(out.Hits, Hit{Trace: hex.EncodeToString(trace), Latest: start})
			return len(out.Hits) == r.cfg.K, nil
		})
	})
	return out, err
}

// holdsLongString loads the span of a long-string index candidate and reports
// whether it holds (scope, key, value): a digest match alone may collide.
func (r *SpanReader) holdsLongString(store *recordlayer.FDBRecordStore, e *recordlayer.IndexEntry, work *queryWork, scope, key, value string) (bool, error) {
	held := false
	err := loadRecords(store, "query", []tuple.Tuple{e.PrimaryKey()}, work, func(_ int, rec *recordlayer.FDBStoredRecord[proto.Message]) error {
		if rec == nil {
			return failf("corruption", "query", "%s entry has no span record", spansLongStrIndex)
		}
		m := rec.Record.ProtoReflect()
		fd := m.Descriptor().Fields().ByName("LONG_STR_ATTRS")
		if fd == nil {
			return failf("ownership", "query", "SPANS has no LONG_STR_ATTRS")
		}
		held = longStringsHold(m.Get(fd).Message(), scope, key, value)
		return nil
	})
	return held, err
}

// longStringsHold reports whether a LONG_STR_ATTRS array holds (scope, key, value).
func longStringsHold(arr protoreflect.Message, scope, key, value string) bool {
	values := arr.Descriptor().Fields().ByName("values")
	if values == nil {
		return false
	}
	list := arr.Get(values).List()
	for i := 0; i < list.Len(); i++ {
		el := list.Get(i).Message()
		f := el.Descriptor().Fields()
		if el.Get(f.ByName("SCOPE")).String() == scope && el.Get(f.ByName("ATTR_KEY")).String() == key &&
			el.Get(f.ByName("STR_VALUE")).String() == value {
			return true
		}
	}
	return false
}

// Span returns every span with the identity, in SPANS_SPAN_ID order (start,
// trace ascending), and the work it read.
func (r *SpanReader) Span(ctx context.Context, span string) ([]SpanMatch, QueryResult, error) {
	out := []SpanMatch{}
	var work QueryResult
	id, ok := lowerHexID(span, 8)
	if !ok {
		return out, work, nil
	}
	err := r.store.read(ctx, "span", func(store *recordlayer.FDBRecordStore) error {
		out, work = []SpanMatch{}, QueryResult{}
		w := newQueryWork(r.cfg)
		defer w.result(&work)
		return scanIndex(ctx, store, "span", spansSpanIDIndex, recordlayer.TupleRangeAllOf(tuple.Tuple{id}), false, w, func(e *recordlayer.IndexEntry) (bool, error) {
			got, err := entryBytes("span", e, 0)
			if err != nil {
				return false, err
			}
			if !bytes.Equal(got, id) {
				return false, fail("corruption", "span", "index returned another span identity")
			}
			start, err := entryInt64("span", e, 1)
			if err != nil {
				return false, err
			}
			trace, err := entryBytes("span", e, 2)
			if err != nil {
				return false, err
			}
			out = append(out, SpanMatch{Trace: hex.EncodeToString(trace), ID: span, Start: start})
			return false, nil
		})
	})
	return out, work, err
}

// Trace returns every span of the trace, ordered (start, span) ascending, and
// the work it read.
func (r *SpanReader) Trace(ctx context.Context, trace string) ([]SpanHit, QueryResult, error) {
	out := []SpanHit{}
	var work QueryResult
	id, ok := lowerHexID(trace, 16)
	if !ok {
		return out, work, nil
	}
	err := r.store.read(ctx, "trace", func(store *recordlayer.FDBRecordStore) error {
		out, work = []SpanHit{}, QueryResult{}
		w := newQueryWork(r.cfg)
		defer w.result(&work)
		return scanTraceSpans(ctx, store, "trace", id, w, func(m protoreflect.Message, f spanFields) error {
			out = append(out, SpanHit{ID: hex.EncodeToString(m.Get(f.spanID).Bytes()), Start: m.Get(f.start).Int()})
			return nil
		})
	})
	sortSpanHits(out)
	return out, work, err
}

// spanFields are the SPANS columns the trace readers decode.
type spanFields struct {
	spanID, start, parent, otlp protoreflect.FieldDescriptor
}

// scanTraceSpans visits the trace's SPANS records in primary-key order.
func scanTraceSpans(ctx context.Context, store *recordlayer.FDBRecordStore, op string, trace []byte, work *queryWork, visit func(protoreflect.Message, spanFields) error) error {
	rt := store.GetRecordMetaData().GetRecordType(spansRecordType)
	if rt == nil {
		return failf("ownership", op, "%s is not in the store's metadata", spansRecordType)
	}
	fields := rt.Descriptor.Fields()
	f := spanFields{spanID: fields.ByName("SPAN_ID"), start: fields.ByName("START_NS"), parent: fields.ByName("PARENT_SPAN_ID"), otlp: fields.ByName("OTLP")}
	if f.spanID == nil || f.start == nil || f.parent == nil || f.otlp == nil {
		return failf("ownership", op, "%s lacks SPAN_ID, START_NS, PARENT_SPAN_ID or OTLP", spansRecordType)
	}
	prefix := tuple.Tuple{trace}
	if rt.PrimaryKeyHasRecordTypePrefix() {
		// A relational table's primary key leads with its record type key.
		prefix = tuple.Tuple{rt.GetRecordTypeKey(), trace}
	}
	props := recordlayer.ForwardScan()
	props.ExecuteProperties.ReturnedRowLimit = work.maxCandidates - work.candidates + 1
	cur := store.ScanRecordsInRange(prefix, prefix, recordlayer.EndpointTypeRangeInclusive, recordlayer.EndpointTypeRangeInclusive, nil, props)
	defer cur.Close()
	for {
		res, err := cur.OnNext(ctx)
		if err != nil {
			return err
		}
		if !res.HasNext() {
			if reason := res.GetNoNextReason(); reason != recordlayer.SourceExhausted {
				return failf("budget", op, "trace scan stopped: %v", reason)
			}
			return nil
		}
		rec := res.GetValue()
		if err := work.charge(op, int64(rec.KeySize+rec.ValueSize)); err != nil {
			return err
		}
		if err := visit(rec.Record.ProtoReflect(), f); err != nil {
			return err
		}
	}
}

// lowerHexID decodes a lowercase hex identity of n bytes; any other text names
// no stored span, as the stored identities are lowercase hex.
func lowerHexID(s string, n int) ([]byte, bool) {
	if len(s) != 2*n {
		return nil, false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return nil, false
		}
	}
	id, err := hex.DecodeString(s)
	return id, err == nil
}

// entryInt64 and entryBytes read an index key column; any other type is a
// corrupt entry.
func entryInt64(op string, e *recordlayer.IndexEntry, i int) (int64, error) {
	if i < len(e.Key) {
		if v, ok := e.Key[i].(int64); ok {
			return v, nil
		}
	}
	return 0, failf("corruption", op, "%s entry column %d is not an integer", e.Index.Name, i)
}

func entryBytes(op string, e *recordlayer.IndexEntry, i int) ([]byte, error) {
	if i < len(e.Key) {
		if v, ok := e.Key[i].([]byte); ok {
			return v, nil
		}
	}
	return nil, failf("corruption", op, "%s entry column %d is not bytes", e.Index.Name, i)
}
