package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

// RFC-210 removes (R1/R2) or narrows (R3) the DISTINCT operator when a
// secondary UNIQUE index proves the projected column already holds one row per
// value. The proof is a statement about the store AT ONE INSTANT. An
// auto-commit paginated statement does not read one instant: fetchPage runs
// EACH page in its own DB.Run transaction, so every page takes a FRESH READ
// VERSION.
//
// The operator that was removed is what carried the cross-page memory. The hash
// DISTINCT threads its whole seen-set through gen.DistinctHashContinuation
// (distinct_stream.go) precisely "so a duplicate spanning a page boundary is not
// re-admitted". Elide it and nothing remembers page 1.
//
// The interleaving below is the one the unique index cannot forbid, because at
// no instant are there two rows carrying V:
//
//	page 1 emits V (the row at pk 1)
//	     DELETE the row at pk 1          -- V leaves the table
//	     INSERT V at pk 10000            -- V re-enters, AHEAD of the cursor
//	page k reaches pk 10000 and emits V again
//
// The scan resumes by primary key, so the re-inserted row sits in front of the
// continuation position and is scanned as if it had always been there.
//
// The control is the same table, the same interleaving, and the same page size
// against EMAIL_PLAIN — a mirror column with no index, which therefore plans the
// full hash distinct and keeps the seen-set. It is what separates "RFC-210's
// elision loses the dedup" from "the driver's paging is non-repeatable-read for
// everyone".
func TestFDB_DistinctUniqueElisionPagingRepro(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_duepr")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_duepr")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE duepr "+
			// Two structurally identical tables so the elided run and the control
			// run each get their own un-mutated starting state.
			"CREATE TABLE t1 (id BIGINT, email STRING, email_plain STRING, PRIMARY KEY (id)) "+
			"CREATE UNIQUE INDEX by_email1 ON t1 (email) "+
			"CREATE TABLE t2 (id BIGINT, email STRING, email_plain STRING, PRIMARY KEY (id)) "+
			"CREATE UNIQUE INDEX by_email2 ON t2 (email) "+
			"CREATE TABLE t3 (id BIGINT, email STRING, email_plain STRING, PRIMARY KEY (id)) "+
			"CREATE UNIQUE INDEX by_email3 ON t3 (email)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_duepr/s WITH TEMPLATE duepr")

	dsn := fmt.Sprintf("fdbsql:///testdb_duepr?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(8)

	const nRows = 40
	for _, tbl := range []string{"t1", "t2", "t3"} {
		var sb strings.Builder
		fmt.Fprintf(&sb, "INSERT INTO %s (id, email, email_plain) VALUES ", tbl)
		for i := 1; i <= nRows; i++ {
			if i > 1 {
				sb.WriteString(", ")
			}
			fmt.Fprintf(&sb, "(%d, 'e%02d@x', 'e%02d@x')", i, i, i)
		}
		mwjoMustExec(t, db, ctx, sb.String())
	}

	// ---- the plan shapes the whole test is interpreted against -----------
	// The proof has two distinct failure directions and each is exercised
	// separately, because a fix that only restores one of them is still broken:
	//
	//	R3 (narrowed)  — the operator survives but retains only exempt keys
	//	R2 (elided)    — a NULL-rejecting conjunct removes the operator entirely
	const (
		narrowedQ = "SELECT DISTINCT email FROM t1"
		controlQ  = "SELECT DISTINCT email_plain FROM t2"
		elidedQ   = "SELECT DISTINCT email FROM t3 WHERE email IS NOT NULL"
	)
	// The plan SHAPES are read inside an EXPLICIT TRANSACTION, and that is the
	// substance of the fix rather than a detail of the harness.
	//
	// The secondary-UNIQUE proof is a statement about an INSTANT, so it is only
	// licensed where the whole result comes from one read version. An explicit
	// transaction gives exactly that, so the proof still fires there and these
	// preconditions can still establish which arm is which. In AUTO-COMMIT —
	// the mode every scenario below actually runs in — the proof is withheld,
	// which is asserted directly further down.
	//
	// Reading these shapes in auto-commit instead would assert the BUG's
	// presence and could never pass alongside its own fix.
	narrowedPlan := duepExplainInTx(t, ctx, db, narrowedQ)
	controlPlan := explainPlan(t, ctx, db, controlQ)
	elidedPlan := duepExplainInTx(t, ctx, db, elidedQ)
	t.Logf("EXPLAIN R3 narrowed %q\n  => %s", narrowedQ, narrowedPlan)
	t.Logf("EXPLAIN R2 elided   %q\n  => %s", elidedQ, elidedPlan)
	t.Logf("EXPLAIN control     %q\n  => %s", controlQ, controlPlan)

	if !strings.Contains(narrowedPlan, "narrowed-by:BY_EMAIL1") {
		t.Fatalf("the R3 side draws NO narrowing proof from BY_EMAIL1: %s\n"+
			"Without the proof this arm compares two full hash distincts and "+
			"cannot say anything about the elision.", narrowedPlan)
	}
	if strings.Contains(elidedPlan, "Distinct(") ||
		!strings.Contains(elidedPlan, "distinct-by:BY_EMAIL3") {
		t.Fatalf("the R2 side did not fully elide: %s\n"+
			"This arm is a claim about the case where NO operator remains; if one "+
			"remains it is testing R3 twice.", elidedPlan)
	}
	// The two arms resume on DIFFERENT key orders, and the interleaving lands
	// ahead of the cursor for a different reason in each. Both are asserted,
	// because a plan shape that drifted would quietly stop being a
	// duplicate-producing scenario while the test still passed.
	//
	//	R3 — base-record scan, resumes by PRIMARY KEY. The continuation sits at
	//	     pk 1; the re-inserted row at pk 10000 is plainly ahead.
	//	R2 — covering scan of BY_EMAIL, resumes by INDEX KEY (email, pk). The
	//	     continuation sits at (V, 1) and the re-inserted entry is (V, 10000),
	//	     which is ahead of it and still behind (V+1, 2). This is why the page
	//	     size is one row: the boundary has to fall between V's two possible
	//	     positions, and only a page that ends immediately after V does.
	if !strings.Contains(narrowedPlan, "Scan(T1)") || strings.Contains(narrowedPlan, "IndexScan") {
		t.Fatalf("the R3 arm no longer resumes by primary key: %s", narrowedPlan)
	}
	if !strings.Contains(elidedPlan, "IndexScan(BY_EMAIL3") {
		t.Fatalf("the R2 arm no longer resumes by index key: %s", elidedPlan)
	}
	if strings.Contains(controlPlan, "distinct-by:") ||
		strings.Contains(controlPlan, "narrowed-by:") {
		t.Fatalf("the CONTROL drew a proof too: %s\nEMAIL_PLAIN must stay unindexed, "+
			"or the control is a copy of the experiment.", controlPlan)
	}
	if !strings.Contains(controlPlan, "Distinct(") {
		t.Fatalf("the CONTROL is not a physical distinct: %s", controlPlan)
	}

	// And the fix itself, stated where it applies: in AUTO-COMMIT — the mode
	// every scenario below runs in — neither arm may draw the proof. Each page
	// takes a fresh read version, so a uniqueness guarantee that holds at each
	// instant separately says nothing about the statement as a whole.
	//
	// This is the assertion that would go red if the gate were removed while
	// the explicit-transaction preconditions above still passed.
	for _, ac := range []struct{ label, query string }{
		{"R3 narrowed", narrowedQ},
		{"R2 elided", elidedQ},
	} {
		plan := explainPlan(t, ctx, db, ac.query)
		t.Logf("EXPLAIN auto-commit %-12s %q\n  => %s", ac.label, ac.query, plan)
		if strings.Contains(plan, "narrowed-by:") || strings.Contains(plan, "distinct-by:") {
			t.Fatalf("%s drew a secondary-UNIQUE proof in AUTO-COMMIT: %s\n"+
				"Each page runs its own transaction at its own read version, so a "+
				"value can be deleted from one row and re-inserted on another "+
				"between pages and be emitted twice. The proof is only licensed "+
				"under a single read version.", ac.label, plan)
		}
		if !strings.Contains(plan, "Distinct(") {
			t.Fatalf("%s has no dedup operator in AUTO-COMMIT: %s\n"+
				"Withholding the proof must restore the full operator, not leave "+
				"the query with no deduplication at all.", ac.label, plan)
		}
	}

	// ---- run all three scenarios ----------------------------------------
	narrowed := duepRun(t, ctx, db, "t1", narrowedQ)
	control := duepRun(t, ctx, db, "t2", controlQ)
	elided := duepRun(t, ctx, db, "t3", elidedQ)

	t.Logf("R3 narrowed rows=%d occurrencesOfV=%d output=%v", len(narrowed.rows), narrowed.vCount, narrowed.rows)
	t.Logf("R2 elided   rows=%d occurrencesOfV=%d output=%v", len(elided.rows), elided.vCount, elided.rows)
	t.Logf("CONTROL     rows=%d occurrencesOfV=%d output=%v", len(control.rows), control.vCount, control.rows)

	// The control is a PRECONDITION, not a finding: if the full hash distinct
	// duplicates too, the defect is in paging generally and the two arms below
	// say nothing about RFC-210.
	if control.vCount != 1 {
		t.Fatalf("the CONTROL emitted %q %d times (want exactly 1).\n"+
			"A full hash distinct carries its seen-set across pages via "+
			"gen.DistinctHashContinuation, so if IT duplicates, the defect is in "+
			"paging generally and not in RFC-210's elision.\nrows=%v",
			duepV, control.vCount, control.rows)
	}
	for _, arm := range []struct {
		tag, plan string
		res       duepResult
	}{
		{"R3 (narrowed)", narrowedPlan, narrowed},
		{"R2 (elided)", elidedPlan, elided},
	} {
		if arm.res.vCount != 1 {
			t.Errorf("%s emitted %q %d times across pages while the control emitted "+
				"it exactly once.\nplan: %s\ncontrol plan: %s\nrows: %v",
				arm.tag, duepV, arm.res.vCount, arm.plan, controlPlan, arm.res.rows)
		}
	}
}

