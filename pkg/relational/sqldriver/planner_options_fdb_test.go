package sqldriver_test

// FDB integration tests for the planner options Java's relational
// PlannerConfiguration reads and Go can act on: DISABLED_PLANNER_RULES,
// DISABLE_PLANNER_REWRITING and PLAN_RIGHT_DEEP. They were defined on the Go
// option enum but nothing in the planner read them, so a user who set one got
// the full default rule set and no diagnostic. These tests drive each option
// through the whole stack — connection options → generator → Cascades planner →
// EXPLAIN and rows against a live store — because that is precisely the layer
// that used to drop them.
//
// Java reads a fourth, INDEX_FETCH_METHOD, which Go cannot honor (no
// remote-fetch implementation at any layer); it is untested here on purpose and
// tracked as its own TODO item rather than given a test that would assert
// nothing.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

// explainOnConn returns the EXPLAIN plan text for q on the pinned connection,
// so the connection's api.Options are the ones in effect. EXPLAIN routes
// through the same planSelectCascades the real query path uses.
func explainOnConn(t *testing.T, ctx context.Context, conn *sql.Conn, q string) string {
	t.Helper()
	var plan string
	if err := conn.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&plan); err != nil {
		t.Fatalf("EXPLAIN %s: %v", q, err)
	}
	return plan
}

// scanInt64Rows drains q into a slice of the first column's values.
func scanInt64Rows(t *testing.T, ctx context.Context, conn *sql.Conn, q string) []int64 {
	t.Helper()
	rows, err := conn.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("query %s: %v", q, err)
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var v sql.NullInt64
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, v.Int64)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func sameInt64s(a, b []int64) bool {
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

// plannerOptsDB creates an isolated db + schema with an indexed table T and a
// self-joinable shape, plus a few rows so a plan can actually be executed.
func plannerOptsDB(t *testing.T, tag string) *sql.DB {
	t.Helper()
	db := setupErrorTestDB(t, "/planopts_"+tag, "planopts"+tag,
		"CREATE TABLE T (id BIGINT, a BIGINT, b BIGINT, c STRING, PRIMARY KEY (id))"+
			" CREATE INDEX idx_a ON T(a)"+
			" CREATE INDEX idx_ab ON T(a, b)")
	ctx := context.Background()
	for i := 1; i <= 4; i++ {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO T (id, a, b, c) VALUES (%d, %d, %d, 'v%d')", i, i%2, i, i)); err != nil {
			t.Fatalf("INSERT %d: %v", i, err)
		}
	}
	return db
}

// TestFDB_PlannerOptions_DisabledPlannerRules pins DISABLED_PLANNER_RULES end
// to end: naming a rule must remove it from the planner's rule set, changing
// the ACCESS PATH the query gets while leaving the answer alone. Disabling
// MatchLeafRule removes index-candidate matching, so the indexed equality falls
// back to a full scan plus a residual filter.
//
// The rule is spelled the way Java spells it (simple class name), so the same
// option value works on both engines.
func TestFDB_PlannerOptions_DisabledPlannerRules(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := plannerOptsDB(t, "rules")
	const q = "SELECT id FROM T WHERE a = 1"

	base := pinEmbeddedConn(t, db, func(*embedded.EmbeddedConnection) {})
	baseExplain := explainOnConn(t, ctx, base, q)
	if !strings.Contains(baseExplain, "IndexScan") {
		t.Fatalf("default plan %q must use an index for the contrast to mean anything", baseExplain)
	}
	baseRows := scanInt64Rows(t, ctx, base, q)
	if len(baseRows) == 0 {
		t.Fatal("fixture produced no rows; the row-equality check would be vacuous")
	}

	off := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptDisabledPlannerRules, []string{"MatchLeafRule"}).Build())
	})
	offExplain := explainOnConn(t, ctx, off, q)
	if strings.Contains(offExplain, "IndexScan") {
		t.Fatalf("DISABLED_PLANNER_RULES=[MatchLeafRule] left an IndexScan in the plan (%q) — "+
			"the option is being accepted and ignored", offExplain)
	}
	if !strings.Contains(offExplain, "Scan(T)") {
		t.Fatalf("with index matching disabled the plan must be a full scan, got %q", offExplain)
	}
	if got := scanInt64Rows(t, ctx, off, q); !sameInt64s(got, baseRows) {
		t.Fatalf("disabling a planner rule changed the ANSWER: %v vs %v — a planner option may "+
			"only change the plan", got, baseRows)
	}
}

