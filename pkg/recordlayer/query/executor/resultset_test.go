package executor

import (
	"context"
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
)

// posResult builds a QueryResult carrying the ordinal output row the executor
// produces at runtime: one slot per column, in column order, named by the
// column's name, valued from m (nil when absent — a SQL NULL). There is no
// name-keyed row on the read path — the reader materializes solely from this
// positional row; these reader-unit tests feed it directly instead of a name map.
func posResult(cols []ColumnDef, m map[string]any) QueryResult {
	fields := make([]values.Field, len(cols))
	slots := make([]any, len(cols))
	for i, c := range cols {
		value := m[c.Name]
		fields[i] = values.Field{Name: c.Name, FieldType: resultSetTestFieldType(c, value), Ordinal: i}
		slots[i] = value
	}
	return QueryResult{Positional: &PositionalRow{Type: &values.RecordType{Fields: fields}, Slots: slots}}
}

// resultSetTestFieldType keeps the reader fixtures honest: their ColumnDef is
// the declared result metadata, so the positional row must carry that same
// concrete type rather than an unrelated UnknownType placeholder. A nil sample
// widens the fixture field because that row demonstrably permits SQL NULL even
// when a particular metadata test intentionally supplied the default
// ColumnNoNulls flag.
func resultSetTestFieldType(column ColumnDef, sample any) values.Type {
	nullable := column.Nullable != api.ColumnNoNulls || sample == nil
	switch column.TypeName {
	case "BIGINT":
		if nullable {
			return values.NullableLong
		}
		return values.NotNullLong
	case "STRING":
		if nullable {
			return values.NullableString
		}
		return values.NotNullString
	case "DOUBLE":
		if nullable {
			return values.NullableDouble
		}
		return values.NotNullDouble
	case "BOOLEAN":
		if nullable {
			return values.NullableBoolean
		}
		return values.NotNullBoolean
	case "BYTES":
		if nullable {
			return values.NullableBytes
		}
		return values.NotNullBytes
	default:
		panic("resultset test fixture has no exact type for " + column.TypeName)
	}
}

func TestPosResult_CarriesDeclaredExactTypes(t *testing.T) {
	t.Parallel()
	result := posResult(
		[]ColumnDef{
			{Name: "ID", TypeName: "BIGINT"},
			{Name: "NAME", TypeName: "STRING", Nullable: api.ColumnNullable},
			{Name: "SCORE", TypeName: "DOUBLE"},
			{Name: "ACTIVE", TypeName: "BOOLEAN"},
			{Name: "PAYLOAD", TypeName: "BYTES"},
		},
		map[string]any{
			"ID":      int64(1),
			"NAME":    "alice",
			"SCORE":   nil,
			"ACTIVE":  true,
			"PAYLOAD": []byte{1},
		},
	)
	want := []values.Type{
		values.NotNullLong,
		values.NullableString,
		values.NullableDouble,
		values.NotNullBoolean,
		values.NotNullBytes,
	}
	for i, field := range result.Positional.Type.Fields {
		if field.FieldType == nil || !field.FieldType.Equals(want[i]) {
			t.Fatalf("field %d type = %v, want exact %v", i, field.FieldType, want[i])
		}
		if field.FieldType.Equals(values.UnknownType) {
			t.Fatalf("field %d regressed to UnknownType", i)
		}
	}
}

func TestResultSet_IterateRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cols := []ColumnDef{
		{Name: "ID", TypeName: "BIGINT"},
		{Name: "NAME", TypeName: "STRING"},
	}
	cursor := recordlayer.FromList([]QueryResult{
		posResult(cols, map[string]any{"ID": int64(1), "NAME": "alice"}),
		posResult(cols, map[string]any{"ID": int64(2), "NAME": "bob"}),
	})

	rs := NewRecordLayerResultSet(ctx, cursor, cols)
	defer rs.Close()

	count := 0
	for rs.Next() {
		count++
		id, err := rs.Long(1)
		if err != nil {
			t.Fatalf("Long: %v", err)
		}
		name, err := rs.String(2)
		if err != nil {
			t.Fatalf("String: %v", err)
		}
		if count == 1 && (id != 1 || name != "alice") {
			t.Errorf("row 1: id=%d name=%s, want 1/alice", id, name)
		}
		if count == 2 && (id != 2 || name != "bob") {
			t.Errorf("row 2: id=%d name=%s, want 2/bob", id, name)
		}
	}
	if rs.Err() != nil {
		t.Fatalf("Err: %v", rs.Err())
	}
	if count != 2 {
		t.Fatalf("got %d rows, want 2", count)
	}
}