// duepV is the value moved across the cursor. It is the email of the row at
// primary key 1, so it is emitted on page 1 of a primary-key-ordered scan.
const duepV = "e01@x"

// duepReinsertPK sits far past the initial key range, so the re-inserted row is
// AHEAD of the continuation position when it appears.
const duepReinsertPK = 10000

// duepPageScanLimit makes every page exactly one row, so the page boundary
// falls immediately after V is emitted. That is required by the index-ordered
// R2 arm (see the plan-shape comment above) and harmless to the others.
const duepPageScanLimit = 1

type duepResult struct {
	rows   []string
	vCount int
}

// duepRun executes q on a page-limited connection with NO explicit transaction,
// consumes exactly one row, performs the delete/re-insert on a SEPARATE
// connection, and then drains the rest.
//
// One row is enough to guarantee the mutation lands between page fetches:
// page 1 has already been fetched and committed by the time Next() returns, and
// page 2 is not fetched until the buffer runs dry.
func duepRun(t *testing.T, ctx context.Context, db *sql.DB, table, q string) duepResult {
	t.Helper()
	conn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, int64(duepPageScanLimit)).Build())
	})
	rows, err := conn.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	var s sql.NullString
	mutated := false
	for rows.Next() {
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if !s.Valid {
			t.Fatalf("unexpected NULL from %q — this fixture has none", q)
		}
		out = append(out, s.String)
		if !mutated {
			if s.String != duepV {
				t.Fatalf("the first row of %q was %q, want %q — the scan is not "+
					"primary-key ordered from pk 1 and the interleaving below would "+
					"move a value the cursor has not yet emitted.", q, s.String, duepV)
			}
			// The mutation. Two separate auto-commit statements on the pool, so
			// they are committed and visible before the next page's read version.
			// The UNIQUE index is never violated: V is absent between them.
			mwjoMustExec(t, db, ctx,
				fmt.Sprintf("DELETE FROM %s WHERE id = 1", table))
			mwjoMustExec(t, db, ctx,
				fmt.Sprintf("INSERT INTO %s (id, email, email_plain) VALUES (%d, '%s', '%s')",
					table, duepReinsertPK, duepV, duepV))
			mutated = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err %q: %v", q, err)
	}
	if !mutated {
		t.Fatalf("%q returned no rows at all", q)
	}
	res := duepResult{rows: out}
	for _, v := range out {
		if v == duepV {
			res.vCount++
		}
	}
	return res
}

// duepExplainInTx runs EXPLAIN inside an EXPLICIT transaction, where all pages
// share one read version and the secondary-UNIQUE proof is therefore licensed.
func duepExplainInTx(t *testing.T, ctx context.Context, db *sql.DB, query string) string {
	t.Helper()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	var plan string
	if err := tx.QueryRowContext(ctx, "EXPLAIN "+query).Scan(&plan); err != nil {
		t.Fatalf("EXPLAIN %q in a transaction: %v", query, err)
	}
	return plan
}
