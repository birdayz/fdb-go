package embedded

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// These pins all guard the same class of defect: a shape that used to PLAN and
// stopped, because exact resolution reaches a construct the resolver cannot
// express. Each records the query, the plan it must produce, and the mechanism
// — the plan text alone would not say which of them broke.

const rfc232GapDDL = `CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))
CREATE TABLE t2 (id BIGINT, v BIGINT, PRIMARY KEY (id))
CREATE TABLE flat (id BIGINT, c1 BIGINT, c2 BIGINT, PRIMARY KEY (id))`

// TestParenthesisedGroupingKeyPlans pins GROUP BY over a PARENTHESISED
// expression.
//
// `(c1 = 1)` is a one-field row constructor, so the grouping key is
// RECORD-typed, and the pre-aggregate ordering requirement is expressed over a
// key's PRIMITIVE LEAVES (Java's Values.primitiveAccessorsForType). The leaf
// walk addressed field i of the key by building a field access over the key —
// and since RFC-232 a FieldValue's child must be a resolved quantified object,
// so `{_0: predicate}._0` cannot be built at all. The walk returned an error,
// the rule reads any expansion error as "this key has no leaf decomposition"
// and declines, and declining the only aggregation strategy means the query
// does not plan:
//
//	SELECT max(c2) FROM flat GROUP BY (c1 = 1)   ->   0AF00: no plan found
//
// while the same query without the parentheses planned. A constructor is its
// own accessor: leaf i is the value that constructed slot i.
func TestParenthesisedGroupingKeyPlans(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		sql  string
		want string
	}{
		{
			name: "parenthesised computed key",
			sql:  `SELECT max(c2) FROM flat GROUP BY (c1 = 1)`,
			want: "Project([_current.MAX(C2)#1], StreamingAgg(keys=[{_0: predicate}], " +
				"InMemorySort([predicate ASC], Scan(FLAT))))",
		},
		{
			// The parenthesised and bare spellings of one key side by side.
			// They are DIFFERENT keys to the planner (a row and a scalar), so
			// both survive into the aggregation — the arm exists because a
			// collapse would silently change the grouping.
			name: "parenthesised beside its bare twin",
			sql:  `SELECT max(c2) FROM flat GROUP BY (c1 = 1), c1 = 1`,
			want: "Project([_current.MAX(C2)#2], StreamingAgg(keys=[{_0: predicate}, predicate], " +
				"InMemorySort([predicate ASC, predicate ASC], Scan(FLAT))))",
		},
		{
			// The unparenthesised control. It planned throughout, and it is
			// what made the failure above look like a grouping-key type
			// problem rather than an accessor-construction one.
			name: "bare computed key",
			sql:  `SELECT max(c2) FROM flat GROUP BY c1 = 1`,
			want: "Project([_current.MAX(C2)#1], StreamingAgg(keys=[predicate], " +
				"InMemorySort([predicate ASC], Scan(FLAT))))",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := explainWithOptions(t, tc.sql, rfc232GapDDL, nil); got != tc.want {
				t.Errorf("plan = %q,\nwant %q", got, tc.want)
			}
		})
	}
}

// TestDerivedTableOverUnionWithParenthesisedBranchesPlans pins a derived table
// whose body is a UNION with PARENTHESISED branches.
//
// The derived source's columns are typed by walking the union one BRANCH at a
// time, and a parenthesised branch presents as an anonymous derived query the
// per-branch walk has no alias for. It declines, and since RFC-232 a declined
// derived source is not a fallback to a text-resolved column but no column at
// all — the outer projection then reports `projection slot 0 has no resolved
// Value`, and an ORDER BY over it reports `ORDER BY key "ID" has no resolved
// Value or ordinal metadata`. The union BODY has no such gap; its exact result
// type is the union row.
func TestDerivedTableOverUnionWithParenthesisedBranchesPlans(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		sql  string
	}{
		{
			name: "both branches parenthesised",
			sql:  `SELECT id FROM ((SELECT id FROM t) UNION ALL (SELECT id FROM t2)) AS u`,
		},
		{
			// One side only: the walk declines on the FIRST branch it cannot
			// shape, so a mixed body proves the decline is per-branch and not
			// a property of the whole union.
			name: "left branch parenthesised",
			sql:  `SELECT id FROM ((SELECT id FROM t) UNION ALL SELECT id FROM t2) AS u`,
		},
		{
			name: "right branch parenthesised",
			sql:  `SELECT id FROM (SELECT id FROM t UNION ALL (SELECT id FROM t2)) AS u`,
		},
		{
			// The reported shape: parenthesised branches carrying their own
			// ORDER BY and LIMIT, with an ORDER BY over the derived table.
			name: "parenthesised branches with limits, ordered outside",
			sql: `SELECT id FROM ((SELECT id FROM t ORDER BY id LIMIT 2) ` +
				`UNION ALL (SELECT id FROM t2 ORDER BY id LIMIT 2)) AS u ORDER BY id`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := explainWithOptions(t, tc.sql, rfc232GapDDL, nil)
			// Assert the column is READ, not merely that something planned: the
			// defect's signature is a projection with no resolved Value, and a
			// plan that reached EXPLAIN at all has resolved it.
			if !strings.Contains(got, "_current.ID#0") {
				t.Errorf("plan does not read the union's ID column by ordinal:\n%s", got)
			}
			if !strings.Contains(got, "Union") {
				t.Errorf("plan lost the union:\n%s", got)
			}
		})
	}
}

