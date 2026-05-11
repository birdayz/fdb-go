package executor

import (
	"context"
	"fmt"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
)

// RecordLayerResultSet wraps a RecordCursor[QueryResult] and implements
// api.ResultSet. Mirrors Java's RecordLayerResultSet: Next() advances
// the cursor, typed accessors read from the current row's datum map.
//
// Column metadata is provided at construction time (derived from the
// plan's result type or the schema catalog). Column accessors are
// 1-indexed per JDBC convention.
type RecordLayerResultSet struct {
	ctx      context.Context
	cursor   recordlayer.RecordCursor[QueryResult]
	columns  []ColumnDef
	colIndex map[string]int // upper-case name → 0-based index

	current          QueryResult
	lastContinuation recordlayer.RecordCursorContinuation
	hasRow           bool
	wasNull          bool
	err              error
	closed           bool
}

// ColumnDef describes one column in the result set.
type ColumnDef struct {
	Name     string // key for datum map lookup
	Label    string // display name (alias); empty means use Name
	TypeName string // JDBC type name: BIGINT, STRING, DOUBLE, etc.
	Nullable int    // api.ColumnNoNulls / ColumnNullable / ColumnNullableUnknown
}

// NewRecordLayerResultSet constructs a ResultSet from an executor cursor
// and column definitions.
func NewRecordLayerResultSet(
	ctx context.Context,
	cursor recordlayer.RecordCursor[QueryResult],
	columns []ColumnDef,
) *RecordLayerResultSet {
	idx := make(map[string]int, len(columns))
	for i, c := range columns {
		idx[strings.ToUpper(c.Name)] = i
	}
	return &RecordLayerResultSet{
		ctx:      ctx,
		cursor:   cursor,
		columns:  columns,
		colIndex: idx,
	}
}

func (rs *RecordLayerResultSet) Next() bool {
	if rs.closed || rs.err != nil {
		return false
	}
	result, err := rs.cursor.OnNext(rs.ctx)
	if err != nil {
		rs.err = err
		rs.hasRow = false
		return false
	}
	rs.lastContinuation = result.GetContinuation()
	if !result.HasNext() {
		rs.hasRow = false
		return false
	}
	rs.current = result.GetValue()
	rs.hasRow = true
	return true
}

func (rs *RecordLayerResultSet) Err() error { return rs.err }

func (rs *RecordLayerResultSet) Close() error {
	if rs.closed {
		return nil
	}
	rs.closed = true
	return rs.cursor.Close()
}

func (rs *RecordLayerResultSet) MetaData() api.ResultSetMetaData {
	return &resultSetMetaData{columns: rs.columns}
}

func (rs *RecordLayerResultSet) WasNull() bool { return rs.wasNull }

func (rs *RecordLayerResultSet) columnValue(columnIndex int) (any, error) {
	if !rs.hasRow {
		return nil, api.NewError(api.ErrCodeInvalidCursorState, "ResultSet exhausted or not advanced")
	}
	if columnIndex < 1 || columnIndex > len(rs.columns) {
		return nil, api.NewError(api.ErrCodeInvalidColumnReference,
			fmt.Sprintf("column index %d out of range [1, %d]", columnIndex, len(rs.columns)))
	}
	colName := strings.ToUpper(rs.columns[columnIndex-1].Name)
	m, ok := rs.current.Datum.(map[string]any)
	if !ok {
		rs.wasNull = true
		return nil, nil
	}
	v, exists := m[colName]
	rs.wasNull = !exists || v == nil
	return v, nil
}

func (rs *RecordLayerResultSet) columnValueByName(name string) (any, error) {
	idx, ok := rs.colIndex[strings.ToUpper(name)]
	if !ok {
		return nil, api.NewError(api.ErrCodeInvalidColumnReference,
			fmt.Sprintf("column %q not found", name))
	}
	return rs.columnValue(idx + 1)
}

func (rs *RecordLayerResultSet) Long(columnIndex int) (int64, error) {
	v, err := rs.columnValue(columnIndex)
	if err != nil {
		return 0, err
	}
	return toLong(v)
}

func (rs *RecordLayerResultSet) Float(columnIndex int) (float32, error) {
	v, err := rs.columnValue(columnIndex)
	if err != nil {
		return 0, err
	}
	return toFloat32(v)
}

func (rs *RecordLayerResultSet) Double(columnIndex int) (float64, error) {
	v, err := rs.columnValue(columnIndex)
	if err != nil {
		return 0, err
	}
	return toFloat64Coerce(v)
}

