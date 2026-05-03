package embedded

import (
	"database/sql/driver"
	"io"
	"math"
	"reflect"
	"testing"
)

func TestStaticRows_Columns(t *testing.T) {
	t.Parallel()
	r := &staticRows{cols: []string{"TABLE_NAME", "TABLE_TYPE"}}
	cols := r.Columns()
	if len(cols) != 2 {
		t.Fatalf("expected 2 columns, got %d", len(cols))
	}
}

func TestStaticRows_Next(t *testing.T) {
	t.Parallel()
	r := &staticRows{
		cols: []string{"A"},
		rows: [][]driver.Value{
			{int64(1)},
			{int64(2)},
		},
	}
	dest := make([]driver.Value, 1)
	if err := r.Next(dest); err != nil {
		t.Fatalf("Next: %v", err)
	}
	if dest[0] != int64(1) {
		t.Fatalf("expected 1, got %v", dest[0])
	}
	if err := r.Next(dest); err != nil {
		t.Fatalf("Next: %v", err)
	}
	if dest[0] != int64(2) {
		t.Fatalf("expected 2, got %v", dest[0])
	}
	if err := r.Next(dest); err != io.EOF {
		t.Fatalf("expected io.EOF, got %v", err)
	}
}

func TestStaticRows_Close(t *testing.T) {
	t.Parallel()
	r := &staticRows{
		cols: []string{"A"},
		rows: [][]driver.Value{{int64(1)}},
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	dest := make([]driver.Value, 1)
	if err := r.Next(dest); err != io.EOF {
		t.Fatalf("expected io.EOF after close, got %v", err)
	}
}

func TestStaticRows_ColumnTypeDatabaseTypeName(t *testing.T) {
	t.Parallel()
	r := &staticRows{
		cols:     []string{"ID", "NAME", "ACTIVE"},
		colTypes: []string{"BIGINT", "STRING", "BOOLEAN"},
	}
	tests := []struct {
		idx  int
		want string
	}{
		{0, "BIGINT"},
		{1, "STRING"},
		{2, "BOOLEAN"},
		{-1, ""},
		{3, ""},
	}
	for _, tt := range tests {
		if got := r.ColumnTypeDatabaseTypeName(tt.idx); got != tt.want {
			t.Errorf("idx=%d: got %q, want %q", tt.idx, got, tt.want)
		}
	}
}

func TestStaticRows_ColumnTypeScanType(t *testing.T) {
	t.Parallel()
	r := &staticRows{
		cols:     []string{"A", "B", "C", "D", "E", "F", "G"},
		colTypes: []string{"BIGINT", "STRING", "BOOLEAN", "DOUBLE", "FLOAT", "BYTES", "INTEGER"},
	}
	tests := []struct {
		idx  int
		want reflect.Type
	}{
		{0, reflect.TypeFor[int64]()},
		{1, reflect.TypeFor[string]()},
		{2, reflect.TypeFor[bool]()},
		{3, reflect.TypeFor[float64]()},
		{4, reflect.TypeFor[float32]()},
		{5, reflect.TypeFor[[]byte]()},
		{6, reflect.TypeFor[int32]()},
		{-1, reflect.TypeFor[any]()},
		{7, reflect.TypeFor[any]()},
	}
	for _, tt := range tests {
		if got := r.ColumnTypeScanType(tt.idx); got != tt.want {
			t.Errorf("idx=%d: got %v, want %v", tt.idx, got, tt.want)
		}
	}
}

func TestStaticRows_ColumnTypeNullable(t *testing.T) {
	t.Parallel()
	r := &staticRows{
		cols:     []string{"A"},
		colTypes: []string{"BIGINT"},
	}
	nullable, ok := r.ColumnTypeNullable(0)
	if !nullable || !ok {
		t.Fatalf("idx=0: nullable=%v ok=%v, want true/true", nullable, ok)
	}
	nullable, ok = r.ColumnTypeNullable(-1)
	if !nullable || ok {
		t.Fatalf("idx=-1: nullable=%v ok=%v, want true/false", nullable, ok)
	}
	nullable, ok = r.ColumnTypeNullable(1)
	if !nullable || ok {
		t.Fatalf("idx=1 (out of bounds): nullable=%v ok=%v, want true/false", nullable, ok)
	}
}

func TestStaticRows_ColumnTypeLength(t *testing.T) {
	t.Parallel()
	r := &staticRows{
		cols:     []string{"S", "B", "I"},
		colTypes: []string{"STRING", "BYTES", "BIGINT"},
	}
	length, ok := r.ColumnTypeLength(0)
	if length != math.MaxInt64 || !ok {
		t.Errorf("STRING: length=%d ok=%v, want MaxInt64/true", length, ok)
	}
	length, ok = r.ColumnTypeLength(1)
	if length != math.MaxInt64 || !ok {
		t.Errorf("BYTES: length=%d ok=%v, want MaxInt64/true", length, ok)
	}
	length, ok = r.ColumnTypeLength(2)
	if length != 0 || ok {
		t.Errorf("BIGINT: length=%d ok=%v, want 0/false", length, ok)
	}
	length, ok = r.ColumnTypeLength(-1)
	if length != 0 || ok {
		t.Errorf("out of bounds: length=%d ok=%v, want 0/false", length, ok)
	}
}

func TestStaticRows_ColumnTypePrecisionScale(t *testing.T) {
	t.Parallel()
	r := &staticRows{
		cols:     []string{"A"},
		colTypes: []string{"DOUBLE"},
	}
	prec, scale, ok := r.ColumnTypePrecisionScale(0)
	if prec != 0 || scale != 0 || ok {
		t.Errorf("got prec=%d scale=%d ok=%v, want 0/0/false", prec, scale, ok)
	}
}

func TestDbTypeToScanType_AllTypes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		typeName string
		want     reflect.Type
	}{
		{"BIGINT", reflect.TypeFor[int64]()},
		{"INTEGER", reflect.TypeFor[int32]()},
		{"INT", reflect.TypeFor[int32]()},
		{"STRING", reflect.TypeFor[string]()},
		{"BOOLEAN", reflect.TypeFor[bool]()},
		{"DOUBLE", reflect.TypeFor[float64]()},
		{"FLOAT", reflect.TypeFor[float32]()},
		{"BYTES", reflect.TypeFor[[]byte]()},
		{"unknown", reflect.TypeFor[any]()},
		{"", reflect.TypeFor[any]()},
		{"bigint", reflect.TypeFor[int64]()},
	}
	for _, tt := range tests {
		if got := dbTypeToScanType(tt.typeName); got != tt.want {
			t.Errorf("dbTypeToScanType(%q) = %v, want %v", tt.typeName, got, tt.want)
		}
	}
}

func TestEmptyRows(t *testing.T) {
	t.Parallel()
	r := emptyRows{}
	if cols := r.Columns(); len(cols) != 0 {
		t.Fatalf("expected empty columns, got %v", cols)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := r.Next(nil); err != io.EOF {
		t.Fatalf("expected io.EOF, got %v", err)
	}
}
