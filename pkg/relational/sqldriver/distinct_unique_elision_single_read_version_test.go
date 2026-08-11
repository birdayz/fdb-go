package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

// RFC-210's secondary-UNIQUE DISTINCT proof is admitted only when the whole
// statement is produced at ONE read version (PlannerConfiguration
// .SingleReadVersion). This is the POSITIVE direction of that gate: the
// optimization must still FIRE on an explicit transaction, where the condition
// genuinely holds — every page runs on the captured transaction, so the whole
// result is one instant of the store and a cross-row uniqueness proof is valid
// for the whole statement rather than page by page.
//
// Without this test the gate is only proved able to turn the feature OFF, which
// a `return secondaryUniqueProof{}` at the top of the function would also
// achieve.
//
// The negative direction — auto-commit must NOT draw the proof, and the rows it
// returns must not duplicate under a cross-page delete/re-insert — is
// distinct_unique_elision_paging_repro_test.go.
func TestFDB_DistinctUniqueElisionFiresInExplicitTx(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	// A one-shot clock spike, so the retry below fires on EVERY run instead of
	// waiting for a loaded machine to supply the condition. Safe to arm for the
	// whole test: preflightTxBudget runs under `if r.tx != nil`, so the DDL and
	// seed statements below — all autocommit — never meet it.
	key, clk := spikedClusterKey(t, 30*time.Second)
	setup := openSpiked(t, key, "/testdb_dusrv", "")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_dusrv")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE dusrv "+
			"CREATE TABLE t1 (id BIGINT, email STRING, PRIMARY KEY (id)) "+
			"CREATE UNIQUE INDEX by_email1 ON t1 (email) "+
			"CREATE TABLE t3 (id BIGINT, email STRING, PRIMARY KEY (id)) "+
			"CREATE UNIQUE INDEX by_email3 ON t3 (email)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_dusrv/s WITH TEMPLATE dusrv")

	db := openSpiked(t, key, "/testdb_dusrv", "s")

	const nRows = 8
	for _, tbl := range []string{"t1", "t3"} {
		var sb strings.Builder
		fmt.Fprintf(&sb, "INSERT INTO %s (id, email) VALUES ", tbl)
		for i := 1; i <= nRows; i++ {
			if i > 1 {
				sb.WriteString(", ")
			}
			fmt.Fprintf(&sb, "(%d, 'e%02d@x')", i, i)
		}
		mwjoMustExec(t, db, ctx, sb.String())
	}

	// The same two arms the repro exercises: R3 narrows the operator to the
	// index's exempt keys, R2 removes it outright once a NULL-rejecting conjunct
	// discharges the exempt set.
	const (
		narrowedQ = "SELECT DISTINCT email FROM t1"
		elidedQ   = "SELECT DISTINCT email FROM t3 WHERE email IS NOT NULL"
	)

	// One row per page, so the drain below crosses a page boundary after every
	// single row — the same paging pressure the repro applies, but on a
	// transaction, where the proof is admissible.
	conn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, int64(1)).Build())
	})
	// THE WHOLE TRANSACTION IS RETRYABLE. Four statements — two EXPLAINs and two
	// drains — run on ONE read version, and that version dies five seconds after
	// it opened in wall clock however busy the machine was. The drains make this
	// the most exposed shape in the suite rather than the least: the scanned-rows
	// limit of 1 puts a page boundary after every row, so eight rows per query
	// means dozens of pages, and every page is a preflight against the same
	// four-second budget.
	//
	// It retries FROM THE PINNED CONNECTION, not from the pool. The whole test
	// depends on OptExecutionScannedRowsLimit being set on THIS connection; a
	// retry that began the second attempt from db would silently get an
	// unconfigured connection, the paging pressure would vanish, and the test
	// would keep passing while measuring nothing. That is why retryTx takes a
	// txBeginner and *sql.Conn satisfies it.
	var narrowedPlan, elidedPlan string
	drained := map[string][]string{}
	var attemptsRun int
	retryTx(t, conn, spikeOnce(clk, &attemptsRun), func(a txAttempt) error {
		narrowedPlan, elidedPlan = "", ""
		drained = map[string][]string{}

		var err error
		if narrowedPlan, err = explainPlanOnErr(ctx, a.tx, narrowedQ); err != nil {
			return err
		}
		if elidedPlan, err = explainPlanOnErr(ctx, a.tx, elidedQ); err != nil {
			return err
		}
		for _, q := range []string{narrowedQ, elidedQ} {
			got, derr := dusrvDrainTxErr(ctx, a.tx, q)
			if derr != nil {
				return derr
			}
			drained[q] = got
		}
		return nil
	})
	mustHaveRetried(t, attemptsRun)

	t.Logf("in-tx EXPLAIN R3 %q\n  => %s", narrowedQ, narrowedPlan)
	t.Logf("in-tx EXPLAIN R2 %q\n  => %s", elidedQ, elidedPlan)

	if !strings.Contains(narrowedPlan, "narrowed-by:BY_EMAIL1") {
		t.Errorf("R3 did not narrow on an EXPLICIT transaction: %s\n"+
			"An explicit transaction runs every page on one read version, which is "+
			"exactly the condition SingleReadVersion asserts. If the proof is "+
			"withheld here the gate is not gating on read-version scope — it is "+
			"switching RFC-210 off entirely.", narrowedPlan)
	}
	if strings.Contains(elidedPlan, "Distinct(") ||
		!strings.Contains(elidedPlan, "distinct-by:BY_EMAIL3") {
		t.Errorf("R2 did not fully elide on an EXPLICIT transaction: %s", elidedPlan)
	}

	// The elided plan must also be RIGHT, not merely shaped correctly: reading
	// it to completion inside the transaction returns every value exactly once.
	for _, tc := range []struct{ tag, q string }{
		{"R3 narrowed", narrowedQ},
		{"R2 elided", elidedQ},
	} {
		got := drained[tc.q]
		if len(got) != nRows {
			t.Errorf("%s returned %d rows, want %d: %v", tc.tag, len(got), nRows, got)
		}
		seen := make(map[string]int, len(got))
		for _, v := range got {
			seen[v]++
		}
		for v, n := range seen {
			if n != 1 {
				t.Errorf("%s emitted %q %d times inside one transaction", tc.tag, v, n)
			}
		}
	}
}

