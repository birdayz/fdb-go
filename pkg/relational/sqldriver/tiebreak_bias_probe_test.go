package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/core/embedded"
)

// WHAT ACTUALLY DECIDES THE JOIN ORDER, AND DOES A STATISTIC CORRECT IT?
//
// The measured win from collected statistics (2m34s -> 22ms at 1M rows) is the
// value of ONE corrected decision. It says nothing about how often the decision
// needs correcting, and the mirrored-pair test that produced it forces exactly
// one wrong arm out of two BY CONSTRUCTION.
//
// Two earlier versions of this probe answered the wrong question. Both varied
// the table IDENTIFIERS while holding the STRUCTURE fixed — the small table was
// the primary-key side of the join in every variant — and both reported 12/12
// correct, which read as "statistics are not needed" when it actually meant
// "this probe built the lucky arrangement twelve times".
//
// So this varies the thing that appears to decide: WHICH SIDE OF THE JOIN THE
// BIG TABLE IS ON. The join is `fk = pk`. Driving from the pk-side means
// scanning it and probing the other side's index; driving from the fk-side means
// scanning it and probing a primary key. Both are executable, so nothing but
// cardinality should prefer one — and cardinality is exactly what the planner
// cannot see without statistics.
//
// Each arrangement is then planned TWICE over the SAME rows: once with
// statistics off, once with them collected and on. The pairing is what makes
// the second column a statement about the STATISTIC rather than about the
// schema — an unpaired "with statistics it was right 6/6" is equally consistent
// with "this arrangement was always right".
//
// This is a MEASUREMENT that reports; it gates on the population being real and
// on statistics never making a decision WORSE.
func TestFDB_JoinOrderStatisticsCorrectionRate(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	type variant struct {
		name   string
		pkRows int // rows in the table whose PRIMARY KEY is the join target
		fkRows int // rows in the table carrying the foreign key
	}
	variants := []variant{
		{"big-on-fk-side/a", 10, 2000},
		{"big-on-fk-side/b", 10, 2000},
		{"big-on-fk-side/c", 25, 3000},
		{"big-on-PK-side/a", 2000, 10},
		{"big-on-PK-side/b", 2000, 10},
		{"big-on-PK-side/c", 3000, 25},
	}

	var rightOff, rightOn, measured, repaired, broken int
	for i, v := range variants {
		off, on := joinOrderArrangement(t, ctx, i, v.pkRows, v.fkRows)
		// Correct = drive from whichever side has FEWER rows.
		smallerIsPK := v.pkRows < v.fkRows
		okOff := off.drivesPK == smallerIsPK
		okOn := on.drivesPK == smallerIsPK
		measured++
		if okOff {
			rightOff++
		}
		if okOn {
			rightOn++
		}
		switch {
		case !okOff && okOn:
			repaired++
		case okOff && !okOn:
			broken++
		}
		t.Logf("%-18s pk-side=%-5d fk-side=%-5d | stats OFF: drives-pk=%-5v correct=%-5v | stats ON: drives-pk=%-5v correct=%-5v",
			v.name, v.pkRows, v.fkRows, off.drivesPK, okOff, on.drivesPK, okOn)
	}

	if measured != len(variants) {
		t.Fatalf("measured %d of %d — a partial population makes the rate below a "+
			"statement about the failures, not the planner", measured, len(variants))
	}
	t.Logf("")
	t.Logf("JOIN ORDER over %d two-table joins, each planned twice on the SAME rows:", measured)
	t.Logf("  statistics OFF, drove the smaller side: %d/%d", rightOff, measured)
	t.Logf("  statistics ON,  drove the smaller side: %d/%d", rightOn, measured)
	t.Logf("  decisions statistics REPAIRED: %d", repaired)
	t.Logf("  decisions statistics BROKE:    %d", broken)
	t.Logf("")
	t.Logf("Read the OFF drives-pk column against big-on-PK-side. If the planner drives from")
	t.Logf("the pk-side regardless of size, the choice is not a coin flip — it is a fixed")
	t.Logf("structural preference, and statistics help exactly when the pk-side is bigger.")

	// The measurement reports a rate; it does not assert one. But a statistic
	// that makes a join order WORSE than no statistic at all refutes the whole
	// premise of the feature, so that direction is a hard failure.
	if broken > 0 {
		t.Fatalf("collected statistics turned %d correct join order(s) into wrong ones — "+
			"a statistic that degrades a decision is worse than no statistic", broken)
	}
}

