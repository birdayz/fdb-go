package values

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/fnv"
	"sync/atomic"
)

// ExactTypeHandle is an immutable read view of a checked type snapshot.
// Type returns a fresh ordinary graph and CanonicalBytes returns a defensive
// copy, so no caller can mutate the identity held by a QOV or memo boundary.
type ExactTypeHandle interface {
	Type() Type
	CanonicalBytes() []byte
	RelationInner() (ExactTypeHandle, bool)
	isExactTypeHandleView()
}

type exactField struct {
	name    string
	ordinal int
	typ     *exactType
}

type exactType struct {
	code       TypeCode
	nullable   bool
	anyRecord  bool
	name       string
	fields     []exactField
	element    *exactType
	enumValues []EnumValue
	canonical  []byte
	hash       uint64
	// internHashValue buckets this node in the intern table. It is NOT the
	// canonical hash above: the intern probe has to be computable BEFORE the
	// node exists, so it folds the source shape plus the children's intern
	// hashes rather than a canonical encoding nothing has built yet. It also
	// folds the record/enum NAME, which canonical identity excludes.
	internHashValue uint64

	// protoShape memoizes "can this protobuf descriptor read the shape I
	// captured" — see protoRecordShapeCompatible, which is the answer being
	// cached and is a pure function of (descriptor identity, this handle).
	//
	// IT IS A MEMO, NOT STATE, and the distinction is what keeps this off the
	// immutability contract in the type doc above: every field below the line
	// is fixed at construction, and this one holds a derivation of them that
	// cannot come out differently. Clearing it would cost time and change
	// nothing.
	//
	// A SINGLE ENTRY IS THE RIGHT SIZE because of who asks. The reader is
	// readProtoOrdinal, once per column read per ROW, and every row of one scan
	// presents the same descriptor — so a one-entry cache is at ~100% after the
	// first row, while the walk it replaces is O(fields) per column and
	// therefore O(fields²) per row. That is what it cost: on the 1M-row stress
	// suite the widest scan ran 1.97× master, and every query in that suite
	// regressed in proportion to the rows it touched while the point lookups
	// got faster.
	//
	// Lock-free rather than mutex-guarded because a stale read is harmless: two
	// goroutines racing on different descriptors recompute and overwrite, and
	// each stores a verdict that is correct for the descriptor it names. The
	// pointer is swapped whole, so a reader never sees a descriptor paired with
	// another descriptor's answer.
	protoShape atomic.Pointer[protoShapeVerdict]

	// thawCache holds the thawed ordinary Type graph for SHARED readers.
	//
	// thaw() builds a fresh graph on every call, and that is the right default
	// for the public handle: ExactTypeHandle.Type promises a graph no caller can
	// use to mutate the identity a QOV or memo boundary holds. But the row path
	// does not want a private graph — it wants to ANSWER A QUESTION about the
	// type (does this row's shape equal the carrier's, is this scalar nullable),
	// and it asked once per row. On a 20k-row scan that was ~4M allocations
	// spent rebuilding a graph that is a pure function of an immutable handle.
	//
	// Shared readers go through thawShared and must treat the result as
	// READ-ONLY; the fresh-graph promise stays intact for Type().
	thawCache atomic.Pointer[thawedExactType]
}

// thawedExactType boxes a thawed graph so it can be published atomically.
type thawedExactType struct{ typ Type }

// thawShared returns the thawed graph WITHOUT copying it. Callers must not
// mutate the result — see exactType.thawCache for the split between this and
// the defensive Type().
func (e *exactType) thawShared() Type {
	if e == nil {
		return nil
	}
	if cached := e.thawCache.Load(); cached != nil {
		return cached.typ
	}
	thawed := e.thaw()
	e.thawCache.Store(&thawedExactType{typ: thawed})
	return thawed
}

func (*exactType) isExactTypeHandleView() {}

func (e *exactType) Type() Type {
	if e == nil {
		return nil
	}
	return e.thaw()
}

func (e *exactType) CanonicalBytes() []byte {
	if e == nil {
		return nil
	}
	return append([]byte(nil), e.canonical...)
}

func (e *exactType) RelationInner() (ExactTypeHandle, bool) {
	if e == nil || e.code != TypeCodeRelation || e.element == nil {
		return nil, false
	}
	return e.element, true
}