// TestExistsOverAJoinUnderAJoinPlans pins the narrowing of a FlatMap's INNER
// lineage crossing.
//
// Which legs a value is pushed through is decided against the value as it
// ARRIVES. The outer crossing can then re-root it onto the outer leg's own row,
// and offering that row to the inner leg's materializer is a hard error rather
// than a decline:
//
//	RecordQueryFlatMapPlan inner lineage: … reanchor.field: current root and
//	target have different exact types:
//	root RECORD(ID,COL1,ID,COL1), target RECORD(ID,T1_ID,ID,T2_ID)
//
// — the OUTER join's row offered to the EXISTS leg's join. The decision has to
// be re-asked after the crossing, against what the value can now answer.
func TestExistsOverAJoinUnderAJoinPlans(t *testing.T) {
	t.Parallel()
	const ddl = `CREATE TABLE t1 (id BIGINT, col1 BIGINT, PRIMARY KEY (id))
CREATE TABLE t2 (id BIGINT, t1_id BIGINT, PRIMARY KEY (id))
CREATE TABLE t3 (id BIGINT, t2_id BIGINT, PRIMARY KEY (id))`
	const sql = `SELECT t1.id FROM t1 JOIN t1 AS x ON x.id = t1.id ` +
		`WHERE EXISTS (SELECT 1 FROM t2, t3 WHERE t2.t1_id = t1.id)`

	got := explainWithOptions(t, sql, ddl, nil)
	// Both joins must survive: the outer one the projection reads through, and
	// the inner one inside the EXISTS leg. Collapsing either would "plan" while
	// answering a different query.
	if strings.Count(got, "NestedLoopJoin") != 2 {
		t.Errorf("plan has %d NestedLoopJoin, want 2 (the outer self-join and the EXISTS leg's):\n%s",
			strings.Count(got, "NestedLoopJoin"), got)
	}
	// #0 is t1.id in the outer merged row — the read whose lineage crossing is
	// the subject here.
	if !strings.Contains(got, "Project([_current.ID#0]") {
		t.Errorf("plan does not project the outer join's first ID:\n%s", got)
	}
}

// TestHiddenSortColumnDoesNotCaptureAnOutputAlias pins that a value already
// expressed on a producer's OUTPUT row keeps its ordinal.
//
// `SELECT col1 AS id, EXISTS (…) AS e FROM t1 ORDER BY t1.id` appends the sort
// key to the fold's output as a THIRD column, and the cleanup projection then
// re-projects output slots 0 and 1 by ordinal — correctly. The producer bridge
// re-derived those addresses anyway, and it answers by NAME: the fold's only
// slot whose accessor is named ID is the appended `T1.ID#0`, because slot 0 is
// `T1.COL1#1`. Output slot 0 came back reading slot 2, so the query returned
// t1.id under the label the user aliased onto col1 — 1,2,3 where 99,98,97 was
// asked for, and no error anywhere.
//
// The vacuity risk here is the alias: rename it and the collision disappears,
// so the `id2` arm is what proves the other arm is testing the collision and
// not the shape.
func TestHiddenSortColumnDoesNotCaptureAnOutputAlias(t *testing.T) {
	t.Parallel()
	const ddl = `CREATE TABLE t1 (id BIGINT, col1 BIGINT, PRIMARY KEY (id))
CREATE TABLE t2 (id BIGINT, t1_id BIGINT, PRIMARY KEY (id))`
	for _, tc := range []struct {
		name string
		sql  string
		want string
	}{
		{
			// The alias COLLIDES with the appended sort key's accessor name.
			name: "output alias collides with the hidden sort key",
			sql: `SELECT col1 AS id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS e ` +
				`FROM t1 ORDER BY t1.id`,
			want: "Project([_current.ID#0, _current.E#1], " +
				"FlatMap(outer=Scan(T1), inner=FirstOrDefault(PredicatesFilter(Scan(T2), [1 preds]))))",
		},
		{
			name: "descending, same collision",
			sql: `SELECT col1 AS id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS e ` +
				`FROM t1 ORDER BY t1.id DESC`,
			want: "Project([_current.ID#0, _current.E#1], " +
				"FlatMap(outer=Scan(T1) REVERSE, inner=FirstOrDefault(PredicatesFilter(Scan(T2), [1 preds]))))",
		},
		{
			// The VACUITY GUARD: no collision, and it planned correctly
			// throughout. Without it the arms above read as a test of the
			// EXISTS-fold shape rather than of the name capture.
			name: "no collision",
			sql: `SELECT col1 AS id2, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS e ` +
				`FROM t1 ORDER BY t1.id`,
			want: "Project([_current.ID2#0, _current.E#1], " +
				"FlatMap(outer=Scan(T1), inner=FirstOrDefault(PredicatesFilter(Scan(T2), [1 preds]))))",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := explainWithOptions(t, tc.sql, ddl, nil); got != tc.want {
				t.Errorf("plan = %q,\nwant %q", got, tc.want)
			}
		})
	}
}

