package values

// Phase 4.0 Type hierarchy seed.
//
// Mirrors the bare-minimum surface of Java's
// `com.apple.foundationdb.record.query.plan.cascades.typing.Type` —
// the rich type system that replaces today's flat `ValueType` enum.
// The interim `ValueType` continues to coexist; this file adds the
// richer `Type` interface alongside it so new code can carry full
// type information (notably nullability) while old call sites keep
// working unchanged. Migration is incremental.
//
// Seed scope (this file): TypeCode enum mirroring Java's
// well-known codes, the Type interface (Code + IsNullable + a few
// shape predicates), the PrimitiveType impl plus singletons for the
// common primitives, and adapter functions to / from the legacy
// ValueType enum so callers can bridge piecewise.
//
// Out of scope (Phase 4.0 follow-ups): RecordType, ArrayType,
// EnumType, UuidType, RelationType, TypeRepository, plan-
// serialisation hooks, the full Java conversion / coercion lattice.
// Per RFC-025 §"typing/" the file stays in cascades/values/ until
// the contents grow past ~300 LOC; only then does typing/ become
// its own sub-package.

// TypeCode enumerates the well-known SQL types. Mirrors Java's
// `Type.TypeCode`; numeric values are NOT wire-stable (we don't
// serialise plans yet — RFC-024 punts on hash compatibility).
type TypeCode int

const (
	// TypeCodeUnknown is the zero value — represents "type not yet
	// inferred" rather than the SQL NULL type. Distinct from
	// TypeCodeNull which is "the NULL literal's type".
	TypeCodeUnknown TypeCode = iota
	TypeCodeNull
	TypeCodeBoolean
	TypeCodeInt
	TypeCodeLong
	TypeCodeFloat
	TypeCodeDouble
	TypeCodeString
	TypeCodeBytes
	TypeCodeVersion
	TypeCodeEnum
	TypeCodeRecord
	TypeCodeArray
	TypeCodeRelation
	TypeCodeAny
	TypeCodeNone
	TypeCodeUuid
)

// String renders the code as the SQL-ish type name ("INT", "STRING",
// "BOOLEAN", …). Used by EXPLAIN output and error messages so the
// rendered surface matches Java's TypeCode.name() output.
func (tc TypeCode) String() string {
	switch tc {
	case TypeCodeNull:
		return "NULL"
	case TypeCodeBoolean:
		return "BOOLEAN"
	case TypeCodeInt:
		return "INT"
	case TypeCodeLong:
		return "LONG"
	case TypeCodeFloat:
		return "FLOAT"
	case TypeCodeDouble:
		return "DOUBLE"
	case TypeCodeString:
		return "STRING"
	case TypeCodeBytes:
		return "BYTES"
	case TypeCodeVersion:
		return "VERSION"
	case TypeCodeEnum:
		return "ENUM"
	case TypeCodeRecord:
		return "RECORD"
	case TypeCodeArray:
		return "ARRAY"
	case TypeCodeRelation:
		return "RELATION"
	case TypeCodeAny:
		return "ANY"
	case TypeCodeNone:
		return "NONE"
	case TypeCodeUuid:
		return "UUID"
	}
	return "UNKNOWN"
}

// IsPrimitive reports whether tc names a scalar (vs structured) type.
// Mirrors Java's `TypeCode.isPrimitive()`. Composite shapes (RECORD,
// ARRAY, RELATION) and the special placeholders (UNKNOWN, ANY, NONE,
// FUNCTION) all return false.
func (tc TypeCode) IsPrimitive() bool {
	switch tc {
	case TypeCodeBoolean, TypeCodeInt, TypeCodeLong,
		TypeCodeFloat, TypeCodeDouble,
		TypeCodeString, TypeCodeBytes, TypeCodeVersion,
		TypeCodeUuid:
		return true
	}
	return false
}

// IsNumeric reports whether tc is one of the numeric types
// (arithmetic + comparison promotion targets).
func (tc TypeCode) IsNumeric() bool {
	switch tc {
	case TypeCodeInt, TypeCodeLong, TypeCodeFloat, TypeCodeDouble:
		return true
	}
	return false
}

