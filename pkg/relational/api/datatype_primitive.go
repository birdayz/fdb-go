package api

// Primitive DataType implementations — simple scalar types. Each type
// has two singleton instances (nullable + not-nullable) to match
// Java's enum-driven Primitives table. WithNullable() returns the
// singleton, never a new allocation.
//
// Match one class per Java subtype even though the bodies are nearly
// identical — keeps type switches (the Go equivalent of Java's
// instanceof) unambiguous.

// ---- BooleanType ----

type BooleanType struct{ typeBase }

var (
	boolNotNullable = &BooleanType{typeBase{code: CodeBoolean, isPrimitive: true}}
	boolNullable    = &BooleanType{typeBase{code: CodeBoolean, isPrimitive: true, isNullable: true}}
)

// NewBooleanType returns the singleton BooleanType for the given nullability.
func NewBooleanType(isNullable bool) *BooleanType {
	if isNullable {
		return boolNullable
	}
	return boolNotNullable
}

func (t *BooleanType) IsResolved() bool { return true }
func (t *BooleanType) WithNullable(isNullable bool) DataType {
	return NewBooleanType(isNullable)
}

func (t *BooleanType) Resolve(_ map[string]Named) DataType { return t }
func (t *BooleanType) Equal(other DataType) bool {
	o, ok := other.(*BooleanType)
	return ok && o.isNullable == t.isNullable
}
func (t *BooleanType) String() string { return "boolean" + nullableSuffix(t.isNullable) }

// ---- IntegerType ----

type IntegerType struct{ typeBase }

var (
	intNotNullable = &IntegerType{typeBase{code: CodeInteger, isPrimitive: true}}
	intNullable    = &IntegerType{typeBase{code: CodeInteger, isPrimitive: true, isNullable: true}}
)

func NewIntegerType(isNullable bool) *IntegerType {
	if isNullable {
		return intNullable
	}
	return intNotNullable
}

func (t *IntegerType) IsResolved() bool                      { return true }
func (t *IntegerType) WithNullable(isNullable bool) DataType { return NewIntegerType(isNullable) }
func (t *IntegerType) Resolve(_ map[string]Named) DataType   { return t }
func (t *IntegerType) Equal(other DataType) bool {
	o, ok := other.(*IntegerType)
	return ok && o.isNullable == t.isNullable
}
func (t *IntegerType) String() string { return "int" + nullableSuffix(t.isNullable) }

// ---- LongType ----

type LongType struct{ typeBase }

var (
	longNotNullable = &LongType{typeBase{code: CodeLong, isPrimitive: true}}
	longNullable    = &LongType{typeBase{code: CodeLong, isPrimitive: true, isNullable: true}}
)

func NewLongType(isNullable bool) *LongType {
	if isNullable {
		return longNullable
	}
	return longNotNullable
}

func (t *LongType) IsResolved() bool                      { return true }
func (t *LongType) WithNullable(isNullable bool) DataType { return NewLongType(isNullable) }
func (t *LongType) Resolve(_ map[string]Named) DataType   { return t }
func (t *LongType) Equal(other DataType) bool {
	o, ok := other.(*LongType)
	return ok && o.isNullable == t.isNullable
}
func (t *LongType) String() string { return "long" + nullableSuffix(t.isNullable) }

// ---- FloatType ----

type FloatType struct{ typeBase }

var (
	floatNotNullable = &FloatType{typeBase{code: CodeFloat, isPrimitive: true}}
	floatNullable    = &FloatType{typeBase{code: CodeFloat, isPrimitive: true, isNullable: true}}
)

func NewFloatType(isNullable bool) *FloatType {
	if isNullable {
		return floatNullable
	}
	return floatNotNullable
}

func (t *FloatType) IsResolved() bool                      { return true }
func (t *FloatType) WithNullable(isNullable bool) DataType { return NewFloatType(isNullable) }
func (t *FloatType) Resolve(_ map[string]Named) DataType   { return t }
func (t *FloatType) Equal(other DataType) bool {
	o, ok := other.(*FloatType)
	return ok && o.isNullable == t.isNullable
}
func (t *FloatType) String() string { return "float" + nullableSuffix(t.isNullable) }

// ---- DoubleType ----

type DoubleType struct{ typeBase }

var (
	doubleNotNullable = &DoubleType{typeBase{code: CodeDouble, isPrimitive: true}}
	doubleNullable    = &DoubleType{typeBase{code: CodeDouble, isPrimitive: true, isNullable: true}}
)

func NewDoubleType(isNullable bool) *DoubleType {
	if isNullable {
		return doubleNullable
	}
	return doubleNotNullable
}

func (t *DoubleType) IsResolved() bool                      { return true }
func (t *DoubleType) WithNullable(isNullable bool) DataType { return NewDoubleType(isNullable) }
func (t *DoubleType) Resolve(_ map[string]Named) DataType   { return t }
func (t *DoubleType) Equal(other DataType) bool {
	o, ok := other.(*DoubleType)
	return ok && o.isNullable == t.isNullable
}
func (t *DoubleType) String() string { return "double" + nullableSuffix(t.isNullable) }

// ---- StringType ----

type StringType struct{ typeBase }

var (
	stringNotNullable = &StringType{typeBase{code: CodeString, isPrimitive: true}}
	stringNullable    = &StringType{typeBase{code: CodeString, isPrimitive: true, isNullable: true}}
)

func NewStringType(isNullable bool) *StringType {
	if isNullable {
		return stringNullable
	}
	return stringNotNullable
}