// TestCorrelatedPredicateOverThreeSameShapedLegsReadsItsOwnLeg pins WHICH
// column a correlated predicate over a multi-leg EXISTS body reads.
//
// A producer bridge may map a read onto a same-named slot belonging to ANOTHER
// source, as the compatibility path for a logical source whose storage record
// differs only in nominal naming. Its whole claim is that the one same-named
// slot must be the requested column — and with three legs all shaped
// RECORD(ID), the row holds three copies. Which one the bridge admitted then
// turned on an incidental difference in PATH LENGTH: legs retained as nested
// objects read `$m._0.ID` and failed the name-path compare, while the unmerged
// sibling read `A2.ID` and passed. `b.id` was mapped to a2's slot, the EXISTS
// answered on the wrong column, and the query returned no rows.
//
// The assertion is the ORDINAL. Merged row is [b.id, c.id, a2.id], so the
// correlated comparison must read #0.
func TestCorrelatedPredicateOverThreeSameShapedLegsReadsItsOwnLeg(t *testing.T) {
	t.Parallel()
	const ddl = `CREATE TABLE a (id BIGINT, PRIMARY KEY (id))
CREATE TABLE b (id BIGINT, PRIMARY KEY (id))
CREATE TABLE c (id BIGINT, PRIMARY KEY (id))`
	for _, tc := range []struct {
		name string
		sql  string
		want string
	}{
		{
			name: "correlated on the first leg",
			sql:  `SELECT id FROM a WHERE EXISTS (SELECT 1 FROM b, c, a a2 WHERE b.id = 10 + a.id - 1)`,
			want: "_current.ID#0 = ((10 + A.ID#0) - 1)",
		},
		{
			// The MIDDLE leg, so a bridge that simply took the first or the
			// last same-named slot fails here even if the arm above passes.
			name: "correlated on the middle leg",
			sql:  `SELECT id FROM a WHERE EXISTS (SELECT 1 FROM b, c, a a2 WHERE c.id = 10 + a.id - 1)`,
			want: "_current.ID#1 = ((10 + A.ID#0) - 1)",
		},
		{
			name: "correlated on the last leg",
			sql:  `SELECT id FROM a WHERE EXISTS (SELECT 1 FROM b, c, a a2 WHERE a2.id = 10 + a.id - 1)`,
			want: "_current.ID#2 = ((10 + A.ID#0) - 1)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan, err := PlanPhysicalForTest(tc.sql, ddl, nil)
			if err != nil {
				t.Fatalf("planning: %v", err)
			}
			got := correlatedMergedRowPredicate(t, plan)
			if got != tc.want {
				t.Errorf("correlated predicate over the merged row = %q, want %q\nplan: %s",
					got, tc.want, plan.Explain())
			}
		})
	}
}

// correlatedMergedRowPredicate returns the rendering of the single predicate
// filtering the EXISTS body's merged row — the one whose ordinal says which leg
// it reads.
func correlatedMergedRowPredicate(t *testing.T, plan plans.RecordQueryPlan) string {
	t.Helper()
	var found string
	var walk func(plans.RecordQueryPlan)
	walk = func(p plans.RecordQueryPlan) {
		if p == nil || found != "" {
			return
		}
		if filter, isFilter := p.(*plans.RecordQueryPredicatesFilterPlan); isFilter {
			if _, isJoin := filter.GetInner().(*plans.RecordQueryNestedLoopJoinPlan); isJoin {
				preds := filter.GetPredicates()
				if len(preds) != 1 {
					t.Fatalf("the filter over the EXISTS body has %d predicates, want 1", len(preds))
				}
				found = preds[0].Explain()
				return
			}
		}
		for _, child := range p.GetChildren() {
			walk(child)
		}
	}
	walk(plan)
	if found == "" {
		t.Fatalf("no predicate filters the EXISTS body's merged row:\n%s", plan.Explain())
	}
	return found
}