func TestResultSet_ByName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cols := []ColumnDef{
		{Name: "PRICE", TypeName: "BIGINT"},
		{Name: "ACTIVE", TypeName: "BOOLEAN"},
	}
	cursor := recordlayer.FromList([]QueryResult{
		posResult(cols, map[string]any{"PRICE": int64(42), "ACTIVE": true}),
	})

	rs := NewRecordLayerResultSet(ctx, cursor, cols)
	defer rs.Close()

	if !rs.Next() {
		t.Fatal("expected a row")
	}

	price, err := rs.LongByName("PRICE")
	if err != nil {
		t.Fatalf("LongByName: %v", err)
	}
	if price != 42 {
		t.Errorf("PRICE = %d, want 42", price)
	}

	active, err := rs.BooleanByName("ACTIVE")
	if err != nil {
		t.Fatalf("BooleanByName: %v", err)
	}
	if !active {
		t.Error("ACTIVE = false, want true")
	}
}

func TestResultSet_WasNull(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cols := []ColumnDef{
		{Name: "ID", TypeName: "BIGINT"},
		{Name: "NAME", TypeName: "STRING"},
	}
	cursor := recordlayer.FromList([]QueryResult{
		posResult(cols, map[string]any{"ID": int64(1)}),
	})

	rs := NewRecordLayerResultSet(ctx, cursor, cols)
	defer rs.Close()

	if !rs.Next() {
		t.Fatal("expected a row")
	}

	_, _ = rs.Long(1)
	if rs.WasNull() {
		t.Error("WasNull should be false for ID=1")
	}

	_, _ = rs.String(2)
	if !rs.WasNull() {
		t.Error("WasNull should be true for missing NAME")
	}
}

func TestResultSet_NullAlternation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cols := []ColumnDef{
		{Name: "PK", TypeName: "BIGINT"},
		{Name: "T1", TypeName: "BIGINT"},
		{Name: "T2", TypeName: "STRING"},
		{Name: "T3", TypeName: "DOUBLE"},
		{Name: "T4", TypeName: "BYTES"},
	}
	cursor := recordlayer.FromList([]QueryResult{
		posResult(cols, map[string]any{"PK": int64(100)}),
	})

	rs := NewRecordLayerResultSet(ctx, cursor, cols)
	defer rs.Close()
	if !rs.Next() {
		t.Fatal("expected a row")
	}

	pk, _ := rs.LongByName("PK")
	if pk != 100 || rs.WasNull() {
		t.Errorf("PK: got %d wasNull=%v", pk, rs.WasNull())
	}

	t1, _ := rs.LongByName("T1")
	if t1 != 0 || !rs.WasNull() {
		t.Errorf("T1 null: got %d wasNull=%v, want 0/true", t1, rs.WasNull())
	}

	pk, _ = rs.LongByName("PK")
	if pk != 100 || rs.WasNull() {
		t.Errorf("PK again: got %d wasNull=%v", pk, rs.WasNull())
	}

	t2, _ := rs.StringByName("T2")
	if t2 != "" || !rs.WasNull() {
		t.Errorf("T2 null: got %q wasNull=%v, want empty/true", t2, rs.WasNull())
	}

	pk, _ = rs.LongByName("PK")
	if pk != 100 || rs.WasNull() {
		t.Errorf("PK again: got %d wasNull=%v", pk, rs.WasNull())
	}

	t3, _ := rs.Double(4)
	if t3 != 0 || !rs.WasNull() {
		t.Errorf("T3 null: got %v wasNull=%v, want 0/true", t3, rs.WasNull())
	}

	pk, _ = rs.LongByName("PK")
	if pk != 100 || rs.WasNull() {
		t.Errorf("PK again: got %d wasNull=%v", pk, rs.WasNull())
	}

	t4, _ := rs.BytesByName("T4")
	if t4 != nil || !rs.WasNull() {
		t.Errorf("T4 null: got %v wasNull=%v, want nil/true", t4, rs.WasNull())
	}
}

