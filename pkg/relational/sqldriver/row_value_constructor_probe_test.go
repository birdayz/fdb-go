package sqldriver_test

// Record IN accepts primitive fields with matching positional type codes.
// Unlike IN, binary record equality is rejected by Java RelOpValue.
// Integer literals must be BIGINT here: InOpValue does not promote record fields.

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
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
		"CREATE SCHEMA TEMPLATE rvc CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, PRIMARY KEY (id)) CREATE INDEX rvc_ab AS SELECT a,b FROM t ORDER BY a,b")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_rvc/s WITH TEMPLATE rvc")
	dsn := fmt.Sprintf("fdbsql:///TESTDB_RVC?cluster_file=%s&schema=S", clusterFilePath)
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
	rejected("row_value_in_field_type_mismatch", "(a, b) IN ((1,2),(3,4))", "42804")
	rejected("row_value_in_single_element_type_mismatch", "(a, b) IN ((1,2))", "42804")
	rejected("row_value_not_in_field_type_mismatch", "(a, b) NOT IN ((1,2))", "42804")
	rejected("row_value_in_reversed_type_mismatch", "(b, a) IN ((2,1))", "42804")
	for _, tc := range []struct {
		name, where string
		want        []int64
	}{
		{"record_in", "(a,b) IN ((1L,2L),(3L,4L))", []int64{1, 2}},
		{"record_in_duplicates", "(a,b) IN ((1L,2L),(1L,2L),(3L,4L))", []int64{1, 2}},
		{"record_in_single", "(a,b) IN ((1L,2L))", []int64{1}},
		{"record_not_in", "(a,b) NOT IN ((1L,2L))", []int64{2, 3}},
		{"record_in_reversed", "(b,a) IN ((2L,1L))", []int64{1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.name == "record_in_duplicates" {
				var plan string
				if err := db.QueryRowContext(ctx, "EXPLAIN SELECT id FROM t WHERE "+tc.where).Scan(&plan); err != nil {
					t.Fatal(err)
				}
				t.Logf("record IN duplicate plan: %s", plan)
				if !strings.Contains(plan, "Explode(array_distinct)") || !strings.Contains(plan, "IndexScan(RVC_AB, [=, =])") {
					t.Fatalf("must exercise deduplicated record IN with a two-column index probe, got %s", plan)
				}
			}
			got, err := ids(tc.where)
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("%s: %v, %v; want %v", tc.where, got, err, tc.want)
			}
		})
	}
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