// TestScalarSubqueryLegUnderDuplicateAliasCommaJoinPlans pins the nullability
// pull-up on a FlatMap's null-supplying leg.
//
// A leg the FlatMap null-extends flows a row that can BE null, and the ordinal
// layout refuses to publish a null-supplying window over a row typed NOT NULL —
// correctly, since the pair states two different facts about one leg:
//
//	layout.window[3].source: null-supplying source q$3 must be nullable,
//	but flows RECORD(MAX(Q.QID):LONG?)
//
// Java states it at the producer (Quantifier.pullUpResultColumnsWithNullability
// (true) → QuantifiedObjectValue.of(alias, type.withNullability(true))), and
// the result program has to be rewritten in the same step or the program and
// the window disagree about which exact row the alias names.
//
// The duplicate `AS a` is load-bearing: it is what puts a second same-named
// source beside the scalar subquery's leg, so the layout carries enough windows
// for the null-supplying one to be a later index rather than the first.
func TestScalarSubqueryLegUnderDuplicateAliasCommaJoinPlans(t *testing.T) {
	t.Parallel()
	const ddl = `CREATE TABLE p (id BIGINT, v BIGINT, PRIMARY KEY (id))
CREATE TABLE q (qid BIGINT, PRIMARY KEY (qid))`
	const sql = `SELECT (SELECT MAX(qid) FROM q WHERE a.id = 1) FROM p AS a, q AS a`

	got := explainWithOptions(t, sql, ddl, nil)
	// DefaultOnEmpty is what makes the leg null-supplying; without it the pin
	// would pass over a plan that never exercises the window at all.
	if !strings.Contains(got, "DefaultOnEmpty") {
		t.Errorf("plan lost the null-supplying scalar-subquery leg:\n%s", got)
	}
	if !strings.Contains(got, "StreamingAgg(keys=[]") {
		t.Errorf("plan lost the scalar subquery's aggregation:\n%s", got)
	}
}

// TestOuterJoinResidualStaysAboveTheNullExtension pins WHERE a WHERE-conjunct
// over a LEFT OUTER join is evaluated, and WHICH column it reads.
//
// The conjunct is WHERE-class: it must see the null-extended row, so it belongs
// ABOVE the join. It was instead being rewritten onto the preserved leg — one
// alias named both the null-supplying LEG (3 columns) and the join BOX (5), and
// a correlation substitution replaced the leg's row by the box without checking
// that the two are the same row. Every ordinal crossed untouched, so
// `E.ID#0 IS NULL` became the box's ordinal 0 — `D.ID#0` — which the access path
// then matched as a scan range on DEPT's primary key. The query returned no
// rows, the plan contained no trace of the conjunct, and nothing failed loudly.
//
// The ORDINAL is the assertion. A plan that merely puts a filter above the join
// would pass a shape-only check while reading the wrong column.
func TestOuterJoinResidualStaysAboveTheNullExtension(t *testing.T) {
	t.Parallel()
	const ddl = `CREATE TABLE emp (id BIGINT, dept_id BIGINT, fname STRING, PRIMARY KEY (id))
CREATE TABLE dept (id BIGINT, dname STRING, PRIMARY KEY (id))
CREATE TABLE badge (id BIGINT, emp_id BIGINT, PRIMARY KEY (id))`
	for _, tc := range []struct {
		name string
		sql  string
		want string
	}{
		{
			// The anti-join conjunct. Merged row is
			// [D.ID, D.DNAME, E.ID, E.DEPT_ID, E.FNAME], so E.ID is #2.
			name: "IS NULL anti-join conjunct beside NOT EXISTS",
			sql: `SELECT d.dname FROM dept d LEFT JOIN emp e ON e.dept_id = d.id ` +
				`WHERE e.id IS NULL AND NOT EXISTS (SELECT 1 FROM badge b WHERE b.emp_id = e.id)`,
			want: "_current.ID#2 IS NULL",
		},
		{
			// The same shape with an equality conjunct: E.FNAME is #4. This arm
			// kept working throughout, and it is why the defect read as an
			// IS-NULL problem rather than an ordinal one.
			name: "equality conjunct beside EXISTS",
			sql: `SELECT d.dname FROM dept d LEFT JOIN emp e ON e.dept_id = d.id ` +
				`WHERE e.fname = 'alice' AND EXISTS (SELECT 1 FROM badge b WHERE b.emp_id = e.id)`,
			want: "_current.FNAME#4 = 'alice'",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan, err := PlanPhysicalForTest(tc.sql, ddl, nil)
			if err != nil {
				t.Fatalf("planning: %v", err)
			}
			if !strings.Contains(plan.Explain(), "NestedLoopJoin(LEFT OUTER") {
				t.Errorf("plan lost the LEFT OUTER join:\n%s", plan.Explain())
			}
			if got := residualPredicateOverOuterJoin(t, plan); got != tc.want {
				t.Errorf("residual above the LEFT OUTER join = %q, want %q\nplan: %s",
					got, tc.want, plan.Explain())
			}
		})
	}
}