func (rs *RecordLayerResultSet) String(columnIndex int) (string, error) {
	v, err := rs.columnValue(columnIndex)
	if err != nil {
		return "", err
	}
	if v == nil {
		return "", nil
	}
	return fmt.Sprintf("%v", v), nil
}

func (rs *RecordLayerResultSet) Bytes(columnIndex int) ([]byte, error) {
	v, err := rs.columnValue(columnIndex)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, nil
	}
	if b, ok := v.([]byte); ok {
		return b, nil
	}
	return []byte(fmt.Sprintf("%v", v)), nil
}

func (rs *RecordLayerResultSet) Boolean(columnIndex int) (bool, error) {
	v, err := rs.columnValue(columnIndex)
	if err != nil {
		return false, err
	}
	return toBool(v)
}

func (rs *RecordLayerResultSet) Object(columnIndex int) (any, error) {
	return rs.columnValue(columnIndex)
}

func (rs *RecordLayerResultSet) LongByName(name string) (int64, error) {
	v, err := rs.columnValueByName(name)
	if err != nil {
		return 0, err
	}
	return toLong(v)
}

func (rs *RecordLayerResultSet) StringByName(name string) (string, error) {
	v, err := rs.columnValueByName(name)
	if err != nil {
		return "", err
	}
	if v == nil {
		return "", nil
	}
	return fmt.Sprintf("%v", v), nil
}

func (rs *RecordLayerResultSet) BytesByName(name string) ([]byte, error) {
	v, err := rs.columnValueByName(name)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, nil
	}
	if b, ok := v.([]byte); ok {
		return b, nil
	}
	return []byte(fmt.Sprintf("%v", v)), nil
}

func (rs *RecordLayerResultSet) BooleanByName(name string) (bool, error) {
	v, err := rs.columnValueByName(name)
	if err != nil {
		return false, err
	}
	return toBool(v)
}

func (rs *RecordLayerResultSet) ObjectByName(name string) (any, error) {
	return rs.columnValueByName(name)
}

func (rs *RecordLayerResultSet) Continuation() (api.Continuation, error) {
	if rs.lastContinuation == nil {
		return &exhaustedContinuation{}, nil
	}
	bytes, err := rs.lastContinuation.ToBytes()
	if err != nil {
		return nil, err
	}
	if rs.lastContinuation.IsEnd() {
		return &exhaustedContinuation{}, nil
	}
	return &liveContinuation{
		state:  bytes,
		reason: api.ContinuationUserRequested,
	}, nil
}

type liveContinuation struct {
	state  []byte
	reason api.ContinuationReason
}

func (c *liveContinuation) Serialize() []byte              { return c.state }
func (c *liveContinuation) ExecutionState() []byte         { return c.state }
func (c *liveContinuation) Reason() api.ContinuationReason { return c.reason }

func toLong(v any) (int64, error) {
	if v == nil {
		return 0, nil
	}
	switch n := v.(type) {
	case int64:
		return n, nil
	case int32:
		return int64(n), nil
	case int:
		return int64(n), nil
	case float64:
		return int64(n), nil
	case float32:
		return int64(n), nil
	default:
		return 0, api.NewError(api.ErrCodeCannotConvertType,
			fmt.Sprintf("cannot convert %T to LONG", v))
	}
}

func toFloat32(v any) (float32, error) {
	if v == nil {
		return 0, nil
	}
	switch n := v.(type) {
	case float32:
		return n, nil
	case float64:
		return float32(n), nil
	case int64:
		return float32(n), nil
	case int32:
		return float32(n), nil
	case int:
		return float32(n), nil
	default:
		return 0, api.NewError(api.ErrCodeCannotConvertType,
			fmt.Sprintf("cannot convert %T to FLOAT", v))
	}
}

func toFloat64Coerce(v any) (float64, error) {
	if v == nil {
		return 0, nil
	}
	switch n := v.(type) {
	case float64:
		return n, nil
	case float32:
		return float64(n), nil
	case int64:
		return float64(n), nil
	case int32:
		return float64(n), nil
	case int:
		return float64(n), nil
	default:
		return 0, api.NewError(api.ErrCodeCannotConvertType,
			fmt.Sprintf("cannot convert %T to DOUBLE", v))
	}
}

