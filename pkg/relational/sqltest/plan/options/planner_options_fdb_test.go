package sqltest

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
	"slices"
	"sort"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

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
	db := testkit.SetupErrorDB(t, "/FRL/planopts_"+tag, "planopts"+tag,
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

	base := testkit.PinEmbeddedConn(t, db, func(*embedded.EmbeddedConnection) {})
	baseExplain := testkit.ExplainConn(t, ctx, base, q)
	if !strings.Contains(baseExplain, "IndexScan") {
		t.Fatalf("default plan %q must use an index for the contrast to mean anything", baseExplain)
	}
	baseRows := testkit.ScanInt64Rows(t, ctx, base, q)
	if len(baseRows) == 0 {
		t.Fatal("fixture produced no rows; the row-equality check would be vacuous")
	}

	off := testkit.PinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptDisabledPlannerRules, []string{"MatchLeafRule"}).Build())
	})
	offExplain := testkit.ExplainConn(t, ctx, off, q)
	if strings.Contains(offExplain, "IndexScan") {
		t.Fatalf("DISABLED_PLANNER_RULES=[MatchLeafRule] left an IndexScan in the plan (%q) — "+
			"the option is being accepted and ignored", offExplain)
	}
	if !strings.Contains(offExplain, "Scan(T)") {
		t.Fatalf("with index matching disabled the plan must be a full scan, got %q", offExplain)
	}
	if got := testkit.ScanInt64Rows(t, ctx, off, q); !sameInt64s(got, baseRows) {
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

	base := testkit.PinEmbeddedConn(t, db, func(*embedded.EmbeddedConnection) {})
	baseExplain := testkit.ExplainConn(t, ctx, base, q)
	if !strings.Contains(baseExplain, "FlatMap") {
		t.Fatalf("default plan %q must be the REWRITTEN correlated outer join for the contrast "+
			"to mean anything", baseExplain)
	}
	baseRows := testkit.ScanInt64Rows(t, ctx, base, q)
	if len(baseRows) == 0 {
		t.Fatal("fixture produced no rows; the row-equality check would be vacuous")
	}

	off := testkit.PinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptDisablePlannerRewriting, true).Build())
	})
	offExplain := testkit.ExplainConn(t, ctx, off, q)
	if offExplain == baseExplain {
		t.Fatalf("DISABLE_PLANNER_REWRITING left the plan unchanged (%q) — the option is being "+
			"accepted and ignored", offExplain)
	}
	if !strings.Contains(offExplain, "NestedLoopJoin(LEFT OUTER") {
		t.Fatalf("with rewriting disabled the outer join must stay un-canonicalized, got %q", offExplain)
	}
	if got := testkit.ScanInt64Rows(t, ctx, off, q); !sameInt64s(got, baseRows) {
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
	conn = testkit.PinEmbeddedConn(t, db, func(*embedded.EmbeddedConnection) {})

	// Warm the cache under the defaults.
	first := testkit.ExplainConn(t, ctx, conn, q)
	if !strings.Contains(first, "IndexScan") {
		t.Fatalf("default plan %q must use an index", first)
	}

	setOpts(api.NewOptionsBuilder().
		Set(api.OptDisabledPlannerRules, []string{"MatchLeafRule"}).Build())
	second := testkit.ExplainConn(t, ctx, conn, q)
	if second == first {
		t.Fatalf("the cached default plan survived an option change: %q — the plan-cache key "+
			"does not include the planner options", second)
	}

	// Back to the defaults: the original plan must return, not the one built
	// under the disabled rule.
	setOpts(api.NoOptions())
	third := testkit.ExplainConn(t, ctx, conn, q)
	if third != first {
		t.Fatalf("restoring the default options gave %q, want the original %q", third, first)
	}
}

