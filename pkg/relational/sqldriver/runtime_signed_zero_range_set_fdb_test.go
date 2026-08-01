package sqldriver_test

// End-to-end coverage for RFC-205's runtime physical range sets. These cases
// deliberately constrain a suffix after one or more float equalities: a
// terminal signed-zero widening or a residual-only/base-scan answer would not
// exercise the changed contract.

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

func TestFDB_RuntimeSignedZeroRangeSetAccessPaths(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_rszr")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_rszr")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE rszr "+
		"CREATE TABLE d (id BIGINT NOT NULL, v DOUBLE, w BIGINT, payload STRING, PRIMARY KEY (id)) "+
		"CREATE INDEX d_vw ON d (v, w) "+
		"CREATE TABLE f (id BIGINT NOT NULL, v FLOAT, w BIGINT, payload STRING, PRIMARY KEY (id)) "+
		"CREATE INDEX f_vw ON f (v, w) "+
		"CREATE TABLE m (id BIGINT NOT NULL, v1 DOUBLE, v2 FLOAT, w BIGINT, payload STRING, PRIMARY KEY (id)) "+
		"CREATE INDEX m_v1v2w ON m (v1, v2, w) "+
		"CREATE TABLE pfx (id BIGINT NOT NULL, g BIGINT, v DOUBLE, w BIGINT, PRIMARY KEY (id)) "+
		"CREATE INDEX pfx_gvw ON pfx (g, v, w) "+
		"CREATE TABLE pkd (v DOUBLE NOT NULL, w BIGINT NOT NULL, id BIGINT NOT NULL, payload STRING, PRIMARY KEY (v, w, id)) "+
		"CREATE TABLE u (id BIGINT NOT NULL, v DOUBLE, w BIGINT, PRIMARY KEY (id)) "+
		"CREATE UNIQUE INDEX u_vw ON u (v, w) "+
		"CREATE TABLE o (id BIGINT NOT NULL, kd DOUBLE, kf FLOAT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_rszr/s WITH TEMPLATE rszr")
	dsn := fmt.Sprintf("fdbsql:///testdb_rszr?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// DOUBLE and FLOAT corpora each contain both exact target signs, the two
	// broad-interval flank rows, an ordinary nonzero, and two NULL suffixes.
	mwjoMustExec(t, db, ctx, "INSERT INTO d (id,v,w,payload) VALUES "+
		"(1,-0.0,5,'d-neg'),(2,0.0,5,'d-pos'),(3,-0.0,9,'d-high'),"+
		"(4,0.0,1,'d-low'),(5,7.0,5,'d-seven'),"+
		"(6,-0.0,NULL,'d-neg-null'),(7,0.0,NULL,'d-pos-null')")
	mwjoMustExec(t, db, ctx, "INSERT INTO f (id,v,w,payload) VALUES "+
		"(101,-0.0,5,'f-neg'),(102,0.0,5,'f-pos'),(103,-0.0,9,'f-high'),"+
		"(104,0.0,1,'f-low'),(105,7.0,5,'f-seven')")

	// One row for every physical sign choice of (DOUBLE,FLOAT), plus flanks.
	// IDs follow tuple order: (--), (-+), (+-), (++).
	mwjoMustExec(t, db, ctx, "INSERT INTO m (id,v1,v2,w,payload) VALUES "+
		"(201,-0.0,-0.0,5,'mm'),(202,-0.0,0.0,5,'mp'),"+
		"(203,0.0,-0.0,5,'pm'),(204,0.0,0.0,5,'pp'),"+
		"(205,-0.0,-0.0,9,'high'),(206,0.0,0.0,1,'low')")
	mwjoMustExec(t, db, ctx, "INSERT INTO pfx (id,g,v,w) VALUES "+
		"(211,1,-0.0,5),(212,1,0.0,5),(213,1,-0.0,9),(214,1,0.0,1),"+
		"(215,2,-0.0,5),(216,2,0.0,5)")

	mwjoMustExec(t, db, ctx, "INSERT INTO pkd (v,w,id,payload) VALUES "+
		"(-0.0,5,301,'pk-neg'),(0.0,5,302,'pk-pos'),"+
		"(-0.0,9,303,'pk-high'),(0.0,1,304,'pk-low')")
	mwjoMustExec(t, db, ctx, "INSERT INTO u (id,v,w) VALUES (401,-0.0,5),(402,0.0,5),(403,7.0,5)")
	mwjoMustExec(t, db, ctx, "INSERT INTO o (id,kd,kf) VALUES "+
		"(10,0.0,-0.0),(11,-0.0,0.0),(20,7.0,7.0),(30,NULL,NULL)")

	idsDB := func(t *testing.T, q string, args ...any) []int64 {
		t.Helper()
		rows, queryErr := db.QueryContext(ctx, q, args...)
		if queryErr != nil {
			t.Fatalf("query %q: %v", q, queryErr)
		}
		defer func() { _ = rows.Close() }()
		var out []int64
		for rows.Next() {
			var id int64
			if scanErr := rows.Scan(&id); scanErr != nil {
				t.Fatalf("scan: %v", scanErr)
			}
			out = append(out, id)
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			t.Fatalf("rows: %v", rowsErr)
		}
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		return out
	}
	assertIDs := func(t *testing.T, q string, want []int64, args ...any) {
		t.Helper()
		if got := idsDB(t, q, args...); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("%s args=%v = %v, want %v", q, args, got, want)
		}
	}

	for _, tc := range []struct {
		name string
		q    string
		args []any
		want []int64
	}{
		{"double_param_positive", "SELECT id FROM d WHERE v = ? AND w = 5", []any{0.0}, []int64{1, 2}},
		{"double_param_negative", "SELECT id FROM d WHERE v = ? AND w = 5", []any{math.Copysign(0, -1)}, []int64{1, 2}},
		{"float_param_positive", "SELECT id FROM f WHERE v = ? AND w = 5", []any{0.0}, []int64{101, 102}},
		{"float_param_negative", "SELECT id FROM f WHERE v = ? AND w = 5", []any{math.Copysign(0, -1)}, []int64{101, 102}},
		{"float_coalesce_param", "SELECT id FROM f WHERE v = COALESCE(?, 7.0) AND w = 5", []any{0.0}, []int64{101, 102}},
		{"double_suffix_inequality", "SELECT id FROM d WHERE v = ? AND w >= 5", []any{0.0}, []int64{1, 2, 3}},
		{"double_suffix_is_null", "SELECT id FROM d WHERE v = ? AND w IS NULL", []any{math.Copysign(0, -1)}, []int64{6, 7}},
		{"four_sign_branches_positive", "SELECT id FROM m WHERE v1 = ? AND v2 = ? AND w = 5", []any{0.0, 0.0}, []int64{201, 202, 203, 204}},
		{"four_sign_branches_negative", "SELECT id FROM m WHERE v1 = ? AND v2 = ? AND w = 5", []any{math.Copysign(0, -1), math.Copysign(0, -1)}, []int64{201, 202, 203, 204}},
		{"fixed_prefix_middle_zero", "SELECT id FROM pfx WHERE g = 1 AND v = ? AND w = 5", []any{0.0}, []int64{211, 212}},
		{"composite_float_primary_key", "SELECT id FROM pkd WHERE v = ? AND w = 5", []any{0.0}, []int64{301, 302}},
		{"in_forward", "SELECT id FROM d WHERE v IN (-0.0,0.0) AND w = 5", nil, []int64{1, 2}},
		{"in_reverse", "SELECT id FROM d WHERE v IN (0.0,-0.0) AND w = 5", nil, []int64{1, 2}},
		{"in_repeated", "SELECT id FROM d WHERE v IN (-0.0,0.0,-0.0) AND w = 5", nil, []int64{1, 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertIDs(t, tc.q, tc.want, tc.args...)
		})
	}

	// The physical column type, not the driver's rendering of an untyped zero,
	// chooses FLOAT vs DOUBLE tuple packing. Both suffix constraints stay SARGed.
	for _, tc := range []struct {
		q     string
		index string
	}{
		{"SELECT id FROM d WHERE v = 0 AND w = 5", "D_VW"},
		{"SELECT id FROM f WHERE v = 0 AND w = 5", "F_VW"},
		{"SELECT id FROM m WHERE v1 = 0 AND v2 = 0 AND w = 5", "M_V1V2W"},
		{"SELECT id FROM pfx WHERE g = 1 AND v = 0 AND w = 5", "PFX_GVW"},
	} {
		plan := planExplainVia(t, ctx, db, tc.q)
		if !strings.Contains(plan, "IndexScan("+tc.index) || !strings.Contains(plan, "[=, =") {
			t.Fatalf("%s plan = %s\nwant %s with the equality suffix retained", tc.q, plan, tc.index)
		}
	}

	// Covering and fetch are each applied once above the combined physical
	// ranges. The two shapes must return the same two logical matches.
	coveringQ := "SELECT id FROM d WHERE v = 0 AND w = 5"
	coveringPlan := planExplainVia(t, ctx, db, coveringQ)
	if !strings.Contains(coveringPlan, "COVERING") || strings.Contains(coveringPlan, "Fetch(") {
		t.Fatalf("covering plan = %s, want one COVERING D_VW scan and no Fetch", coveringPlan)
	}
	nonCoveringQ := "SELECT payload FROM d WHERE v = 0 AND w = 5"
	nonCoveringPlan := planExplainVia(t, ctx, db, nonCoveringQ)
	if !strings.Contains(nonCoveringPlan, "Fetch(IndexScan(D_VW") {
		t.Fatalf("non-covering plan = %s, want Fetch(IndexScan(D_VW...))", nonCoveringPlan)
	}
	var payloads []string
	rows, err := db.QueryContext(ctx, nonCoveringQ)
	if err != nil {
		t.Fatalf("non-covering query: %v", err)
	}
	for rows.Next() {
		var payload string
		if scanErr := rows.Scan(&payload); scanErr != nil {
			t.Fatalf("non-covering scan: %v", scanErr)
		}
		payloads = append(payloads, payload)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		t.Fatalf("non-covering rows: %v", rowsErr)
	}
	_ = rows.Close()
	sort.Strings(payloads)
	if fmt.Sprint(payloads) != fmt.Sprint([]string{"d-neg", "d-pos"}) {
		t.Fatalf("non-covering payloads = %v, want [d-neg d-pos]", payloads)
	}

	// A UNIQUE composite index may contain both physical signs. Logical equality
	// returns both, and a scalar subquery must therefore reject the two-row result
	// rather than relying on a one-row point-probe proof.
	assertIDs(t, "SELECT id FROM u WHERE v = 0 AND w = 5", []int64{401, 402})
	var scalarID int64
	scalarErr := db.QueryRowContext(ctx,
		"SELECT (SELECT id FROM u WHERE v = 0 AND w = 5) FROM o WHERE id = 10").Scan(&scalarID)
	if scalarErr == nil || !strings.Contains(scalarErr.Error(), "21000") {
		t.Fatalf("two-sign UNIQUE scalar subquery error = %v, want SQLSTATE 21000", scalarErr)
	}

	// Force a transaction/page stop after every scanned row. The resumed stream
	// must cross all four sign branches with no gap or duplicate, and SQL
	// LIMIT/OFFSET must be global rather than resetting for each branch.
	pagedConn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, 1).
			Build())
	})
	const windowQ = "SELECT id FROM m WHERE v1 = 0 AND v2 = 0 AND w = 5 " +
		"ORDER BY v1, v2, w, id LIMIT 2 OFFSET 1"
	windowRows, err := pagedConn.QueryContext(ctx, windowQ)
	if err != nil {
		t.Fatalf("paged range-set window: %v", err)
	}
	var window []int64
	for windowRows.Next() {
		var id int64
		if scanErr := windowRows.Scan(&id); scanErr != nil {
			t.Fatalf("paged range-set scan: %v", scanErr)
		}
		window = append(window, id)
	}
	if rowsErr := windowRows.Err(); rowsErr != nil {
		t.Fatalf("paged range-set rows: %v", rowsErr)
	}
	_ = windowRows.Close()
	if fmt.Sprint(window) != fmt.Sprint([]int64{202, 203}) {
		t.Fatalf("paged four-branch LIMIT/OFFSET = %v, want [202 203]", window)
	}
}