// The plan cache is per-CONNECTION (newCascadesGenerator mints one on the
// EmbeddedConnection), so the two modes only ever meet on a single connection
// that runs the same SQL both ways. That is the one arrangement in which
// omitting SingleReadVersion from the cache key is observable — and it is an
// ordinary one: an application opens a transaction, runs a query, commits, then
// runs the same query without a transaction. The auto-commit run would be
// served the transaction's plan, DISTINCT already elided, and the gate above
// would never be consulted at all.
//
// Asserted on one pinned connection in that exact order, because the reverse
// order proves nothing: caching the auto-commit plan first and then reading it
// in a transaction only loses an optimization.
func TestFDB_DistinctUniqueElisionNotCachedAcrossReadVersionScope(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_dusrvc")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_dusrvc")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE dusrvc "+
			"CREATE TABLE t1 (id BIGINT, email STRING, PRIMARY KEY (id)) "+
			"CREATE UNIQUE INDEX by_email1 ON t1 (email)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_dusrvc/s WITH TEMPLATE dusrvc")

	dsn := fmt.Sprintf("fdbsql:///testdb_dusrvc?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t1 (id, email) VALUES (1, 'a@x'), (2, 'b@x')")

	const q = "SELECT DISTINCT email FROM t1"
	conn := pinEmbeddedConn(t, db, func(*embedded.EmbeddedConnection) {})

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	inTx := explainPlanOn(t, ctx, tx, q)
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	t.Logf("in-tx (populates the connection's plan cache)\n  => %s", inTx)
	if !strings.Contains(inTx, "narrowed-by:BY_EMAIL1") {
		t.Fatalf("the in-tx plan carries no proof, so nothing worth leaking was "+
			"cached and the auto-commit check below is vacuous: %s", inTx)
	}

	// SAME connection, SAME SQL, no transaction. A cache key that does not
	// separate the two read-version scopes returns the plan above verbatim.
	autoCommit := explainPlanOn(t, ctx, conn, q)
	t.Logf("auto-commit on the SAME connection\n  => %s", autoCommit)
	if strings.Contains(autoCommit, "narrowed-by:") ||
		strings.Contains(autoCommit, "distinct-by:") {
		t.Errorf("the auto-commit run was served the TRANSACTION's plan from the "+
			"connection's plan cache: %s\nSingleReadVersion decides the plan, so it "+
			"must be part of the plan-cache key — otherwise the gate is bypassed "+
			"entirely on the second statement.", autoCommit)
	}
	if !strings.Contains(autoCommit, "Distinct(") {
		t.Errorf("auto-commit plan has no dedup operator: %s", autoCommit)
	}
}