// starOptsDB creates the all-live star schema the join-enumeration budget
// exercise needs: a hub plus six spokes, each joined to the hub only.
func starOptsDB(t *testing.T, tag string) *sql.DB {
	t.Helper()
	ddl := "CREATE TABLE H (id BIGINT, v BIGINT, PRIMARY KEY (id))"
	for i := 1; i <= 6; i++ {
		ddl += fmt.Sprintf(" CREATE TABLE S%d (id BIGINT, hid BIGINT, PRIMARY KEY (id))", i)
	}
	db := testkit.SetupErrorDB(t, "/FRL/planstar_"+tag, "planstar"+tag, ddl)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "INSERT INTO H (id, v) VALUES (1, 10)"); err != nil {
		t.Fatalf("INSERT H: %v", err)
	}
	for i := 1; i <= 6; i++ {
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

// chainDDL and chainQuery are a four-way chain over non-key columns
// (A–B–C–D). Its default plan is BUSHY — the two end pairs join first and then
// meet — which a right-deep search cannot produce, so the option provably
// changes the plan rather than a tie-break. (A star is inert: with three or
// fewer inner legs the restricted enumeration still contains its winner.)
const chainDDL = "CREATE TABLE A (id BIGINT, x BIGINT, PRIMARY KEY (id)) " +
	"CREATE TABLE B (id BIGINT, x BIGINT, y BIGINT, PRIMARY KEY (id)) " +
	"CREATE TABLE C (id BIGINT, y BIGINT, z BIGINT, PRIMARY KEY (id)) " +
	"CREATE TABLE D (id BIGINT, z BIGINT, PRIMARY KEY (id)) " +
	"CREATE TABLE E (id BIGINT, z BIGINT, PRIMARY KEY (id))"

const chainQuery = "SELECT A.id, B.id, C.id, D.id FROM A, B, C, D " +
	"WHERE A.x = B.x AND B.y = C.y AND C.z = D.z"

// seedChain inserts rows under which chainQuery returns six rows: A1, A2 and A3
// each reach C1 through B, and C1 meets D1 and D2. E holds id 1 and z 1 only.
func seedChain(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	for _, stmt := range []string{
		"INSERT INTO A (id, x) VALUES (1, 1), (2, 1), (3, 2)",
		"INSERT INTO B (id, x, y) VALUES (1, 1, 1), (2, 2, 1), (3, 3, 2)",
		"INSERT INTO C (id, y, z) VALUES (1, 1, 1), (2, 1, 2), (3, 9, 1)",
		"INSERT INTO D (id, z) VALUES (1, 1), (2, 1), (3, 5)",
		"INSERT INTO E (id, z) VALUES (1, 1)",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
}

// hasBushyJoin reports whether an EXPLAIN string holds a join with an inner
// join on BOTH sides — the shape a right-deep search excludes. An outer join
// (FlatMap over DefaultOnEmpty, or a LEFT OUTER NLJ) is one quantifier of the
// partitioned select, so only INNER joins count as a side's sub-join.
func hasBushyJoin(plan string) bool {
	for i := range plan {
		for _, op := range []string{"NestedLoopJoin(", "FlatMap("} {
			if !strings.HasPrefix(plan[i:], op) {
				continue
			}
			joinArgs := 0
			for _, arg := range explainArgs(plan[i+len(op):]) {
				if strings.Contains(arg, "NestedLoopJoin(INNER") {
					joinArgs++
				}
			}
			if joinArgs >= 2 {
				return true
			}
		}
	}
	return false
}

// explainArgs splits the argument list starting at s (just past an opening
// parenthesis) at its top-level commas.
func explainArgs(s string) []string {
	var args []string
	depth, start := 0, 0
	for i, r := range s {
		switch r {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth == 0 {
				return append(args, s[start:i])
			}
			depth--
		case ',':
			if depth == 0 {
				args = append(args, s[start:i])
				start = i + 1
			}
		}
	}
	return append(args, s[start:])
}

func TestHasBushyJoin(t *testing.T) {
	t.Parallel()
	for plan, want := range map[string]bool{
		"Project([_current.ID#0], NestedLoopJoin(INNER, [1 preds], NestedLoopJoin(INNER, [1 preds], Scan(D), Scan(C)), NestedLoopJoin(INNER, [1 preds], Scan(A), Scan(B))))":                          true,
		"NestedLoopJoin(INNER, [1 preds], NestedLoopJoin(INNER, [1 preds], FlatMap(outer=Scan(A), inner=DefaultOnEmpty(Scan(E, [=]))), Scan(B)), NestedLoopJoin(INNER, [1 preds], Scan(D), Scan(C)))": true,
		"NestedLoopJoin(INNER, [1 preds], FlatMap(outer=Scan(A), inner=DefaultOnEmpty(Scan(E, [=]))), NestedLoopJoin(INNER, [1 preds], Scan(D), NestedLoopJoin(INNER, [1 preds], Scan(C), Scan(B))))": false,
		"Project([_current.ID#0], NestedLoopJoin(INNER, [1 preds], Scan(A), NestedLoopJoin(INNER, [1 preds], Scan(D), NestedLoopJoin(INNER, [1 preds], Scan(C), Scan(B)))))":                          false,
		"FlatMap(outer=NestedLoopJoin(INNER, [1 preds], Scan(A), Scan(B)), inner=DefaultOnEmpty(Scan(E, [=])))":                                                                                       false,
	} {
		if got := hasBushyJoin(plan); got != want {
			t.Errorf("hasBushyJoin(%s) = %v, want %v", plan, got, want)
		}
	}
}

// TestFDB_PlannerOptions_PlanRightDeepPreservesRows is the differential that
// matters most for a search-space RESTRICTION: excluding bushy join trees must
// change only which plan is chosen, never the answer. The two plans are
// asserted to differ and the right-deep one to hold no bushy join, so row
// equality below compares two genuinely different join trees over a multi-row
// result. The default need not be bushy: Java's own default for this chain is
// right-deep.
func TestFDB_PlannerOptions_PlanRightDeepPreservesRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.SetupErrorDB(t, "/FRL/planchain_rows", "planchainrows", chainDDL)
	seedChain(t, ctx, db)

	base := testkit.PinEmbeddedConn(t, db, func(*embedded.EmbeddedConnection) {})
	rd := testkit.PinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().Set(api.OptPlanRightDeep, true).Build())
	})

	basePlan := testkit.ExplainConn(t, ctx, base, chainQuery)
	rdPlan := testkit.ExplainConn(t, ctx, rd, chainQuery)
	if basePlan == rdPlan || hasBushyJoin(rdPlan) {
		t.Fatalf("want the option to choose a different, non-bushy join tree — row equality "+
			"below would otherwise prove nothing about the option preserving semantics\n"+
			"  default    = %s\n  right-deep = %s", basePlan, rdPlan)
	}

	baseRows := scanAllRowsSorted(t, ctx, base, chainQuery)
	rdRows := scanAllRowsSorted(t, ctx, rd, chainQuery)
	if len(baseRows) != 6 {
		t.Fatalf("fixture produced %d rows, want 6 — the differential needs a multi-row join: %v", len(baseRows), baseRows)
	}
	if !slices.Equal(rdRows, baseRows) {
		t.Fatalf("restricting join enumeration to right-deep trees CHANGED THE ANSWER:\n"+
			"  default    = %v\n  right-deep = %v", baseRows, rdRows)
	}
}