func (e *exactType) thaw() Type {
	switch e.code {
	case TypeCodeRecord:
		if e.anyRecord {
			return anyRecordType{nullable: e.nullable}
		}
		fields := make([]Field, len(e.fields))
		for i, field := range e.fields {
			fields[i] = Field{
				Name:      field.name,
				Ordinal:   field.ordinal,
				FieldType: field.typ.thaw(),
			}
		}
		// Do not route through NewRecordType: exact snapshots intentionally
		// admit duplicate names because ordinal access remains unambiguous.
		return &RecordType{RecordName: e.name, Nullable: e.nullable, Fields: fields}
	case TypeCodeArray:
		return &ArrayType{Nullable: e.nullable, ElementType: e.element.thaw()}
	case TypeCodeEnum:
		return &EnumType{
			EnumName: e.name,
			Nullable: e.nullable,
			Values:   append([]EnumValue(nil), e.enumValues...),
		}
	case TypeCodeRelation:
		return &RelationType{InnerType: e.element.thaw()}
	default:
		return &PrimitiveType{TypeCode: e.code, Nullable: e.nullable}
	}
}

// SnapshotExactType checks and freezes an ordinary Type graph.
func SnapshotExactType(typ Type) (ExactTypeHandle, error) {
	exact, err := snapshotExactType(typ, nil)
	if err != nil {
		return nil, prefixExactPath(err, "type")
	}
	return exact, nil
}

// ExactRelationOf snapshots object and wraps it in exactly one RELATION layer.
func ExactRelationOf(object Type) (ExactTypeHandle, error) {
	handle, err := SnapshotExactType(object)
	if err != nil {
		return nil, err
	}
	inner := handle.(*exactType)
	return internedRelationExactType(inner), nil
}

// AsExactTypeHandle exact-recognizes the package-owned immutable handle. An
// embedded interface or a nil embedded view is not admitted.
func AsExactTypeHandle(value any) (ExactTypeHandle, bool) {
	exact, ok := value.(*exactType)
	if !ok || exact == nil {
		return nil, false
	}
	return exact, true
}

// prefixExactPath prepends one path segment to a resolution error as the
// snapshot recursion unwinds.
//
// The path is built HERE, on the error branch, instead of being threaded down
// as a string the recursion concatenates at every node. Every node used to pay
// `path+".field["+uitoa(i)+"]"` — three allocations per record field, per
// snapshot — to describe a location that is read only when something fails, and
// almost nothing fails. Measured on the stats planner sweep, snapshotExactType
// was 14.7% of samples and runtime.concatstring4 was 16.2% of that, feeding a
// GC that was 44% of the run.
//
// Unwinding produces the identical string: the leaf raises with an empty path,
// each frame prepends its own segment, and SnapshotExactType prepends "type".
func prefixExactPath(err error, segment string) error {
	var resolution *ResolutionError
	if !errors.As(err, &resolution) {
		return err
	}
	return &ResolutionError{
		ErrorCode: resolution.ErrorCode,
		Path:      segment + resolution.Path,
		Detail:    resolution.Detail,
	}
}

// exactPrimitiveCodes is the admitted primitive set — the same list
// snapshotExactType checks a *PrimitiveType against, kept beside the table it
// builds so the two cannot drift.
var exactPrimitiveCodes = [...]TypeCode{
	TypeCodeNull, TypeCodeBoolean, TypeCodeInt, TypeCodeLong,
	TypeCodeFloat, TypeCodeDouble, TypeCodeString, TypeCodeBytes,
	TypeCodeVersion, TypeCodeUuid, TypeCodeDate, TypeCodeTimestamp,
}