// residualPredicateOverOuterJoin returns the rendering of the single predicate
// filtering a LEFT OUTER join, or "" when no such filter exists. It reads the
// predicate rather than the plan text because the plan text renders every
// filter as `[N preds]` — which is exactly why a conjunct that moved onto the
// wrong leg was invisible in EXPLAIN.
func residualPredicateOverOuterJoin(t *testing.T, plan plans.RecordQueryPlan) string {
	t.Helper()
	var found string
	var walk func(plans.RecordQueryPlan)
	walk = func(p plans.RecordQueryPlan) {
		if p == nil || found != "" {
			return
		}
		if filter, isFilter := p.(*plans.RecordQueryPredicatesFilterPlan); isFilter {
			if join, isJoin := filter.GetInner().(*plans.RecordQueryNestedLoopJoinPlan); isJoin &&
				join.GetJoinType() == plans.JoinLeftOuter {
				preds := filter.GetPredicates()
				if len(preds) != 1 {
					t.Fatalf("the filter over the LEFT OUTER join has %d predicates, want 1", len(preds))
				}
				found = preds[0].Explain()
				return
			}
		}
		for _, child := range p.GetChildren() {
			walk(child)
		}
	}
	walk(plan)
	return found
}

// TestUnsupportedScalarFunctionIsRejectedByName pins WHICH rejection an
// unsupported scalar function gets.
//
// Java resolves a call's NAME against its function catalogue during
// encapsulation and rejects an absent one outright — "Unsupported operator IF"
// — without ever typing the arguments. Go walked the arguments first, and
// `IF(price > 50, …)` has a bare comparison in scalar position that this
// walker cannot shape. That error returned first, the call resolved to nothing,
// and the name was gone: the query came back with `projection slot 0 has no
// resolved Value`, which names neither the function nor anything the user can
// act on, and is the same sentence an unrelated resolution gap produces.
func TestUnsupportedScalarFunctionIsRejectedByName(t *testing.T) {
	t.Parallel()
	const ddl = `CREATE TABLE item (id BIGINT, price DOUBLE, PRIMARY KEY (id))`
	for _, tc := range []struct {
		name string
		sql  string
		want string
	}{
		{
			name: "IF with an unshapeable argument",
			sql:  `SELECT IF(price > 50, 'expensive', 'cheap') FROM item WHERE id = 1`,
			want: "Unsupported operator IF",
		},
		{
			// Same function, arguments this walker CAN shape. The rejection
			// must not depend on that: it is a fact about the catalogue.
			name: "IF with shapeable arguments",
			sql:  `SELECT IF(price, 1, 2) FROM item WHERE id = 1`,
			want: "Unsupported operator IF",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := PlanPhysicalForTest(tc.sql, ddl, nil)
			if err == nil {
				t.Fatalf("query planned; want rejection %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.want)
			}
		})
	}
}

