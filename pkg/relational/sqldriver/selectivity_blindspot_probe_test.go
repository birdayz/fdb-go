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

// WHAT COLLECTED TABLE COUNTS STILL CANNOT SEE.
//
// An index probe's estimated row count is
//
//	RecordTypeCardinality(table) * EqualityBoundSelectivity^equalities
//	                             * RangeSelectivity^ranges
//
// (properties.BoundSelectivity, applied in RecordQueryIndexPlan.HintCost).
// Collected statistics make the FIRST factor a measurement. The second stays a
// constant: every equality bound is assumed to keep 0.1 of the rows, whether
// the column holds two distinct values or two million.
//
// So this probe holds the table count FIXED — one table, so both candidate
// access paths read the same collected number — and varies only the thing the
// cost model has no statistic for: how many distinct values each indexed column
// actually has. `hi` is unique (one row per probe); `lo` has two values (half
// the table per probe). A cost model that could see distinctness would prefer
// hi's index by three orders of magnitude. One that multiplies by 0.1 either
// way cannot tell them apart.
//
// The boundary this pins is recorded in RFC-236 §7.1, which also records why a
// distinctness statistic is cheap here when it comes: every site applying
// BoundSelectivity takes SCAN COMPARISONS, so the column is always part of a key,
// and a key is stored sorted.
//
// This is a MEASUREMENT that reports the plan and prices it in index entries.
// It deliberately does NOT assert which path is right: what it pins is the SIZE
// OF THE GAP that no table-cardinality statistic can close, so that the day a
// distinctness statistic is added there is a number to compare against.
func TestFDB_SelectivityBlindSpotWithCollectedStatistics(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	const rows = 2000
	const loDistinct = 2

	dbPath := "/selblind"
	setup := openTestDB(t, dbPath)
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE "+dbPath)
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE selblind"+
			" CREATE TABLE t (id BIGINT, hi BIGINT, lo BIGINT, PRIMARY KEY (id))"+
			" CREATE INDEX t_by_hi ON t (hi)"+
			" CREATE INDEX t_by_lo ON t (lo)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE selblind")

	base := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath)
	db := openDSN(t, base+"&planner_statistics=true")
	for i := 0; i < rows; i++ {
		mwjoMustExec(t, db, ctx,
			fmt.Sprintf("INSERT INTO t VALUES (%d, %d, %d)", i, i, i%loDistinct))
	}

	// Collect, and verify the count — otherwise the plan below is a decision
	// made on statistics whose correctness was never established.
	conn, cErr := db.Conn(ctx)
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
	if got := report.Collected["T"].Count; got != int64(rows) {
		t.Fatalf("collected |t|=%d, want %d", got, rows)
	}

	// The two access paths, priced by reality rather than by the cost model.
	hiRows := probeScalarCount(t, ctx, db, "SELECT COUNT(*) FROM t WHERE hi = 7")
	loRows := probeScalarCount(t, ctx, db, "SELECT COUNT(*) FROM t WHERE lo = 1")
	if hiRows == 0 || loRows == 0 {
		t.Fatalf("degenerate population: hi-probe matched %d rows, lo-probe matched %d — "+
			"with either at zero the comparison below is not measuring a choice",
			hiRows, loRows)
	}

	var plan string
	q := "EXPLAIN SELECT id FROM t WHERE hi = 7 AND lo = 1"
	if err := db.QueryRowContext(ctx, q).Scan(&plan); err != nil {
		t.Fatalf("explain: %v", err)
	}
	up := strings.ToUpper(plan)
	usedHi := strings.Contains(up, "T_BY_HI")
	usedLo := strings.Contains(up, "T_BY_LO")

	// Reading BOTH indexes means the planner intersected them. That is the
	// concrete price of the missing statistic: an intersection must drain the
	// lo leg end-to-end to intersect it against a single hi entry, so it reads
	// hiRows+loRows index entries to reach a result the hi leg alone reaches in
	// hiRows. It looks worthwhile to the cost model precisely because both legs
	// are estimated at the same constant-derived number.
	entries, path := hiRows, "t_by_hi alone"
	switch {
	case usedHi && usedLo:
		entries, path = hiRows+loRows, "INTERSECTION of both indexes"
	case usedLo:
		entries, path = loRows, "t_by_lo alone"
	case !usedHi:
		entries, path = rows, "neither index (table scan)"
	}

	estimate := float64(rows) * 0.1 // EqualityBoundSelectivity, one equality per leg
	t.Logf("")
	t.Logf("SELECTIVITY BLIND SPOT, |t| = %d (collected, verified):", rows)
	t.Logf("  hi = 7   matches %4d row(s)   — %d distinct values in the column", hiRows, rows)
	t.Logf("  lo = 1   matches %4d row(s)   — %d distinct values in the column", loRows, loDistinct)
	t.Logf("  the two access paths really differ by %.0fx", float64(loRows)/float64(hiRows))
	t.Logf("")
	t.Logf("  cost model estimates BOTH legs at %d * EqualityBoundSelectivity = %.0f rows", rows, estimate)
	t.Logf("  chosen path: %s", path)
	t.Logf("  index entries it reads: %d   (cheapest available: %d — %.0fx more)",
		entries, hiRows, float64(entries)/float64(hiRows))
	t.Logf("  %s", plan)
	t.Logf("")
	t.Logf("Collected table counts fix the FIRST factor of that product. The second is a")
	t.Logf("constant, so the two legs above are indistinguishable to the cost model no")
	t.Logf("matter how good the table count is. Closing this needs a DISTINCTNESS")
	t.Logf("statistic (NDV), not a better row count.")

	// The gap only exists while the two paths really do differ. If a schema or
	// data change ever made them comparable, this probe would keep printing a
	// confident ratio while measuring nothing.
	if loRows <= hiRows {
		t.Fatalf("lo-probe matched %d rows and hi-probe %d — the arrangement no longer "+
			"builds a selectivity gap, so this probe measures nothing", loRows, hiRows)
	}
}

func probeScalarCount(t *testing.T, ctx context.Context, db *sql.DB, q string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", q, err)
	}
	return n
}