// Type is the rich type-system handle replacing ValueType. Carries
// the type code plus nullability; concrete impls add structure
// (RecordType.Fields, ArrayType.Element, …) as the port lands them.
//
// Equals tests structural equality — two distinct PrimitiveType
// instances with the same Code + Nullable are equal. Pointer-
// equality is NOT a substitute (the helper constants below exist
// precisely so callers can share a canonical pointer per
// (code, nullable) pair when they need to).
type Type interface {
	// Code returns this type's TypeCode.
	Code() TypeCode

	// IsNullable reports whether the type allows NULL values. SQL
	// columns default to nullable; PRIMARY KEY columns and explicit
	// NOT NULL columns are non-nullable.
	IsNullable() bool

	// Equals reports structural equality with other. Implementations
	// MUST compare Code + Nullable AT MINIMUM; structured types
	// extend the contract to compare child fields / element types.
	Equals(other Type) bool

	// String renders the type in SQL-ish form ("INT NOT NULL",
	// "STRING NULL"). Used by EXPLAIN output.
	String() string
}

// PrimitiveType is the Type impl for scalar types (INT, BOOLEAN,
// STRING, …). Two PrimitiveType values are Equal iff their Code +
// Nullable match.
type PrimitiveType struct {
	TypeCode TypeCode
	Nullable bool
}

// NewPrimitiveType constructs a PrimitiveType. Panics if code is a
// non-primitive code (RECORD / ARRAY / RELATION) — those need their
// dedicated structured-type constructors which the seed doesn't
// provide yet. UNKNOWN / ANY / NONE / NULL are accepted because
// they're frequently useful as placeholder Types even though they're
// not "primitive" per IsPrimitive's sense.
func NewPrimitiveType(code TypeCode, nullable bool) *PrimitiveType {
	switch code {
	case TypeCodeRecord, TypeCodeArray, TypeCodeRelation, TypeCodeEnum:
		panic("NewPrimitiveType: structured TypeCode " + code.String() +
			" requires its dedicated constructor (not yet ported)")
	}
	return &PrimitiveType{TypeCode: code, Nullable: nullable}
}

// Code implements Type.
func (p *PrimitiveType) Code() TypeCode { return p.TypeCode }

// IsNullable implements Type.
func (p *PrimitiveType) IsNullable() bool { return p.Nullable }

// Equals implements Type. Structural — Code + Nullable.
func (p *PrimitiveType) Equals(other Type) bool {
	if other == nil {
		return false
	}
	op, ok := other.(*PrimitiveType)
	if !ok {
		return false
	}
	return p.TypeCode == op.TypeCode && p.Nullable == op.Nullable
}

// String implements Type. Renders as "INT NOT NULL", "STRING NULL", …
func (p *PrimitiveType) String() string {
	if p.Nullable {
		return p.TypeCode.String() + " NULL"
	}
	return p.TypeCode.String() + " NOT NULL"
}