func TestResultSet_ColumnOutOfRange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cursor := recordlayer.FromList([]QueryResult{
		dmap(map[string]any{"X": int64(1)}),
	})
	cols := []ColumnDef{{Name: "X", TypeName: "BIGINT"}}

	rs := NewRecordLayerResultSet(ctx, cursor, cols)
	defer rs.Close()

	if !rs.Next() {
		t.Fatal("expected a row")
	}

	_, err := rs.Long(0)
	if err == nil {
		t.Fatal("expected error for column index 0")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || string(apiErr.Code) != string(api.ErrCodeInvalidColumnReference) {
		t.Errorf("error code = %v, want ErrCodeInvalidColumnReference", err)
	}

	_, err = rs.Long(2)
	if err == nil {
		t.Fatal("expected error for column index 2")
	}
}

func TestResultSet_BeforeAdvance(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cursor := recordlayer.FromList([]QueryResult{
		dmap(map[string]any{"X": int64(1)}),
	})
	cols := []ColumnDef{{Name: "X", TypeName: "BIGINT"}}

	rs := NewRecordLayerResultSet(ctx, cursor, cols)
	defer rs.Close()

	_, err := rs.Long(1)
	if err == nil {
		t.Fatal("expected error before Next()")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || string(apiErr.Code) != string(api.ErrCodeInvalidCursorState) {
		t.Errorf("error code = %v, want ErrCodeInvalidCursorState", err)
	}
}

func TestResultSet_MetaData(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cursor := recordlayer.Empty[QueryResult]()
	cols := []ColumnDef{
		{Name: "ID", TypeName: "BIGINT", Nullable: api.ColumnNoNulls},
		{Name: "NAME", TypeName: "STRING", Nullable: api.ColumnNullable},
	}

	rs := NewRecordLayerResultSet(ctx, cursor, cols)
	defer rs.Close()

	md := rs.MetaData()
	if md.ColumnCount() != 2 {
		t.Fatalf("ColumnCount = %d, want 2", md.ColumnCount())
	}

	name, err := md.ColumnName(1)
	if err != nil {
		t.Fatalf("ColumnName: %v", err)
	}
	if name != "ID" {
		t.Errorf("ColumnName(1) = %q, want ID", name)
	}

	typeName, err := md.ColumnTypeName(2)
	if err != nil {
		t.Fatalf("ColumnTypeName: %v", err)
	}
	if typeName != "STRING" {
		t.Errorf("ColumnTypeName(2) = %q, want STRING", typeName)
	}

	nullable, err := md.ColumnNullable(1)
	if err != nil {
		t.Fatalf("ColumnNullable: %v", err)
	}
	if nullable != api.ColumnNoNulls {
		t.Errorf("ColumnNullable(1) = %d, want ColumnNoNulls", nullable)
	}

	typeCode, err := md.ColumnType(1)
	if err != nil {
		t.Fatalf("ColumnType: %v", err)
	}
	if typeCode != api.JDBCBigInt {
		t.Errorf("ColumnType(1) = %d, want JDBCBigInt(%d)", typeCode, api.JDBCBigInt)
	}
}

func TestResultSet_TypeCoercion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cols := []ColumnDef{
		{Name: "INT_VAL", TypeName: "BIGINT"},
		{Name: "FLOAT_VAL", TypeName: "DOUBLE"},
		{Name: "BOOL_VAL", TypeName: "BOOLEAN"},
		{Name: "STRING_VAL", TypeName: "STRING"},
	}
	cursor := recordlayer.FromList([]QueryResult{
		posResult(cols, map[string]any{
			"INT_VAL":    int64(42),
			"FLOAT_VAL":  float64(3.14),
			"BOOL_VAL":   true,
			"STRING_VAL": "hello",
		}),
	})

	rs := NewRecordLayerResultSet(ctx, cursor, cols)
	defer rs.Close()

	if !rs.Next() {
		t.Fatal("expected a row")
	}

	d, err := rs.Double(1)
	if err != nil {
		t.Fatalf("Double from int: %v", err)
	}
	if d != 42.0 {
		t.Errorf("Double(1) = %v, want 42.0", d)
	}

	l, err := rs.Long(2)
	if err != nil {
		t.Fatalf("Long from float: %v", err)
	}
	if l != 3 {
		t.Errorf("Long(2) = %d, want 3 (truncated)", l)
	}

	s, err := rs.String(3)
	if err != nil {
		t.Fatalf("String from bool: %v", err)
	}
	if s != "true" {
		t.Errorf("String(3) = %q, want 'true'", s)
	}

	obj, err := rs.Object(4)
	if err != nil {
		t.Fatalf("Object: %v", err)
	}
	if obj != "hello" {
		t.Errorf("Object(4) = %v, want 'hello'", obj)
	}
}