// sharedPrimitiveExactTypes holds the ENTIRE population of exact primitive
// types — (admitted code × nullable), two dozen values — built once at init.
//
// An exact type is content-addressed and immutable by construction, so two
// snapshots of the same primitive were always the same value; they were just
// two objects. That mattered because primitives are where the volume is: a
// record's snapshot recurses once per field, so a twelve-column row is one
// record node and eleven primitive leaves. Measured over the pure-planner
// sweep, snapshotExactType ran 243.8 MILLION times and produced 170 distinct
// canonical types, allocating 52.65GB — 22% of everything the planner
// allocated — to rebuild the same handful of leaves.
//
// Sharing is safe precisely because nothing mutates an exact type after
// finishCanonical: the only post-construction write is thawCache, which is
// atomic and holds a value derived from the same immutable state. Sharing that
// cache across every user of a primitive is a second win, not a hazard.
//
// The table is COMPLETE, so a miss is a caller that got past the admitted-code
// check with a code the table does not know — a contradiction, and it fails
// closed by falling back to a fresh node rather than returning nil.
var sharedPrimitiveExactTypes = func() map[TypeCode][2]*exactType {
	table := make(map[TypeCode][2]*exactType, len(exactPrimitiveCodes))
	for _, code := range exactPrimitiveCodes {
		var pair [2]*exactType
		for _, nullable := range [2]bool{false, true} {
			probe := exactProbe{code: code, nullable: nullable}
			exact := &exactType{code: code, nullable: nullable}
			exact.internHashValue = probe.internHash()
			exact.finishCanonical()
			pair[boolIndex(nullable)] = exact
		}
		table[code] = pair
	}
	return table
}()

func boolIndex(b bool) int {
	if b {
		return 1
	}
	return 0
}

func sharedPrimitiveExactType(code TypeCode, nullable bool) *exactType {
	if pair, known := sharedPrimitiveExactTypes[code]; known {
		return pair[boolIndex(nullable)]
	}
	probe := exactProbe{code: code, nullable: nullable}
	exact := &exactType{code: code, nullable: nullable}
	exact.internHashValue = probe.internHash()
	exact.finishCanonical()
	return exact
}

