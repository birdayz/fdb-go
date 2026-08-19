package sqldriver_test

// The indexed/unindexed TWIN harness.
//
// Two schemas in one database hold IDENTICAL data and differ ONLY in which
// indexes exist. Every query is run against both. An index may change the PLAN;
// it may never change the ANSWER, so any row-level difference is a defect in
// index matching, index maintenance or an index-backed operator — and the
// unindexed side is the oracle.
//
// This oracle is worth having next to the corpus's hand-written expectations
// because it needs nobody to know the right answer in advance: it catches
// defects the author of an expectation did not anticipate, which is exactly the
// class a `rows:` block cannot catch. Tests here assert BOTH — the twin
// agreement AND the absolute SQL-correct answer — because agreement alone is
// satisfied by two engines that are wrong in the same way.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// mmTwin is a pair of connections to two schemas over the same table shapes:
// idx has the indexes under test, plain has none.
type mmTwin struct {
	idx   *sql.DB
	plain *sql.DB
	t     *testing.T
	ctx   context.Context
}

// mmNewTwin creates database dbPath with two schemas built from the same table
// DDL: `si` additionally carries indexDDL, `sn` carries nothing. Both are
// returned open.
//
// tableDDL and indexDDL are raw schema-template fragments ("CREATE TABLE ... "
// / "CREATE INDEX ... "), concatenated as the relational DDL expects.
func mmNewTwin(t *testing.T, ctx context.Context, dbPath, templatePrefix, tableDDL, indexDDL string) *mmTwin {
	t.Helper()
	setup := openTestDB(t, dbPath)
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE "+dbPath)
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE "+templatePrefix+"_idx "+tableDDL+indexDDL)
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE "+templatePrefix+"_plain "+tableDDL)
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA "+dbPath+"/si WITH TEMPLATE "+templatePrefix+"_idx")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA "+dbPath+"/sn WITH TEMPLATE "+templatePrefix+"_plain")

	open := func(schema string) *sql.DB {
		dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=%s", dbPath, clusterFilePath, schema)
		db, err := sql.Open("fdbsql", dsn)
		if err != nil {
			t.Fatalf("open %s/%s: %v", dbPath, schema, err)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	return &mmTwin{idx: open("si"), plain: open("sn"), t: t, ctx: ctx}
}

// Exec runs stmt against BOTH schemas. A statement that succeeds on one side and
// fails on the other is itself a finding, so the asymmetry is checked before the
// error is reported.
func (w *mmTwin) Exec(stmt string) {
	w.t.Helper()
	_, ei := w.idx.ExecContext(w.ctx, stmt)
	_, en := w.plain.ExecContext(w.ctx, stmt)
	if (ei == nil) != (en == nil) {
		w.t.Fatalf("DML asymmetry between indexed and unindexed schema\n  stmt: %s\n  indexed:   %v\n  unindexed: %v",
			stmt, ei, en)
	}
	if ei != nil {
		w.t.Fatalf("exec %q failed on both schemas: %v", stmt, ei)
	}
}

// mmRows runs q and renders each row as a |-joined string so a case can state
// its expectation without knowing the column count. NULL renders as "NULL",
// which is distinct from the empty string a NULL-free empty column produces.
func mmRows(t *testing.T, ctx context.Context, db *sql.DB, q string) ([]string, error) {
	t.Helper()
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []string
	for rows.Next() {
		cells := make([]any, len(cols))
		for i := range cells {
			cells[i] = new(sql.NullString)
		}
		if err := rows.Scan(cells...); err != nil {
			return nil, err
		}
		parts := make([]string, len(cells))
		for i, c := range cells {
			v := c.(*sql.NullString)
			if v.Valid {
				parts[i] = v.String
			} else {
				parts[i] = "NULL"
			}
		}
		out = append(out, strings.Join(parts, "|"))
	}
	// rows.Err() is checked separately from the comparison: an iteration that
	// died mid-stream otherwise reads as a short result set, which is the same
	// green an empty table produces.
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// Explain returns the rendered plan for q on the INDEXED side. Used to prove a
// case actually reaches the operator under test — without it, a green is a
// statement about whichever plan the cost model happened to pick.
func (w *mmTwin) Explain(q string) string {
	w.t.Helper()
	var plan string
	if err := w.idx.QueryRowContext(w.ctx, "EXPLAIN "+q).Scan(&plan); err != nil {
		w.t.Fatalf("EXPLAIN %q: %v", q, err)
	}
	return plan
}

// Want asserts the SQL-correct answer on BOTH schemas.
//
// It checks three things and reports them separately, because they fail for
// different reasons: the unindexed side wrong means the scan/executor is wrong,
// the indexed side wrong means the index path is wrong, and the two disagreeing
// with each other localizes it to the index path even when `want` itself is in
// doubt.
func (w *mmTwin) Want(name, q string, want []string) {
	w.t.Helper()
	gi, ei := mmRows(w.t, w.ctx, w.idx, q)
	gn, en := mmRows(w.t, w.ctx, w.plain, q)
	if ei != nil || en != nil {
		w.t.Errorf("%s: query failed\n  q: %s\n  indexed:   %v\n  unindexed: %v", name, q, ei, en)
		return
	}
	if !mmEqRows(gn, want) {
		w.t.Errorf("%s: UNINDEXED (oracle) answer is wrong\n  q: %s\n  got  %v\n  want %v\n  %s",
			name, q, gn, want, mmFirstDiff(gn, want))
	}
	if !mmEqRows(gi, want) {
		w.t.Errorf("%s: INDEXED answer is wrong\n  q: %s\n  got  %v\n  want %v\n  %s\n  plan: %s",
			name, q, gi, want, mmFirstDiff(gi, want), w.Explain(q))
	}
	if !mmEqRows(gi, gn) {
		w.t.Errorf("%s: indexed and unindexed DISAGREE\n  q: %s\n  indexed  : %v\n  unindexed: %v\n  plan: %s",
			name, q, gi, gn, w.Explain(q))
	}
}

// WantKnownDivergence pins a divergence that is KNOWN and not yet repaired.
//
// It asserts three things, and the third is the point: the unindexed answer is
// the correct one, the indexed answer is the specific wrong one produced today,
// and the two still DISAGREE. The last arm is what makes the pin self-retiring
// — the moment the defect is fixed this test fails and says so, rather than
// quietly continuing to describe a state that no longer exists.
//
// This is not a way to accept a wrong answer. It is how a wrong answer stays
// visible while the fix it needs is decided, and every use carries `why`.
func (w *mmTwin) WantKnownDivergence(name, q string, wantIndexed, wantOracle []string, why string) {
	w.t.Helper()
	gi, ei := mmRows(w.t, w.ctx, w.idx, q)
	gn, en := mmRows(w.t, w.ctx, w.plain, q)
	if ei != nil || en != nil {
		w.t.Errorf("%s: query failed\n  q: %s\n  indexed:   %v\n  unindexed: %v", name, q, ei, en)
		return
	}
	if !mmEqRows(gn, wantOracle) {
		w.t.Errorf("%s: UNINDEXED (oracle) answer moved — the SQL-correct result is what this pin "+
			"rests on\n  q: %s\n  got  %v\n  want %v", name, q, gn, wantOracle)
	}
	if !mmEqRows(gi, wantIndexed) {
		w.t.Errorf("%s: the known divergence MOVED. Either it was repaired (re-arm this pin to "+
			"WantKnownDivergence's oracle list and delete the divergence) or it changed shape.\n"+
			"  q: %s\n  indexed got  %v\n  indexed want %v\n  why: %s", name, q, gi, wantIndexed, why)
	}
	if mmEqRows(gi, gn) {
		w.t.Errorf("%s: indexed and unindexed now AGREE, so the divergence this pin describes is "+
			"gone. Replace this call with Want(...) asserting the correct answer.\n  q: %s\n  both: %v\n"+
			"  why: %s", name, q, gi, why)
	}
}

// ExplainInTx is Explain taken inside an EXPLICIT TRANSACTION, and the two are
// not interchangeable: the secondary-UNIQUE distinct proof is licensed only
// where the whole result comes from ONE read version, which an explicit
// transaction provides and auto-commit does not — in auto-commit each page
// takes a fresh read version, a value can move between pages and be emitted
// twice, so the proof is deliberately withheld.
//
// A test that reads a plan in auto-commit and expects an elision therefore sees
// the UN-elided plan and is asserting the wrong thing about a correct engine.
func (w *mmTwin) ExplainInTx(q string) string {
	w.t.Helper()
	tx, err := w.idx.BeginTx(w.ctx, nil)
	if err != nil {
		w.t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	var plan string
	if err := tx.QueryRowContext(w.ctx, "EXPLAIN "+q).Scan(&plan); err != nil {
		w.t.Fatalf("EXPLAIN %q in a transaction: %v", q, err)
	}
	return plan
}

// WantInTx is Want with both sides read inside their own explicit transaction,
// so the single-read-version proofs are licensed. Use it for any case whose
// point is an optimization gated on one read version; use Want otherwise.
func (w *mmTwin) WantInTx(name, q string, want []string) {
	w.t.Helper()
	read := func(db *sql.DB) ([]string, error) {
		tx, err := db.BeginTx(w.ctx, nil)
		if err != nil {
			return nil, err
		}
		defer func() { _ = tx.Rollback() }()
		rows, err := tx.QueryContext(w.ctx, q)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			return nil, err
		}
		var out []string
		for rows.Next() {
			cells := make([]any, len(cols))
			for i := range cells {
				cells[i] = new(sql.NullString)
			}
			if err := rows.Scan(cells...); err != nil {
				return nil, err
			}
			parts := make([]string, len(cells))
			for i, c := range cells {
				v := c.(*sql.NullString)
				if v.Valid {
					parts[i] = v.String
				} else {
					parts[i] = "NULL"
				}
			}
			out = append(out, strings.Join(parts, "|"))
		}
		return out, rows.Err()
	}
	gi, ei := read(w.idx)
	gn, en := read(w.plain)
	if ei != nil || en != nil {
		w.t.Errorf("%s: query failed in a transaction\n  q: %s\n  indexed: %v\n  unindexed: %v",
			name, q, ei, en)
		return
	}
	if !mmEqRows(gn, want) {
		w.t.Errorf("%s: UNINDEXED (oracle) answer is wrong\n  q: %s\n  got  %v\n  want %v",
			name, q, gn, want)
	}
	if !mmEqRows(gi, want) {
		w.t.Errorf("%s: INDEXED answer is wrong\n  q: %s\n  got  %v\n  want %v\n  plan: %s",
			name, q, gi, want, w.ExplainInTx(q))
	}
}

// WantPlanContains fails unless the indexed plan contains marker. A row
// assertion that silently stopped exercising the operator under test is a green
// that proves nothing, so every case whose point is an index-backed operator
// pins the operator too.
func (w *mmTwin) WantPlanContains(name, q, marker string) {
	w.t.Helper()
	plan := w.Explain(q)
	if !strings.Contains(plan, marker) {
		w.t.Errorf("%s: plan does not reach %s — the row assertion below proves nothing about it\n  q: %s\n  plan: %s",
			name, marker, q, plan)
	}
}

func mmEqRows(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func mmFirstDiff(got, want []string) string {
	n := len(got)
	if len(want) < n {
		n = len(want)
	}
	for i := 0; i < n; i++ {
		if got[i] != want[i] {
			return fmt.Sprintf("first difference at row %d: got %q, want %q", i, got[i], want[i])
		}
	}
	if len(got) != len(want) {
		return fmt.Sprintf("common prefix agrees; lengths differ: got %d rows, want %d", len(got), len(want))
	}
	return ""
}
