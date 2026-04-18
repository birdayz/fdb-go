package api

// DataType represents a relational SQL data type. Mirrors Java's
// com.apple.foundationdb.relational.api.metadata.DataType.
//
// Implementations are in this package. The set is closed — matching
// Java's sealed subclasses.
//
// A DataType is:
//   - either flat (primitive) or nested (composite),
//   - maps to a JDBC SQL type code (see JDBCType),
//   - carries a nullability flag,
//   - is immutable,
//   - may be unresolved (see UnresolvedType) until fixed up via Resolve.
type DataType interface {
	// Code returns the type's discriminator.
	Code() Code
	// IsNullable reports whether the type admits NULL.
	IsNullable() bool
	// IsPrimitive reports whether the type is a flat scalar.
	IsPrimitive() bool
	// IsResolved reports whether this type and all of its constituent
	// types are resolved (no UnresolvedType anywhere in the tree).
	IsResolved() bool
	// WithNullable returns a copy of the type with the given nullability.
	// For singleton primitives it returns the corresponding cached
	// instance; for composites it returns a new value.
	WithNullable(isNullable bool) DataType
	// Resolve replaces UnresolvedType references anywhere in this
	// type's tree using the given resolution map. If the type is
	// already resolved, returns the receiver unchanged.
	Resolve(resolution map[string]Named) DataType
	// Equal reports structural equality. Matches Java's equals()
	// override on each concrete type.
	Equal(other DataType) bool
	// String renders the type as human-readable text. The exact format
	// matches Java's toString() — "int", "int ∪ ∅", "[int]", etc.
	String() string
}

// Named is a DataType that carries a user-visible name
// (EnumType, StructType, UnresolvedType).
type Named interface {
	Name() string
}

// CompositeType is a DataType with nested structure
// (ArrayType, StructType).
type CompositeType interface {
	// HasIdenticalStructure reports structural compatibility. Unlike
	// Equal, this treats composites with the same shape as identical
	// regardless of name identity. Default behavior delegates to Equal.
	HasIdenticalStructure(other any) bool
}

// Code is the closed set of discriminators for DataType. Matches
// Java's enum DataType.Code 1:1.
type Code uint8

const (
	CodeBoolean Code = iota
	CodeLong
	CodeInteger
	CodeFloat
	CodeDouble
	CodeString
	CodeBytes
	CodeVersion
	CodeEnum
	CodeUUID
	CodeStruct
	CodeArray
	CodeUnknown
	CodeNull
	CodeVector
)

// String returns the enum name (matches Java's Code.name()).
func (c Code) String() string {
	switch c {
	case CodeBoolean:
		return "BOOLEAN"
	case CodeLong:
		return "LONG"
	case CodeInteger:
		return "INTEGER"
	case CodeFloat:
		return "FLOAT"
	case CodeDouble:
		return "DOUBLE"
	case CodeString:
		return "STRING"
	case CodeBytes:
		return "BYTES"
	case CodeVersion:
		return "VERSION"
	case CodeEnum:
		return "ENUM"
	case CodeUUID:
		return "UUID"
	case CodeStruct:
		return "STRUCT"
	case CodeArray:
		return "ARRAY"
	case CodeUnknown:
		return "UNKNOWN"
	case CodeNull:
		return "NULL"
	case CodeVector:
		return "VECTOR"
	default:
		return "?"
	}
}

// JDBC SQL type constants, mirroring java.sql.Types. Repeated here so
// callers do not need a separate "jdbc" constants package.
//
// Values come from Java's java.sql.Types and must not change.
const (
	JDBCBit           int = -7
	JDBCTinyInt       int = -6
	JDBCSmallInt      int = 5
	JDBCInteger       int = 4
	JDBCBigInt        int = -5
	JDBCFloat         int = 6
	JDBCReal          int = 7
	JDBCDouble        int = 8
	JDBCNumeric       int = 2
	JDBCDecimal       int = 3
	JDBCChar          int = 1
	JDBCVarchar       int = 12
	JDBCLongVarchar   int = -1
	JDBCDate          int = 91
	JDBCTime          int = 92
	JDBCTimestamp     int = 93
	JDBCBinary        int = -2
	JDBCVarBinary     int = -3
	JDBCLongVarBinary int = -4
	JDBCNull          int = 0
	JDBCOther         int = 1111
	JDBCJavaObject    int = 2000
	JDBCStruct        int = 2002
	JDBCArray         int = 2003
	JDBCBoolean       int = 16
)

// JDBCType returns the JDBC-compatible SQL type code for a DataType
// Code. Mirrors Java's DataType.getJdbcSqlCode() table.
func JDBCType(c Code) int {
	switch c {
	case CodeBoolean:
		return JDBCBoolean
	case CodeLong:
		return JDBCBigInt
	case CodeInteger:
		return JDBCInteger
	case CodeFloat:
		return JDBCFloat
	case CodeDouble:
		return JDBCDouble
	case CodeString:
		return JDBCVarchar
	case CodeBytes, CodeVersion:
		return JDBCBinary
	case CodeEnum, CodeUUID, CodeVector, CodeUnknown:
		return JDBCOther
	case CodeStruct:
		return JDBCStruct
	case CodeArray:
		return JDBCArray
	case CodeNull:
		return JDBCNull
	default:
		return JDBCOther
	}
}

// typeBase holds shared fields for every DataType implementation.
// Embedded — not exposed directly. Mirrors Java's abstract DataType
// constructor args (isNullable, isPrimitive, code).
type typeBase struct {
	code        Code
	isNullable  bool
	isPrimitive bool
}

func (b typeBase) Code() Code        { return b.code }
func (b typeBase) IsNullable() bool  { return b.isNullable }
func (b typeBase) IsPrimitive() bool { return b.isPrimitive }

// nullableSuffix is "" for non-nullable and " ∪ ∅" for nullable,
// matching Java's toString() convention on primitive types.
func nullableSuffix(isNullable bool) string {
	if isNullable {
		return " ∪ ∅"
	}
	return ""
}