// snapshotExactType walks typ into its immutable exact form.
//
// active is the ANCESTOR chain, carried as a slice rather than a map with a
// deferred delete. A type graph is shallow, so a linear scan over a handful of
// entries beats hashing, and passing `append(active, identity)` BY VALUE gives
// the delete for free: a sibling's recursion sees the caller's length, so each
// branch observes exactly its own ancestors. The map form cost a `mapassign`
// plus a `mapdelete` plus a `defer` at every node — 23.8% and 6.6% of this
// function respectively — and allocated a fresh map per top-level call.
func snapshotExactType(typ Type, active []any) (*exactType, error) {
	const path = ""
	if typ == nil {
		return nil, resolutionError(TypeNil, path, "type is nil")
	}

	var identity any
	switch typed := typ.(type) {
	case anyRecordType:
		identity = typed
	case *PrimitiveType:
		if typed == nil {
			return nil, resolutionError(TypeTypedNil, path, "primitive type is typed nil")
		}
		identity = typed
	case *RecordType:
		if typed == nil {
			return nil, resolutionError(TypeTypedNil, path, "record type is typed nil")
		}
		identity = typed
	case *ArrayType:
		if typed == nil {
			return nil, resolutionError(TypeTypedNil, path, "array type is typed nil")
		}
		identity = typed
	case *EnumType:
		if typed == nil {
			return nil, resolutionError(TypeTypedNil, path, "enum type is typed nil")
		}
		identity = typed
	case *RelationType:
		if typed == nil {
			return nil, resolutionError(TypeTypedNil, path, "relation type is typed nil")
		}
		identity = typed
	default:
		return nil, resolutionError(TypeMalformedCode, path, "unsupported concrete Type implementation")
	}
	for _, ancestor := range active {
		if ancestor == identity {
			return nil, resolutionError(TypeCycle, path, "type graph contains a cycle")
		}
	}
	active = append(active, identity)

	switch typed := typ.(type) {
	case anyRecordType:
		probe := exactProbe{code: TypeCodeRecord, nullable: typed.nullable, anyRecord: true}
		return internedExactType(&probe, func() *exactType {
			return &exactType{code: TypeCodeRecord, nullable: typed.nullable, anyRecord: true}
		}), nil
	case *PrimitiveType:
		switch typed.TypeCode {
		case TypeCodeUnknown, TypeCodeAny, TypeCodeNone:
			return nil, resolutionError(TypeUnresolved, path, "placeholder type is not exact")
		case TypeCodeNull, TypeCodeBoolean, TypeCodeInt, TypeCodeLong,
			TypeCodeFloat, TypeCodeDouble, TypeCodeString, TypeCodeBytes,
			TypeCodeVersion, TypeCodeUuid, TypeCodeDate, TypeCodeTimestamp:
			// Admitted below.
		default:
			return nil, resolutionError(TypeMalformedCode, path, "primitive carries a structured or unknown code")
		}
		return sharedPrimitiveExactType(typed.TypeCode, typed.Nullable), nil
	case *RecordType:
		// The child handles are collected into a small stack array before the
		// probe runs. They are all this arm needs to identify the record, and
		// they are 8 bytes each against the 32 an exactField costs — which
		// matters because the whole point of probing first is that the common
		// call allocates NOTHING.
		var childBuf [16]*exactType
		children := childBuf[:0]
		if len(typed.Fields) > cap(children) {
			children = make([]*exactType, 0, len(typed.Fields))
		}
		for i, field := range typed.Fields {
			if field.Ordinal != i {
				return nil, resolutionError(TypeMalformedOrdinal, path, "record field ordinal does not equal its position")
			}
			fieldType, err := snapshotExactType(field.FieldType, active)
			if err != nil {
				return nil, prefixExactPath(err, ".field["+uitoa(uint64(i))+"]")
			}
			children = append(children, fieldType)
		}
		probe := exactProbe{
			code:      TypeCodeRecord,
			nullable:  typed.Nullable,
			name:      typed.RecordName,
			srcFields: typed.Fields,
			children:  children,
		}
		return internedExactType(&probe, func() *exactType {
			fields := make([]exactField, len(typed.Fields))
			for i, field := range typed.Fields {
				fields[i] = exactField{name: field.Name, ordinal: i, typ: children[i]}
			}
			return &exactType{
				code:     TypeCodeRecord,
				nullable: typed.Nullable,
				name:     typed.RecordName,
				fields:   fields,
			}
		}), nil
	case *ArrayType:
		if typed.ElementType == nil {
			return nil, resolutionError(TypeErased, path, "array element type is erased")
		}
		element, err := snapshotExactType(typed.ElementType, active)
		if err != nil {
			return nil, prefixExactPath(err, ".element")
		}
		probe := exactProbe{code: TypeCodeArray, nullable: typed.Nullable, element: element}
		return internedExactType(&probe, func() *exactType {
			return &exactType{code: TypeCodeArray, nullable: typed.Nullable, element: element}
		}), nil
	case *EnumType:
		// The duplicate checks run on EVERY call, not only on a miss: they are
		// admission, and an invalid enum must be refused whether or not an equal
		// valid one has been interned before it.
		seenNames := make(map[string]struct{}, len(typed.Values))
		seenNumbers := make(map[int32]struct{}, len(typed.Values))
		for _, value := range typed.Values {
			if _, duplicate := seenNames[value.Name]; duplicate {
				return nil, resolutionError(TypeMalformedCode, path, "duplicate enum value name")
			}
			if _, duplicate := seenNumbers[value.Number]; duplicate {
				return nil, resolutionError(TypeMalformedCode, path, "duplicate enum value number")
			}
			seenNames[value.Name] = struct{}{}
			seenNumbers[value.Number] = struct{}{}
		}
		probe := exactProbe{
			code:       TypeCodeEnum,
			nullable:   typed.Nullable,
			name:       typed.EnumName,
			enumValues: typed.Values,
		}
		return internedExactType(&probe, func() *exactType {
			return &exactType{
				code:       TypeCodeEnum,
				nullable:   typed.Nullable,
				name:       typed.EnumName,
				enumValues: append([]EnumValue(nil), typed.Values...),
			}
		}), nil
	case *RelationType:
		if typed.InnerType == nil {
			return nil, resolutionError(TypeErased, path, "relation inner type is erased")
		}
		inner, err := snapshotExactType(typed.InnerType, active)
		if err != nil {
			return nil, prefixExactPath(err, ".inner")
		}
		return internedRelationExactType(inner), nil
	}
	return nil, resolutionError(TypeMalformedCode, path, "unsupported concrete Type implementation")
}

// internedRelationExactType is the RELATION wrapper as its own entry point,
// because ExactRelationOf builds one from an already-snapshotted inner handle
// rather than from a *RelationType.
func internedRelationExactType(inner *exactType) *exactType {
	probe := exactProbe{code: TypeCodeRelation, element: inner}
	return internedExactType(&probe, func() *exactType {
		return &exactType{code: TypeCodeRelation, element: inner}
	})
}