// outerJoinChainQueries put a LEFT OUTER leg at opposite ends of the chain:
// null-extending at the TOP (above the inner sub-join the partitioner
// reorders) and at the BOTTOM (the inner joins are re-associated around it).
var outerJoinChainQueries = map[string]string{
	"outer_join_on_top": "SELECT A.id, B.id, C.id, D.id, E.id " +
		"FROM A JOIN B ON A.x = B.x JOIN C ON B.y = C.y JOIN D ON C.z = D.z " +
		"LEFT JOIN E ON D.id = E.id",
	"outer_join_at_bottom": "SELECT A.id, B.id, C.id, D.id, E.id " +
		"FROM A LEFT JOIN E ON A.id = E.id JOIN B ON A.x = B.x " +
		"JOIN C ON B.y = C.y JOIN D ON C.z = D.z",
}

// TestFDB_PlannerOptions_PlanRightDeepPreservesOuterJoinRows is the differential
// for the one dimension where restricting join enumeration could plausibly
// change the ANSWER rather than just the plan: outer joins are neither
// associative nor commutative with inner joins, and ShouldJoinRightDeep is
// consumed by PartitionSelectRule, which the inner legs around an outer join
// flow through. Null extension is exercised, not assumed: some but not all of
// the six rows null-extend in each query.
func TestFDB_PlannerOptions_PlanRightDeepPreservesOuterJoinRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.SetupErrorDB(t, "/FRL/planchain_oj", "planchainoj", chainDDL)
	seedChain(t, ctx, db)

	base := testkit.PinEmbeddedConn(t, db, func(*embedded.EmbeddedConnection) {})
	rd := testkit.PinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().Set(api.OptPlanRightDeep, true).Build())
	})

	for name, q := range outerJoinChainQueries {
		t.Run(name, func(t *testing.T) {
			basePlan := testkit.ExplainConn(t, ctx, base, q)
			rdPlan := testkit.ExplainConn(t, ctx, rd, q)
			if !strings.Contains(basePlan, "LEFT OUTER") && !strings.Contains(basePlan, "DefaultOnEmpty") {
				t.Fatalf("default plan %q has no outer join; this fixture is not testing what it claims", basePlan)
			}
			if basePlan == rdPlan || hasBushyJoin(rdPlan) {
				t.Fatalf("want the option to choose a different, non-bushy join tree\n  default    = %s\n  right-deep = %s", basePlan, rdPlan)
			}

			baseRows := scanAllRowsSorted(t, ctx, base, q)
			rdRows := scanAllRowsSorted(t, ctx, rd, q)
			nullExtended := 0
			for _, r := range baseRows {
				if strings.Contains(r, "<nil>") {
					nullExtended++
				}
			}
			if len(baseRows) != 6 || nullExtended == 0 || nullExtended == len(baseRows) {
				t.Fatalf("default returned %d rows, %d null-extended; want 6 with some but not all "+
					"null-extended: %v", len(baseRows), nullExtended, baseRows)
			}
			if !slices.Equal(rdRows, baseRows) {
				t.Fatalf("restricting join enumeration to right-deep trees CHANGED THE ANSWER "+
					"for an OUTER join:\n  default    = %v\n  right-deep = %v", baseRows, rdRows)
			}
		})
	}
}

