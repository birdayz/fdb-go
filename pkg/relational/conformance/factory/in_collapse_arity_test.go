package factory_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// Every IN-list arity must survive the single-element collapse, on an outer
// join whose predicate reads the join's right-hand relation.
//
// This is the probe that localised the QOV-binding defect's arity, kept
// because the fact it established is what made the shape legible: while the
// bug was live, ONLY a list of exactly two identical elements failed.
// `IN (7)`, `IN (7,7,7)`, `IN (7,1,7)` and `IN (7,7) AND r.id > 0` all ran
// clean, so "duplicate IN list" alone never explained it — a three-element
// list deduplicates to one value and takes the identical collapse path.
//
// It is kept as a FORWARD guard rather than deleted with the bug: the collapse
// branch is reached by every list that deduplicates to one value, and this is
// the only test that walks that branch across arities. A regression that
// re-broke just one arity would otherwise show up as a single rowdiff seed
// months later.
//
// Two instrument details, both of which changed a verdict during the hunt.
// The connection is FRESH per predicate and the list is walked in both
// directions, because the queries otherwise share a plan cache and a verdict
// could depend on what ran before it. And `qovExec` DRAINS the rows: the
// resolution error surfaces while producing rows, not at QueryContext, so a
// probe that only checked the QueryContext error reports every arm clean.
func TestFDB_InCollapseArityAllRun(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	const ddl = "CREATE TABLE T_RD (id BIGINT, a BIGINT, b BIGINT, c BIGINT, s STRING, f BOOLEAN, d DOUBLE, e FLOAT, PRIMARY KEY (id)) " +
		"CREATE INDEX idx_c ON T_RD (c) CREATE INDEX idx_ab ON T_RD (a, b) CREATE INDEX idx_b ON T_RD (b) CREATE INDEX idx_a ON T_RD (a)"
	const insert = "INSERT INTO T_RD VALUES (1, 2, 9, 7, ' a', TRUE, 9.0, 2.0), (2, 7, 3, 7, 'gamma', FALSE, 9.0, NULL), (3, 6, 8, 1, 'b ', NULL, -1.0, 7.0)"

	setupDB, err := sql.Open("fdbsql", "fdbsql:///__SYS?cluster_file="+clusterFilePath+"&schema=CATALOG")
	if err != nil {
		t.Fatalf("open sys: %v", err)
	}
	defer setupDB.Close()
	const dbPath, schema, tmpl = "/INARITY", "inarity", "inarityt"
	if _, err := setupDB.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("create database: %v", err)
	}
	defer setupDB.ExecContext(ctx, "DROP DATABASE "+dbPath) //nolint:errcheck
	for _, stmt := range []string{
		fmt.Sprintf("CREATE SCHEMA TEMPLATE %s %s", tmpl, ddl),
		fmt.Sprintf("CREATE SCHEMA %s/%s WITH TEMPLATE %s", dbPath, schema, tmpl),
	} {
		if _, err := setupDB.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=%s", dbPath, clusterFilePath, schema))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer conn.Close() //nolint:errcheck
	if _, err := conn.ExecContext(ctx, insert); err != nil {
		t.Fatalf("insert: %v", err)
	}

	preds := []string{
		"r.c IN (7)",
		"r.c IN (7, 7)",
		"r.c IN (7, 1)",
		"r.c IN (7, 7, 7)",
		"r.c IN (7, 1, 7)",
		"r.c IN (7, 1, 1)",
		"r.c IN (1, 7)",
		"r.c = 7",
		"r.c = 7 OR r.c = 7",
		"r.c IN (7, 7) AND r.id > 0",
		"NOT (r.c IN (7, 7))",
	}
	// A FRESH connection per predicate, and the list run in both directions.
	// The queries share a plan cache otherwise, so a verdict could depend on
	// what ran before it — which would make the whole shape a statement about
	// evaluation ORDER rather than about the predicate.
	ran := 0
	runAll := func(label string, ps []string) {
		for _, p := range ps {
			c2, err := db.Conn(ctx)
			if err != nil {
				t.Fatalf("conn: %v", err)
			}
			q := "SELECT l.id AS l_id, r.id AS r_id FROM t_rd AS l RIGHT JOIN t_rd AS r ON l.id = r.a WHERE " + p
			err = qovExec(ctx, c2, q)
			c2.Close() //nolint:errcheck
			ran++
			status := "runs"
			if isQOVBindingError(err) {
				status = "QOV-ERROR"
			} else if err != nil {
				status = "other-err: " + err.Error()
			}
			t.Logf("INARITY %-4s %-28s %s\n", label, p, status)
			if isQOVBindingError(err) {
				t.Errorf("%s: %q raised the QOV binding error — InComparisonToExplodeRule's "+
					"single-element collapse must reuse f.GetInner() instead of minting a fresh "+
					"quantifier, or the memo group's alternatives disagree on their result correlation",
					label, p)
			} else if err != nil {
				t.Errorf("%s: %q failed for a different reason: %v", label, p, err)
			}
		}
	}
	runAll("fwd", preds)
	rev := make([]string, len(preds))
	for i := range preds {
		rev[i] = preds[len(preds)-1-i]
	}
	runAll("rev", rev)

	// The population guard. Every arm must have EXECUTED — a loop that stopped
	// issuing queries reports zero errors for exactly the same reason a correct
	// engine does.
	if want := 2 * len(preds); ran != want {
		t.Fatalf("executed %d queries, want %d (%d predicates x 2 directions)", ran, want, len(preds))
	}
}