// TestFDB_PlannerOptions_DisablePlannerRewriting pins DISABLE_PLANNER_REWRITING
// end to end. Java's disableRewritingRules() turns off RewritingRuleSet.
// OPTIONAL_RULES, which includes the outer-join canonicalizer; without it a
// LEFT OUTER join is planned as a plain nested-loop outer join over full scans
// instead of the correlated, index-driven form. Same rows, different plan.
func TestFDB_PlannerOptions_DisablePlannerRewriting(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := plannerOptsDB(t, "rewrite")
	const q = "SELECT T.id FROM T LEFT JOIN T AS U ON T.a = U.a"

	base := pinEmbeddedConn(t, db, func(*embedded.EmbeddedConnection) {})
	baseExplain := explainOnConn(t, ctx, base, q)
	if !strings.Contains(baseExplain, "FlatMap") {
		t.Fatalf("default plan %q must be the REWRITTEN correlated outer join for the contrast "+
			"to mean anything", baseExplain)
	}
	baseRows := scanInt64Rows(t, ctx, base, q)
	if len(baseRows) == 0 {
		t.Fatal("fixture produced no rows; the row-equality check would be vacuous")
	}

	off := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptDisablePlannerRewriting, true).Build())
	})
	offExplain := explainOnConn(t, ctx, off, q)
	if offExplain == baseExplain {
		t.Fatalf("DISABLE_PLANNER_REWRITING left the plan unchanged (%q) — the option is being "+
			"accepted and ignored", offExplain)
	}
	if !strings.Contains(offExplain, "NestedLoopJoin(LEFT OUTER") {
		t.Fatalf("with rewriting disabled the outer join must stay un-canonicalized, got %q", offExplain)
	}
	if got := scanInt64Rows(t, ctx, off, q); !sameInt64s(got, baseRows) {
		t.Fatalf("disabling rewriting changed the ANSWER: %v vs %v", got, baseRows)
	}
}

// TestFDB_PlannerOptions_PlanCacheKeyedByOptions pins the plan-cache half of
// the wiring on ONE connection: the plan cache is per-connection, so changing a
// planner option mid-connection would otherwise keep serving the plan built
// under the previous options — a wrong-plan bug, not a stale-cost one. Java's
// QueryCacheKey carries the whole PlannerConfiguration for this reason.
func TestFDB_PlannerOptions_PlanCacheKeyedByOptions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := plannerOptsDB(t, "cache")
	const q = "SELECT id FROM T WHERE a = 1"

	var conn *sql.Conn
	setOpts := func(o *api.Options) {
		if err := conn.Raw(func(driverConn any) error {
			driverConn.(*embedded.EmbeddedConnection).SetOptions(o)
			return nil
		}); err != nil {
			t.Fatalf("Raw: %v", err)
		}
	}
	conn = pinEmbeddedConn(t, db, func(*embedded.EmbeddedConnection) {})

	// Warm the cache under the defaults.
	first := explainOnConn(t, ctx, conn, q)
	if !strings.Contains(first, "IndexScan") {
		t.Fatalf("default plan %q must use an index", first)
	}

	setOpts(api.NewOptionsBuilder().
		Set(api.OptDisabledPlannerRules, []string{"MatchLeafRule"}).Build())
	second := explainOnConn(t, ctx, conn, q)
	if second == first {
		t.Fatalf("the cached default plan survived an option change: %q — the plan-cache key "+
			"does not include the planner options", second)
	}

	// Back to the defaults: the original plan must return, not the one built
	// under the disabled rule.
	setOpts(api.NoOptions())
	third := explainOnConn(t, ctx, conn, q)
	if third != first {
		t.Fatalf("restoring the default options gave %q, want the original %q", third, first)
	}
}

// starOptsDB creates the all-live star schema the join-enumeration budget
// exercise needs: a hub plus five spokes, each joined to the hub only.
func starOptsDB(t *testing.T, tag string) *sql.DB {
	t.Helper()
	ddl := "CREATE TABLE H (id BIGINT, v BIGINT, PRIMARY KEY (id))"
	for i := 1; i <= 5; i++ {
		ddl += fmt.Sprintf(" CREATE TABLE S%d (id BIGINT, hid BIGINT, PRIMARY KEY (id))", i)
	}
	db := setupErrorTestDB(t, "/planstar_"+tag, "planstar"+tag, ddl)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "INSERT INTO H (id, v) VALUES (1, 10)"); err != nil {
		t.Fatalf("INSERT H: %v", err)
	}
	for i := 1; i <= 5; i++ {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO S%d (id, hid) VALUES (%d, 1)", i, i)); err != nil {
			t.Fatalf("INSERT S%d: %v", i, err)
		}
	}
	return db
}

