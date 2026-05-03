package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
)

func TestResultSet_IterateRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cursor := recordlayer.FromList([]QueryResult{
		{Datum: map[string]any{"ID": int64(1), "NAME": "alice"}},
		{Datum: map[string]any{"ID": int64(2), "NAME": "bob"}},
	})
	cols := []ColumnDef{
		{Name: "ID", TypeName: "BIGINT"},
		{Name: "NAME", TypeName: "STRING"},
	}

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

	cursor := recordlayer.FromList([]QueryResult{
		{Datum: map[string]any{"PRICE": int64(42), "ACTIVE": true}},
	})
	cols := []ColumnDef{
		{Name: "PRICE", TypeName: "BIGINT"},
		{Name: "ACTIVE", TypeName: "BOOLEAN"},
	}

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

	cursor := recordlayer.FromList([]QueryResult{
		{Datum: map[string]any{"ID": int64(1)}},
	})
	cols := []ColumnDef{
		{Name: "ID", TypeName: "BIGINT"},
		{Name: "NAME", TypeName: "STRING"},
	}

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

	cursor := recordlayer.FromList([]QueryResult{
		{Datum: map[string]any{"PK": int64(100)}},
	})
	cols := []ColumnDef{
		{Name: "PK", TypeName: "BIGINT"},
		{Name: "T1", TypeName: "BIGINT"},
		{Name: "T2", TypeName: "STRING"},
		{Name: "T3", TypeName: "DOUBLE"},
		{Name: "T4", TypeName: "BYTES"},
	}

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
		{Datum: map[string]any{"X": int64(1)}},
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
		{Datum: map[string]any{"X": int64(1)}},
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

	cursor := recordlayer.FromList([]QueryResult{
		{Datum: map[string]any{
			"INT_VAL":    int64(42),
			"FLOAT_VAL":  float64(3.14),
			"BOOL_VAL":   true,
			"STRING_VAL": "hello",
		}},
	})
	cols := []ColumnDef{
		{Name: "INT_VAL", TypeName: "BIGINT"},
		{Name: "FLOAT_VAL", TypeName: "DOUBLE"},
		{Name: "BOOL_VAL", TypeName: "BOOLEAN"},
		{Name: "STRING_VAL", TypeName: "STRING"},
	}

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
			cursor := recordlayer.FromList([]QueryResult{
				{Datum: map[string]any{"V": tc.value}},
			})
			cols := []ColumnDef{{Name: "V", TypeName: "STRING"}}
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