func TestResultSet_CoercionMatrix(t *testing.T) {
	t.Parallel()

	type coercionCase struct {
		name    string
		value   any
		getLong bool
		longVal int64
		getFlt  bool
		fltVal  float32
		getDbl  bool
		dblVal  float64
		getBool bool
		boolVal bool
		getStr  bool
		strVal  string
	}

	cases := []coercionCase{
		{name: "nil", value: nil, getLong: true, longVal: 0, getFlt: true, fltVal: 0, getDbl: true, dblVal: 0, getBool: true, boolVal: false, getStr: true, strVal: ""},
		{name: "bool_true", value: true, getLong: false, getFlt: false, getDbl: false, getBool: true, boolVal: true, getStr: true, strVal: "true"},
		{name: "bool_false", value: false, getLong: false, getFlt: false, getDbl: false, getBool: true, boolVal: false, getStr: true, strVal: "false"},
		{name: "int64_42", value: int64(42), getLong: true, longVal: 42, getFlt: true, fltVal: 42, getDbl: true, dblVal: 42, getBool: false, getStr: true, strVal: "42"},
		{name: "int32_7", value: int32(7), getLong: true, longVal: 7, getFlt: true, fltVal: 7, getDbl: true, dblVal: 7, getBool: false, getStr: true, strVal: "7"},
		{name: "float64_3.14", value: float64(3.14), getLong: true, longVal: 3, getFlt: true, fltVal: 3.14, getDbl: true, dblVal: 3.14, getBool: false, getStr: true, strVal: "3.14"},
		{name: "float32_1.5", value: float32(1.5), getLong: true, longVal: 1, getFlt: true, fltVal: 1.5, getDbl: true, dblVal: 1.5, getBool: false, getStr: true, strVal: "1.5"},
		{name: "string_hello", value: "hello", getLong: false, getFlt: false, getDbl: false, getBool: false, getStr: true, strVal: "hello"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			cols := []ColumnDef{{Name: "V", TypeName: "STRING"}}
			cursor := recordlayer.FromList([]QueryResult{
				posResult(cols, map[string]any{"V": tc.value}),
			})
			rs := NewRecordLayerResultSet(ctx, cursor, cols)
			defer rs.Close()
			if !rs.Next() {
				t.Fatal("expected a row")
			}

			l, err := rs.Long(1)
			if tc.getLong {
				if err != nil {
					t.Fatalf("Long: %v", err)
				}
				if l != tc.longVal {
					t.Errorf("Long = %d, want %d", l, tc.longVal)
				}
			} else {
				assertCannotConvert(t, err, "Long")
			}

			f, err := rs.Float(1)
			if tc.getFlt {
				if err != nil {
					t.Fatalf("Float: %v", err)
				}
				if f != tc.fltVal {
					t.Errorf("Float = %v, want %v", f, tc.fltVal)
				}
			} else {
				assertCannotConvert(t, err, "Float")
			}

			d, err := rs.Double(1)
			if tc.getDbl {
				if err != nil {
					t.Fatalf("Double: %v", err)
				}
				if d != tc.dblVal {
					t.Errorf("Double = %v, want %v", d, tc.dblVal)
				}
			} else {
				assertCannotConvert(t, err, "Double")
			}

			b, err := rs.Boolean(1)
			if tc.getBool {
				if err != nil {
					t.Fatalf("Boolean: %v", err)
				}
				if b != tc.boolVal {
					t.Errorf("Boolean = %v, want %v", b, tc.boolVal)
				}
			} else {
				assertCannotConvert(t, err, "Boolean")
			}

			s, err := rs.String(1)
			if tc.getStr {
				if err != nil {
					t.Fatalf("String: %v", err)
				}
				if s != tc.strVal {
					t.Errorf("String = %q, want %q", s, tc.strVal)
				}
			} else {
				assertCannotConvert(t, err, "String")
			}
		})
	}
}