// arrangementPlan is one EXPLAIN of the join under one statistics setting.
type arrangementPlan struct {
	plan     string
	drivesPK bool
}

// joinOrderArrangement builds `fk = pk` over two tables with the given row
// counts and plans the join TWICE over the same rows: statistics off, then
// statistics collected and on.
func joinOrderArrangement(t *testing.T, ctx context.Context, i, pkRows, fkRows int) (off, on arrangementPlan) {
	t.Helper()
	dbPath := fmt.Sprintf("/joinorder_%d", i)
	setup := openTestDB(t, dbPath)
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE "+dbPath)
	tmpl := fmt.Sprintf("joinorder_%d", i)
	// SYMMETRIC: the fk side is reachable through its index, the pk side through
	// its primary key. Both directions execute, so only row counts distinguish
	// them — and row counts are what the planner cannot see without statistics.
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE "+tmpl+
			" CREATE TABLE pkside (id BIGINT, v BIGINT, PRIMARY KEY (id))"+
			" CREATE TABLE fkside (id BIGINT, fk BIGINT, PRIMARY KEY (id))"+
			" CREATE INDEX fkside_by_fk ON fkside (fk)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE "+tmpl)

	base := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath)
	plain := openDSN(t, base)
	for r := 0; r < pkRows; r++ {
		mwjoMustExec(t, plain, ctx, fmt.Sprintf("INSERT INTO pkside VALUES (%d, %d)", r, r))
	}
	for r := 0; r < fkRows; r++ {
		mwjoMustExec(t, plain, ctx, fmt.Sprintf("INSERT INTO fkside VALUES (%d, %d)", r, r%pkRows))
	}

	const q = "EXPLAIN SELECT pkside.v, fkside.id FROM pkside, fkside WHERE fkside.fk = pkside.id"
	off = explainJoin(t, ctx, plain, q)

	// Collect through the driver connection, the way `frl stats` does, and
	// verify the counts BEFORE reading the plan — otherwise the second column
	// could be reporting a decision made on statistics that are simply wrong.
	statsDB := openDSN(t, base+"&planner_statistics=true")
	conn, cErr := statsDB.Conn(ctx)
	if cErr != nil {
		t.Fatalf("conn: %v", cErr)
	}
	defer conn.Close()
	var report *recordlayer.CollectionReport
	if err := conn.Raw(func(dc any) error {
		ec, ok := dc.(*embedded.EmbeddedConnection)
		if !ok {
			return fmt.Errorf("driver conn is %T, not *embedded.EmbeddedConnection", dc)
		}
		var e error
		report, e = ec.CollectStatistics(ctx, recordlayer.CollectOptions{BatchSize: 500})
		return e
	}); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if got := report.Collected["PKSIDE"].Count; got != int64(pkRows) {
		t.Fatalf("collected |pkside|=%d, want %d", got, pkRows)
	}
	if got := report.Collected["FKSIDE"].Count; got != int64(fkRows) {
		t.Fatalf("collected |fkside|=%d, want %d", got, fkRows)
	}
	on = explainJoin(t, ctx, statsDB, q)
	return off, on
}

func openDSN(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("open %s: %v", dsn, err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func explainJoin(t *testing.T, ctx context.Context, db *sql.DB, q string) arrangementPlan {
	t.Helper()
	var plan string
	if err := db.QueryRowContext(ctx, q).Scan(&plan); err != nil {
		t.Fatalf("explain: %v", err)
	}
	plan = strings.ToUpper(plan)
	return arrangementPlan{plan: plan, drivesPK: strings.Contains(plan, "OUTER=SCAN(PKSIDE)")}
}
