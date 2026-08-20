package sqldriver_test

// RFC-236 acceptance: do collected statistics actually change the plan, and do
// they change it BECAUSE OF THE DATA?
//
// The obvious test — "with statistics on, drive from the small table" — is not
// worth much on its own. The planner is deterministic, so a single arrangement
// passes or fails, and it cannot distinguish "the counts drove it" from "the
// tie-break happened to land there". RFC-235 section 18 measured that the
// tie-break is doing exactly that today, and that it is decided by a hash over
// plan structure.
//
// So this test is DIRECTIONAL. The same schema and the same SQL are planned over
// two data arrangements that are mirror images: in one, `small` has a single row
// and `big` has many; in the other the row counts are swapped. Then:
//
//	statistics OFF -> the two plans must be IDENTICAL.
//	    Same schema, same SQL; the planner cannot see row counts, so data cannot
//	    reach the decision. If these differ, something other than statistics is
//	    reading the data and this test is measuring the wrong thing.
//
//	statistics ON  -> the two plans must DIFFER, and each must drive from the
//	    table that is actually smaller in ITS arrangement.
//
// A fixed tie-break cannot produce a driver that FOLLOWS the row counts across a
// mirrored pair. That is what makes this unsatisfiable by writing a test to
// satisfy it, which was the objection to the single-arrangement version.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/core/embedded"
)

func TestFDB_CollectedStatisticsDriveJoinOrder(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	// arrangement builds a schema with `smallRows` in A and `bigRows` in B, and
	// returns the EXPLAIN of the join under the given statistics setting.
	arrangement := func(t *testing.T, name string, aRows, bRows int, useStats bool) string {
		t.Helper()
		dbPath := "/statsjoin_" + name
		setup := openTestDB(t, dbPath)
		mwjoMustExec(t, setup, ctx, "CREATE DATABASE "+dbPath)
		mwjoMustExec(t, setup, ctx,
			"CREATE SCHEMA TEMPLATE statsjoin_"+name+
				" CREATE TABLE a (id BIGINT, v BIGINT, PRIMARY KEY (id))"+
				" CREATE TABLE b (id BIGINT, a_id BIGINT, PRIMARY KEY (id))"+
				" CREATE INDEX b_by_a ON b (a_id)")
		mwjoMustExec(t, setup, ctx, "CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE statsjoin_"+name)

		dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath)
		if useStats {
			dsn += "&planner_statistics=true"
		}
		db, err := sql.Open("fdbsql", dsn)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { db.Close() })

		for i := 0; i < aRows; i++ {
			mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO a VALUES (%d, %d)", i, i))
		}
		for i := 0; i < bRows; i++ {
			mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO b VALUES (%d, %d)", i, i%maxInt(aRows, 1)))
		}

		if useStats {
			// Collect through the driver connection, the way `frl stats` will.
			conn, cErr := db.Conn(ctx)
			if cErr != nil {
				t.Fatalf("conn: %v", cErr)
			}
			defer conn.Close()
			var report *recordlayer.CollectionReport
			rawErr := conn.Raw(func(dc any) error {
				ec, ok := dc.(*embedded.EmbeddedConnection)
				if !ok {
					return fmt.Errorf("driver conn is %T, not *embedded.EmbeddedConnection", dc)
				}
				var e error
				report, e = ec.CollectStatistics(ctx, recordlayer.CollectOptions{BatchSize: 500})
				return e
			})
			if rawErr != nil {
				t.Fatalf("collect: %v", rawErr)
			}
			// The collection itself must be right, or the plan assertion below
			// is measuring a coincidence rather than a decision.
			if got := report.Collected["A"].Count; got != int64(aRows) {
				t.Fatalf("%s: collected |a|=%d, want %d", name, got, aRows)
			}
			if got := report.Collected["B"].Count; got != int64(bRows) {
				t.Fatalf("%s: collected |b|=%d, want %d", name, got, bRows)
			}
		}

		var plan string
		row := db.QueryRowContext(ctx,
			"EXPLAIN SELECT a.v, b.id FROM a, b WHERE b.a_id = a.id")
		if err := row.Scan(&plan); err != nil {
			t.Fatalf("%s: explain: %v", name, err)
		}
		return strings.ToUpper(plan)
	}

	const few, many = 1, 400

	t.Run("statistics_OFF_the_plan_ignores_the_data", func(t *testing.T) {
		t.Parallel()
		aSmall := arrangement(t, "off_asmall", few, many, false)
		aBig := arrangement(t, "off_abig", many, few, false)
		if aSmall != aBig {
			t.Fatalf("with statistics OFF the two arrangements planned DIFFERENTLY:\n"+
				"  a=1,b=400: %s\n  a=400,b=1: %s\n"+
				"  Same schema, same SQL — so something other than collected statistics is\n"+
				"  reading row counts, and the ON assertions below would be measuring it\n"+
				"  rather than this feature.", aSmall, aBig)
		}
	})

	t.Run("statistics_ON_the_driver_FOLLOWS_the_row_counts", func(t *testing.T) {
		t.Parallel()
		aSmall := arrangement(t, "on_asmall", few, many, true)
		aBig := arrangement(t, "on_abig", many, few, true)

		if aSmall == aBig {
			t.Fatalf("with statistics ON the mirrored arrangements planned IDENTICALLY:\n"+
				"  %s\n"+
				"  The row counts differ by 400x in opposite directions, so a cost model that\n"+
				"  consumed them could not produce the same plan for both. Either the gates\n"+
				"  refused (check completeness and freshness) or the counts never reached the\n"+
				"  cost model.", aSmall)
		}

		// And the direction must be RIGHT, not merely different: each plan drives
		// from whichever table is smaller in its own arrangement.
		if !strings.Contains(aSmall, "OUTER=SCAN(A)") {
			t.Errorf("a=1, b=400: expected the plan to drive from the 1-row A, got: %s", aSmall)
		}
		if strings.Contains(aBig, "OUTER=SCAN(A)") {
			t.Errorf("a=400, b=1: expected the plan NOT to drive from the 400-row A, got: %s", aBig)
		}
	})
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
