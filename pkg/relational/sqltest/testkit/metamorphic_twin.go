package testkit

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
	"errors"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// mmTwin is a pair of connections to two schemas over the same table shapes:
// idx has the indexes under test, plain has none.
type Twin struct {
	Idx   *sql.DB
	Plain *sql.DB
	T     *testing.T
	Ctx   context.Context
}

// mmNewTwin creates database dbPath with two schemas built from the same table
// DDL: `si` additionally carries indexDDL, `sn` carries nothing. Both are
// returned open.
//
// tableDDL and indexDDL are raw schema-template fragments ("CREATE TABLE ... "
// / "CREATE INDEX ... "), concatenated as the relational DDL expects.
func NewTwin(t *testing.T, ctx context.Context, dbPath, templatePrefix, tableDDL, indexDDL string) *Twin {
	t.Helper()
	setup := OpenDB(t, dbPath)
	MustExecCtx(t, setup, ctx, "CREATE DATABASE "+dbPath)
	MustExecCtx(t, setup, ctx, "CREATE SCHEMA TEMPLATE "+templatePrefix+"_idx "+tableDDL+indexDDL)
	MustExecCtx(t, setup, ctx, "CREATE SCHEMA TEMPLATE "+templatePrefix+"_plain "+tableDDL)
	MustExecCtx(t, setup, ctx, "CREATE SCHEMA "+dbPath+"/si WITH TEMPLATE "+templatePrefix+"_idx")
	MustExecCtx(t, setup, ctx, "CREATE SCHEMA "+dbPath+"/sn WITH TEMPLATE "+templatePrefix+"_plain")

	open := func(schema string) *sql.DB {
		dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=%s", strings.ToUpper(dbPath), clusterFilePath, strings.ToUpper(schema))
		db, err := sql.Open("fdbsql", dsn)
		if err != nil {
			t.Fatalf("open %s/%s: %v", dbPath, schema, err)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	return &Twin{Idx: open("si"), Plain: open("sn"), T: t, Ctx: ctx}
}

// Sub rebinds the twin to a SUBTEST's *testing.T, sharing the same two
// connections.
//
// Without it a twin built in the parent keeps reporting against the parent, so
// a `t.Run` subtest whose every query failed still prints `--- PASS` and only
// the parent turns red. That is the reporting failure this repo names as its
// dominant false positive, wearing its most convincing face: the failures ARE
// printed and the parent IS red, so nothing is lost from a full log — but the
// per-subtest verdict, which is what a reader scans and what a CI summary
// surfaces, says the opposite of what happened. Observed live: with the IN-list
// flatten mutated out, `in_list_items` reported PASS while all nine of its
// queries returned 0AF00.
//
//	t.Run("name", func(t *testing.T) { w := w.Sub(t); w.Want(…) })
func (w *Twin) Sub(t *testing.T) *Twin {
	return &Twin{Idx: w.Idx, Plain: w.Plain, T: t, Ctx: w.Ctx}
}

// Exec runs stmt against BOTH schemas. A statement that succeeds on one side and
// fails on the other is itself a finding, so the asymmetry is checked before the
// error is reported.
func (w *Twin) Exec(stmt string) {
	w.T.Helper()
	_, ei := w.Idx.ExecContext(w.Ctx, stmt)
	_, en := w.Plain.ExecContext(w.Ctx, stmt)
	if (ei == nil) != (en == nil) {
		w.T.Fatalf("DML asymmetry between indexed and unindexed schema\n  stmt: %s\n  indexed:   %v\n  unindexed: %v",
			stmt, ei, en)
	}
	if ei != nil {
		w.T.Fatalf("exec %q failed on both schemas: %v", stmt, ei)
	}
}

// mmRows runs q and renders each row as a |-joined string so a case can state
// its expectation without knowing the column count. NULL renders as "NULL",
// which is distinct from the empty string a NULL-free empty column produces.
func QueryRowStrings(t *testing.T, ctx context.Context, db *sql.DB, q string) ([]string, error) {
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
func (w *Twin) Explain(q string) string {
	w.T.Helper()
	var plan string
	if err := w.Idx.QueryRowContext(w.Ctx, "EXPLAIN "+q).Scan(&plan); err != nil {
		w.T.Fatalf("EXPLAIN %q: %v", q, err)
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
func (w *Twin) Want(name, q string, want []string) {
	w.T.Helper()
	gi, ei := QueryRowStrings(w.T, w.Ctx, w.Idx, q)
	gn, en := QueryRowStrings(w.T, w.Ctx, w.Plain, q)
	if ei != nil || en != nil {
		w.T.Errorf("%s: query failed\n  q: %s\n  indexed:   %v\n  unindexed: %v", name, q, ei, en)
		return
	}
	if !EqualRows(gn, want) {
		w.T.Errorf("%s: UNINDEXED (oracle) answer is wrong\n  q: %s\n  got  %v\n  want %v\n  %s",
			name, q, gn, want, MmFirstDiff(gn, want))
	}
	if !EqualRows(gi, want) {
		w.T.Errorf("%s: INDEXED answer is wrong\n  q: %s\n  got  %v\n  want %v\n  %s\n  plan: %s",
			name, q, gi, want, MmFirstDiff(gi, want), w.Explain(q))
	}
	if !EqualRows(gi, gn) {
		w.T.Errorf("%s: indexed and unindexed DISAGREE\n  q: %s\n  indexed  : %v\n  unindexed: %v\n  plan: %s",
			name, q, gi, gn, w.Explain(q))
	}
}

// WantRejected asserts that BOTH schemas refuse q with the same SQLSTATE.
//
// Rejection is part of the answer, so it belongs to the twin invariant like any
// other: an index may change the PLAN, it may never change whether a query is
// ACCEPTED. A shape that errors unindexed and plans indexed (or the reverse)
// is a defect in index matching even though no row was ever compared, and
// nothing else here would catch it — Want() reports a query that failed on both
// sides as a single "query failed" line and moves on.
//
// The code is compared, not the message: wording is free to differ between the
// two paths, a SQLSTATE is not.
func (w *Twin) WantRejected(name, q, wantCode string) {
	w.T.Helper()
	code := func(db *sql.DB) (string, error) {
		_, err := db.QueryContext(w.Ctx, q)
		if err == nil {
			return "", nil
		}
		var apiErr *api.Error
		if errors.As(err, &apiErr) {
			return string(apiErr.Code), err
		}
		return "<not-an-api.Error:" + err.Error() + ">", err
	}
	ci, ei := code(w.Idx)
	cn, en := code(w.Plain)
	if ei == nil || en == nil {
		w.T.Errorf("%s: expected BOTH schemas to reject\n  q: %s\n  indexed err  : %v\n  unindexed err: %v",
			name, q, ei, en)
		return
	}
	if ci != cn {
		w.T.Errorf("%s: the two schemas reject with DIFFERENT sqlstates, so an index changed "+
			"whether/how the query is accepted\n  q: %s\n  indexed  : %s (%v)\n  unindexed: %s (%v)",
			name, q, ci, ei, cn, en)
	}
	if ci != wantCode {
		w.T.Errorf("%s: wrong sqlstate\n  q: %s\n  got  %s (%v)\n  want %s",
			name, q, ci, ei, wantCode)
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
func (w *Twin) WantKnownDivergence(name, q string, wantIndexed, wantOracle []string, why string) {
	w.T.Helper()
	gi, ei := QueryRowStrings(w.T, w.Ctx, w.Idx, q)
	gn, en := QueryRowStrings(w.T, w.Ctx, w.Plain, q)
	if ei != nil || en != nil {
		w.T.Errorf("%s: query failed\n  q: %s\n  indexed:   %v\n  unindexed: %v", name, q, ei, en)
		return
	}
	if !EqualRows(gn, wantOracle) {
		w.T.Errorf("%s: UNINDEXED (oracle) answer moved — the SQL-correct result is what this pin "+
			"rests on\n  q: %s\n  got  %v\n  want %v", name, q, gn, wantOracle)
	}
	if !EqualRows(gi, wantIndexed) {
		w.T.Errorf("%s: the known divergence MOVED. Either it was repaired (re-arm this pin to "+
			"WantKnownDivergence's oracle list and delete the divergence) or it changed shape.\n"+
			"  q: %s\n  indexed got  %v\n  indexed want %v\n  why: %s", name, q, gi, wantIndexed, why)
	}
	if EqualRows(gi, gn) {
		w.T.Errorf("%s: indexed and unindexed now AGREE, so the divergence this pin describes is "+
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
func (w *Twin) ExplainInTx(q string) string {
	w.T.Helper()
	tx, err := w.Idx.BeginTx(w.Ctx, nil)
	if err != nil {
		w.T.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	var plan string
	if err := tx.QueryRowContext(w.Ctx, "EXPLAIN "+q).Scan(&plan); err != nil {
		w.T.Fatalf("EXPLAIN %q in a transaction: %v", q, err)
	}
	return plan
}

// WantInTx is Want with both sides read inside their own explicit transaction,
// so the single-read-version proofs are licensed. Use it for any case whose
// point is an optimization gated on one read version; use Want otherwise.
func (w *Twin) WantInTx(name, q string, want []string) {
	w.T.Helper()
	read := func(db *sql.DB) ([]string, error) {
		tx, err := db.BeginTx(w.Ctx, nil)
		if err != nil {
			return nil, err
		}
		defer func() { _ = tx.Rollback() }()
		rows, err := tx.QueryContext(w.Ctx, q)
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
	gi, ei := read(w.Idx)
	gn, en := read(w.Plain)
	if ei != nil || en != nil {
		w.T.Errorf("%s: query failed in a transaction\n  q: %s\n  indexed: %v\n  unindexed: %v",
			name, q, ei, en)
		return
	}
	if !EqualRows(gn, want) {
		w.T.Errorf("%s: UNINDEXED (oracle) answer is wrong\n  q: %s\n  got  %v\n  want %v",
			name, q, gn, want)
	}
	if !EqualRows(gi, want) {
		w.T.Errorf("%s: INDEXED answer is wrong\n  q: %s\n  got  %v\n  want %v\n  plan: %s",
			name, q, gi, want, w.ExplainInTx(q))
	}
}

// WantPlanContains fails unless the indexed plan contains marker. A row
// assertion that silently stopped exercising the operator under test is a green
// that proves nothing, so every case whose point is an index-backed operator
// pins the operator too.
func (w *Twin) WantPlanContains(name, q, marker string) {
	w.T.Helper()
	plan := w.Explain(q)
	if !strings.Contains(plan, marker) {
		w.T.Errorf("%s: plan does not reach %s — the row assertion below proves nothing about it\n  q: %s\n  plan: %s",
			name, marker, q, plan)
	}
}

func EqualRows(a, b []string) bool {
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

func MmFirstDiff(got, want []string) string {
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
