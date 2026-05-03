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
