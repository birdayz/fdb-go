package sqldriver_test

// RFC-173 S4 :2908/:3033 — projected EXISTS over a BURIED INNER box
// `(p JOIN q) JOIN r`. The 3-way `p JOIN q ... JOIN r ...` associates left, so
// the fold's left leg is the `(p JOIN q)` INNER cluster — a buried gated box,
// NOT a top-level scan.
//
// CURRENT STATE (empirically characterized): this DECLINES cleanly (0AF00). The
// buried box plans fine OUTSIDE a fold (`SELECT p.v FROM (p JOIN q) JOIN r` works)
// and a scan-leg (2-leg) projected-EXISTS fold works (commit 2 ordinalized it).
// The gap is the N-WAY EXISTENTIAL FOLD: under AXIS-1 the INNER box is mergeable,
// so SelectMergeRule flattens it and the fold becomes a 4-quantifier
// `[ForEach(p),ForEach(q),ForEach(r),Existential]` select; the executor dispatch
// (rule_implement_nested_loop_join.go:46-54) matches only 2/3 quantifiers → no
// plan → 0AF00. (The flat N-way INNER join itself plans fine — only the N-way
// existential wrap is missing.) So it is a REACH GAP (Java folds
// `(p JOIN q) JOIN r` under projected EXISTS and answers [[10 true]]; Go declines),
// same family as the LEFT buried box (rfc173_f2left_buriedbox) but WITHOUT LEFT's
// null-extension. RFC :2908/:3033 REVISED DESIGN (N-way flat existential).
//
// This pin documents the reach gap. When the N-way existential generalization
// lands (dispatch relaxed to N ForEach + Existential; implementJoinWithExistential
// + reconstructFoldStep1Seed generalized 2→N), this flips to asserting the
// DISCRIMINATING rows (a p-only projected + p-only EXISTS-correlated column so a
// mis-bind to q/r flips the answer) with Java-derived expected values.

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
)

func TestFDB_RFC173S4_BuriedInnerBoxProjectedExists(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/rfc173s4bibx"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE rfc173s4bibx_tmpl"+
		" CREATE TABLE p (id BIGINT, v BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE q (qid BIGINT, PRIMARY KEY (qid))"+
		" CREATE TABLE r (rid BIGINT, PRIMARY KEY (rid))"+
		" CREATE TABLE e (eid BIGINT, PRIMARY KEY (eid))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE rfc173s4bibx_tmpl"); err != nil {
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
		"INSERT INTO r VALUES (1)",
		"INSERT INTO e VALUES (1)",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}

	// (p JOIN q ON q.qid = p.id) = {(id=1, v=10)}; JOIN r ON r.rid = p.id keeps
	// id=1. EXISTS(e.eid = p.id=1) → true. Java answers [[10 true]]; Go currently
	// DECLINES (0AF00) — the fold has no ordinal seed for the buried-box leg yet.
	sqlText := "SELECT p.v, EXISTS (SELECT 1 FROM e WHERE e.eid = p.id) " +
		"FROM p JOIN q ON q.qid = p.id JOIN r ON r.rid = p.id"
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
			got = append(got, [2]any{v, ex.Valid && ex.Bool})
		}
		t.Fatalf("buried INNER box projected EXISTS produced rows %v — the :2908/:3033 executor"+
			" widening is not yet landed, so this must still be a clean 0AF00 reach-gap decline."+
			" If rows appear here, the widening landed and this pin should flip to assert [[10 true]]"+
			" with the dual-window cross-agreement", got)
	}
	// The decline must be the clean 0AF00 reach-gap decline, never a planner
	// failure of a different kind.
	var apiErr *api.Error
	if errors.As(err, &apiErr) {
		if string(apiErr.Code) != "0AF00" {
			t.Fatalf("buried INNER box decline sqlstate %q, want 0AF00: %v", apiErr.Code, err)
		}
		return
	}
	if !strings.Contains(err.Error(), "0AF00") {
		t.Fatalf("buried INNER box decline is not the clean 0AF00 reach-gap decline: %v", err)
	}
}