// TestDerivedTableOrderByKeepsItsOwnSourceAnchor pins WHICH ROW a derived
// table's own ORDER BY key reads.
//
// The ORDER BY key upgrade locates its sort with a full-subtree walk, so an
// ENCLOSING select with no ORDER BY of its own still reached the sort the
// derived table had already built, and rebound its keys against the enclosing
// scope — where the derived table is registered under its outer alias. The key
// `id` then read A.ID#0: the derived table's OUTPUT row, addressed by a sort
// that runs BELOW the projection producing it. Nothing binds A there, so the
// query died at execution with
//
//	exact QOV "A" (RECORD<ID LONG NULL> NOT NULL) has no declared runtime binding
//
// The capture is name-driven, and that is what makes it easy to miss: `g` is
// not an output column of `a`, so it stayed correctly on T while only the key
// COLLIDING with an output name moved. An arm with a single key would have
// passed with the defect fully present — both keys are asserted together.
func TestDerivedTableOrderByKeepsItsOwnSourceAnchor(t *testing.T) {
	t.Parallel()
	const ddl = `CREATE TABLE t (id BIGINT, g BIGINT, PRIMARY KEY (id))
CREATE TABLE t2 (id BIGINT, PRIMARY KEY (id))`
	const innerSort = "InMemorySort([_current.G#1 DESC, _current.ID#0 ASC], Scan(T))"
	for _, tc := range []struct {
		name string
		sql  string
		want string
	}{
		{
			// The control: the same SELECT with nothing wrapped around it.
			// It anchored on T throughout, and it is what says the two arms
			// below record a CAPTURE rather than the ordinary spelling.
			name: "bare, no enclosing select",
			sql:  `SELECT id FROM t ORDER BY g DESC, id ASC LIMIT 4`,
			want: "Limit(4, Project([_current.ID#0], " + innerSort + "))",
		},
		{
			name: "wrapped in a select that has no ORDER BY",
			sql:  `SELECT a.id FROM (SELECT id FROM t ORDER BY g DESC, id ASC LIMIT 4) a`,
			want: "Project([_current.ID#0], Limit(4, Project([_current.ID#0], " + innerSort + ")))",
		},
		{
			// The runtime failure's exact shape: the derived table is a join
			// leg, so the outer row is a merged row and the capture is not
			// even same-shaped with what it claimed to read.
			name: "derived table as a join leg",
			sql: `SELECT a.id, b.id FROM (SELECT id FROM t ORDER BY g DESC, id ASC LIMIT 4) a, t2 b ` +
				`WHERE b.id > a.id`,
			want: "Project([_current.ID#0, _current.ID#1], FlatMap(outer=Limit(4, " +
				"Project([_current.ID#0], " + innerSort + ")), inner=Scan(T2, [<>])))",
		},
		{
			// The other direction, which must NOT be broken by the ownership
			// guard: an enclosing ORDER BY is genuinely about the derived
			// table's output row, so its sort sits ABOVE the projection that
			// produces that row and reads slot 0 of it. Both sorts render on
			// `_current` because each is anchored to its OWN input; what
			// separates them is WHICH plan they sit in, which is why the whole
			// plan string is compared rather than the key alone.
			name: "enclosing ORDER BY still anchors on the derived output",
			sql:  `SELECT a.id FROM (SELECT id FROM t ORDER BY g DESC, id ASC LIMIT 4) a ORDER BY a.id DESC`,
			want: "Project([_current.ID#0], InMemorySort([_current.ID#0 DESC], Limit(4, " +
				"Project([_current.ID#0], " + innerSort + "))))",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := explainWithOptions(t, tc.sql, ddl, nil); got != tc.want {
				t.Errorf("plan = %q,\nwant %q", got, tc.want)
			}
		})
	}
}