// scanAllRowsSorted drains q into one string per row (all columns), sorted.
// Sorted because the queries under comparison have no ORDER BY: a different
// join order legitimately emits the same multiset in a different sequence, and
// the property being tested is that the ANSWER is identical, not the order.
func scanAllRowsSorted(t *testing.T, ctx context.Context, conn *sql.Conn, q string) []string {
	t.Helper()
	rows, err := conn.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("query %s: %v", q, err)
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, fmt.Sprintf("%v", vals))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	sort.Strings(out)
	return out
}

// threeSpokeStarQuery is the hub+3 star. Unlike hub+5 it converges with the
// option BOTH off and on, which is what makes an answer-equality comparison
// possible at all.
const threeSpokeStarQuery = "SELECT H.id, H.v, S1.id, S2.id, S3.id " +
	"FROM H, S1, S2, S3 " +
	"WHERE H.id = S1.hid AND H.id = S2.hid AND H.id = S3.hid"

// TestFDB_PlannerOptions_PlanRightDeepPreservesRows is the differential that
// matters most for a search-space RESTRICTION: excluding bushy join trees must
// change only which plan is chosen, never the answer. hub+5 cannot provide it
// (the default side does not converge), so this uses hub+3, which converges
// both ways over a fixture with several hub rows and several spokes per hub —
// a multi-row join result, not a single row that any plan would get right.
//
// The plans are asserted to actually DIFFER first. Without that, identical
// rows would be evidence of nothing: if the option changed no plan, row
// equality would be trivially true and the test would pass while proving
// nothing about semantics preservation.
func TestFDB_PlannerOptions_PlanRightDeepPreservesRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	ddl := "CREATE TABLE H (id BIGINT, v BIGINT, PRIMARY KEY (id))"
	for i := 1; i <= 3; i++ {
		ddl += fmt.Sprintf(" CREATE TABLE S%d (id BIGINT, hid BIGINT, PRIMARY KEY (id))", i)
	}
	db := setupErrorTestDB(t, "/planstar_rows", "planstarrows", ddl)

	// Three hubs; hub 1 has two rows in every spoke, hub 2 has one, hub 3 has
	// none. That yields 2*2*2 + 1 = 9 result rows and an outer-hub row that
	// must NOT appear — enough structure that a wrong join order would show.
	for h := 1; h <= 3; h++ {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO H (id, v) VALUES (%d, %d)", h, h*10)); err != nil {
			t.Fatalf("INSERT H %d: %v", h, err)
		}
	}
	rowID := 0
	for s := 1; s <= 3; s++ {
		for _, hub := range []int{1, 1, 2} {
			rowID++
			if _, err := db.ExecContext(ctx, fmt.Sprintf(
				"INSERT INTO S%d (id, hid) VALUES (%d, %d)", s, rowID, hub)); err != nil {
				t.Fatalf("INSERT S%d: %v", s, err)
			}
		}
	}

	base := pinEmbeddedConn(t, db, func(*embedded.EmbeddedConnection) {})
	rd := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().Set(api.OptPlanRightDeep, true).Build())
	})

	basePlan := explainOnConn(t, ctx, base, threeSpokeStarQuery)
	rdPlan := explainOnConn(t, ctx, rd, threeSpokeStarQuery)
	if basePlan == rdPlan {
		t.Fatalf("PLAN_RIGHT_DEEP produced the same plan as the default (%q) — row equality "+
			"below would then prove nothing about the option preserving semantics", basePlan)
	}

	baseRows := scanAllRowsSorted(t, ctx, base, threeSpokeStarQuery)
	rdRows := scanAllRowsSorted(t, ctx, rd, threeSpokeStarQuery)
	if len(baseRows) != 9 {
		t.Fatalf("fixture produced %d rows, want 9 — the differential needs a multi-row join", len(baseRows))
	}
	if len(rdRows) != len(baseRows) {
		t.Fatalf("right-deep returned %d rows, default returned %d", len(rdRows), len(baseRows))
	}
	for i := range baseRows {
		if rdRows[i] != baseRows[i] {
			t.Fatalf("restricting join enumeration to right-deep trees CHANGED THE ANSWER:\n"+
				"  default   = %v\n  right-deep = %v", baseRows, rdRows)
		}
	}
	t.Logf("hub+3 star: default plan %q vs right-deep plan %q, %d rows identical",
		basePlan, rdPlan, len(baseRows))
}

