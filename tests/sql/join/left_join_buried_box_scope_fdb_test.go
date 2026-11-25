package sqltest

// A projected EXISTS over a BURIED box under a LEFT JOIN: `(p JOIN q) LEFT JOIN
// s` (the 3-way clause associates left, so the LEFT's preserved leg is the
// inner join box). The block is one Select whose EXISTS reads the
// null-extended s, so Go answers as Java does: `[[10 false]]`.

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

func TestFDB_ProjectedExistsOverABuriedLeftJoinBox(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/FRL/f2lbb"
	setup := testkit.OpenDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE f2lbb_tmpl"+
		" CREATE TABLE p (id BIGINT, v BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE q (qid BIGINT, PRIMARY KEY (qid))"+
		" CREATE TABLE s (sid BIGINT, PRIMARY KEY (sid))"+
		" CREATE TABLE r (rid BIGINT, PRIMARY KEY (rid))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE f2lbb_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+strings.ToUpper(dbPath)+"?cluster_file="+testkit.ClusterFile()+"&schema=MAIN")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, stmt := range []string{
		"INSERT INTO p VALUES (1, 10), (2, 20)",
		"INSERT INTO q VALUES (1)",
		"INSERT INTO r VALUES (5)",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}

	// (p JOIN q ON q.qid = p.id) = {(p.v=10)}; LEFT JOIN s (empty) null-extends
	// s, so EXISTS(r.rid = s.sid = NULL) is false. Java: [[10 false]].
	sqlText := "SELECT p.v, EXISTS (SELECT 1 FROM r WHERE r.rid = s.sid) " +
		"FROM p JOIN q ON q.qid = p.id LEFT JOIN s ON s.sid = p.id"
	rows, err := db.QueryContext(ctx, sqlText)
	if err != nil {
		t.Fatalf("projected EXISTS over a buried LEFT JOIN box errored (Java answers it): %v", err)
	}
	defer rows.Close()
	var got [][2]any
	for rows.Next() {
		var v int64
		var ex sql.NullBool
		if scanErr := rows.Scan(&v, &ex); scanErr != nil {
			t.Fatalf("scan: %v", scanErr)
		}
		got = append(got, [2]any{v, ex})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(got) != 1 || got[0] != [2]any{int64(10), sql.NullBool{Bool: false, Valid: true}} {
		t.Fatalf("got %v, want [[10 false]]: s is null-extended, so EXISTS over s.sid is false", got)
	}
}