// Canonical singletons for the most common (code, nullable) pairs.
// Callers that need to share a pointer (e.g. for fast equality
// checks via `==`) use these. Mirrors Java's Type.NULL / NONE /
// UUID_NULL_INSTANCE constants.
var (
	// NullableInt is INT NULL — INT column with no NOT NULL constraint.
	NullableInt Type = &PrimitiveType{TypeCode: TypeCodeInt, Nullable: true}
	// NotNullInt is INT NOT NULL — typical PRIMARY KEY column shape.
	NotNullInt Type = &PrimitiveType{TypeCode: TypeCodeInt, Nullable: false}

	// NullableLong is LONG NULL (BIGINT default).
	NullableLong Type = &PrimitiveType{TypeCode: TypeCodeLong, Nullable: true}
	// NotNullLong is LONG NOT NULL.
	NotNullLong Type = &PrimitiveType{TypeCode: TypeCodeLong, Nullable: false}

	// NullableFloat is FLOAT NULL.
	NullableFloat Type = &PrimitiveType{TypeCode: TypeCodeFloat, Nullable: true}
	// NotNullFloat is FLOAT NOT NULL.
	NotNullFloat Type = &PrimitiveType{TypeCode: TypeCodeFloat, Nullable: false}

	// NullableDouble is DOUBLE NULL.
	NullableDouble Type = &PrimitiveType{TypeCode: TypeCodeDouble, Nullable: true}
	// NotNullDouble is DOUBLE NOT NULL.
	NotNullDouble Type = &PrimitiveType{TypeCode: TypeCodeDouble, Nullable: false}

	// NullableString is STRING NULL (VARCHAR default).
	NullableString Type = &PrimitiveType{TypeCode: TypeCodeString, Nullable: true}
	// NotNullString is STRING NOT NULL.
	NotNullString Type = &PrimitiveType{TypeCode: TypeCodeString, Nullable: false}

	// NullableBoolean is BOOLEAN NULL.
	NullableBoolean Type = &PrimitiveType{TypeCode: TypeCodeBoolean, Nullable: true}
	// NotNullBoolean is BOOLEAN NOT NULL.
	NotNullBoolean Type = &PrimitiveType{TypeCode: TypeCodeBoolean, Nullable: false}

	// NullableBytes is BYTES NULL.
	NullableBytes Type = &PrimitiveType{TypeCode: TypeCodeBytes, Nullable: true}
	// NotNullBytes is BYTES NOT NULL.
	NotNullBytes Type = &PrimitiveType{TypeCode: TypeCodeBytes, Nullable: false}

	// NullType is the type of the NULL literal — always nullable
	// (a NULL is by definition not a value of a specific type, but
	// can be assigned to any nullable column). Distinct from
	// UnknownType: NULL has a concrete code, UNKNOWN doesn't.
	NullType Type = &PrimitiveType{TypeCode: TypeCodeNull, Nullable: true}

	// UnknownType is the placeholder for "type not yet inferred" —
	// used by Value impls that don't yet have a real type computed.
	// Pre-Phase-4.0 the legacy `ValueType` had a TypeUnknown that
	// served the same purpose; this is the new-API replacement.
	UnknownType Type = &PrimitiveType{TypeCode: TypeCodeUnknown, Nullable: true}
)

// Typed is the interface things-with-a-type implement. Mirrors Java's
// Typed. Values, expressions, and table columns will eventually all
// implement it; today it's a forward-compat hook so call sites can
// start writing `t.Type()` against the rich Type instead of the
// legacy ValueType.
type Typed interface {
	// Type returns this thing's Type. Never nil — implementations
	// return UnknownType when the type genuinely isn't known yet.
	RichType() Type
}

// FromValueType bridges the legacy ValueType to the new Type
// interface. nullable lets the caller carry NOT NULL information
// from the schema; ValueType itself doesn't track nullability.
//
// Used by call sites that have a ValueType in hand (existing API
// surface) and need to feed a Type to a new-API consumer. Once the
// migration is complete the legacy ValueType retires and this
// adapter goes with it.
func FromValueType(vt ValueType, nullable bool) Type {
	switch vt {
	case TypeBool:
		if nullable {
			return NullableBoolean
		}
		return NotNullBoolean
	case TypeInt:
		// The seed ValueType conflates INT and LONG. Map to LONG
		// since BIGINT is the Java Record Layer's default integer
		// width and it round-trips int64 cleanly.
		if nullable {
			return NullableLong
		}
		return NotNullLong
	case TypeFloat:
		// Same conflation: ValueType.TypeFloat covers both FLOAT
		// (32-bit) and DOUBLE (64-bit). Map to DOUBLE since Java's
		// Record Layer defaults to double-precision and our runtime
		// representation is float64.
		if nullable {
			return NullableDouble
		}
		return NotNullDouble
	case TypeString:
		if nullable {
			return NullableString
		}
		return NotNullString
	}
	return UnknownType
}

// --- RecordType ----------------------------------------------------