func (t *StringType) IsResolved() bool                      { return true }
func (t *StringType) WithNullable(isNullable bool) DataType { return NewStringType(isNullable) }
func (t *StringType) Resolve(_ map[string]Named) DataType   { return t }
func (t *StringType) Equal(other DataType) bool {
	o, ok := other.(*StringType)
	return ok && o.isNullable == t.isNullable
}
func (t *StringType) String() string { return "string" + nullableSuffix(t.isNullable) }

// ---- BytesType ----

type BytesType struct{ typeBase }

var (
	bytesNotNullable = &BytesType{typeBase{code: CodeBytes, isPrimitive: true}}
	bytesNullable    = &BytesType{typeBase{code: CodeBytes, isPrimitive: true, isNullable: true}}
)

func NewBytesType(isNullable bool) *BytesType {
	if isNullable {
		return bytesNullable
	}
	return bytesNotNullable
}

func (t *BytesType) IsResolved() bool                      { return true }
func (t *BytesType) WithNullable(isNullable bool) DataType { return NewBytesType(isNullable) }
func (t *BytesType) Resolve(_ map[string]Named) DataType   { return t }
func (t *BytesType) Equal(other DataType) bool {
	o, ok := other.(*BytesType)
	return ok && o.isNullable == t.isNullable
}
func (t *BytesType) String() string { return "bytes" + nullableSuffix(t.isNullable) }

// ---- VersionType ----

type VersionType struct{ typeBase }

var (
	versionNotNullable = &VersionType{typeBase{code: CodeVersion, isPrimitive: true}}
	versionNullable    = &VersionType{typeBase{code: CodeVersion, isPrimitive: true, isNullable: true}}
)

func NewVersionType(isNullable bool) *VersionType {
	if isNullable {
		return versionNullable
	}
	return versionNotNullable
}

func (t *VersionType) IsResolved() bool                      { return true }
func (t *VersionType) WithNullable(isNullable bool) DataType { return NewVersionType(isNullable) }
func (t *VersionType) Resolve(_ map[string]Named) DataType   { return t }
func (t *VersionType) Equal(other DataType) bool {
	o, ok := other.(*VersionType)
	return ok && o.isNullable == t.isNullable
}
func (t *VersionType) String() string { return "version" + nullableSuffix(t.isNullable) }

// ---- UUIDType ----

type UUIDType struct{ typeBase }

var (
	uuidNotNullable = &UUIDType{typeBase{code: CodeUUID, isPrimitive: true}}
	uuidNullable    = &UUIDType{typeBase{code: CodeUUID, isPrimitive: true, isNullable: true}}
)

func NewUUIDType(isNullable bool) *UUIDType {
	if isNullable {
		return uuidNullable
	}
	return uuidNotNullable
}

func (t *UUIDType) IsResolved() bool                      { return true }
func (t *UUIDType) WithNullable(isNullable bool) DataType { return NewUUIDType(isNullable) }
func (t *UUIDType) Resolve(_ map[string]Named) DataType   { return t }
func (t *UUIDType) Equal(other DataType) bool {
	o, ok := other.(*UUIDType)
	return ok && o.isNullable == t.isNullable
}
func (t *UUIDType) String() string { return "uuid" + nullableSuffix(t.isNullable) }

// ---- NullType ----
//
// NullType is always nullable. Attempting to set non-nullable panics —
// matches Java's NullType.withNullable(false) throwing INTERNAL_ERROR.

type NullType struct{ typeBase }

var nullSingleton = &NullType{typeBase{code: CodeNull, isPrimitive: true, isNullable: true}}

// NewNullType returns the singleton NULL type.
func NewNullType() *NullType { return nullSingleton }

func (t *NullType) IsResolved() bool { return true }

// WithNullable panics on false — NULL is always nullable. Matches
// Java's behavior of throwing INTERNAL_ERROR.
func (t *NullType) WithNullable(isNullable bool) DataType {
	if !isNullable {
		panic(NewError(ErrCodeInternalError, "NULL type cannot be non-nullable"))
	}
	return t
}

func (t *NullType) Resolve(_ map[string]Named) DataType { return t }

// Equal: NullType is a singleton (NewNullType returns the same
// pointer every time, WithNullable(false) panics). Type assertion
// is the only condition needed — any *NullType is the singleton, so
// the previous `o.isNullable == t.isNullable` check was unreachable
// dead code (always `true == true`). Pin the singleton invariant
// in the test instead.
func (*NullType) Equal(other DataType) bool {
	_, ok := other.(*NullType)
	return ok
}
func (t *NullType) String() string { return "null" }

// ---- UnknownType ----
//
// UnknownType represents an unresolvable placeholder. Calling
// WithNullable or Resolve panics (matches Java's throw).

type UnknownType struct{ typeBase }

var unknownSingleton = &UnknownType{typeBase{code: CodeUnknown}}

// NewUnknownType returns the singleton UnknownType.
func NewUnknownType() *UnknownType { return unknownSingleton }

func (t *UnknownType) IsResolved() bool { return false }

func (t *UnknownType) WithNullable(bool) DataType {
	panic(NewError(ErrCodeInternalError, "attempt to set nullability on unknown type"))
}

func (t *UnknownType) Resolve(_ map[string]Named) DataType {
	panic(NewError(ErrCodeInternalError, "cannot resolve unknown type"))
}

func (t *UnknownType) Equal(other DataType) bool {
	_, ok := other.(*UnknownType)
	return ok
}

func (t *UnknownType) String() string { return "???" }
