package api

// Array is a typed SQL array column value. Mirrors Java's
// RelationalArray — lean Go shape, no java.sql.Array inheritance.
//
// Array is homogeneous: every element has the declared BaseType().
// Element values returned by Elements follow the driver.Value
// conventions — primitives map to Go-native types, nested STRUCTs
// become Struct, nested arrays become Array.
type Array interface {
	// MetaData returns the array's metadata (base type + element count).
	MetaData() ArrayMetaData

	// BaseType returns the JDBC type code of the array element type
	// (matches java.sql.Array.getBaseType()).
	BaseType() int
	// BaseTypeName returns the display name of the element type.
	BaseTypeName() string

	// Length returns the number of elements.
	Length() int

	// Element returns the element at oneBasedIndex (1-indexed per
	// JDBC convention). Returns ErrCodeInvalidColumnReference if out
	// of range.
	Element(oneBasedIndex int) (any, error)
	// Elements returns every element in order. Convenience wrapper
	// around repeated Element calls.
	Elements() []any
}

// ArrayMetaData describes an Array's element shape. Mirrors Java's
// ArrayMetaData.
type ArrayMetaData interface {
	// ElementType returns the JDBC type code of elements.
	ElementType() int
	// ElementTypeName returns the display name of the element type.
	ElementTypeName() string
	// ElementDataType returns the full DataType — carries struct/enum
	// detail that the JDBC code alone can't convey. Relational-specific
	// extension over JDBC's ArrayMetaData.
	ElementDataType() DataType
	// Nullable reports whether elements may be NULL. One of the
	// ColumnNullable* constants.
	Nullable() int
}