func assertCannotConvert(t *testing.T, err error, method string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected CANNOT_CONVERT_TYPE error", method)
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || string(apiErr.Code) != string(api.ErrCodeCannotConvertType) {
		t.Errorf("%s: error code = %v, want ErrCodeCannotConvertType", method, err)
	}
}

func TestResultSet_EmptyCursor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cursor := recordlayer.Empty[QueryResult]()
	cols := []ColumnDef{{Name: "X", TypeName: "BIGINT"}}

	rs := NewRecordLayerResultSet(ctx, cursor, cols)
	defer rs.Close()

	if rs.Next() {
		t.Fatal("expected no rows")
	}
	if rs.Err() != nil {
		t.Fatalf("Err: %v", rs.Err())
	}
}

func TestResultSet_Continuation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cursor := recordlayer.Empty[QueryResult]()
	cols := []ColumnDef{{Name: "X", TypeName: "BIGINT"}}

	rs := NewRecordLayerResultSet(ctx, cursor, cols)
	defer rs.Close()

	cont, err := rs.Continuation()
	if err != nil {
		t.Fatalf("Continuation: %v", err)
	}
	if cont.Reason() != api.ContinuationCursorAfterLast {
		t.Errorf("reason = %v, want CursorAfterLast", cont.Reason())
	}
}

func TestResultSet_CloseIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cursor := recordlayer.Empty[QueryResult]()
	rs := NewRecordLayerResultSet(ctx, cursor, nil)

	if err := rs.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := rs.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if rs.Next() {
		t.Fatal("Next after Close should return false")
	}
}

// TestResultSet_PositionalDupNameRead pins duplicate-name `SELECT *` at the
// driver read boundary: a row carrying an ALIGNED positional row (one slot
// per column, per-slot names matching the columns' unqualified display
// names) reads column i from slot i-1 — so two same-named columns from
// different join legs surface their OWN values instead of collapsing to one
// (a name-keyed lookup on the bare column name would return only the
// last-written value).
func TestResultSet_PositionalDupNameRead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// The gated 2-way ordinal join's `SELECT *` shape over a(id,name) × b(id,name):
	// merged type [ID NAME ID NAME] (duplicates preserved — raw RecordType).
	mergedType := &values.RecordType{Fields: []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "NAME", FieldType: values.NotNullString, Ordinal: 1},
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 2},
		{Name: "NAME", FieldType: values.NotNullString, Ordinal: 3},
	}}
	pos := &PositionalRow{Type: mergedType, Slots: []any{int64(1), "alpha", int64(1), "x"}}
	cursor := recordlayer.FromList([]QueryResult{
		{Positional: pos},
	})
	cols := []ColumnDef{
		{Name: "ID", TypeName: "BIGINT"},
		{Name: "NAME", TypeName: "STRING"},
		{Name: "ID", TypeName: "BIGINT"},
		{Name: "NAME", TypeName: "STRING"},
	}

	rs := NewRecordLayerResultSet(ctx, cursor, cols)
	defer rs.Close()

	if !rs.Next() {
		t.Fatal("expected a row")
	}
	want := []any{int64(1), "alpha", int64(1), "x"}
	for i, w := range want {
		v, err := rs.Object(i + 1)
		if err != nil {
			t.Fatalf("Object(%d): %v", i+1, err)
		}
		if v != w {
			t.Errorf("column %d = %v, want %v (per-leg positional value, not the last-written value a name-keyed read would return)", i+1, v, w)
		}
	}
}