// TestNestedOuterJoinBoxCrossesItsProducer pins that a column read survives the
// lineage crossing out of a NESTED outer-join box.
//
// A chained outer join binds its outer leg under a box alias (LB$BOX names the
// LA⋈LB pair), and the enclosing join's result program addresses that leg's
// columns through it. Carrying a read across that boundary needs the
// already-resolved ordinal path as proof, and the ordinal proof was gated on
// BYTE-equal root types — which an outer join guarantees will not hold: its
// output columns are pulled up with nullability (Java's
// Quantifier.pullUpResultColumnsWithNullability(true)), so the producer's leg
// root is nullable while the lineage crossing mints the same leg from the
// child's own non-nullable carrier. With the ordinal proof discarded,
// ownership-by-NAME was the only evidence left, and it is ambiguous the moment
// the box holds two same-named columns — which `FULL OUTER JOIN` of two tables
// that both have a K is. The crossing declined, and the projection reached the
// executor still rooted on a box nothing binds:
//
//	exact QOV "LB$BOX" (…) has no declared runtime binding
//
// The ordinals are the assertion: the box row is
// [AID, K, ARR, BID, K, CID, CV, X], so LA.K is #1 and LB.K is #4. Two
// same-named columns are told apart by nothing else, which is the same reason
// the name evidence could not settle it.
func TestNestedOuterJoinBoxCrossesItsProducer(t *testing.T) {
	t.Parallel()
	const ddl = `CREATE TABLE la (aid BIGINT, k BIGINT, arr INTEGER ARRAY, PRIMARY KEY (aid))
CREATE TABLE lb (bid BIGINT, k BIGINT, PRIMARY KEY (bid))
CREATE TABLE cc (cid BIGINT, cv BIGINT, PRIMARY KEY (cid))`
	for _, tc := range []struct {
		name string
		sql  string
		want string
	}{
		{
			// THE reproducer: three legs, so the FlatMap's outer is a join
			// whose own outer leg is another join — the nested box.
			name: "nested box under a lateral unnest",
			sql: `SELECT la.k, lb.k, cc.cv, x FROM la FULL OUTER JOIN lb ON la.aid = lb.bid ` +
				`FULL OUTER JOIN cc ON la.aid = cc.cid, la.arr AS x WHERE la.k = 100`,
			want: "Project([_current.K#1, _current.K#4, _current.CV#6, _current.X#7], " +
				"FlatMap(outer=PredicatesFilter(NestedLoopJoin(FULL OUTER, [1 preds], " +
				"NestedLoopJoin(FULL OUTER, [1 preds], Scan(LA), Scan(LB)), Scan(CC)), [1 preds]), " +
				"inner=Explode(field)))",
		},
		{
			// The DEPTH control, and it is what makes the arm above a
			// statement about nesting rather than about outer joins or
			// duplicate names. Two legs put both K columns directly under
			// their own aliases (LA, LB), so ownership names one slot each
			// and the crossing never needed the ordinal proof. It planned
			// correctly throughout.
			name: "one box, no nesting",
			sql:  `SELECT la.k, lb.k FROM la FULL OUTER JOIN lb ON la.aid = lb.bid, la.arr AS x`,
			want: "Project([_current.K#1, _current.K#4], " +
				"FlatMap(outer=NestedLoopJoin(FULL OUTER, [1 preds], Scan(LA), Scan(LB)), " +
				"inner=Explode(field)))",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := explainWithOptions(t, tc.sql, ddl, nil); got != tc.want {
				t.Errorf("plan = %q,\nwant %q", got, tc.want)
			}
		})
	}
}

// TestOuterJoinPadsItsNullSupplyingLegsColumns pins WHERE the join algebra's
// nullability lands.
//
// An outer join pads its null-supplying side, so that side's columns are
// nullable in the join's output whatever the catalog declares — Java's
// pullUpResultColumnsWithNullability. Three layers derive that row
// independently: the derived-table scope, the logical join's result type, and
// the physical seed. Only the first two said so; the SELECT scope handed the
// projection a read carrying the BASE TABLE's type, so the projection's row
// disagreed with its own input, and reading the same block as a derived table
// reported the disagreement as an unplannable query:
//
//	projection declaration and target differ beyond top-level field names:
//	declared …DTAGS:ARRAY<LONG>?…, target …DTAGS:ARRAY<LONG>…
//
// Only an ARRAY column can witness this: the DDL refuses NOT NULL on a scalar
// column (`0A000: NOT NULL is only allowed for ARRAY column type`) and every
// scalar arrives from the catalog already nullable, so a scalar is nullable
// before and after. BOTH legs carry one, because the two directions need
// separate witnesses — a fix that widens every leg satisfies the padded side
// and destroys the preserved side.
func TestOuterJoinPadsItsNullSupplyingLegsColumns(t *testing.T) {
	t.Parallel()
	const ddl = `CREATE TABLE emp (eid BIGINT, etags BIGINT ARRAY NOT NULL, PRIMARY KEY (eid))
CREATE TABLE dept (did BIGINT, dtags BIGINT ARRAY NOT NULL, PRIMARY KEY (did))`
	const body = `SELECT a.eid, a.etags, b.did, b.dtags FROM emp AS a LEFT JOIN dept AS b ON b.did = a.eid`
	for _, tc := range []struct {
		name string
		sql  string
		want string
	}{
		{
			// The join's own row: the padded leg's ARRAY goes nullable, the
			// preserved leg's does not.
			name: "left join, one leg padded",
			sql:  body,
			want: "RECORD(EID:LONG?,ETAGS:ARRAY<LONG>,DID:LONG?,DTAGS:ARRAY<LONG>?)",
		},
		{
			// FULL pads BOTH, which is the arm that fails if the derivation
			// is written as "the right leg of an outer join".
			name: "full join, both legs padded",
			sql:  `SELECT a.eid, a.etags, b.did, b.dtags FROM emp AS a FULL OUTER JOIN dept AS b ON b.did = a.eid`,
			want: "RECORD(EID:LONG?,ETAGS:ARRAY<LONG>?,DID:LONG?,DTAGS:ARRAY<LONG>?)",
		},
		{
			// RIGHT pads everything to its LEFT — the direction that makes
			// the verdict final only after the last join clause is read.
			name: "right join pads the accumulated left",
			sql:  `SELECT a.eid, a.etags, b.did, b.dtags FROM emp AS a RIGHT JOIN dept AS b ON b.did = a.eid`,
			want: "RECORD(EID:LONG?,ETAGS:ARRAY<LONG>?,DID:LONG?,DTAGS:ARRAY<LONG>)",
		},
		{
			// The CONTROL: an inner join pads nothing, so both stay NOT NULL.
			// Without it the arms above read as "arrays are nullable".
			name: "inner join pads nothing",
			sql:  `SELECT a.eid, a.etags, b.did, b.dtags FROM emp AS a JOIN dept AS b ON b.did = a.eid`,
			want: "RECORD(EID:LONG?,ETAGS:ARRAY<LONG>,DID:LONG?,DTAGS:ARRAY<LONG>)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan, err := PlanPhysicalForTest(tc.sql, ddl, nil)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			if got := values.DescribeType(plan.GetResultValue().Type()); got != tc.want {
				t.Errorf("row = %s,\nwant %s", got, tc.want)
			}
		})
	}

	// The consumer that made this reachable: the same block read as a DERIVED
	// TABLE. Its scope derives the row independently, and the two derivations
	// disagreeing is what turned a planning question into a refused query.
	t.Run("the same body as a derived table plans", func(t *testing.T) {
		t.Parallel()
		for _, sql := range []string{
			`SELECT d.eid FROM (` + body + `) AS d WHERE d.dtags IS NULL ORDER BY d.eid`,
			`SELECT d.eid FROM (` + body + `) AS d WHERE d.dtags IS NOT NULL ORDER BY d.eid`,
			`SELECT d.eid FROM (` + body + `) AS d WHERE d.etags IS NULL`,
		} {
			if _, err := PlanPhysicalForTest(sql, ddl, nil); err != nil {
				t.Errorf("%s\n  %v", sql, err)
			}
		}
	})
}

