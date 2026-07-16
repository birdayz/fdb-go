package sqldriver_test

// Regression pin for a projected EXISTS over a BURIED box under a LEFT JOIN:
// `(p JOIN q) LEFT JOIN s` (the 3-way clause associates left, so the LEFT's
// preserved leg is the inner join box). The preserved leg is a JOIN, not a
// scan, so it is not ordinal-safe (the executor's legIsOrdinalSafe rejects a
// join leg). Folding a buried box like this has no name-model producer any
// more, so Go DECLINES the query cleanly (0AF00) rather than minting a fresh
// one. This is a Java-parity REACH gap (Java folds and answers `[[10
// false]]`; Go rejects), not a correctness bug.
//
// This pin GUARDS the scope boundary: a future change that re-enables the
// buried box (by adding back a producer for the outer-join fold) flips this
// from a clean decline to rows and trips the test. The scan-leg-scope
// answers for the same shape (where the LEFT box IS ordinal-safe) are pinned
// separately.

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
)

func TestFDB_LeftJoinBuriedBoxDeclinesCleanly(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/f2lbb"
	setup := openTestDB(t, dbPath)
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
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
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

	// (p JOIN q ON q.qid = p.id) = {(p.v=10)}; LEFT JOIN s (empty) null-extends s.
	// Java answers [[10 false]] (EXISTS(r.rid = s.sid=NULL) → false). Go's scan-leg
	// scope excludes the buried preserved leg → clean 0AF00 decline (reach gap).
	sqlText := "SELECT p.v, EXISTS (SELECT 1 FROM r WHERE r.rid = s.sid) " +
		"FROM p JOIN q ON q.qid = p.id LEFT JOIN s ON s.sid = p.id"
	rows, err := db.QueryContext(ctx, sqlText)
	if err == nil {
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
		t.Fatalf("buried-box projected EXISTS over LEFT JOIN produced rows %v — expected a clean 0AF00"+
			" decline (scan-leg scope only; producing rows here means a name-model producer was"+
			" re-added for the outer-join fold)", got)
	}
	// The decline must be the clean scope-boundary 0AF00, never a planner failure
	// ("no plan found") or a resolution error — those would signal the fold was
	// built but left unimplemented rather than declined at the source.
	var apiErr *api.Error
	if errors.As(err, &apiErr) {
		if string(apiErr.Code) != "0AF00" {
			t.Fatalf("buried-box decline has sqlstate %q, want 0AF00 (clean reach-gap decline): %v", apiErr.Code, err)
		}
		return
	}
	if !strings.Contains(err.Error(), "0AF00") {
		t.Fatalf("buried-box decline is not the clean 0AF00 reach-gap decline: %v", err)
	}
}
