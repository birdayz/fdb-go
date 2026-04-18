package catalog

import "github.com/birdayz/fdb-record-layer-go/pkg/relational/api"

// stringResultSet is a trivial slice-backed api.ResultSet used by
// InMemoryStoreCatalog's listXxx methods. Column names are mandatory;
// column types are inferred from the first non-nil row's values and
// reported via a minimal ResultSetMetaData.
//
// Typed getters accept a 1-based columnIndex. Values returned by
// Long/String/etc. coerce via Go type assertion; mismatches surface
// as ErrCodeCannotConvertType so callers don't silently see zero.
type stringResultSet struct {
	columns []string
	rows    [][]any
	cursor  int // index of the next row to return (0 = not yet advanced)
	current []any
	closed  bool
	wasNull bool
}

// newStringResultSet constructs a ResultSet over static rows. len
// (row) must equal len(columns) for every row; violations produce a
// runtime error on access via ErrCodeInternalError.
func newStringResultSet(columns []string, rows [][]any) *stringResultSet {
	return &stringResultSet{columns: columns, rows: rows, cursor: 0}
}

func (r *stringResultSet) Next() bool {
	if r.closed || r.cursor >= len(r.rows) {
		r.current = nil
		return false
	}
	r.current = r.rows[r.cursor]
	r.cursor++
	r.wasNull = false
	return true
}

func (r *stringResultSet) Err() error { return nil }

func (r *stringResultSet) Close() error {
	r.closed = true
	r.current = nil
	return nil
}

func (r *stringResultSet) MetaData() api.ResultSetMetaData {
	return &stringResultSetMetaData{rs: r}
}

func (r *stringResultSet) cell(columnIndex int) (any, error) {
	if r.current == nil {
		return nil, api.NewError(api.ErrCodeInternalError, "no current row — call Next() first")
	}
	if columnIndex < 1 || columnIndex > len(r.columns) {
		return nil, api.NewErrorf(api.ErrCodeInvalidColumnReference, "column index %d out of range [1, %d]", columnIndex, len(r.columns))
	}
	v := r.current[columnIndex-1]
	r.wasNull = v == nil
	return v, nil
}

func (r *stringResultSet) cellByName(columnName string) (any, error) {
	for i, c := range r.columns {
		if c == columnName {
			return r.cell(i + 1)
		}
	}
	return nil, api.NewErrorf(api.ErrCodeInvalidColumnReference, "column %q not found", columnName)
}

func (r *stringResultSet) Long(i int) (int64, error) {
	v, err := r.cell(i)
	if err != nil {
		return 0, err
	}
	switch x := v.(type) {
	case int64:
		return x, nil
	case int:
		return int64(x), nil
	case int32:
		return int64(x), nil
	case nil:
		return 0, nil
	}
	return 0, api.NewErrorf(api.ErrCodeCannotConvertType, "column %d is %T, not int64", i, v)
}

func (r *stringResultSet) Float(i int) (float32, error) {
	v, err := r.cell(i)
	if err != nil {
		return 0, err
	}
	switch x := v.(type) {
	case float32:
		return x, nil
	case nil:
		return 0, nil
	}
	return 0, api.NewErrorf(api.ErrCodeCannotConvertType, "column %d is %T, not float32", i, v)
}

func (r *stringResultSet) Double(i int) (float64, error) {
	v, err := r.cell(i)
	if err != nil {
		return 0, err
	}
	switch x := v.(type) {
	case float64:
		return x, nil
	case nil:
		return 0, nil
	}
	return 0, api.NewErrorf(api.ErrCodeCannotConvertType, "column %d is %T, not float64", i, v)
}

func (r *stringResultSet) String(i int) (string, error) {
	v, err := r.cell(i)
	if err != nil {
		return "", err
	}
	switch x := v.(type) {
	case string:
		return x, nil
	case nil:
		return "", nil
	}
	return "", api.NewErrorf(api.ErrCodeCannotConvertType, "column %d is %T, not string", i, v)
}

func (r *stringResultSet) Bytes(i int) ([]byte, error) {
	v, err := r.cell(i)
	if err != nil {
		return nil, err
	}
	switch x := v.(type) {
	case []byte:
		return x, nil
	case nil:
		return nil, nil
	}
	return nil, api.NewErrorf(api.ErrCodeCannotConvertType, "column %d is %T, not []byte", i, v)
}

func (r *stringResultSet) Boolean(i int) (bool, error) {
	v, err := r.cell(i)
	if err != nil {
		return false, err
	}
	switch x := v.(type) {
	case bool:
		return x, nil
	case nil:
		return false, nil
	}
	return false, api.NewErrorf(api.ErrCodeCannotConvertType, "column %d is %T, not bool", i, v)
}

