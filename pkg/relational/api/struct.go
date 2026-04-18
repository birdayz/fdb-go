package api

// Struct is a SQL STRUCT column value. Mirrors Java's RelationalStruct
// — lean Go shape, no java.sql.Struct inheritance.
//
// A Struct holds named + ordered attributes (matching StructType
// metadata). Accessors are 1-indexed per JDBC convention.
type Struct interface {
	// MetaData returns the struct's metadata.
	MetaData() StructMetaData

	// AttributeCount returns the number of attributes without the
	// error return that the getter methods need.
	AttributeCount() int

	// Attribute returns the attribute value at oneBasedIndex (1-indexed
	// per JDBC convention). Returns ErrCodeInvalidColumnReference if
	// out of range.
	Attribute(oneBasedIndex int) (any, error)
	// AttributeByName looks up by name (case-sensitive; uppercasing
	// is caller's responsibility).
	AttributeByName(name string) (any, error)

	// Attributes returns every attribute in declared order.
	Attributes() []any
}

// StructMetaData describes a Struct's shape. Mirrors Java's
// StructMetaData.
type StructMetaData interface {
	// TypeName returns the struct type's user-visible name.
	TypeName() string
	// AttributeCount returns the number of attributes.
	AttributeCount() int
	// AttributeName returns the name of the attribute at oneBasedIndex.
	AttributeName(oneBasedIndex int) (string, error)
	// AttributeType returns the JDBC type code of the attribute.
	AttributeType(oneBasedIndex int) (int, error)
	// AttributeTypeName returns the display name of the attribute
	// type (e.g. "INTEGER", "STRING").
	AttributeTypeName(oneBasedIndex int) (string, error)
	// AttributeDataType returns the full DataType for richer struct/
	// array detail (relational-specific extension over JDBC).
	AttributeDataType(oneBasedIndex int) (DataType, error)
	// AttributeNullable reports whether the attribute admits NULL.
	// One of the ColumnNullable* constants.
	AttributeNullable(oneBasedIndex int) (int, error)
}