// TestResultSet_DottedAliasPositionalAlign pins the dotted-output-alias regression: a quoted
// output alias containing a dot — `SELECT v AS "A.B"`. The projection emits a slot
// named "A.B" and the column label is "A.B"; the positional-alignment check must
// NOT leaf-strip the slot to "B" and reject the row with XX000 "no positional
// output row aligned". A genuinely permuted qualifier ("X.NAME" slot vs
// "Y.NAME" label) must still be rejected — the fix strips only the slot side,
// never the display side.
func TestResultSet_DottedAliasPositionalAlign(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	rowType := &values.RecordType{Fields: []values.Field{
		{Name: "A.B", FieldType: values.NotNullLong, Ordinal: 0},
	}}
	pos := &PositionalRow{Type: rowType, Slots: []any{int64(7)}}
	cursor := recordlayer.FromList([]QueryResult{{Positional: pos}})
	cols := []ColumnDef{{Name: "A.B", Label: "A.B", TypeName: "BIGINT"}}

	rs := NewRecordLayerResultSet(ctx, cursor, cols)
	defer rs.Close()
	if !rs.Next() {
		t.Fatal("expected a row")
	}
	v, err := rs.Object(1)
	if err != nil {
		t.Fatalf("Object(1) over a dotted alias must succeed (positional align), got: %v", err)
	}
	if v != int64(7) {
		t.Fatalf("dotted alias value = %v, want 7", v)
	}

	// Guard: a permuted qualifier must NOT falsely align (slot X.NAME vs label
	// Y.NAME are different columns; stripping both sides would wrongly match).
	permRow := &PositionalRow{
		Type:  &values.RecordType{Fields: []values.Field{{Name: "X.NAME", FieldType: values.NotNullString, Ordinal: 0}}},
		Slots: []any{"wrong"},
	}
	permRS := NewRecordLayerResultSet(ctx, recordlayer.FromList([]QueryResult{{Positional: permRow}}), []ColumnDef{{Name: "Y.NAME", Label: "Y.NAME", TypeName: "STRING"}})
	defer permRS.Close()
	if permRS.positionalAligned(permRow) {
		t.Fatal("permuted qualifier (X.NAME slot vs Y.NAME label) must NOT positionally align")
	}
}

func TestResultSet_DuplicateDisplayNamesAlignDeduplicatedExactSlotsByOrdinal(t *testing.T) {
	t.Parallel()
	row := &PositionalRow{
		Type: &values.RecordType{Fields: []values.Field{
			{Name: "ID", FieldType: values.NullableLong, Ordinal: 0},
			{Name: "ID_2", FieldType: values.NullableLong, Ordinal: 1},
			{Name: "COUNT(*)", FieldType: values.NotNullLong, Ordinal: 2},
		}},
		Slots: []any{int64(2), int64(7), int64(1)},
	}
	columns := []ColumnDef{
		{Name: "PO.ID", Label: "ID", TypeName: "BIGINT"},
		{Name: "PI.ID", Label: "ID", TypeName: "BIGINT"},
		{Name: "COUNT(*)", Label: "COUNT(*)", TypeName: "BIGINT"},
	}
	rs := NewRecordLayerResultSet(
		context.Background(), recordlayer.FromList([]QueryResult{{Positional: row}}), columns)
	defer rs.Close()
	if !rs.Next() {
		t.Fatalf("expected row: %v", rs.Err())
	}
	for i, want := range []any{int64(2), int64(7), int64(1)} {
		got, err := rs.Object(i + 1)
		if err != nil || got != want {
			t.Fatalf("Object(%d) = (%v,%v), want (%v,nil)", i+1, got, err, want)
		}
	}

	// A unique output name still cannot borrow the duplicate-display exception.
	badColumns := append([]ColumnDef(nil), columns...)
	badColumns[1] = ColumnDef{Name: "PI.ID", Label: "OTHER", TypeName: "BIGINT"}
	bad := NewRecordLayerResultSet(
		context.Background(), recordlayer.FromList([]QueryResult{{Positional: row}}), badColumns)
	defer bad.Close()
	if !bad.Next() {
		t.Fatalf("expected bad-control row: %v", bad.Err())
	}
	if _, err := bad.Object(2); err == nil {
		t.Fatal("unique mismatched display name was accepted as duplicate output")
	}
}

