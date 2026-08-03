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
	// Positional is the ordinal-model row (a typed PositionalRow, field
	// values indexed by ordinal) — the SOLE runtime row. Every producer emits it
	// (scans/covering scans, projection/map, aggregate output, and join merges
	// via concatLegPositionals), and FieldValue resolution reads it by ordinal,
	// loud on a miss (OrdinalResolutionError). There is no name-keyed row model:
	// runtime name resolution would be first-match and silently wrong on
	// duplicate column names, where the plan-time ordinal is exact.
	Positional *PositionalRow
	Record     *recordlayer.FDBStoredRecord[proto.Message]
	PrimaryKey tuple.Tuple
}

// FromStoredRecord builds a QueryResult from a stored record. The row is the
// ordinal PositionalRow built from the proto message (protoToPositional — one
// slot per descriptor field in declaration order; FieldValue reads it by ordinal).
//
// When the record's store stores row versions, the row is extended with the
// trailing __ROW_VERSION pseudo-slot carrying the record version's 12 bytes
// (nil when the record has no version) — the runtime half of Java's
// PseudoField.fillInIfApplicable (PseudoField.java:72-100), keyed on the same
// two gates: the metadata stores versions, and the descriptor does not define
// a REAL field of that name (real-column-wins).
func FromStoredRecord(rec *recordlayer.FDBStoredRecord[proto.Message]) QueryResult {
	positional := protoToPositional(rec.Record)
	if positional != nil && rec.Store != nil {
		if md := rec.Store.GetRecordMetaData(); md != nil && md.IsStoreRecordVersions() {
			desc := rec.Record.ProtoReflect().Descriptor()
			if desc.Fields().ByName(protoreflect.Name(values.PseudoFieldRowVersion)) == nil {
				var verBytes any
				if rec.Version != nil {
					verBytes = rec.Version.ToBytes()
				}
				positional = &PositionalRow{
					Type:  positionalTypeWithRowVersion(desc),
					Slots: append(positional.Slots, verBytes),
				}
			}
		}
	}
	return QueryResult{
		Positional: positional,
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
// positionalTypeKey distinguishes the plain row shape from the
// __ROW_VERSION-extended one. Both live in ONE cache under ONE counter so the
// bound below actually covers both: a second sync.Map "with the same lifecycle"
// shares nothing — the wipe never reaches it, and descriptor churn in a
// version-storing store grows it forever.
type positionalTypeKey struct {
	desc           protoreflect.MessageDescriptor
	withRowVersion bool
}

var positionalTypeCache sync.Map // positionalTypeKey -> *values.RecordType

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

// protoToPositional builds the ordinal-model PositionalRow from a proto
// message, one slot per descriptor field in declaration order (the field's
// ordinal), with an UPPER-cased field name and an UnknownType placeholder (the
// runtime row carries names and ordinals; slot types are not refined). An
// unset field is a nil slot (SQL NULL). This is the row FieldValue resolution
// reads by ordinal; the test-only protoToMap oracle cross-checks it
// field-for-field (the shadow test in name_oracle_test.go).
//
// A REPEATED field is read before any presence test, so an empty one
// materializes as the empty array and never as NULL — Java's
// MessageHelpers.getFieldOnMessage branches on isRepeated() first for exactly
// this reason. An empty repeated field is byte-identical to an absent one on
// the wire, so emptiness cannot be recovered from presence; it is the TYPE
// that settles it. A non-nullable array is stored FLAT repeated (in both
// engines), and its type forbids NULL, so absent must read back as []. A
// NULLABLE array is stored as the singular NullableArrayWrapper message, where
// presence is real: an absent wrapper is NULL, a present wrapper holding zero
// values is []. Both arms fall out of the repeated-first branch below.
func protoToPositional(msg proto.Message) *PositionalRow {
	if msg == nil {
		return nil
	}
	refl := msg.ProtoReflect()
	desc := refl.Descriptor()
	fields := desc.Fields()
	n := fields.Len()
	rt := PositionalTypeForDescriptor(desc)
	slots := make([]any, n)
	for i := 0; i < n; i++ {
		fd := fields.Get(i)
		if fd.IsList() || fd.IsMap() || refl.Has(fd) {
			slots[i] = protoFieldToGo(fd, refl.Get(fd))
		}
	}
	return &PositionalRow{Type: rt, Slots: slots}
}

// PositionalTypeForDescriptor returns the LOGICAL RecordType for a message
// descriptor — one field per descriptor field in declaration order (the
// field's ordinal), UPPER-cased name, UnknownType placeholder. THE single authority
// for a stored record's logical row shape: every physical access path that
// serves a stored record's columns (base scan rows via protoToPositional,
// covering-index rows via coveringIndexCursor) MUST shape its rows by this
// type, so a plan-time LOGICAL ordinal reads the same slot on every path
// (mirrors Java's IndexKeyValueToPartialRecord, which reconstructs a
// descriptor-shaped partial record for exactly this reason). Cached per
// descriptor (see positionalTypeCache).
func PositionalTypeForDescriptor(desc protoreflect.MessageDescriptor) *values.RecordType {
	return cachedPositionalType(positionalTypeKey{desc: desc}, func() *values.RecordType {
		fields := desc.Fields()
		n := fields.Len()
		rtFields := make([]values.Field, n)
		for i := 0; i < n; i++ {
			rtFields[i] = values.Field{Name: strings.ToUpper(string(fields.Get(i).Name())), FieldType: values.UnknownType, Ordinal: i}
		}
		return values.NewRecordType("", false, rtFields)
	})
}

// cachedPositionalType is the SINGLE miss path for every cached row shape: one
// lock, one counter, one wipe. Hits stay lock-free on the sync.Map; misses are
// first-sight-of-a-key rare, so the lock is off the hot path.
//
// Every shape must come through here. A separate map "with the same lifecycle"
// shares nothing in practice — the wipe below cannot reach it, and under
// descriptor churn (dynamicpb across repeated schema loads in a long-lived
// multi-tenant process) it grows without bound.
func cachedPositionalType(key positionalTypeKey, build func() *values.RecordType) *values.RecordType {
	if v, ok := positionalTypeCache.Load(key); ok {
		return v.(*values.RecordType)
	}
	positionalTypeCacheMu.Lock()
	defer positionalTypeCacheMu.Unlock()
	// Re-check under the lock: a racing miss on the same key may have stored
	// while we waited.
	if v, ok := positionalTypeCache.Load(key); ok {
		return v.(*values.RecordType)
	}
	rt := build()
	if positionalTypeCacheSize.Add(1) > positionalTypeCacheCap {
		// Descriptor churn must not grow the cache without bound: wipe and
		// re-warm. Under the miss lock the bound is EXACT — no racing store
		// can land after the reset (a lock-free wipe let concurrent misses
		// transiently overshoot the cap).
		positionalTypeCache.Range(func(k, _ any) bool {
			positionalTypeCache.Delete(k)
			return true
		})
		positionalTypeCacheSize.Store(1)
	}
	positionalTypeCache.Store(key, rt)
	return rt
}

// positionalTypeWithRowVersion is PositionalTypeForDescriptor plus the
// trailing __ROW_VERSION pseudo-slot — the runtime row shape for a record
// type in a version-storing store (Java: RecordMetaData.getPlannerType →
// addPseudoFields, RecordMetaData.java:732-739). Callers must have checked
// that the descriptor does not define a REAL field of that name.
func positionalTypeWithRowVersion(desc protoreflect.MessageDescriptor) *values.RecordType {
	// The base type is resolved BEFORE cachedPositionalType is entered: the
	// miss lock is not reentrant, so a build closure must never call back into
	// the cache.
	base := PositionalTypeForDescriptor(desc)
	return cachedPositionalType(positionalTypeKey{desc: desc, withRowVersion: true}, func() *values.RecordType {
		fields := make([]values.Field, 0, len(base.Fields)+1)
		fields = append(fields, base.Fields...)
		fields = append(fields, values.Field{
			Name:      values.PseudoFieldRowVersion,
			FieldType: values.NullableVersion,
			Ordinal:   len(base.Fields),
		})
		return values.NewRecordType("", false, fields)
	})
}

// PositionalTypeForRecordLayout is the metadata-aware row-shape authority:
// the descriptor's positional type, extended with the __ROW_VERSION
// pseudo-slot when the store's metadata stores row versions and the
// descriptor does not declare a REAL field of that name (real-column-wins,
// Java Type.Record.addPseudoFields, Type.java:2358-2368). Every planner- or
// runtime-facing derivation of a stored record's row layout must go through
// this so plan-time ordinals and runtime slots agree on version-storing
// stores.
func PositionalTypeForRecordLayout(desc protoreflect.MessageDescriptor, storeRowVersions bool) *values.RecordType {
	if !storeRowVersions || desc.Fields().ByName(protoreflect.Name(values.PseudoFieldRowVersion)) != nil {
		return PositionalTypeForDescriptor(desc)
	}
	return positionalTypeWithRowVersion(desc)
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
