package javacorpus_test

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/relational/api"
	_ "fdb.dev/pkg/relational/sqldriver"
)

// TestFDB_ResultColumnMetadataCarriesNestedTypes pins CQ-74's close: a result
// column keeps its full type, as Java's RelationalResultSetMetaData does.
//
//   - database/sql's DatabaseTypeName is Java's getColumnTypeName: "ARRAY" for
//     an array column, "STRUCT" for a struct column (SqlTypeNamesSupport), and
//     neither advertises a scalar scan type.
//   - The metadata the driver hands api.WithResultSetMetaDataObserver carries
//     the array's element type and the struct's declared type name and fields
//     (Java's getArrayMetaData / getStructMetaData), which the corpus runner's
//     `resultMetadata:` check reads.
//
// Column metadata comes from the plan, so an empty table exercises it exactly.
func TestFDB_ResultColumnMetadataCarriesNestedTypes(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	const id = "ARRMETA"
	cat, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///__SYS?cluster_file=%s", clusterFilePath))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer cat.Close()

	tmpl, dbPath, schema := id+"_TEMPLATE", "/FRL/"+id+"_DB", id+"_SCHEMA"
	for _, stmt := range []string{
		"DROP SCHEMA TEMPLATE IF EXISTS " + tmpl,
		"DROP DATABASE IF EXISTS " + dbPath,
		"CREATE SCHEMA TEMPLATE " + tmpl +
			" CREATE TYPE AS STRUCT point(x bigint, y bigint)" +
			" CREATE TABLE t1(pk integer, x integer array, pt point, pts point array, primary key(pk))",
		"CREATE DATABASE " + dbPath,
		fmt.Sprintf("CREATE SCHEMA %s/%s WITH TEMPLATE %s", dbPath, schema, tmpl),
	} {
		if _, err := cat.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		_, _ = cat.Exec("DROP DATABASE IF EXISTS " + dbPath)
		_, _ = cat.Exec("DROP SCHEMA TEMPLATE IF EXISTS " + tmpl)
	})

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=%s", strings.ToUpper(dbPath), clusterFilePath, strings.ToUpper(schema)))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	var meta api.ResultSetMetaData
	qctx := api.WithResultSetMetaDataObserver(ctx, func(md api.ResultSetMetaData) { meta = md })
	rows, err := db.QueryContext(qctx, "SELECT pk, x, pt, pts FROM t1")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	defer rows.Close()

	cts, err := rows.ColumnTypes()
	if err != nil {
		t.Fatalf("ColumnTypes: %v", err)
	}
	wantNames := []string{"INTEGER", "ARRAY", "STRUCT", "ARRAY"}
	if len(cts) != len(wantNames) {
		t.Fatalf("got %d columns, want %d", len(cts), len(wantNames))
	}
	for i, want := range wantNames {
		if got := cts[i].DatabaseTypeName(); got != want {
			t.Errorf("column %s: DatabaseTypeName %q, want Java's getColumnTypeName %q", cts[i].Name(), got, want)
		}
	}
	anyType := reflect.TypeOf((*any)(nil)).Elem()
	for _, i := range []int{1, 2, 3} {
		if st := cts[i].ScanType(); st != anyType {
			t.Errorf("column %s advertises scan type %v; a composite column is not a scalar", cts[i].Name(), st)
		}
	}

	if meta == nil {
		t.Fatal("the driver reported no result set metadata to the observer")
	}
	point := api.NewStructType("POINT", []api.StructField{
		api.NewStructField("X", api.NewLongType(true), 0),
		api.NewStructField("Y", api.NewLongType(true), 1),
	}, true)
	// An array's elements are NOT NULL (Java's arrays hold no NULL element;
	// the DDL declares the element so).
	for i, want := range []api.DataType{
		api.NewIntegerType(true),
		api.NewArrayType(api.NewIntegerType(false), true),
		point,
		api.NewArrayType(point.WithNullable(false), true),
	} {
		got, err := meta.ColumnDataType(i + 1)
		if err != nil {
			t.Fatalf("ColumnDataType(%d): %v", i+1, err)
		}
		if !got.Equal(want) || !strings.EqualFold(nameOf(got), nameOf(want)) {
			t.Errorf("column %d: ColumnDataType %v (%s), want %v (%s)", i+1, got, nameOf(got), want, nameOf(want))
		}
	}
}

// nameOf is a struct type's (or an array of structs' element's) declared name.
func nameOf(dt api.DataType) string {
	if at, ok := dt.(*api.ArrayType); ok {
		dt = at.ElementType()
	}
	if st, ok := dt.(*api.StructType); ok {
		return st.Name()
	}
	return ""
}
