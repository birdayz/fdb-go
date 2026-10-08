package testkit

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// gojDB sets up emp + dept for GROUP-BY-over-join tests. Groups by dept:
//
//	eng (did=1): Alice(100), Bob(90) → COUNT 2, MAX 100, SUM 190
//	sales (did=2): Charlie(80)       → COUNT 1, MAX 80,  SUM 80
func GojDB(t *testing.T, tag string) (*sql.DB, context.Context) {
	t.Helper()
	ctx := context.Background()
	dbPath := "/FRL/goj_" + tag
	setup := OpenDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	tmpl := "goj_tmpl_" + tag
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE "+tmpl+
		" CREATE TABLE dept (did BIGINT, dname STRING, PRIMARY KEY (did))"+
		" CREATE TABLE emp (eid BIGINT, did BIGINT, ename STRING, salary BIGINT, PRIMARY KEY (eid))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE "+tmpl); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+strings.ToUpper(dbPath)+"?cluster_file="+clusterFilePath+"&schema=MAIN")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.ExecContext(ctx, "INSERT INTO dept VALUES (1,'eng'),(2,'sales')"); err != nil {
		t.Fatalf("seed dept: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO emp VALUES (10,1,'Alice',100),(20,1,'Bob',90),(30,2,'Charlie',80)"); err != nil {
		t.Fatalf("seed emp: %v", err)
	}
	return db, ctx
}

type GojRow struct {
	Dname string
	Cnt   int64
	Mx    int64
}

func GojRead(t *testing.T, ctx context.Context, db *sql.DB, q string) []GojRow {
	t.Helper()
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	defer rows.Close()
	var got []GojRow
	for rows.Next() {
		var r GojRow
		if err := rows.Scan(&r.Dname, &r.Cnt, &r.Mx); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	return got
}
