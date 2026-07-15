package executor

import (
	"context"
	"fmt"
	"strings"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
)

// resultValueString renders a column value as a string for the api.ResultSet
// String()/Bytes() accessors (Java ResultSet.getString parity). A UUID flows
// through the engine as a neutral [16]byte (RFC-162); a bare %v would print a Go
// array literal, so render the canonical 36-char form — the same string the
// database/sql driver boundary (materializeDriverValue) surfaces via Object().
func resultValueString(v any) string {
	switch u := v.(type) {
	case [16]byte:
		return tuple.UUID(u).String()
	case tuple.UUID:
		return u.String()
	default:
		return fmt.Sprintf("%v", v)
	}
}

// RecordLayerResultSet wraps a RecordCursor[QueryResult] and implements
// api.ResultSet. Mirrors Java's RecordLayerResultSet: Next() advances
// the cursor, typed accessors read from the current row's positional slots.
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
	lastNoNextReason recordlayer.NoNextReason
	hasRow           bool
	wasNull          bool
	err              error
	closed           bool

	// posAlignType/posAligned memoize positionalAligned's verdict per row TYPE
	// (row types are plan-invariant and pointer-shared across a cursor's rows —
	// projType, ordinalJoinBirth.OutputType, positionalTypeCache — so the
	// per-column name comparison runs once, not per row).
	posAlignType *values.RecordType
	posAligned   bool
}

// ColumnDef describes one column in the result set.
type ColumnDef struct {
	Name     string // output column name (positional slot / by-name lookup key)
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
	rs.lastNoNextReason = result.GetNoNextReason()
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
	// RFC-173: the ordinal-model positional row is the SOLE runtime output row —
	// read column i from slot i-1. positionalAligned proves the slots are parallel
	// to this result set's columns (count + per-slot name match, with computed /
	// aggregate renderings aligned by ordinal since their two naming authorities
	// disagree on spelling). Ordinal reads are also what makes duplicate-name
	// `SELECT *` correct: an ordinal join's output has two same-named columns per
	// leg pair (bare display names, distinct by ordinal); the positional row keeps
	// each leg's value in its own slot, which a last-wins name map could not.
	if row := rs.current.Positional; row != nil && rs.positionalAligned(row) {
		v, _ := row.Get(columnIndex - 1)
		rs.wasNull = v == nil
		return v, nil
	}
	// Correct-or-loud: a materialized row with no aligned positional output row is
	// a producer that failed to emit its ordinal output — a Go planner/executor
	// bug, never a name->NULL to paper over.
	return nil, api.NewError(api.ErrCodeInternalError,
		fmt.Sprintf("RFC-173: result row carries no positional output row aligned to column %q (%d columns) — the plan's top operator did not emit an ordinal output row",
			rs.columns[columnIndex-1].Name, len(rs.columns)))
}

// positionalAligned reports whether a positional row's slots are parallel to
// this result set's columns: one slot per column AND every slot's type name
// equals the column's unqualified display name (the alias/Label when set, else
// the bare leaf of Name), case-insensitively. The guard is deliberately
// all-or-nothing: a single mismatch means the positional row is NOT this
// result set's output shape (a source row under aliased output, a permuted
// union leg, a covering superset) and every read goes loud.
//
// Both sides are compared on their BARE LEAF (qualifier stripped): a projection
// over a gated ordinal join names its output positional slots by the qualified
// output name (ProjectionColumnName → "C.NAME"), while the result-set column
// display name is the bare leaf ("NAME"). The two denote the SAME output column
// and the read is purely by ordinal (slot i ↔ column i, both in output order) —
// so leaf-name equality is the correct structural sanity check. Duplicate leaf
// names (`SELECT c.id, o.id` → [ID, ID]) stay safe: alignment is by ordinal, and
// each column reads its own slot regardless of the shared name.
func (rs *RecordLayerResultSet) positionalAligned(row *PositionalRow) bool {
	if row.Type == nil {
		return false
	}
	if row.Type == rs.posAlignType {
		return rs.posAligned // memoized: row types are pointer-shared per plan
	}
	aligned := len(row.Type.Fields) == len(rs.columns) && len(row.Slots) == len(rs.columns)
	if aligned {
		for i, f := range row.Type.Fields {
			disp := columnDisplayName(rs.columns[i])
			// A column whose display name is NOT a plain (dotted) identifier has no
			// canonical user-facing spelling to match — it is a synthesized rendering
			// of a computed expression or an aggregate:
			//   - the ANONYMOUS `_i` placeholder deriveProjectionColumnDef assigns an
			//     unaliased non-field projection (`SELECT UPPER(x)`), while the emitted
			//     slot is named by ProjectionColumnName ("UPPER(X)");
			//   - an AGGREGATE column ("SUM(QTY*UNIT_PRICE)", "MAX(X.COL2)", a `... AS
			//     revenue` alias) whose ColumnDef name and positional slot name are
			//     produced by two different rendering authorities that disagree on
			//     spacing, internal qualifiers, or alias-vs-rendering.
			// In every such case the slot IS this output column: both the positional
			// slots and the result-set columns are built in output/projection order, so
			// slot i ↔ column i by construction. Align by ordinal (the count guard and
			// the plain-identifier name checks below still hold for every other slot —
			// so a permuted union leg or a source row under aliased output, whose
			// columns ARE plain identifiers, is still rejected).
			if isAnonymousColumnName(disp) || !isPlainColumnRef(disp) || !isPlainColumnRef(f.Name) {
				continue
			}
			// Align when the slot name equals the display name OUTRIGHT (a quoted
			// output alias may itself contain a dot — `SELECT v AS "A.B"` emits slot
			// "A.B" and label "A.B", which must NOT be leaf-stripped to "B"), or when
			// the slot's bare leaf equals a bare display ("C.NAME" slot ↔ "NAME"
			// label). Stripping BOTH sides would falsely align permuted qualifiers
			// ("X.NAME" vs "Y.NAME"), so only the slot side is stripped.
			if !strings.EqualFold(f.Name, disp) && !strings.EqualFold(bareLeafName(f.Name), disp) {
				aligned = false
				break
			}
		}
	}
	rs.posAlignType = row.Type
	rs.posAligned = aligned
	return aligned
}

