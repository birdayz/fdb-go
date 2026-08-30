package factory_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// The QOV-binding failure's minimal shape, re-measured.
//
// TODO.md records this defect as a three-clause conjunction: "an OUTER join, a
// DUPLICATE-valued `IN` list, and an INDEXED column of the NULL-PADDED side",
// measured over LEFT JOIN only. A rowdiff sweep of seeds 88000001..88002326
// hit it again at seed 88001928 (which reproduces standalone in ~5s via
// ROWDIFF_SEED_START=88001928 ROWDIFF_SEEDS=1) on
//
//	FROM t_rd AS l RIGHT JOIN t_rd AS r ON l.id = r.a WHERE r.c IN (7, 7)
//
// where `r` is the PRESERVED side of the RIGHT JOIN — the position the recorded
// table lists as OK for the LEFT-JOIN spelling.
//
// It is an EXECUTION failure, not a planning one: the message comes from
// quantifiedObjectValue.Evaluate (values.go:5426), so the planner emits a plan
// whose QOV names a quantifier the executor never binds. Planning the same SQL
// through embedded.PlanQueryForTest returns no error for ANY arm below, which
// is why this probe executes instead of planning — a plan-only version of this
// test passes against the live defect.
func qovExec(ctx context.Context, conn *sql.Conn, query string) error {
	rows, err := conn.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		// Drain: the resolution error surfaces while producing rows, not at
		// QueryContext. A probe that only checked the QueryContext error would
		// report every arm clean.
		cols, cerr := rows.Columns()
		if cerr != nil {
			return cerr
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
	}
	return rows.Err()
}

func isQOVBindingError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "has no declared runtime binding")
}