func toBool(v any) (bool, error) {
	if v == nil {
		return false, nil
	}
	switch b := v.(type) {
	case bool:
		return b, nil
	default:
		return false, api.NewError(api.ErrCodeCannotConvertType,
			fmt.Sprintf("cannot convert %T to BOOLEAN", v))
	}
}

// resultSetMetaData provides column metadata for RecordLayerResultSet.
type resultSetMetaData struct {
	columns []ColumnDef
}

func (m *resultSetMetaData) ColumnCount() int { return len(m.columns) }

func (m *resultSetMetaData) ColumnName(columnIndex int) (string, error) {
	if columnIndex < 1 || columnIndex > len(m.columns) {
		return "", api.NewError(api.ErrCodeInvalidColumnReference,
			fmt.Sprintf("column index %d out of range [1, %d]", columnIndex, len(m.columns)))
	}
	return m.columns[columnIndex-1].Name, nil
}

func (m *resultSetMetaData) ColumnLabel(columnIndex int) (string, error) {
	if columnIndex < 1 || columnIndex > len(m.columns) {
		return "", api.NewError(api.ErrCodeInvalidColumnReference,
			fmt.Sprintf("column index %d out of range [1, %d]", columnIndex, len(m.columns)))
	}
	col := m.columns[columnIndex-1]
	if col.Label != "" {
		return col.Label, nil
	}
	return col.Name, nil
}

func (m *resultSetMetaData) ColumnType(columnIndex int) (int, error) {
	name, err := m.ColumnTypeName(columnIndex)
	if err != nil {
		return 0, err
	}
	return jdbcTypeCode(name), nil
}

func (m *resultSetMetaData) ColumnTypeName(columnIndex int) (string, error) {
	if columnIndex < 1 || columnIndex > len(m.columns) {
		return "", api.NewError(api.ErrCodeInvalidColumnReference,
			fmt.Sprintf("column index %d out of range [1, %d]", columnIndex, len(m.columns)))
	}
	return m.columns[columnIndex-1].TypeName, nil
}

func (m *resultSetMetaData) ColumnNullable(columnIndex int) (int, error) {
	if columnIndex < 1 || columnIndex > len(m.columns) {
		return 0, api.NewError(api.ErrCodeInvalidColumnReference,
			fmt.Sprintf("column index %d out of range [1, %d]", columnIndex, len(m.columns)))
	}
	return m.columns[columnIndex-1].Nullable, nil
}

func (m *resultSetMetaData) ColumnDataType(columnIndex int) (api.DataType, error) {
	name, err := m.ColumnTypeName(columnIndex)
	if err != nil {
		return nil, err
	}
	return dataTypeFromName(name), nil
}

func dataTypeFromName(typeName string) api.DataType {
	switch strings.ToUpper(typeName) {
	case "BIGINT":
		return api.NewLongType(true)
	case "INTEGER":
		return api.NewIntegerType(true)
	case "DOUBLE":
		return api.NewDoubleType(true)
	case "FLOAT":
		return api.NewFloatType(true)
	case "BOOLEAN":
		return api.NewBooleanType(true)
	case "STRING", "VARCHAR":
		return api.NewStringType(true)
	case "BYTES", "BINARY", "VARBINARY":
		return api.NewBytesType(true)
	case "DATE":
		return api.NewDateType(true)
	case "TIMESTAMP":
		return api.NewTimestampType(true)
	default:
		return api.NewStringType(true)
	}
}

func jdbcTypeCode(typeName string) int {
	switch strings.ToUpper(typeName) {
	case "BIGINT":
		return api.JDBCBigInt
	case "INTEGER":
		return api.JDBCInteger
	case "DOUBLE":
		return api.JDBCDouble
	case "FLOAT":
		return api.JDBCFloat
	case "BOOLEAN":
		return api.JDBCBoolean
	case "STRING", "VARCHAR":
		return api.JDBCVarchar
	case "BYTES", "BINARY", "VARBINARY":
		return api.JDBCBinary
	case "DATE":
		return api.JDBCDate
	case "TIMESTAMP":
		return api.JDBCTimestamp
	default:
		return api.JDBCOther
	}
}

// exhaustedContinuation is the continuation returned when the cursor
// is exhausted. Matches Java's CursorAfterLast.
type exhaustedContinuation struct{}

func (c *exhaustedContinuation) Serialize() []byte      { return []byte{} }
func (c *exhaustedContinuation) ExecutionState() []byte { return []byte{} }
func (c *exhaustedContinuation) Reason() api.ContinuationReason {
	return api.ContinuationCursorAfterLast
}