// dusrvDrainTx runs q on tx with a one-row page limit and returns every value.
//
// The page limit matters even here — in fact especially here. It forces the
// statement across many page boundaries, which is what makes the run a claim
// about paging at all: the plan whose DISTINCT was elided still has to return
// each value once, and it does because all those pages share the transaction's
// single read version.
func dusrvDrainTx(t *testing.T, ctx context.Context, tx *sql.Tx, q string) []string {
	t.Helper()
	out, err := dusrvDrainTxErr(ctx, tx, q)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return out
}

// dusrvDrainTxErr is dusrvDrainTx that RETURNS its driver error, which is what
// a retried transaction body needs. A drain crosses many page boundaries by
// design, and every one of those pages is a place the whole-transaction budget
// pre-emption can arrive; fatalling there would end the test before the retry
// loop could classify the error.
func dusrvDrainTxErr(ctx context.Context, tx *sql.Tx, q string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query %q: %w", q, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s sql.NullString
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("scan %q: %w", q, err)
		}
		out = append(out, s.String)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows.Err %q: %w", q, err)
	}
	return out, nil
}

// explainPlanOn is explainPlan against any queryer, so an EXPLAIN can be issued
// INSIDE an explicit transaction. The distinction is load-bearing rather than
// cosmetic: the plan a statement gets now depends on whether it runs on one read
// version, so an EXPLAIN issued on the pool and one issued on a transaction are
// answering different questions and may legitimately differ.
func explainPlanOn(t *testing.T, ctx context.Context, q dusrvQueryer, stmt string) string {
	t.Helper()
	plan, err := explainPlanOnErr(ctx, q, stmt)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return plan
}

// explainPlanOnErr is explainPlanOn that RETURNS its driver error, for use
// inside a retried transaction body. An EXPLAIN issued on a transaction is a
// read page like any other and can be pre-empted for outliving the MVCC window.
func explainPlanOnErr(ctx context.Context, q dusrvQueryer, stmt string) (string, error) {
	rows, err := q.QueryContext(ctx, "EXPLAIN "+stmt)
	if err != nil {
		return "", fmt.Errorf("EXPLAIN %s: %w", stmt, err)
	}
	defer func() { _ = rows.Close() }()
	var plan strings.Builder
	cols, _ := rows.Columns()
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "", fmt.Errorf("scan explain %s: %w", stmt, err)
		}
		for _, v := range vals {
			switch s := v.(type) {
			case string:
				plan.WriteString(s)
			case []byte:
				plan.Write(s)
			}
			plan.WriteString(" ")
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("explain rows.Err %s: %w", stmt, err)
	}
	return plan.String(), nil
}

// dusrvQueryer is satisfied by *sql.DB, *sql.Conn and *sql.Tx alike.
type dusrvQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}