// Field is one field of a RecordType. Mirrors Java's Record.Field —
// name + type + ordinal. The Ordinal carries the declared position
// for stable ordering across maps; two Fields with the same Name
// but different Ordinals are NOT equal.
type Field struct {
	// Name is the field's identifier. Empty string is legal and
	// represents an anonymous field — `RECORD<INT, STRING>` produces
	// fields with Name="" but distinct Ordinals.
	Name string
	// FieldType is the field's type. Never nil — anonymous /
	// untyped fields use UnknownType.
	FieldType Type
	// Ordinal is the field's declared position (0-based). Two fields
	// with the same Name but different Ordinals are distinct fields
	// (same as Java's Record.Field.fieldIndex).
	Ordinal int
}

// Equals reports whether two Fields are structurally equal: same
// Name + Ordinal + FieldType.Equals.
func (f Field) Equals(other Field) bool {
	if f.Name != other.Name || f.Ordinal != other.Ordinal {
		return false
	}
	if f.FieldType == nil || other.FieldType == nil {
		return f.FieldType == other.FieldType
	}
	return f.FieldType.Equals(other.FieldType)
}

// RecordType is the Type impl for struct-shaped data. Mirrors Java's
// Record nested type. Two RecordType instances are Equal iff their
// Name + Nullable match AND their Fields slice is element-wise
// equal (same length, each Field equals at the same index).
//
// Anonymous records (no name) are common — `RECORD<INT, STRING>`
// produces a RecordType with Name="" and the corresponding Fields.
// Named records carry a schema-level table or struct name.
type RecordType struct {
	// RecordName is the optional record name. Empty string means
	// anonymous — frequently the case for projection result rows
	// that haven't been bound to a named struct.
	RecordName string
	// Nullable reports whether the record allows NULL — i.e. a
	// nullable column whose type is this RecordType. Anonymous
	// records typically default to nullable since plan-time
	// inference can't always prove non-nullness.
	Nullable bool
	// Fields are the record's fields in declared order. Empty slice
	// means a record with no fields (legal — `RECORD<>` is the unit
	// type). Never nil.
	Fields []Field
}

// NewRecordType constructs a RecordType. The Fields slice is
// defensively copied; callers' modifications to their input slice
// won't affect the constructed type.
//
// Panics on duplicate field names within Fields (anonymous fields
// with Name="" are exempt — they're disambiguated by Ordinal). Java
// errors at the same point with SemanticException; the Go seed
// panics so callers get an immediate stack trace.
func NewRecordType(name string, nullable bool, fields []Field) *RecordType {
	seenNames := make(map[string]struct{}, len(fields))
	out := make([]Field, len(fields))
	for i, f := range fields {
		if f.Name != "" {
			if _, dup := seenNames[f.Name]; dup {
				panic("NewRecordType: duplicate field name " + f.Name)
			}
			seenNames[f.Name] = struct{}{}
		}
		out[i] = f
	}
	return &RecordType{
		RecordName: name,
		Nullable:   nullable,
		Fields:     out,
	}
}

// Code implements Type — always TypeCodeRecord.
func (*RecordType) Code() TypeCode { return TypeCodeRecord }

// IsNullable implements Type.
func (r *RecordType) IsNullable() bool { return r.Nullable }

// Equals implements Type. Structural — name + nullable + element-
// wise field equality.
func (r *RecordType) Equals(other Type) bool {
	if other == nil {
		return false
	}
	or, ok := other.(*RecordType)
	if !ok {
		return false
	}
	if r.RecordName != or.RecordName || r.Nullable != or.Nullable {
		return false
	}
	if len(r.Fields) != len(or.Fields) {
		return false
	}
	for i := range r.Fields {
		if !r.Fields[i].Equals(or.Fields[i]) {
			return false
		}
	}
	return true
}

// String implements Type. Renders as
// `[name] RECORD<f1 INT, f2 STRING NULL> [NOT NULL | NULL]`.
func (r *RecordType) String() string {
	var b []byte
	if r.RecordName != "" {
		b = append(b, r.RecordName...)
		b = append(b, ' ')
	}
	b = append(b, "RECORD<"...)
	for i, f := range r.Fields {
		if i > 0 {
			b = append(b, ',', ' ')
		}
		if f.Name != "" {
			b = append(b, f.Name...)
			b = append(b, ' ')
		}
		if f.FieldType == nil {
			b = append(b, "?"...)
		} else {
			b = append(b, f.FieldType.String()...)
		}
	}
	b = append(b, '>')
	if r.Nullable {
		b = append(b, " NULL"...)
	} else {
		b = append(b, " NOT NULL"...)
	}
	return string(b)
}

