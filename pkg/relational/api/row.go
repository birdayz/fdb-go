package api

// Row is a single row returned from a scan or query.
//
// Mirrors Java's com.apple.foundationdb.relational.api.Row. Fields
// may themselves be nested Rows (for STRUCT types) or iterables of
// Rows (for ARRAY types).
//
// Accessors return an error (wrapping ErrCodeInvalidColumnReference
// or ErrCodeCannotConvertType) instead of throwing, matching the Go
// error idiom.
type Row interface {
	// NumFields returns the field count.
	NumFields() int

	// Long returns the value at position as an int64. Returns
	// ErrCodeCannotConvertType if the field is not integer-coercible,
	// or ErrCodeInvalidColumnReference if position is out of range.
	Long(position int) (int64, error)

	// Float returns the value at position as a float32.
	Float(position int) (float32, error)

	// Double returns the value at position as a float64.
	Double(position int) (float64, error)

	// String returns the value at position as a string.
	String(position int) (string, error)

	// Bytes returns the value at position as []byte.
	Bytes(position int) ([]byte, error)

	// Row returns the value at position as a nested Row (STRUCT).
	Row(position int) (Row, error)

	// Array returns the value at position as a sequence of Rows
	// (ARRAY). Callers iterate with the returned Iterable.
	Array(position int) (RowIterable, error)

	// Object returns the value at position as an untyped any.
	// Returns ErrCodeInvalidColumnReference if position is out of range.
	Object(position int) (any, error)

	// StartsWith reports whether the receiver begins with the given
	// prefix row (element-wise comparison of the first len(prefix)
	// fields).
	StartsWith(prefix Row) bool

	// Prefix returns a view of this row's first length fields.
	Prefix(length int) Row
}

// RowIterable is a cursor over nested rows (for ARRAY-typed fields).
// Idiomatic Go: return io.EOF-equivalent via Next returning false.
type RowIterable interface {
	// Next advances the cursor. Returns false when exhausted.
	Next() bool
	// Row returns the current row. Undefined if Next returned false.
	Row() Row
	// Close releases any resources. Idempotent.
	Close() error
}
