package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// WHAT ACTUALLY DECIDES THE JOIN ORDER WITHOUT STATISTICS?
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
// This is a MEASUREMENT that reports; it gates only on the population being real.
func TestFDB_JoinOrderWithoutStatistics(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	type variant struct {
		name    string
		pkRows  int // rows in the table whose PRIMARY KEY is the join target
		fkRows  int // rows in the table carrying the foreign key
		bigOnPK bool
	}
	variants := []variant{
		{"big-on-fk-side/a", 10, 2000, false},
		{"big-on-fk-side/b", 10, 2000, false},
		{"big-on-fk-side/c", 25, 3000, false},
		{"big-on-PK-side/a", 2000, 10, true},
		{"big-on-PK-side/b", 2000, 10, true},
		{"big-on-PK-side/c", 3000, 25, true},
	}

	wrong, right := 0, 0
	for i, v := range variants {
		plan, drivesPK := joinOrderArrangement(t, ctx, i, v.pkRows, v.fkRows)
		// Correct = drive from whichever side has FEWER rows.
		smallerIsPK := v.pkRows < v.fkRows
		correct := drivesPK == smallerIsPK
		if correct {
			right++
		} else {
			wrong++
		}
		t.Logf("%-18s pk-side=%-5d fk-side=%-5d drives-pk=%-5v correct=%-5v  %s",
			v.name, v.pkRows, v.fkRows, drivesPK, correct, plan)
	}

	total := right + wrong
	if total != len(variants) {
		t.Fatalf("measured %d of %d — a partial population makes the rate below a "+
			"statement about the failures, not the planner", total, len(variants))
	}
	t.Logf("")
	t.Logf("JOIN ORDER WITHOUT STATISTICS, over %d two-table joins:", total)
	t.Logf("  drove the smaller side (correct): %d", right)
	t.Logf("  drove the larger side (wrong):    %d = %.0f%%",
		wrong, 100*float64(wrong)/float64(total))
	t.Logf("")
	t.Logf("Read the drives-pk column against big-on-PK-side. If the planner drives from")
	t.Logf("the pk-side regardless of size, the choice is not a coin flip — it is a fixed")
	t.Logf("structural preference, and statistics help exactly when the pk-side is bigger.")
}

// joinOrderArrangement builds `fk = pk` over two tables with the given row
// counts and returns the EXPLAIN plus whether the plan drives from the pk-side.
func joinOrderArrangement(t *testing.T, ctx context.Context, i, pkRows, fkRows int) (string, bool) {
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

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	for r := 0; r < pkRows; r++ {
		mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO pkside VALUES (%d, %d)", r, r))
	}
	for r := 0; r < fkRows; r++ {
		mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO fkside VALUES (%d, %d)", r, r%pkRows))
	}

	var plan string
	q := "EXPLAIN SELECT pkside.v, fkside.id FROM pkside, fkside WHERE fkside.fk = pkside.id"
	if err := db.QueryRowContext(ctx, q).Scan(&plan); err != nil {
		t.Fatalf("explain: %v", err)
	}
	plan = strings.ToUpper(plan)
	return plan, strings.Contains(plan, "OUTER=SCAN(PKSIDE)")
}