// TestResultSet_PositionalMisalignedIsLoud pins the correct-or-loud contract:
// there is no name-keyed row on the read path, so a positional row whose
// shape does NOT match the result-set columns (count or per-slot plain-name
// mismatch) can no longer be answered — the reader returns a loud XX000
// rather than silently reading a stale/absent name key. (A misaligned
// positional row is a producer bug: the top operator failed to emit the
// output row in the result-set's shape.)
func TestResultSet_PositionalMisalignedIsLoud(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	srcType := &values.RecordType{Fields: []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "NAME", FieldType: values.NotNullString, Ordinal: 1},
	}}

	assertLoud := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("expected XX000 correct-or-loud error on misaligned positional row")
		}
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || string(apiErr.Code) != string(api.ErrCodeInternalError) {
			t.Errorf("error code = %v, want ErrCodeInternalError (XX000)", err)
		}
	}

	t.Run("name_mismatch", func(t *testing.T) {
		t.Parallel()
		// Source row (ID, NAME) under a projection whose output column is the alias
		// THE_ID: plain-identifier slot names don't match the columns → not aligned.
		pos := &PositionalRow{Type: srcType, Slots: []any{int64(7), "src"}}
		cursor := recordlayer.FromList([]QueryResult{{Positional: pos}})
		cols := []ColumnDef{
			{Name: "THE_ID", Label: "THE_ID", TypeName: "BIGINT"},
			{Name: "LABEL", TypeName: "STRING"},
		}
		rs := NewRecordLayerResultSet(ctx, cursor, cols)
		defer rs.Close()
		if !rs.Next() {
			t.Fatal("expected a row")
		}
		_, err := rs.Object(1)
		assertLoud(t, err)
	})

	t.Run("count_mismatch", func(t *testing.T) {
		t.Parallel()
		// Positional carries the full source record; columns are a subset → count
		// mismatch → not aligned → loud.
		pos := &PositionalRow{Type: srcType, Slots: []any{int64(99), "src"}}
		cursor := recordlayer.FromList([]QueryResult{{Positional: pos}})
		cols := []ColumnDef{{Name: "ID", TypeName: "BIGINT"}}
		rs := NewRecordLayerResultSet(ctx, cursor, cols)
		defer rs.Close()
		if !rs.Next() {
			t.Fatal("expected a row")
		}
		_, err := rs.Object(1)
		assertLoud(t, err)
	})

	t.Run("qualified_name_leaf_matches", func(t *testing.T) {
		t.Parallel()
		// Label-less columns with name-model qualified Names ("A.ID") align by their
		// bare leaf against the positional slot names — the read is by ordinal.
		pos := &PositionalRow{Type: srcType, Slots: []any{int64(99), "pos"}}
		cursor := recordlayer.FromList([]QueryResult{{Positional: pos}})
		cols := []ColumnDef{
			{Name: "A.ID", TypeName: "BIGINT"},
			{Name: "A.NAME", TypeName: "STRING"},
		}
		rs := NewRecordLayerResultSet(ctx, cursor, cols)
		defer rs.Close()
		if !rs.Next() {
			t.Fatal("expected a row")
		}
		if v, _ := rs.Object(1); v != int64(99) {
			t.Errorf("column 1 = %v, want 99 (positional read by leaf-name alignment)", v)
		}
		if v, _ := rs.Object(2); v != "pos" {
			t.Errorf("column 2 = %v, want pos (positional read by leaf-name alignment)", v)
		}
	})
}