// outerJoinStarQueries are two five-way shapes that mix four inner-joined legs
// with one LEFT OUTER leg, placed at opposite ends of the plan tree: the first
// null-extends at the TOP (the outer join sits above the inner sub-join the
// partitioner reorders), the second at the BOTTOM (the outer join is the
// innermost leg and the inner joins are re-associated around it).
//
// Four inner legs is not decoration. Right-deep restricts a partition to a
// single "upper" quantifier, and with three or fewer inner legs the restricted
// enumeration still contains the winner, so the option is inert and the plans
// come out identical; at four it bites. Pure LEFT-JOIN chains are inert for a
// different reason — Go lowers each ON clause to its own binary join rather
// than to a quantifier of the flat select PartitionSelectRule bipartitions, so
// nothing there is enumerated at all.
var outerJoinStarQueries = map[string]string{
	"outer_join_on_top": "SELECT H.id, S1.id, S2.id, S3.id, S4.id " +
		"FROM H JOIN S2 ON H.id = S2.hid JOIN S3 ON H.id = S3.hid JOIN S4 ON H.id = S4.hid " +
		"LEFT JOIN S1 ON H.id = S1.hid",
	"outer_join_at_bottom": "SELECT H.id, S1.id, S2.id, S3.id, S4.id " +
		"FROM H LEFT JOIN S1 ON H.id = S1.hid JOIN S2 ON H.id = S2.hid " +
		"JOIN S3 ON H.id = S3.hid JOIN S4 ON H.id = S4.hid",
}

// TestFDB_PlannerOptions_PlanRightDeepPreservesOuterJoinRows is the differential
// for the one dimension where restricting join enumeration could plausibly
// change the ANSWER rather than just the plan.
//
// For inner equijoins, right-deep is semantics-preserving by associativity and
// commutativity, so the hub+3 differential above mostly confirms wiring. Outer
// joins are the real risk: they are neither associative nor commutative with
// respect to inner joins, null extension depends on which side is preserved,
// and ShouldJoinRightDeep is consumed by PartitionSelectRule, which the inner
// legs surrounding an outer join flow through. If a restricted partition set
// could re-associate an inner join across a null-extension boundary, rows would
// change — silently, since both plans are well-formed.
//
// The fixture is built so null extension is actually exercised: one hub has
// inner matches on every leg but NO row in the outer leg, so its result row
// must appear with a NULL there. A LEFT JOIN whose right side always matches
// would make this test an inner-join test wearing a costume, so the
// null-extended count is asserted, not assumed.
func TestFDB_PlannerOptions_PlanRightDeepPreservesOuterJoinRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	ddl := "CREATE TABLE H (id BIGINT, v BIGINT, PRIMARY KEY (id))"
	for i := 1; i <= 4; i++ {
		ddl += fmt.Sprintf(" CREATE TABLE S%d (id BIGINT, hid BIGINT, PRIMARY KEY (id))", i)
	}
	db := setupErrorTestDB(t, "/planstar_oj", "planstaroj", ddl)

	for h := 1; h <= 3; h++ {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO H (id, v) VALUES (%d, %d)", h, h*10)); err != nil {
			t.Fatalf("INSERT H %d: %v", h, err)
		}
	}
	// Inner legs S2..S4: hub 1 twice, hub 2 once, hub 3 never.
	rowID := 0
	for s := 2; s <= 4; s++ {
		for _, hub := range []int{1, 1, 2} {
			rowID++
			if _, err := db.ExecContext(ctx, fmt.Sprintf(
				"INSERT INTO S%d (id, hid) VALUES (%d, %d)", s, rowID, hub)); err != nil {
				t.Fatalf("INSERT S%d: %v", s, err)
			}
		}
	}
	// Outer leg S1: hub 1 only. Hub 2 therefore survives the inner joins and
	// must null-extend; hub 3 must not appear at all.
	for _, id := range []int{101, 102} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO S1 (id, hid) VALUES (%d, 1)", id)); err != nil {
			t.Fatalf("INSERT S1: %v", err)
		}
	}

	base := pinEmbeddedConn(t, db, func(*embedded.EmbeddedConnection) {})
	rd := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().Set(api.OptPlanRightDeep, true).Build())
	})

	for name, q := range outerJoinStarQueries {
		name, q := name, q
		t.Run(name, func(t *testing.T) {
			basePlan := explainOnConn(t, ctx, base, q)
			rdPlan := explainOnConn(t, ctx, rd, q)
			if !strings.Contains(basePlan, "LEFT OUTER") {
				t.Fatalf("default plan %q has no outer join; this fixture is not testing what it claims", basePlan)
			}
			if basePlan == rdPlan {
				t.Fatalf("PLAN_RIGHT_DEEP produced the same plan as the default (%q) — the row "+
					"comparison below would then prove nothing about outer joins under a "+
					"restricted partition set", basePlan)
			}

			baseRows := scanAllRowsSorted(t, ctx, base, q)
			rdRows := scanAllRowsSorted(t, ctx, rd, q)

			// 8 combinations for hub 1 (2*2*2 inner) x 2 outer matches = 16,
			// plus hub 2's single inner combination null-extended = 17.
			const wantRows = 17
			if len(baseRows) != wantRows {
				t.Fatalf("default returned %d rows, want %d: %v", len(baseRows), wantRows, baseRows)
			}
			nullExtended := 0
			for _, r := range baseRows {
				if strings.Contains(r, "<nil>") {
					nullExtended++
				}
			}
			if nullExtended != 1 {
				t.Fatalf("fixture produced %d null-extended rows, want exactly 1 — without one "+
					"this is an inner-join test in disguise: %v", nullExtended, baseRows)
			}

			if len(rdRows) != len(baseRows) {
				t.Fatalf("right-deep returned %d rows, default returned %d — restricting join "+
					"enumeration changed the answer across a null-extension boundary\n"+
					"  default    = %v\n  right-deep = %v", len(rdRows), len(baseRows), baseRows, rdRows)
			}
			for i := range baseRows {
				if rdRows[i] != baseRows[i] {
					t.Fatalf("restricting join enumeration to right-deep trees CHANGED THE ANSWER "+
						"for an OUTER join:\n  default    = %v\n  right-deep = %v", baseRows, rdRows)
				}
			}
			t.Logf("%s: %d rows (%d null-extended) identical\n  default    = %s\n  right-deep = %s",
				name, len(baseRows), nullExtended, basePlan, rdPlan)
		})
	}
}

