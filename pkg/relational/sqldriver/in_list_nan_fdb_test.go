package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// An IN list deduplicates by SQL `=`, under which every NaN equals every NaN.
func TestFDB_InListNaN(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	setup := openTestDB(t, "/FRL/testdb_in_nan")
	mustExec(t, setup, ctx, "CREATE DATABASE /FRL/testdb_in_nan")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE in_nan_tmpl "+
		"CREATE TABLE F (id BIGINT, f DOUBLE, PRIMARY KEY (id)) CREATE INDEX F_F ON F (f) "+
		"CREATE TABLE G (id BIGINT, f DOUBLE, PRIMARY KEY (id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_in_nan/s WITH TEMPLATE in_nan_tmpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///FRL/TESTDB_IN_NAN?cluster_file=%s&schema=S", clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	for _, tbl := range []string{"F", "G"} {
		mustExec(t, db, ctx, "INSERT INTO "+tbl+" VALUES (1, 0.0), (3, 1.5), (4, CAST('NaN' AS DOUBLE))")
	}
	for _, tbl := range []string{"F", "G"} {
		for q, want := range map[string]string{
			"SELECT id FROM %s WHERE f IN (CAST('NaN' AS DOUBLE), CAST('NaN' AS DOUBLE)) ORDER BY id": "[4]",
			"SELECT id FROM %s WHERE f = CAST('NaN' AS DOUBLE) ORDER BY id":                           "[4]",
			"SELECT id FROM %s WHERE f IN (CAST('NaN' AS DOUBLE), 1.5) ORDER BY id":                   "[3 4]",
		} {
			q = fmt.Sprintf(q, tbl)
			rows, err := db.QueryContext(ctx, q)
			if err != nil {
				t.Errorf("%s: %v", q, err)
				continue
			}
			var got []int64
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id); err != nil {
					t.Fatal(err)
				}
				got = append(got, id)
			}
			if err := rows.Err(); err != nil {
				t.Errorf("%s: %v", q, err)
			}
			rows.Close()
			if fmt.Sprint(got) != want {
				t.Errorf("%s: %v, want %s", q, got, want)
			}
		}
	}
}