// TestResultSet_PreRFC229SortContinuationNamesFailClosed MEASURES what a
// MemorySortContinuation minted by a PRE-RFC-229 binary does when resumed by a
// post-229 one, over a nested projection.
//
// WHY THE TOKEN CARRIES NAMES AT ALL. encodeSortedRecords serializes the
// buffered row's positional slot NAMES into the token (continuation.go, the
// payload's "n" field) and decodeSortedRecords rebuilds the PositionalRow type
// from them (positionalTypeFromNames). Those names are
// values.ProjectionColumnName's output, which RFC-229 §2.3 changed for a nested
// projection: `SELECT n.sk, n.co ORDER BY ...` wrote ["N","N"] before and
// ["N.SK","N.CO"] after. A token therefore outlives the naming rule that minted
// it, and continuations are on the non-negotiable-compatibility list.
//
// THE MEASURED ANSWER: it FAILS CLOSED. The rebuilt row's slot names do not
// match the new binary's column labels, positionalAligned rejects the row
// wholesale (it is deliberately all-or-nothing), and every read returns a loud
// XX000 — the same correct-or-loud path a producer bug takes. There is no arm
// in which slot 0 is served for column 1: alignment is refused for the row, not
// repaired per slot. So the observable break is "a resumed page errors", never
// "a resumed page returns the wrong column's data".
//
// That is what makes accepting the break defensible rather than reckless, and
// it is why this is a test and not a paragraph: if positionalAligned ever grows
// a tolerant arm — a leaf-strip on BOTH sides, a fallback to name lookup, a
// per-slot repair — this shape stops failing closed and starts serving `N` for
// `SK`, silently, on exactly the tokens a rolling upgrade produces.
func TestResultSet_PreRFC229SortContinuationNamesFailClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// The row a PRE-229 binary buffered and encoded: both slots of one struct
	// root named after the root, which is the defect §2.3 fixed.
	oldToken := &PositionalRow{
		Type: &values.RecordType{Fields: []values.Field{
			// PRE-229 continuation payloads encoded names only; the legacy
			// decoder therefore has no declared scalar type to recover here.
			{Name: "N", FieldType: values.UnknownType, Ordinal: 0},
			{Name: "N", FieldType: values.UnknownType, Ordinal: 1},
		}},
		Slots: []any{int64(10), int64(21)},
	}
	// The columns a POST-229 binary reports for the same query.
	newCols := []ColumnDef{
		{Name: "N.SK", Label: "SK", TypeName: "BIGINT"},
		{Name: "N.CO", Label: "CO", TypeName: "BIGINT"},
	}

	rs := NewRecordLayerResultSet(ctx, recordlayer.FromList([]QueryResult{{Positional: oldToken}}), newCols)
	defer rs.Close()
	if rs.positionalAligned(oldToken) {
		t.Fatalf("a PRE-RFC-229 sort continuation's slot names [N N] now ALIGN " +
			"against post-229 columns [SK CO].\n" +
			"  That is the dangerous direction, not the safe one: aligning means " +
			"every read is served BY ORDINAL from a row whose names disagree with " +
			"the columns, so the alignment check has stopped being a shape proof. " +
			"The break must stay a loud rejection until the decode side actually " +
			"reconciles old names, not become a silent acceptance.")
	}
	if !rs.Next() {
		t.Fatal("expected a row")
	}
	v, err := rs.Object(1)
	if err == nil {
		t.Fatalf("reading column 1 off a pre-229 continuation row returned %v with "+
			"no error — the resumed page must fail LOUDLY rather than serve a slot "+
			"whose name does not match its column", v)
	}
	if !containsSubstr(err.Error(), "no positional output row aligned") {
		t.Fatalf("pre-229 continuation resume failed with %v, want the "+
			"correct-or-loud alignment error — a different failure would mean the "+
			"break is being reported by something else and this pin has stopped "+
			"measuring the channel it names", err)
	}

	// CONTROL — the SAME row shape under its OWN binary's columns still reads.
	// Without this the assertions above are satisfied by a result set that
	// rejects everything, which would prove nothing about names.
	okRS := NewRecordLayerResultSet(ctx,
		recordlayer.FromList([]QueryResult{{Positional: oldToken}}),
		[]ColumnDef{
			{Name: "N", Label: "N", TypeName: "BIGINT"},
			{Name: "N", Label: "N", TypeName: "BIGINT"},
		})
	defer okRS.Close()
	if !okRS.Next() {
		t.Fatal("expected a row from the control")
	}
	if v, err := okRS.Object(1); err != nil || v != int64(10) {
		t.Fatalf("the control read column 1 as (%v, %v), want (10, nil) — the "+
			"rejection above must be about the NAMES disagreeing, not about this "+
			"row being unreadable in general", v, err)
	}
}

// containsSubstr is a local substring test kept off the `strings` import so
// this file's import set does not move for one assertion.
func containsSubstr(haystack, needle string) bool {
	if len(needle) > len(haystack) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
