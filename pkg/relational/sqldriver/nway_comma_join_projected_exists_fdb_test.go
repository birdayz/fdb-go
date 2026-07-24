package sqldriver_test

// RFC-190 190.1: N-way flat existential fold, comma-join shape (regression pin).
//
// N-way comma-join projected EXISTS: `FROM na a, nb b, nc c WHERE a.id = b.id
// AND b.id = c.id` is a flat 3-way INNER cross-product narrowed by WHERE
// conjuncts (not explicit JOIN..ON syntax) — the comma-join shape of the same
// RFC-190 190.1 N-WAY FLAT EXISTENTIAL fold the buried_inner harness pins for
// explicit JOIN..ON. Before the PartitionSelectRule live-existential guard +
// isIdentityInnerRV fix, this shape either declined or (per the retired
// ordinal N-way arm) could publish a corrupted merged row, observed upstream
// as an EXISTS that read ALWAYS-TRUE regardless of correlation. Data: nd holds
// {1,3} — id=1 and id=3 must read EXISTS=true, id=2 must read EXISTS=false;
// an always-true bug would report [true, true, true].

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_NWayCommaJoinProjectedExists(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/s4nwcj"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE s4nwcj_tmpl"+
		" CREATE TABLE na (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE nb (id BIGINT NOT NULL, PRIMARY KEY (id))"+
		" CREATE TABLE nc (id BIGINT NOT NULL, PRIMARY KEY (id))"+
		" CREATE TABLE nd (id BIGINT NOT NULL, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE s4nwcj_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, stmt := range []string{
		"INSERT INTO na VALUES (1, 100), (2, 200), (3, 300)",
		"INSERT INTO nb VALUES (1), (2), (3)",
		"INSERT INTO nc VALUES (1), (2), (3)",
		"INSERT INTO nd VALUES (1), (3)",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}

	sqlText := "SELECT a.v, EXISTS (SELECT 1 FROM nd d WHERE d.id = a.id) AS has_d " +
		"FROM na a, nb b, nc c WHERE a.id = b.id AND b.id = c.id"
	rows, err := db.QueryContext(ctx, sqlText)
	if err != nil {
		t.Fatalf("N-way comma-join projected EXISTS must plan, got: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var v int64
		var ex sql.NullBool
		if scanErr := rows.Scan(&v, &ex); scanErr != nil {
			t.Fatalf("scan: %v", scanErr)
		}
		got = append(got, fmt.Sprintf("%d|%v", v, ex.Valid && ex.Bool))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	sort.Strings(got)
	want := []string{"100|true", "200|false", "300|true"}
	if len(got) != len(want) {
		t.Fatalf("comma-join rows = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("comma-join rows = %v, want %v (an all-true result means the EXISTS"+
				" correlation was lost/corrupted — the historical always-true bug)", got, want)
		}
	}
}
