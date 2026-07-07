package sqldriver_test

// RFC-173 S4 F2-LEFT SCOPE BOUNDARY — a projected EXISTS over a BURIED box
// `(p JOIN q) LEFT JOIN s` (the 3-way `p JOIN q ... LEFT JOIN s ...` associates
// left, so the LEFT's preserved leg is the inner join box). The preserved leg is
// NOT an ordinal-safe scan, so it does not ordinalize (the executor's
// legIsOrdinalSafe rejects the join leg → gatedSeedStep1 false). F2-LEFT's scope
// is SCAN legs only: folding the buried box would fall through to
// the name-model :698 (NewAnchoredJoinRecord) path with a null-extended
// name-keyed row — correct today via the coexistence Datum but a producer the S4
// demolition removes. So Go DECLINES it cleanly (0AF00) as a Java-parity REACH
// gap (Java folds and answers `[[10 false]]`; Go rejects), NEVER minting a
// name-model producer for the outer-join fold.
//
// This pin GUARDS the scope boundary: a future change that re-enables the buried
// box via the name-model path (the exact producer the demolition must have zeroed)
// flips this from a clean decline to rows and trips the test. The scan-leg
// answers live in rfc173_f2left_projected_exists_fdb_test.go.

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
)

func TestFDB_RFC173F2Left_BuriedBoxDeclinesCleanly(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/rfc173f2lbb"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE rfc173f2lbb_tmpl"+
		" CREATE TABLE p (id BIGINT, v BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE q (qid BIGINT, PRIMARY KEY (qid))"+
		" CREATE TABLE s (sid BIGINT, PRIMARY KEY (sid))"+
		" CREATE TABLE r (rid BIGINT, PRIMARY KEY (rid))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE rfc173f2lbb_tmpl"); err != nil {
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
			" decline (scan-leg scope only; producing rows here means the name-model :698 path was"+
			" re-enabled for the outer-join fold — the exact producer the S4 demolition must have zeroed)", got)
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
