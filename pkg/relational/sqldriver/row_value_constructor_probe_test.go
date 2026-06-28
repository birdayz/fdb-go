package sqldriver_test

// Probes row-value constructors and a couple of WHERE edges: a multi-column row-value
// IN `(a,b) IN ((1,2),...)` and row-value equality `(a,b) = (1,2)` are parsed but not
// plannable → 0AF00 (clean rejection, not wrong rows); an empty `IN ()` list is a
// syntax error (42601); and the tautology `a = a` returns all rows with a non-NULL
// value but excludes NULL-valued rows (NULL = NULL is UNKNOWN, 3VL).

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func TestFDB_RowValueConstructorProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_rvc")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_rvc")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE rvc CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_rvc/s WITH TEMPLATE rvc")
	dsn := fmt.Sprintf("fdbsql:///testdb_rvc?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a, b) VALUES (1,1,2),(2,3,4)")
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, b) VALUES (3, 9)") // a NULL

	ids := func(where string) ([]int64, error) {
		rows, err := db.QueryContext(ctx, "SELECT id FROM t WHERE "+where)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var o []int64
		for rows.Next() {
			var v int64
			_ = rows.Scan(&v)
			o = append(o, v)
		}
		sort.Slice(o, func(i, j int) bool { return o[i] < o[j] })
		return o, rows.Err()
	}
	rejected := func(name, where, code string) {
		t.Run(name, func(t *testing.T) {
			_, err := ids(where)
			if err == nil || !strings.Contains(err.Error(), code) {
				t.Errorf("%s error = %v, want %s", where, err, code)
			}
		})
	}
	rejected("row_value_in_unsupported", "(a, b) IN ((1,2),(3,4))", "0AF00")
	rejected("row_value_eq_unsupported", "(a, b) = (1, 2)", "0AF00")
	rejected("empty_in_list_syntax", "a IN ()", "42601")

	t.Run("self_equality_excludes_null_3vl", func(t *testing.T) {
		got, err := ids("a = a")
		if err != nil {
			t.Fatalf("a = a: %v", err)
		}
		// ids 1,2 have non-NULL a; id 3 (a NULL) excluded — NULL = NULL is UNKNOWN.
		if len(got) != 2 || got[0] != 1 || got[1] != 2 {
			t.Errorf("a = a = %v, want [1 2] (NULL row excluded by 3VL)", got)
		}
	})
}
