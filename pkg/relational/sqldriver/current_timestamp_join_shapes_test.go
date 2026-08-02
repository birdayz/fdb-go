package sqldriver_test

// Statement stability of the CURRENT_TIMESTAMP family across JOIN shapes —
// the end-to-end surface over the ordinal join's BAKED result-value build
// (evaluateOrdinalJoinRow / evaluateOrdinalJoinBareRow), which evaluates rows
// through its own RowEvalContext rather than the frontier's clock-aware wrap.
// That build carries the statement clock (ordinalJoinBuild.Clock, threaded
// from both cursor constructors and pinned at the unit level in
// executor's ordinal_join_clock_test.go); this battery holds the SQL-visible
// contract: however the planner routes a clocked value through a join —
// projection, derived table, scalar subquery, EXISTS, predicate — every
// timestamp observed within one statement is the same instant.
//
// This is also the negative-result pin for the baked path's reach: today no
// shape here routes a CURRENT_* value into the baked build's own evaluation
// (the frontier wrap or the scalar-subquery pre-binding supplies it first).
// If planner folding ever changes that — a clocked value folded into a join's
// baked result value — this battery is the tripwire that re-arms: a clockless
// build would drift across rows and fail the single-distinct assertion below.

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

func TestFDB_CurrentTimestamp_JoinShapes_StatementStable(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/testdb_cts_joins", "cts_joins",
		"CREATE TABLE A (id BIGINT, v BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE B (id BIGINT, aid BIGINT, PRIMARY KEY (id))")
	ctx := context.Background()
	for i := 1; i <= 20; i++ {
		if _, err := db.ExecContext(ctx, "INSERT INTO A VALUES (?, ?)", i, i*10); err != nil {
			t.Fatalf("seed A: %v", err)
		}
		if _, err := db.ExecContext(ctx, "INSERT INTO B VALUES (?, ?)", i, (i%10)+1); err != nil {
			t.Fatalf("seed B: %v", err)
		}
	}

	// tsCols marks which result columns carry a CURRENT_TIMESTAMP-derived
	// value (asserted single-distinct per statement). Shapes without one
	// exercise the clocked PREDICATE routes and assert execution + rows.
	shapes := []struct {
		name   string
		query  string
		tsCols []int
		cols   int
		// expectErrCode marks a shape today's planner REJECTS loudly. It is
		// asserted (not skipped) so the day the shape becomes plannable this
		// battery goes red and demands the stability assertion instead.
		expectErrCode string
	}{
		{"inner join projection", "SELECT a.id, CURRENT_TIMESTAMP FROM A a JOIN B b ON a.id = b.aid", []int{1}, 2, ""},
		{"comma join bare projection", "SELECT CURRENT_TIMESTAMP FROM A a, B b WHERE a.id = b.aid", []int{0}, 1, ""},
		{"exists outer projection", "SELECT a.id, CURRENT_TIMESTAMP FROM A a WHERE EXISTS (SELECT 1 FROM B b WHERE b.aid = a.id)", []int{1}, 2, ""},
		{"derived-table ts through join", "SELECT x.ts FROM (SELECT id, CURRENT_TIMESTAMP AS ts FROM A) x JOIN B b ON x.id = b.aid", []int{0}, 1, "0AF00"},
		{"clocked join predicate", "SELECT a.id FROM A a JOIN B b ON a.id = b.aid WHERE CURRENT_TIMESTAMP = CURRENT_TIMESTAMP", nil, 1, ""},
		{"left join projection", "SELECT a.id, b.id, CURRENT_TIMESTAMP FROM A a LEFT JOIN B b ON a.id = b.aid", []int{2}, 3, ""},
		{"correlated scalar subquery ts", "SELECT (SELECT CURRENT_TIMESTAMP FROM B b WHERE b.id = a.id) FROM A a", []int{0}, 1, ""},
		{"clocked exists predicate", "SELECT a.id FROM A a WHERE EXISTS (SELECT 1 FROM B b WHERE b.aid = a.id AND CURRENT_TIMESTAMP = CURRENT_TIMESTAMP)", nil, 1, ""},
	}

	for _, s := range shapes {
		t.Run(s.name, func(t *testing.T) {
			t.Parallel()
			// Loop across a wall-clock second boundary so per-row drift
			// (two instants inside one statement) cannot hide between
			// second ticks.
			deadline := time.Now().Add(2500 * time.Millisecond)
			execs := 0
			for execs == 0 || time.Now().Before(deadline) {
				rows, err := db.QueryContext(ctx, s.query)
				if s.expectErrCode != "" {
					if err == nil {
						rows.Close()
						t.Fatalf("%q now PLANS (was rejected with %s) — the shape gained reach; replace this asserted rejection with the timestamp-stability assertion the other shapes carry", s.query, s.expectErrCode)
					}
					if !strings.Contains(err.Error(), s.expectErrCode) {
						t.Fatalf("%q rejected with %v, want code %s — the rejection changed shape; re-verify what guards this route", s.query, err, s.expectErrCode)
					}
					return
				}
				if err != nil {
					t.Fatalf("%q: %v", s.query, err)
				}
				distinct := map[string]bool{}
				n := 0
				for rows.Next() {
					scan := make([]any, s.cols)
					for i := range scan {
						scan[i] = new(sql.NullString)
					}
					if err := rows.Scan(scan...); err != nil {
						rows.Close()
						t.Fatalf("%q scan: %v", s.query, err)
					}
					for _, c := range s.tsCols {
						ns := scan[c].(*sql.NullString)
						if !ns.Valid {
							rows.Close()
							t.Fatalf("%q row %d: NULL timestamp column %d", s.query, n, c)
						}
						distinct[ns.String] = true
					}
					n++
				}
				rerr := rows.Err()
				rows.Close()
				if rerr != nil {
					t.Fatalf("%q rows: %v", s.query, rerr)
				}
				if n == 0 {
					t.Fatalf("%q returned no rows — the shape no longer exercises its join route", s.query)
				}
				if len(s.tsCols) > 0 && len(distinct) != 1 {
					t.Fatalf("%q drifted within ONE statement after %d execs: %d distinct timestamps — a join route evaluated CURRENT_TIMESTAMP without the statement clock (check ordinalJoinBuild.Clock threading and the frontier wrap)", s.query, execs, len(distinct))
				}
				execs++
			}
		})
	}
}