// TestStructUnnestElementProjectsAsOneColumn pins that projecting a lateral
// unnest's element WHOLE is one column, for a STRUCT element as much as for a
// scalar one.
//
// Two rules disagreed about the same SQL and both decided it on the element's
// TYPE. The projection-row derivation refused a lone record-typed bare QOV as a
// "one-slot whole-row wrap" — true of a machinery row, false of `x`, which the
// user named. And the projection-elimination rule, matching the same
// record-typed shape from the other side, treated the projection as an identity
// over its inner and yielded the 4-column merged row into a reference declaring
// one column. The scalar twin planned throughout, because a scalar element made
// the projected QOV scalar and neither rule looked at it.
//
// Both are now decided by IDENTITY: `x` is a named source, so projecting it is
// a column of that source. The `SELECT *` arm is the other end — it builds no
// projection at all, which is what says the element is not being flattened.
func TestStructUnnestElementProjectsAsOneColumn(t *testing.T) {
	t.Parallel()
	const ddl = `CREATE TYPE AS STRUCT sitem (sku STRING, qty BIGINT)
CREATE TABLE ts (id BIGINT, items sitem ARRAY, name STRING, PRIMARY KEY (id))`
	for _, tc := range []struct {
		name string
		sql  string
		want string
	}{
		{
			// THE reproducer: the element alone, so the projection list is a
			// lone record-typed bare QOV.
			name: "the struct element alone",
			sql:  `SELECT "X" FROM TS, TS."ITEMS" AS "X"`,
			want: "Project([_current.X#3], FlatMap(outer=Scan(TS), inner=Explode(field)))",
		},
		{
			// The VACUITY GUARD for the projection-row derivation: with a
			// second column the list is no longer one slot, so neither rule
			// could reach it, and it planned throughout. The element still
			// lands at merged ordinal 3 — [ID, ITEMS, NAME, X] — which is what
			// says the two arms read the same slot.
			name: "the element beside another column",
			sql:  `SELECT "ID", "X" FROM TS, TS."ITEMS" AS "X"`,
			want: "Project([_current.ID#0, _current.X#3], FlatMap(outer=Scan(TS), inner=Explode(field)))",
		},
		{
			// SELECT * builds no projection, so the element stays ONE column
			// of the merged row rather than being flattened into SKU/QTY.
			name: "select star keeps the element whole",
			sql:  `SELECT * FROM TS, TS."ITEMS" AS "X"`,
			want: "FlatMap(outer=Scan(TS), inner=Explode(field))",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := explainWithOptions(t, tc.sql, ddl, nil); got != tc.want {
				t.Errorf("plan = %q,\nwant %q", got, tc.want)
			}
		})
	}
}
