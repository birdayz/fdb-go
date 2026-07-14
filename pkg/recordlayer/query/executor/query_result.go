package executor

import (
	"strings"
	"sync"
	"sync/atomic"

	"google.golang.org/protobuf/proto"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/reflect/protoreflect"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// uuidProtoMessageName is the fully-qualified tuple_fields.UUID message that
// fdb-relational stores UUID column values in. (Canonical copy lives in
// pkg/relational/core/functions/proto_value.go; duplicated here because the
// record-layer executor cannot depend on the relational layer.)
const uuidProtoMessageName = "com.apple.foundationdb.record.UUID"

// QueryResult is the row type flowing through plan execution cursors.
// Wraps the ordinal-model row (Positional), an optional stored record
// (when the row originated from a scan), and an optional primary key.
// Mirrors Java's QueryResult.
type QueryResult struct {
	// Positional is the RFC-173 ordinal-model row (a typed PositionalRow, field
	// values indexed by ordinal) — post-cap the SOLE runtime row. Every producer
	// emits it (scans/covering scans, projection/map, aggregate output, and join
	// merges via concatLegPositionals), and FieldValue resolution reads it by
	// ordinal, loud on a miss (OrdinalResolutionError). The name-keyed Datum row
	// model it replaced is retired.
	Positional *PositionalRow
	Record     *recordlayer.FDBStoredRecord[proto.Message]
	PrimaryKey tuple.Tuple
	// Complete marks a computed/synthetic row whose column set is authoritative —
	// every legal column is present (nil-valued for SQL NULL), with no proto-style
	// optional-field omissions. Set by aggregate output (finalizeGroup/
	// emptyScalarResult), projection output (executeProjection), and the unnest
	// FlatMap's RC-evaluated rows; left false by raw stored records
	// (FromStoredRecord, which legitimately omit unset optionals) and join merges.
	//
	// VESTIGIAL post-cap: its two former consumers — the RFC-048 W1 strict
	// unresolved-reference check and the name-model merge's alias fabrication
	// (qualifyAlias) — are both retired, so nothing reads Complete today. It is
	// still round-tripped through the sort continuation (encodeSortContinuation's
	// versioned payload). Pending removal with the Slice-4 name-model demolition;
	// dropping it is a continuation-payload format change, so it is not done here.
	Complete bool
}

// FromStoredRecord builds a QueryResult from a stored record. The row is the
// ordinal PositionalRow built from the proto message (protoToPositional — one
// slot per descriptor field in declaration order; FieldValue reads it by ordinal).
func FromStoredRecord(rec *recordlayer.FDBStoredRecord[proto.Message]) QueryResult {
	return QueryResult{
		Positional: protoToPositional(rec.Record),
		Record:     rec,
		PrimaryKey: rec.PrimaryKey,
	}
}

// positionalTypeCache caches the row-invariant PositionalRow.Type per message
// descriptor. The RecordType depends only on the descriptor (field names in
// declaration order), never the row, and rebuilding it per scanned row made
// protoToPositional cost more than the sparse protoToMap itself
// (BenchmarkProtoToPositional_Order). The cached type is shared across rows and
// goroutines — read-only after construction (FieldIndex / shadow reads only).
// Keyed by the descriptor: a per-message-type singleton for generated code,
// and per-schema-load for dynamicpb — a long-lived multi-tenant process that
// keeps loading schemas mints fresh descriptor instances forever, so the cache
// is BOUNDED (wipe-at-cap in protoToPositional): entries are pure derivations,
// a rare wipe just re-warms. A racy duplicate Store is harmless: both values
// are structurally equal.
var positionalTypeCache sync.Map // protoreflect.MessageDescriptor -> *values.RecordType

// positionalTypeCacheMu serializes the MISS path (build + wipe-at-cap +
// store) so the bound is exact: without it, misses racing a wipe re-stored
// after the counter reset and the map transiently overshot the cap (review
// finding). Cache HITS stay lock-free on the sync.Map — misses are
// first-sight-of-a-descriptor rare, so the lock is off the hot path.
var positionalTypeCacheMu sync.Mutex

// positionalTypeCacheSize is the entry count, mutated only under
// positionalTypeCacheMu.
var positionalTypeCacheSize atomic.Int64

// positionalTypeCacheCap bounds the cache. Far above any real live-schema
// population, so a wipe only fires under descriptor churn — the
// unbounded-growth class it exists to stop.
const positionalTypeCacheCap = 4096

// protoToPositional is the RFC-173 ordinal-model counterpart of protoToMap: it
// builds a PositionalRow from a proto message, one slot per descriptor field in
// declaration order (the field's ordinal), with an UPPER-cased field name and a
// dark UnknownType (type refinement comes with the later slices). An unset field
// is a nil slot — matching protoToMap omitting the key (SQL NULL) — so the
// positional row and the map agree field-for-field (pinned by the shadow test).
// Since Slice 1 this row is what the non-join frontier RESOLVES against; the
// name-keyed map is still emitted for coexistence (retired in Slice 4).
func protoToPositional(msg proto.Message) *PositionalRow {
	if msg == nil {
		return nil
	}
	refl := msg.ProtoReflect()
	desc := refl.Descriptor()
	fields := desc.Fields()
	n := fields.Len()
	var rt *values.RecordType
	if v, ok := positionalTypeCache.Load(desc); ok {
		rt = v.(*values.RecordType)
	} else {
		positionalTypeCacheMu.Lock()
		// Re-check under the lock: a racing miss on the same descriptor may
		// have stored while we waited.
		if v, ok := positionalTypeCache.Load(desc); ok {
			rt = v.(*values.RecordType)
		} else {
			rtFields := make([]values.Field, n)
			for i := 0; i < n; i++ {
				rtFields[i] = values.Field{Name: strings.ToUpper(string(fields.Get(i).Name())), FieldType: values.UnknownType, Ordinal: i}
			}
			rt = values.NewRecordType("", false, rtFields)
			if positionalTypeCacheSize.Add(1) > positionalTypeCacheCap {
				// Descriptor churn (dynamicpb across many schema loads) must
				// not grow the cache without bound: wipe and re-warm. Under
				// the miss lock the bound is EXACT — no racing store can
				// land after the reset (review finding: the lock-free wipe
				// let concurrent misses transiently overshoot the cap).
				positionalTypeCache.Range(func(k, _ any) bool {
					positionalTypeCache.Delete(k)
					return true
				})
				positionalTypeCacheSize.Store(1)
			}
			positionalTypeCache.Store(desc, rt)
		}
		positionalTypeCacheMu.Unlock()
	}
	slots := make([]any, n)
	for i := 0; i < n; i++ {
		fd := fields.Get(i)
		if refl.Has(fd) {
			slots[i] = protoFieldToGo(fd, refl.Get(fd))
		}
	}
	return &PositionalRow{Type: rt, Slots: slots}
}

// protoToMap converts a proto.Message to map[string]any with
// UPPER-case keys. Only set fields are included; unset fields are
// omitted (NULL semantics — FieldValue.Evaluate returns nil for
// missing keys).
//
// An EMPTY repeated field is omitted too (protoreflect's Has()
// reports false for an empty list) and so reads back as SQL NULL.
// Go writes a plain repeated field with no nullable-array wrapper,
// so a NULL array and an empty array are wire-indistinguishable;
// both materialize as NULL here. CARDINALITY of such a column is
// therefore NULL, not 0, for an empty/unset array — an instance of
// the RFC-143 §3a nullable-array-wrapper divergence (Java's wrapper
// distinguishes the two), out of scope for the Phase 1 function. The
// function itself is correct: nil array → nil, populated array →
// len.
func protoToMap(msg proto.Message) map[string]any {
	if msg == nil {
		return nil
	}
	refl := msg.ProtoReflect()
	desc := refl.Descriptor()
	fields := desc.Fields()
	m := make(map[string]any, fields.Len())
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if !refl.Has(fd) {
			continue
		}
		key := strings.ToUpper(string(fd.Name()))
		m[key] = protoFieldToGo(fd, refl.Get(fd))
	}
	return m
}

// protoFieldToGo converts a protoreflect.Value to a native Go value suitable
// for Value.Evaluate consumption. Delegates to values.ProtoFieldToRowValue —
// the SINGLE conversion shared with the cascades values layer's struct
// descent, so the record→row materialization here and the fused-path descent
// there cannot drift (a UUID leaf surfaces as the neutral [16]byte in both,
// nested messages stay raw, repeated fields become []any). See that function
// for why UUID is [16]byte, not a string.
func protoFieldToGo(fd protoreflect.FieldDescriptor, v protoreflect.Value) any {
	return values.ProtoFieldToRowValue(fd, v)
}

// scalarProtoToGo delegates to the single kind-based scalar converter in the
// values layer (no drift between record→row and struct descent). Kept as a
// named executor helper for its exhaustive per-kind tests.
func scalarProtoToGo(kind protoreflect.Kind, v protoreflect.Value) any {
	return values.ProtoScalarKindToRowValue(kind, v)
}
