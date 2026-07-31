package javacorpus_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	_ "fdb.dev/pkg/relational/sqldriver"
)

// TestFDB_ArrayColumnMetadataIsTruncated is the LIVE sentinel for CQ-74's array
// half, and it is what makes `unsupported:result-metadata-nested` a temporary
// decline rather than a permanent one.
//
// It runs a real query against a real array column and reads back exactly what
// `database/sql` gives a caller. Java's `RelationalResultSetMetaData` reports
// `ARRAY(INTEGER)` for this column (`ArrayMetaData.getElementTypeName` folded
// by `CheckResultMetadataConfig.buildArrayTypeName`) and a scan type that is
// not a scalar; Go reports the bare ELEMENT type and an int32 scan type. Both
// are wrong, and both are consequences of one truncation — `executor.ColumnDef`
// carrying a single type-name string.
//
// No INSERT is needed and none is used: `integer array` is accepted by the DDL,
// and column metadata is derived from the PLAN, so an empty table exercises the
// derivation exactly. That matters because inserting an array literal fails on
// an unrelated gap (CQ-72), which would have made this probe untestable if the
// rows were required.
//
// WHEN THIS TEST FAILS, THAT IS THE HAND-OVER: the driver has started carrying
// the array type, so delete this test, delete the runner's `{array: …}`
// declining branch in metadataDescends, and let the directive be compared.
func TestFDB_ArrayColumnMetadataIsTruncated(t *testing.T) {
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

	tmpl, dbPath, schema := id+"_TEMPLATE", "/"+id+"_DB", id+"_SCHEMA"
	for _, stmt := range []string{
		"DROP SCHEMA TEMPLATE IF EXISTS " + tmpl,
		"DROP DATABASE IF EXISTS " + dbPath,
		"CREATE SCHEMA TEMPLATE " + tmpl +
			" CREATE TABLE t1(pk integer, x integer array, primary key(pk))",
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

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=%s", dbPath, clusterFilePath, schema))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, "SELECT pk, x FROM t1")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	defer rows.Close()

	cts, err := rows.ColumnTypes()
	if err != nil {
		t.Fatalf("ColumnTypes: %v", err)
	}
	if len(cts) != 2 {
		t.Fatalf("got %d columns, want 2 (PK, X)", len(cts))
	}

	// The scalar column is the control: it proves the metadata path works and
	// that the array column's answer is not a general failure to report types.
	if got := cts[0].DatabaseTypeName(); got != "INTEGER" {
		t.Fatalf("PK reports %q, want INTEGER — the control column is wrong, so this "+
			"probe cannot say anything about the array column", got)
	}

	const javaArrayTypeName = "ARRAY(INTEGER)"
	gotName := cts[1].DatabaseTypeName()
	if gotName == javaArrayTypeName {
		t.Fatalf("X now reports %q, which is Java's spelling — CQ-74's array half is CLOSED. "+
			"Delete this test and the `{array: …}` branch of metadataDescends so the "+
			"resultMetadata directive compares array columns instead of declining them.", gotName)
	}
	if gotName != "INTEGER" {
		t.Fatalf("X reports %q; the measured truncation is the bare ELEMENT type %q. A THIRD "+
			"answer means the derivation changed without closing the gap — re-measure CQ-74 "+
			"before trusting its booking.", gotName, "INTEGER")
	}

	// The second consequence, same root cause: an array column advertises a
	// scalar scan type, so a caller following ScanType would allocate an int32
	// for a list.
	if st := cts[1].ScanType(); st == nil || st.Kind().String() != "int32" {
		t.Fatalf("X advertises scan type %v; the measured truncation makes it int32 (it follows "+
			"DatabaseTypeName). If this changed, CQ-74's second consequence needs re-measuring.", st)
	}
}