// sixSpokeStarQuery is the hub+6 all-live star: every leg is projected, so
// none can be pruned, and every leg joins only the hub.
const sixSpokeStarQuery = "SELECT H.id, S1.id, S2.id, S3.id, S4.id, S5.id, S6.id " +
	"FROM H, S1, S2, S3, S4, S5, S6 " +
	"WHERE H.id = S1.hid AND H.id = S2.hid AND H.id = S3.hid AND H.id = S4.hid AND H.id = S5.hid AND H.id = S6.hid"

// TestFDB_PlannerOptions_PlanRightDeep is CQ-9's stated goal delivered through
// Java's own opt-in lever. The hub+6 all-live star exhausts the 150,000-task
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

	base := testkit.PinEmbeddedConn(t, db, func(*embedded.EmbeddedConnection) {})
	rows, err := base.QueryContext(ctx, sixSpokeStarQuery)
	if rows != nil {
		_ = rows.Close()
	}
	testkit.AssertPlannerCapHit(t, err)

	rd := testkit.PinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().Set(api.OptPlanRightDeep, true).Build())
	})
	rdRows, rdErr := rd.QueryContext(ctx, sixSpokeStarQuery)
	if rdErr != nil {
		t.Fatalf("PLAN_RIGHT_DEEP did not bring the hub+6 star inside the planning budget: %v", rdErr)
	}
	defer func() { _ = rdRows.Close() }()

	n := 0
	for rdRows.Next() {
		var hub, s1, s2, s3, s4, s5, s6 sql.NullInt64
		if err := rdRows.Scan(&hub, &s1, &s2, &s3, &s4, &s5, &s6); err != nil {
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
		t.Fatalf("hub+6 star returned %d rows, want 1 — a right-deep plan must still be CORRECT", n)
	}

	// Explicitly false must be identical to unset: the Java-identical default
	// is not something setting the option can move.
	off := testkit.PinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().Set(api.OptPlanRightDeep, false).Build())
	})
	offRows, offErr := off.QueryContext(ctx, sixSpokeStarQuery)
	if offRows != nil {
		_ = offRows.Close()
	}
	testkit.AssertPlannerCapHit(t, offErr)
}