func TestFDB_QOVBindingMinimalShape(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	// The seed's own schema and rows, verbatim. Minimising the DDL is a
	// separate question from establishing the shape, and a probe that
	// minimised first would not have been able to tell "my simplification
	// killed it" from "this arm is genuinely clean".
	const ddl = "CREATE TABLE T_RD (id BIGINT, a BIGINT, b BIGINT, c BIGINT, s STRING, f BOOLEAN, d DOUBLE, e FLOAT, PRIMARY KEY (id)) " +
		"CREATE INDEX idx_c ON T_RD (c) CREATE INDEX idx_ab ON T_RD (a, b) CREATE INDEX idx_b ON T_RD (b) CREATE INDEX idx_a ON T_RD (a)"
	const insert = "INSERT INTO T_RD VALUES " +
		"(1, 2, 9, 7, ' a', TRUE, 9.0, 2.0), (2, 7, 3, 7, 'gamma', FALSE, 9.0, NULL), " +
		"(3, 6, 8, 1, 'b ', NULL, -9007199254740992.0, 7.0), (5, 7, -1, 9, 'b ', TRUE, 6.0, 9.0), " +
		"(8, 3, 6, 2, 'gamma', NULL, 7.0, 7.0), (15, 4, 5, 7, 'b ', TRUE, 0.1, NULL), " +
		"(16, NULL, 5, 8, 'beta', TRUE, 5.0, NULL), (20, 6, 7, 1, 'b ', TRUE, 8.0, 7.0)"

	setupDB, err := sql.Open("fdbsql", "fdbsql:///__SYS?cluster_file="+clusterFilePath+"&schema=CATALOG")
	if err != nil {
		t.Fatalf("open sys: %v", err)
	}
	defer setupDB.Close()
	const dbPath, schema, tmpl = "/QOVSHAPE", "qovshape", "qovshapet"
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

	// The full cross, generated rather than hand-listed, so no arm is missing
	// because nobody thought of it. Four axes: join type, which relation the
	// predicate reads, whether that column is indexed, and whether the IN list
	// repeats a value.
	type arm struct {
		join   string // LEFT / RIGHT / INNER
		alias  string // l or r
		col    string // c (indexed) or s (unindexed)
		dupIn  bool
		gotErr bool
	}
	var matrix []arm
	inList := func(col string, dup bool) string {
		if col == "c" {
			if dup {
				return "(7, 7)"
			}
			return "(7, 1)"
		}
		if dup {
			return "('b ', 'b ')"
		}
		return "('b ', 'gamma')"
	}
	for _, join := range []string{"LEFT", "RIGHT", "INNER"} {
		for _, alias := range []string{"l", "r"} {
			for _, col := range []string{"c", "s"} {
				for _, dup := range []bool{true, false} {
					q := fmt.Sprintf(
						"SELECT l.id AS l_id, r.id AS r_id FROM t_rd AS l %s JOIN t_rd AS r ON l.id = r.a WHERE %s.%s IN %s",
						join, alias, col, inList(col, dup))
					// A NON-QOV error must not be read as "this arm is clean".
					//
					// isQOVBindingError answers false for every error that is
					// not the binding failure, so folding the result straight
					// into the matrix made a type error, a syntax error or an
					// infrastructure failure indistinguishable from a query
					// that ran and returned rows. Twenty-four arms could fail
					// for unrelated reasons and this matrix would report a
					// clean engine.
					err := qovExec(ctx, conn, q)
					if err != nil && !isQOVBindingError(err) {
						t.Errorf("%s JOIN, %s.%s, dup=%v: failed for a DIFFERENT reason, so this arm "+
							"proves nothing about the QOV defect: %v", join, alias, col, dup, err)
					}
					matrix = append(matrix, arm{join, alias, col, dup, isQOVBindingError(err)})
				}
			}
		}
	}
	t.Log("QOV --- full matrix (join / predicate alias / column / duplicate IN) ---")
	for _, a := range matrix {
		idx := "indexed"
		if a.col == "s" {
			idx = "UNindexed"
		}
		status := "runs"
		if a.gotErr {
			status = "QOV-ERROR"
		}
		t.Logf("QOV   %-5s %s.%s %-9s dup=%-5v  %s\n", a.join, a.alias, a.col, idx, a.dupIn, status)
	}
	// ZERO arms may error. The alarm direction here is deliberately the
	// opposite of the one this probe was born with.
	//
	// While the defect was live, exactly 3 of these 24 arms failed:
	//
	//	LEFT   r.c  indexed    dup=true
	//	RIGHT  r.c  indexed    dup=true
	//	RIGHT  r.s  UNindexed  dup=true
	//
	// and the guard watched for that count changing, because a shift meant the
	// shape had moved. The fix in InComparisonToExplodeRule's single-element
	// collapse (it re-minted the inner quantifier, publishing an alternative
	// whose result correlation differed from the rest of its memo group) makes
	// zero the steady state, so a floor at 3 is now unsatisfiable and the thing
	// worth watching is GROWTH: any arm erroring means the defect came back.
	//
	// The 3-arm shape is kept in this comment rather than deleted because it is
	// what makes a future non-zero readable — it says which arms to look at
	// first, and it records that the shape is NOT the one TODO.md originally
	// carried ("an INDEXED column of the NULL-PADDED side"): it is the join
	// clause's RIGHT-HAND relation whichever side that is, and the index is
	// load-bearing only for LEFT JOIN.
	var errored int
	for _, a := range matrix {
		if !a.gotErr {
			continue
		}
		errored++
		t.Errorf("%s JOIN, predicate on %s.%s, dup=%v: QOV binding error is BACK — "+
			"InComparisonToExplodeRule's single-element collapse must keep reusing f.GetInner() "+
			"rather than minting a fresh quantifier, or the group's alternatives disagree on their "+
			"result correlation and the executor cannot bind it",
			a.join, a.alias, a.col, a.dupIn)
	}
	// The population guard that still matters. Every arm must have RUN: a
	// matrix that silently stopped executing queries reports zero errors for
	// the same reason a fixed engine does.
	if len(matrix) != 24 {
		t.Fatalf("matrix has %d arms, want 24 (LEFT/RIGHT/INNER x l/r x indexed/unindexed x "+
			"dup/distinct) — the cross stopped covering the shape", len(matrix))
	}
	t.Logf("QOV binding: %d of %d arms error (want 0)", errored, len(matrix))
}