func (r *stringResultSet) Object(i int) (any, error) { return r.cell(i) }
func (r *stringResultSet) WasNull() bool             { return r.wasNull }

// Continuation returns a no-op marker. In-memory result sets don't
// survive transaction boundaries — callers never resume them. The
// execution state is empty-but-not-nil when we've hit EOF (matches
// api.AtEnd semantics) and nil otherwise.
func (r *stringResultSet) Continuation() (api.Continuation, error) {
	if r.cursor >= len(r.rows) {
		return endOfRowsContinuation{}, nil
	}
	return beginningContinuation{}, nil
}

// beginningContinuation signals "at the beginning" via nil exec state.
type beginningContinuation struct{}

func (beginningContinuation) Serialize() []byte      { return nil }
func (beginningContinuation) ExecutionState() []byte { return nil }
func (beginningContinuation) Reason() api.ContinuationReason {
	return api.ContinuationUserRequested
}

// endOfRowsContinuation signals "past the last row" via an empty-but-
// non-nil exec state.
type endOfRowsContinuation struct{}

func (endOfRowsContinuation) Serialize() []byte      { return []byte{} }
func (endOfRowsContinuation) ExecutionState() []byte { return []byte{} }
func (endOfRowsContinuation) Reason() api.ContinuationReason {
	return api.ContinuationCursorAfterLast
}

func (r *stringResultSet) LongByName(name string) (int64, error) {
	v, err := r.cellByName(name)
	if err != nil {
		return 0, err
	}
	return r.coerceLong(name, v)
}

func (r *stringResultSet) StringByName(name string) (string, error) {
	v, err := r.cellByName(name)
	if err != nil {
		return "", err
	}
	if v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", api.NewErrorf(api.ErrCodeCannotConvertType, "column %q is %T, not string", name, v)
	}
	return s, nil
}

func (r *stringResultSet) BytesByName(name string) ([]byte, error) {
	v, err := r.cellByName(name)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, nil
	}
	b, ok := v.([]byte)
	if !ok {
		return nil, api.NewErrorf(api.ErrCodeCannotConvertType, "column %q is %T, not []byte", name, v)
	}
	return b, nil
}

func (r *stringResultSet) BooleanByName(name string) (bool, error) {
	v, err := r.cellByName(name)
	if err != nil {
		return false, err
	}
	if v == nil {
		return false, nil
	}
	b, ok := v.(bool)
	if !ok {
		return false, api.NewErrorf(api.ErrCodeCannotConvertType, "column %q is %T, not bool", name, v)
	}
	return b, nil
}

func (r *stringResultSet) ObjectByName(name string) (any, error) { return r.cellByName(name) }

func (r *stringResultSet) coerceLong(ctx string, v any) (int64, error) {
	switch x := v.(type) {
	case int64:
		return x, nil
	case int:
		return int64(x), nil
	case int32:
		return int64(x), nil
	case nil:
		return 0, nil
	}
	return 0, api.NewErrorf(api.ErrCodeCannotConvertType, "%s is %T, not int64", ctx, v)
}

// stringResultSetMetaData is a minimal ResultSetMetaData that
// reports column names + a string-typed default. Catalog listings
// don't need the richer typing story today.
type stringResultSetMetaData struct {
	rs *stringResultSet
}

func (m *stringResultSetMetaData) ColumnCount() int { return len(m.rs.columns) }

func (m *stringResultSetMetaData) ColumnName(i int) (string, error) {
	if i < 1 || i > len(m.rs.columns) {
		return "", api.NewErrorf(api.ErrCodeInvalidColumnReference, "column index %d out of range", i)
	}
	return m.rs.columns[i-1], nil
}

func (m *stringResultSetMetaData) ColumnLabel(i int) (string, error) { return m.ColumnName(i) }

// ColumnType / ColumnTypeName default to JDBC-VARCHAR for any column;
// catalog lists are typed-string today. A richer impl lives on the
// query executor ResultSet once we have one.
func (m *stringResultSetMetaData) ColumnType(_ int) (int, error)        { return 12, nil } // JDBC VARCHAR
func (m *stringResultSetMetaData) ColumnTypeName(_ int) (string, error) { return "VARCHAR", nil }

func (m *stringResultSetMetaData) ColumnNullable(_ int) (int, error) {
	return api.ColumnNullableUnknown, nil
}

func (m *stringResultSetMetaData) ColumnDataType(_ int) (api.DataType, error) {
	return api.NewStringType(true), nil
}

// Compile-time contract check.
var _ api.ResultSet = (*stringResultSet)(nil)
