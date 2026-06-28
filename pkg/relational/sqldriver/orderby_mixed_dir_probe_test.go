package sqldriver_test

// Probes multi-column ORDER BY with mixed ASC/DESC directions per key, including
// over an index on (a, b). a-ascending with b-descending ties (and the reverse)
// must order correctly.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_OrderByMixedDirProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_omd")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_omd")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE omd "+
		"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, PRIMARY KEY (id)) "+
		"CREATE INDEX t_ab ON t (a, b)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_omd/s WITH TEMPLATE omd")
	dsn := fmt.Sprintf("fdbsql:///testdb_omd?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// a=1: b in {10,20}; a=2: b in {5,15}
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a, b) VALUES (1,1,10),(2,1,20),(3,2,5),(4,2,15)")

	ids := func(orderBy string) []int64 {
		rows, err := db.QueryContext(ctx, "SELECT id FROM t ORDER BY "+orderBy)
		if err != nil {
			t.Fatalf("ORDER BY %s: %v", orderBy, err)
		}
		defer rows.Close()
		var o []int64
		for rows.Next() {
			var v int64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan: %v", err)
			}
			o = append(o, v)
		}
		return o
	}
	eq := func(g, w []int64) bool {
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
	ck := func(name, orderBy string, want []int64) {
		t.Run(name, func(t *testing.T) {
			if got := ids(orderBy); !eq(got, want) {
				t.Errorf("ORDER BY %s = %v, want %v", orderBy, got, want)
			}
		})
	}

	ck("asc_asc", "a ASC, b ASC", []int64{1, 2, 3, 4})   // a1:(10,20) a2:(5,15)
	ck("asc_desc", "a ASC, b DESC", []int64{2, 1, 4, 3}) // a1:(20,10) a2:(15,5)
	ck("desc_asc", "a DESC, b ASC", []int64{3, 4, 1, 2}) // a2:(5,15) a1:(10,20)
	ck("desc_desc", "a DESC, b DESC", []int64{4, 3, 2, 1})
}