// fiveSpokeStarQuery is the hub+5 all-live star: every leg is projected, so
// none can be pruned, and every leg joins only the hub.
const fiveSpokeStarQuery = "SELECT H.id, S1.id, S2.id, S3.id, S4.id, S5.id " +
	"FROM H, S1, S2, S3, S4, S5 " +
	"WHERE H.id = S1.hid AND H.id = S2.hid AND H.id = S3.hid AND H.id = S4.hid AND H.id = S5.hid"

// TestFDB_PlannerOptions_PlanRightDeep is CQ-9's stated goal delivered through
// Java's own opt-in lever. The hub+5 all-live star exhausts the 100,000-task
// planning budget at DEFAULT settings — and would in Java too, whose
// PLAN_RIGHT_DEEP likewise defaults to false — so this is not a divergence to
// close but a knob to wire. With the option set, join enumeration is restricted
// to right-deep trees, the star converges, and the query runs.
//
// The default half is asserted as well: bounding enumeration by default would
// change the plan shape of every multi-way join in the engine, which is not
// something an option-wiring change gets to do silently.
func TestFDB_PlannerOptions_PlanRightDeep(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := starOptsDB(t, "rd")

	base := pinEmbeddedConn(t, db, func(*embedded.EmbeddedConnection) {})
	rows, err := base.QueryContext(ctx, fiveSpokeStarQuery)
	if rows != nil {
		_ = rows.Close()
	}
	assertPlannerCapHit(t, err)

	rd := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().Set(api.OptPlanRightDeep, true).Build())
	})
	rdRows, rdErr := rd.QueryContext(ctx, fiveSpokeStarQuery)
	if rdErr != nil {
		t.Fatalf("PLAN_RIGHT_DEEP did not bring the hub+5 star inside the planning budget: %v", rdErr)
	}
	defer func() { _ = rdRows.Close() }()

	n := 0
	for rdRows.Next() {
		var hub, s1, s2, s3, s4, s5 sql.NullInt64
		if err := rdRows.Scan(&hub, &s1, &s2, &s3, &s4, &s5); err != nil {
			t.Fatalf("scan star row: %v", err)
		}
		if hub.Int64 != 1 {
			t.Fatalf("hub id = %d, want 1", hub.Int64)
		}
		n++
	}
	if err := rdRows.Err(); err != nil {
		t.Fatalf("star rows: %v", err)
	}
	// One hub row joined to exactly one row per spoke.
	if n != 1 {
		t.Fatalf("hub+5 star returned %d rows, want 1 — a right-deep plan must still be CORRECT", n)
	}

	// Explicitly false must be identical to unset: the Java-identical default
	// is not something setting the option can move.
	off := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().Set(api.OptPlanRightDeep, false).Build())
	})
	offRows, offErr := off.QueryContext(ctx, fiveSpokeStarQuery)
	if offRows != nil {
		_ = offRows.Close()
	}
	assertPlannerCapHit(t, offErr)
}