// bareLeafName strips a leading qualifier from a positional slot's name
// ("C.NAME" → "NAME"), the same reduction columnDisplayName applies to a
// column's Name. A name with no qualifier (an aggregate output "COUNT(*)", a
// bare column) is returned unchanged.
func bareLeafName(s string) string {
	if dot := strings.LastIndex(s, "."); dot >= 0 {
		return s[dot+1:]
	}
	return s
}

// isPlainColumnRef reports whether s is a plain (optionally dotted) SQL column
// reference — an identifier of letters/digits/underscore, e.g. "NAME", "C.ID",
// "CUSTOMER_ID". Anything containing a paren, space, arithmetic operator, comma,
// etc. is a SYNTHESIZED rendering of a computed expression or an aggregate
// ("SUM(QTY * PRICE)", "UPPER(X)", "(A + B)"), which has no canonical spelling to
// name-match on — such columns align by ordinal instead. A plain reference is the
// only kind whose two naming authorities are guaranteed to agree.
func isPlainColumnRef(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

// isAnonymousColumnName reports whether s is the positional placeholder `_i`
// (an underscore followed by one-or-more digits) that deriveProjectionColumnDef
// assigns to an unaliased non-field projection. Such a column has no user-facing
// name, so a positional slot at the same ordinal aligns by position alone.
func isAnonymousColumnName(s string) bool {
	if len(s) < 2 || s[0] != '_' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// columnDisplayName is the column's unqualified user-visible name: the Label
// (alias) when set, else Name's bare leaf ("A.ID" → "ID" — a column Name may be
// leg-qualified, but a positional slot is named by its bare display name).
func columnDisplayName(c ColumnDef) string {
	if c.Label != "" {
		return c.Label
	}
	if dot := strings.LastIndex(c.Name, "."); dot >= 0 {
		return c.Name[dot+1:]
	}
	return c.Name
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
	return resultValueString(v), nil
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
	return []byte(resultValueString(v)), nil
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
	return resultValueString(v), nil
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
	return []byte(resultValueString(v)), nil
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

// GetContinuation returns the raw cursor continuation from the last
// Next() call. Used by the paginating execution loop to resume across
// FDB transactions.
func (rs *RecordLayerResultSet) GetContinuation() recordlayer.RecordCursorContinuation {
	return rs.lastContinuation
}

// GetNoNextReason returns the NoNextReason from the last Next() call. This is the
// AUTHORITATIVE exhaustion signal (SourceExhausted ⇔ end-of-results), and distinguishes a
// non-terminal out-of-band stop (scan/time/byte limit) from a clean ReturnLimitReached/exhaustion
// when the continuation has nil bytes — see RFC-127 (Java carries noNextReason as a first-class field
// for exactly this; its nil-byte START continuation is otherwise ambiguous with end).
func (rs *RecordLayerResultSet) GetNoNextReason() recordlayer.NoNextReason {
	return rs.lastNoNextReason
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