// LookupField returns the named field plus a found flag. Empty name
// always returns (Field{}, false) — anonymous fields aren't
// addressable by name.
func (r *RecordType) LookupField(name string) (Field, bool) {
	if name == "" {
		return Field{}, false
	}
	for _, f := range r.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return Field{}, false
}

// GetField returns the field at the given ordinal plus a found flag.
// Negative or out-of-range ordinals return (Field{}, false).
func (r *RecordType) GetField(ordinal int) (Field, bool) {
	if ordinal < 0 || ordinal >= len(r.Fields) {
		return Field{}, false
	}
	return r.Fields[ordinal], true
}

// --- ArrayType -----------------------------------------------------

// ArrayType is the Type impl for ordered collections. Mirrors Java's
// Array nested type. Carries an ElementType (the type of the array's
// values) plus a Nullable flag (whether the array column itself can
// be NULL).
//
// Two ArrayType instances are Equal iff their Nullable + ElementType
// match. nil ElementType represents an array whose element type isn't
// inferred yet (e.g. an empty array literal pre-type-inference) and
// is equal only to another ArrayType with nil ElementType.
type ArrayType struct {
	// Nullable reports whether the array column allows NULL.
	Nullable bool
	// ElementType is the type of the array's values. May be nil
	// when type inference hasn't filled it in (typically transient
	// during plan-time analysis; runtime arrays always have a
	// concrete element type by the time they're evaluated).
	ElementType Type
}

// NewArrayType constructs an ArrayType. nil elementType is allowed
// for the "type not yet inferred" case; callers can fill it in via
// WithElementType once inference produces a concrete type.
func NewArrayType(nullable bool, elementType Type) *ArrayType {
	return &ArrayType{Nullable: nullable, ElementType: elementType}
}

// Code implements Type — always TypeCodeArray.
func (*ArrayType) Code() TypeCode { return TypeCodeArray }

// IsNullable implements Type.
func (a *ArrayType) IsNullable() bool { return a.Nullable }

// Equals implements Type. Structural — Nullable + ElementType.Equals.
// Two ArrayTypes both with nil ElementType are equal; one nil + one
// non-nil are not.
func (a *ArrayType) Equals(other Type) bool {
	if other == nil {
		return false
	}
	oa, ok := other.(*ArrayType)
	if !ok {
		return false
	}
	if a.Nullable != oa.Nullable {
		return false
	}
	if a.ElementType == nil || oa.ElementType == nil {
		return a.ElementType == oa.ElementType
	}
	return a.ElementType.Equals(oa.ElementType)
}

// String implements Type. Renders as `ARRAY<INT NOT NULL> NULL` /
// `ARRAY<?>` (when ElementType is nil).
func (a *ArrayType) String() string {
	elemStr := "?"
	if a.ElementType != nil {
		elemStr = a.ElementType.String()
	}
	suffix := " NOT NULL"
	if a.Nullable {
		suffix = " NULL"
	}
	return "ARRAY<" + elemStr + ">" + suffix
}

// ToValueType bridges the new Type back to the legacy ValueType.
// LONG / DOUBLE both fold into the seed's TypeInt / TypeFloat (the
// legacy enum doesn't distinguish widths). Structured types and
// special placeholders (NULL, ANY, NONE, RECORD, ARRAY, ENUM,
// RELATION, UUID, VERSION) all degrade to TypeUnknown — the legacy
// enum has no representation for them.
func ToValueType(t Type) ValueType {
	if t == nil {
		return TypeUnknown
	}
	switch t.Code() {
	case TypeCodeBoolean:
		return TypeBool
	case TypeCodeInt, TypeCodeLong:
		return TypeInt
	case TypeCodeFloat, TypeCodeDouble:
		return TypeFloat
	case TypeCodeString:
		return TypeString
	}
	return TypeUnknown
}