func (e *exactType) finishCanonical() {
	encoded := make([]byte, 0, 32)
	encoded = binary.AppendUvarint(encoded, uint64(e.code))
	if e.nullable {
		encoded = append(encoded, 1)
	} else {
		encoded = append(encoded, 0)
	}
	// TypeCodeRecord alone cannot distinguish Java's erased AnyRecord from
	// the concrete zero-field unit record. Keep that concrete-kind bit in the
	// immutable identity so equality and hashing cannot collapse the two.
	if e.anyRecord {
		encoded = append(encoded, 1)
	} else {
		encoded = append(encoded, 0)
	}
	// The record/enum NAME is deliberately absent from the identity: it is
	// provenance, not shape, and Java's Type.Record/Type.Enum equals+hashCode
	// exclude it too. Keeping it here would make the exact channel's identity
	// STRICTER than the Type equality it exists to make checkable, so one row
	// reached by two routes would carry two "exact" identities.
	encoded = binary.AppendUvarint(encoded, uint64(len(e.fields)))
	for _, field := range e.fields {
		encoded = appendCanonicalString(encoded, field.name)
		encoded = binary.AppendVarint(encoded, int64(field.ordinal))
		encoded = appendCanonicalBytes(encoded, field.typ.canonical)
	}
	if e.element == nil {
		encoded = append(encoded, 0)
	} else {
		encoded = append(encoded, 1)
		encoded = appendCanonicalBytes(encoded, e.element.canonical)
	}
	encoded = binary.AppendUvarint(encoded, uint64(len(e.enumValues)))
	for _, value := range e.enumValues {
		encoded = appendCanonicalString(encoded, value.Name)
		encoded = binary.AppendVarint(encoded, int64(value.Number))
	}
	e.canonical = encoded
	h := fnv.New64a()
	_, _ = h.Write(encoded)
	e.hash = h.Sum64()
}

func appendCanonicalString(dst []byte, value string) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(value)))
	return append(dst, value...)
}

func appendCanonicalBytes(dst, value []byte) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(value)))
	return append(dst, value...)
}

func exactTypesEqual(left, right *exactType) bool {
	if left == nil || right == nil {
		return left == right
	}
	// One handle compared against itself is the DOMINANT case on the row path,
	// not a lucky one: isAdmittedFieldValue runs this on every Evaluate, and a
	// baked reference's rootType and its owner's flowed type are the same
	// captured snapshot. Equal pointers are equal values whether or not the two
	// are the same allocation, so this only ever skips work.
	if left == right {
		return true
	}
	if left.hash != right.hash {
		return false
	}
	return bytes.Equal(left.canonical, right.canonical)
}

// exactRowShapesAgree is the exact-handle twin of QuantifiedRowShapesAgree —
// same rule, same reason (see that doc): equality of the bound row's SHAPE, with
// the top-level nullable bit excluded because it states that the row may be
// ABSENT, which every binder carries structurally instead. Nested nullability is
// compared byte-for-byte with the children's canonical forms, so only the one
// outermost bit is ever ignored, and only at a lookup.
func exactRowShapesAgree(left, right *exactType) bool {
	if left == nil || right == nil {
		return left == right
	}
	if left.nullable == right.nullable {
		return exactTypesEqual(left, right)
	}
	if left.code != right.code || left.anyRecord != right.anyRecord {
		return false
	}
	if len(left.fields) != len(right.fields) || len(left.enumValues) != len(right.enumValues) {
		return false
	}
	for i := range left.fields {
		if left.fields[i].name != right.fields[i].name ||
			left.fields[i].ordinal != right.fields[i].ordinal ||
			!exactTypesEqual(left.fields[i].typ, right.fields[i].typ) {
			return false
		}
	}
	for i := range left.enumValues {
		if left.enumValues[i] != right.enumValues[i] {
			return false
		}
	}
	return exactTypesEqual(left.element, right.element)
}

