//go:generate go run go.uber.org/mock/mockgen -source=$GOFILE -destination=mocks_$GOFILE -package=api

package api

// ResultSet is a cursor over rows produced by a query. Mirrors
// Java's RelationalResultSet — lean Go subset.
//
// The shape is iterator-style: Next advances, then typed accessors
// read the current row's columns. Accessors are 1-indexed (column 1
// is the first column) to match JDBC/Java convention exactly so that
// migrating Java code is frictionless.
type ResultSet interface {
	// Next advances to the next row. Returns true if there is one,
	// false on end-of-rows or error. Call Err() after Next()=false to
	// check for an error vs. a clean end-of-cursor.
	Next() bool
	// Err returns any error that terminated iteration, or nil on a
	// clean EOF. Idiomatic Go — matches sql.Rows.Err() shape.
	Err() error

	// Close releases resources. Safe to call multiple times.
	Close() error

	// MetaData returns the ResultSetMetaData describing columns.
	MetaData() ResultSetMetaData

	// Typed column getters. columnIndex is 1-based per JDBC. Return
	// ErrCodeInvalidColumnReference for out-of-range index and
	// ErrCodeCannotConvertType for unsupported coercion.
	Long(columnIndex int) (int64, error)
	Float(columnIndex int) (float32, error)
	Double(columnIndex int) (float64, error)
	String(columnIndex int) (string, error)
	Bytes(columnIndex int) ([]byte, error)
	Boolean(columnIndex int) (bool, error)
	Object(columnIndex int) (any, error)

	// WasNull reports whether the last column read was SQL NULL.
	// Matches JDBC's wasNull() protocol.
	WasNull() bool

	// Continuation returns the current cursor position, suitable for
	// resumption in a later transaction.
	Continuation() (Continuation, error)

	// LongByName / StringByName / etc. accept a column name instead of
	// a numeric index. Convenience accessors on top of the positional
	// ones — implementations should use MetaData to resolve names.
	LongByName(columnName string) (int64, error)
	StringByName(columnName string) (string, error)
	BytesByName(columnName string) ([]byte, error)
	BooleanByName(columnName string) (bool, error)
	ObjectByName(columnName string) (any, error)
}

// ResultSetMetaData describes the shape of a ResultSet's columns.
// Mirrors Java's RelationalResultSetMetaData — lean Go subset.
//
// columnIndex parameters are 1-based per JDBC convention.
type ResultSetMetaData interface {
	// ColumnCount returns the number of columns.
	ColumnCount() int
	// ColumnName returns the column name at columnIndex.
	ColumnName(columnIndex int) (string, error)
	// ColumnLabel returns the alias used in the query (fallback:
	// ColumnName). Matches JDBC's ResultSetMetaData.getColumnLabel.
	ColumnLabel(columnIndex int) (string, error)
	// ColumnType returns the JDBC type code for columnIndex.
	ColumnType(columnIndex int) (int, error)
	// ColumnTypeName returns the SQL display name ("INTEGER", ...).
	ColumnTypeName(columnIndex int) (string, error)
	// ColumnNullable reports whether the column admits NULL. Value
	// is one of the JDBC ColumnNullable* constants.
	ColumnNullable(columnIndex int) (int, error)
	// ColumnDataType returns the richer DataType for the column.
	// Does not exist in JDBC — relational-specific extension to
	// carry struct/array/enum detail.
	ColumnDataType(columnIndex int) (DataType, error)
}

// ResultSetMetaData column-nullability flags, matching JDBC's
// java.sql.ResultSetMetaData constants.
const (
	// ColumnNoNulls means the column disallows NULL.
	ColumnNoNulls int = 0
	// ColumnNullable means the column allows NULL.
	ColumnNullable int = 1
	// ColumnNullableUnknown means nullability is unknown.
	ColumnNullableUnknown int = 2
)
