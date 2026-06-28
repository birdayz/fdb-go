package sqldriver_test

// Probes result-set column-type metadata (Rows.ColumnTypes().DatabaseTypeName())
// — clients rely on it. Base columns report BIGINT/DOUBLE/STRING/BOOLEAN/BINARY;
// expression results report the right inferred type (arithmetic preserves/widens,
// a comparison is BOOLEAN, a CAST is the target type, a string function is STRING).

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_ColumnMetadataProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_colmetap")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_colmetap")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE colmetap "+
			"CREATE TABLE t (id BIGINT NOT NULL, d DOUBLE, s STRING, flag BOOLEAN, bin BYTES, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_colmetap/s WITH TEMPLATE colmetap")
	dsn := fmt.Sprintf("fdbsql:///testdb_colmetap?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, e := db.ExecContext(ctx, "INSERT INTO t (id, d, s, flag, bin) VALUES (1, 1.5, 'x', true, ?)", []byte{0xab}); e != nil {
		t.Fatalf("insert: %v", e)
	}

	typeNames := func(q string) []string {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		cts, err := rows.ColumnTypes()
		if err != nil {
			t.Fatalf("ColumnTypes: %v", err)
		}
		var out []string
		for _, ct := range cts {
			out = append(out, ct.DatabaseTypeName())
		}
		return out
	}
	eq := func(g, w []string) bool {
		if len(g) != len(w) {
			return false
		}
		for i := range g {
			if g[i] != w[i] {
				return false
			}
		}
		return true
	}

	t.Run("base_column_types", func(t *testing.T) {
		got := typeNames("SELECT id, d, s, flag, bin FROM t")
		want := []string{"BIGINT", "DOUBLE", "STRING", "BOOLEAN", "BINARY"}
		if !eq(got, want) {
			t.Errorf("base column types = %v, want %v", got, want)
		}
	})
	t.Run("expression_types", func(t *testing.T) {
		// id+1 → BIGINT, d*2.0 → DOUBLE, UPPER(s) → STRING.
		got := typeNames("SELECT id + 1, d * 2.0, UPPER(s) FROM t")
		want := []string{"BIGINT", "DOUBLE", "STRING"}
		if !eq(got, want) {
			t.Errorf("expression types = %v, want %v", got, want)
		}
	})
	t.Run("comparison_and_cast_types", func(t *testing.T) {
		// id>1 → BOOLEAN, d → DOUBLE, CAST(id AS DOUBLE) → DOUBLE.
		got := typeNames("SELECT id > 1, d, CAST(id AS DOUBLE) FROM t")
		want := []string{"BOOLEAN", "DOUBLE", "DOUBLE"}
		if !eq(got, want) {
			t.Errorf("comparison/cast types = %v, want %v", got, want)
		}
	})
	t.Run("numeric_aggregate_types", func(t *testing.T) {
		// COUNT(*) → BIGINT, SUM(d) → DOUBLE. (MIN/MAX on STRING is a conformant
		// rejection, pinned in the cross-engine corpus, so not exercised here.)
		got := typeNames("SELECT COUNT(*), SUM(d) FROM t")
		want := []string{"BIGINT", "DOUBLE"}
		if !eq(got, want) {
			t.Errorf("aggregate types = %v, want %v", got, want)
		}
	})
}