func TestFDB_RuntimeSignedZeroCorrelatedFloatAndDouble(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := setupPlanShapeDB(t, "rsz_corr_widths",
		"CREATE TABLE d (id BIGINT NOT NULL, v DOUBLE, w BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX d_vw ON d (v, w) "+
			"CREATE TABLE f (id BIGINT NOT NULL, v FLOAT, w BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX f_vw ON f (v, w) "+
			"CREATE TABLE o (id BIGINT NOT NULL, kd DOUBLE, kf FLOAT, PRIMARY KEY (id))")
	mwjoMustExec(t, db, ctx, "INSERT INTO d VALUES (1,-0.0,5),(2,0.0,5),(3,-0.0,9),(4,0.0,1)")
	mwjoMustExec(t, db, ctx, "INSERT INTO f VALUES (101,-0.0,5),(102,0.0,5),(103,-0.0,9),(104,0.0,1)")
	mwjoMustExec(t, db, ctx, "INSERT INTO o VALUES (10,0.0,-0.0),(11,-0.0,0.0),(30,NULL,NULL)")

	for _, tc := range []struct {
		name  string
		q     string
		index string
		want  []int64
	}{
		{"double_outer_positive", "SELECT d.id FROM d,o WHERE d.v=o.kd AND d.w=5 AND o.id=10", "D_VW", []int64{1, 2}},
		{"double_outer_negative", "SELECT d.id FROM d,o WHERE d.v=o.kd AND d.w=5 AND o.id=11", "D_VW", []int64{1, 2}},
		{"float_outer_negative", "SELECT f.id FROM f,o WHERE f.v=o.kf AND f.w=5 AND o.id=10", "F_VW", []int64{101, 102}},
		{"float_outer_positive", "SELECT f.id FROM f,o WHERE f.v=o.kf AND f.w=5 AND o.id=11", "F_VW", []int64{101, 102}},
		{"double_outer_null", "SELECT d.id FROM d,o WHERE d.v=o.kd AND d.w=5 AND o.id=30", "D_VW", nil},
		{"float_outer_null", "SELECT f.id FROM f,o WHERE f.v=o.kf AND f.w=5 AND o.id=30", "F_VW", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := planExplainVia(t, ctx, db, tc.q)
			if !strings.Contains(plan, tc.index) || !strings.Contains(plan, "[=, =]") {
				t.Fatalf("plan = %s\nwant correlated %s composite probe with its suffix", plan, tc.index)
			}
			rows, queryErr := db.QueryContext(ctx, tc.q)
			if queryErr != nil {
				t.Fatalf("query: %v", queryErr)
			}
			var got []int64
			for rows.Next() {
				var id int64
				if scanErr := rows.Scan(&id); scanErr != nil {
					t.Fatalf("scan: %v", scanErr)
				}
				got = append(got, id)
			}
			if rowsErr := rows.Err(); rowsErr != nil {
				t.Fatalf("rows: %v", rowsErr)
			}
			_ = rows.Close()
			sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("%s = %v, want %v", tc.q, got, tc.want)
			}
		})
	}
}