// describeExactType renders a compact, comparable shape for a diagnostic:
// `RECORD(LID:LONG,VAL:LONG?)`, `ARRAY<INT>`, `LONG?`. A trailing `?` marks
// nullable.
//
// It exists because a type-conflict error that names neither the correlation nor
// how the two types differ makes every occurrence a fresh investigation — and
// these conflicts arrive in clusters, where the ONE thing worth knowing first is
// whether the disagreement is arity, nullability, or field naming.
//
// Every attribute exact equality compares is rendered, and only the ones that
// can differ are ever spelled out: a field ORDINAL as `#n` when it is not the
// field's own position. Rendering less than equality compares makes two
// provably-unequal types print identically, which reads as "the diagnostic is
// wrong" and sends the next reader down the wrong path — it already cost one
// investigation a full lap.
//
// The record/enum NAME is rendered too — as `RECORD@Name(…)` — even though
// equality does NOT compare it. It is the provenance of the shape, and when a
// conflict IS elsewhere it is usually the fastest way to see which two routes
// produced the two sides.
func describeExactType(e *exactType) string {
	if e == nil {
		return "<nil>"
	}
	out := e.code.String()
	switch {
	case e.anyRecord:
		out = "RECORD(*)"
	case e.code == TypeCodeRecord:
		if e.name != "" {
			out += "@" + e.name
		}
		out += "("
		for i, field := range e.fields {
			if i > 0 {
				out += ","
			}
			out += field.name
			if field.ordinal != i {
				out += "#" + uitoa(uint64(field.ordinal))
			}
			out += ":" + describeExactType(field.typ)
		}
		out += ")"
	case e.code == TypeCodeEnum:
		if e.name != "" {
			out += "@" + e.name
		}
	case e.code == TypeCodeArray || e.code == TypeCodeRelation:
		out += "<" + describeExactType(e.element) + ">"
	}
	if e.nullable {
		out += "?"
	}
	return out
}

// exactTypeConflict is the shared rendering for the "two exact types were
// expected to be the same one" family. Every arm reports WHICH correlation and
// BOTH shapes, in that order, so the reader can tell a nullability-only
// disagreement from a different row.
func exactTypeConflict(path, what string, correlation CorrelationIdentifier, have, want *exactType) error {
	return resolutionError(CorrelationTypeConflict, path+"["+correlation.Name()+"]",
		what+": bound "+describeExactType(have)+" but read as "+describeExactType(want))
}

// QuantifiedRowShapesAgree reports whether two exact QOV flowed types denote the
// SAME BOUND ROW. It is the one comparison every runtime QOV *lookup* uses, and
// it deliberately excludes the top-level nullable bit.
//
// That bit is not part of a row's shape: on a quantifier it declares that the
// quantifier's row may be ABSENT, and absence is carried structurally by the
// binding itself — a nil row, or a positive explicit-absence proof — never by
// the row's type. Evaluating FieldValue(qov, i) against a present row is
// bit-identical whichever way the bit is set, and against an absent one both
// yield NULL, so the bit can decide nothing at a lookup.
//
// One alias therefore legitimately carries two QOVs differing only there. That
// pairing is Java's, not a Go accident: an outer join's OUTPUT columns are
// pulled up through Quantifier.pullUpResultColumnsWithNullability(true), which
// mints QuantifiedObjectValue.of(alias, rowType.withNullability(true)), while
// the same alias's ON/WHERE predicates keep reading getFlowedObjectValue() —
// the leg's own, non-nullable row. Java binds both by alias alone; Go's exact
// channel keeps the strictly stronger check on everything that IS shape.
//
// Field names, ordinals, arity, record names and every NESTED nullability still
// have to match exactly, so a foreign owner spelled with the same alias is
// still refused. Type DERIVATION sites (layout window construction, the
// null-supplying proofs, metadata nullability) must keep comparing with Equals:
// there the bit is the whole point.
func QuantifiedRowShapesAgree(left, right Type) bool {
	if left == nil || right == nil {
		return false
	}
	if left.IsNullable() == right.IsNullable() {
		return left.Equals(right)
	}
	return left.Equals(WithNullability(right, left.IsNullable()))
}

// DescribeType renders a Type as a compact, comparable shape for a diagnostic:
// `RECORD(LID:LONG,VAL:LONG?)`, `ARRAY<INT>`, `LONG?`. A trailing `?` marks
// nullable. It is the public twin of the rendering the exact-type conflicts use,
// for callers outside this package that hold a Type rather than a handle.
func DescribeType(typ Type) string {
	handle, err := SnapshotExactType(typ)
	if err != nil {
		if typ == nil {
			return "<nil>"
		}
		return typ.Code().String() + "(inexact)"
	}
	return describeExactType(handle.(*exactType))
}
