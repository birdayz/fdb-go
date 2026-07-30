package sqldriver_test

// The structural precondition of an internal error that was sighted ONCE and
// never reproduced.
//
// The sighting, during a full-suite run with leg-identity census recording on:
//
//	ordinal join build: result value contains baked ordinal references but is a
//	*values.FieldValue, want *RecordConstructorValue (seed or folded projection
//	RC) — planner bug
//
// That error comes from executor.newOrdinalJoinBuild, which every join cursor
// calls at construction. Its condition is exactly two facts about the join
// PLAN's result value: it contains a FrontierPinned baked ordinal
// (values.ContainsBakedOrdinal) and it is not a *RecordConstructorValue. The
// guard is deliberately loud — an ordinal-build join whose result value is not
// an RC must die at construction rather than be silently demoted to a non-build
// cursor, which would read leg slots off a row that has no leg windows.
//
// Pinning the ROW ANSWER of the one query that was observed failing is not
// enough, and the repo rule for a negative result says so: what has to be pinned
// is the thing that makes the observed error unreachable. So this pins the
// guard's precondition directly, over a whole shape class rather than one query —
// every join plan the planner emits for a comma-join / projected-EXISTS battery
// must satisfy `ContainsBakedOrdinal ⟹ is an RC`.
//
// Why that is the right invariant and not a restatement of the guard: the guard
// runs at EXECUTION, only for the plan that wins and only for queries someone
// runs. This runs at PLANNING, over every join node of every plan in the battery,
// including ones no row test exercises.
//
// WHAT IT FOUND: the invariant is VIOLATED today, and the violating shape is the
// sighted one — a 3-way comma join with a projected EXISTS whose legs are tied by
// equijoin predicates. The plan is a chain of correlated FlatMaps and the MIDDLE
// one (outer = the A-B merge select, inner = the third leg) carries a bare baked
// FieldValue where the merged row belongs. Executing either arm fails
// deterministically with the sighted error; the FDB reproducer is
// TestFDB_CommaJoin3ProjectedExistsWithEquijoins. The defect is PRE-EXISTING,
// verified by running both reproducers unchanged at this branch's base commit.
//
// It is not fixed here. Correcting which result value that rule arm hands its
// FlatMap is a Cascades planner change — it decides which row the parent reads —
// and this change is a leg-identity retyping. So the violating shapes are
// enumerated as DEBT below, in the same shape as pkg/docscheck's field-decision
// debt list: the gate fails on any NEW violating shape, and equally on a STALE
// entry, so the list can only shrink and a fix cannot land unnoticed.
//
// Planning-only, so it needs no FDB / Docker.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/core/embedded"
)

// knownNonRCJoinResultValueDebt names the shapes whose planned join tree violates
// the invariant TODAY, with the reason. An entry is a LIVE DEFECT, not an
// exemption: executing these shapes fails outright.
//
// The list ratchets in BOTH directions. A new violating shape fails the gate, and
// so does an entry that no longer violates — a fix must delete its line, so the
// fix cannot land silently and the list cannot rot into a permanent allowlist.
var knownNonRCJoinResultValueDebt = map[string]string{
	"comma3_projected_exists_eq_preds": "the correlated-FlatMap arm for a 3-way comma join whose legs " +
		"are tied by equijoins hands the MIDDLE FlatMap (outer = the A-B merge select, inner = the " +
		"third leg) a bare baked FieldValue instead of the merged row. Pre-existing. Executing it " +
		"fails; see TestFDB_CommaJoin3ProjectedExistsWithEquijoins. Which result value that arm " +
		"passes is a Cascades planner decision about which row the parent reads",
	"comma3_projected_exists_1col_eq_preds": "the same defect with a single-column projection, the " +
		"arity for which a bare (non-RC) result value is representable at the top as well",
}

// joinResultValue is one join node's result value plus the plan kind carrying it,
// so a failure message can name the node.
type joinResultValue struct {
	rv   values.Value
	kind string
}

// collectJoinResultValues gathers every join node of a plan tree. Both join
// flavours reach newOrdinalJoinBuild — the materialized nested-loop join through
// newNLJCursor and the correlated FlatMap through newFlatMapCursor — so both are
// in scope.
func collectJoinResultValues(p plans.RecordQueryPlan, out []joinResultValue) []joinResultValue {
	if p == nil {
		return out
	}
	switch t := p.(type) {
	case *plans.RecordQueryNestedLoopJoinPlan:
		out = append(out, joinResultValue{t.GetResultValue(), "RecordQueryNestedLoopJoinPlan"})
	case *plans.RecordQueryFlatMapPlan:
		out = append(out, joinResultValue{t.GetResultValue(), "RecordQueryFlatMapPlan"})
	}
	for _, c := range p.GetChildren() {
		out = collectJoinResultValues(c, out)
	}
	return out
}

func TestJoinResultValueWithBakedOrdinalsIsAlwaysAnRC(t *testing.T) {
	t.Parallel()
	md := existsGatherSchemaMetadata(t)
	violated := map[string]bool{}

	// The battery centres on the sighted family — a comma join with a PROJECTED
	// EXISTS — and varies the axes that could plausibly collapse a result value to
	// a bare value rather than an RC:
	//
	//   - projection ARITY, one column being the only arity for which a bare
	//     FieldValue is representable at the top (the sighting reported a
	//     *FieldValue);
	//   - join arity, 2-way and 3-way, because the 3-quantifier existential path is
	//     a different rule arm that can pass a select's own result value through
	//     verbatim instead of rebuilding a seed;
	//   - whether the legs are tied by EQUIJOIN predicates, which is what routes
	//     the join to the correlated FlatMap arm rather than a materialized NLJ,
	//     and — measured — is exactly the axis separating the violating shapes from
	//     the clean ones;
	//   - whether the EXISTS is PROJECTED or a WHERE filter, since only the
	//     projected form folds the existential into the result value.
	for _, tc := range []struct{ name, sql string }{
		{"comma2_projected_exists_1col", `SELECT EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") FROM A, B`},
		{"comma2_projected_exists_2col", `SELECT A."K", EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") FROM A, B`},
		{"comma3_projected_exists_1col", `SELECT EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") FROM A, B, EEV`},
		{"comma3_projected_exists_2col", `SELECT A."K", EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") FROM A, B, EEV`},
		{"comma3_projected_exists_eq_preds", `SELECT A."K", EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") FROM A, B, EEV WHERE A."AID" = B."BID" AND B."BID" = EEV."VK"`},
		{"comma3_projected_exists_1col_eq_preds", `SELECT EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") FROM A, B, EEV WHERE A."AID" = B."BID" AND B."BID" = EEV."VK"`},
		{"comma2_where_exists_1col", `SELECT A."K" FROM A, B WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`},
		{"comma3_where_exists_1col", `SELECT A."K" FROM A, B, EEV WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`},
		{"comma2_single_leg_col", `SELECT B."K" FROM A, B`},
		{"comma3_single_leg_col", `SELECT EEV."VK" FROM A, B, EEV`},
		{"comma2_constant_projection", `SELECT 1 FROM A, B`},
		{"comma3_count_star", `SELECT COUNT(*) FROM A, B, EEV`},
		{"on_join3_projected_exists", `SELECT A."K", EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") FROM A JOIN B ON B."BID" = A."AID" JOIN EEV ON EEV."VK" = A."AID"`},
		{"on_join3_projected_exists_1col", `SELECT EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") FROM A JOIN B ON B."BID" = A."AID" JOIN EEV ON EEV."VK" = A."AID"`},
		{"leftbox_projected_exists", `SELECT A."K", EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") FROM A LEFT JOIN B ON A."AID" = B."BID"`},
		{"leftbox_projected_exists_1col", `SELECT EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") FROM A LEFT JOIN B ON A."AID" = B."BID"`},
	} {
		plan, err := embedded.PlanRecordQueryWithMetadata(tc.sql, md, nil)
		if err != nil {
			// A shape that legitimately declines to plan contributes nothing, and this
			// test is not the place that decides which shapes must plan —
			// TestJoinUnnestExistsPlanSmoke and TestBoxJoinMultiExistsPlanSweep are.
			t.Logf("%s did not plan (%v) — no join nodes contributed", tc.name, err)
			continue
		}
		joins := collectJoinResultValues(plan, nil)
		if len(joins) == 0 {
			t.Errorf("%s planned with NO join node — the shape no longer exercises a join "+
				"plan, so its place in this battery is vacuous. Either the query changed "+
				"meaning or the planner stopped producing a join for it; find out which "+
				"before deleting the case.\n  sql: %s", tc.name, tc.sql)
			continue
		}
		for _, j := range joins {
			if j.rv == nil || !values.ContainsBakedOrdinal(j.rv) {
				continue
			}
			if _, isRC := j.rv.(*values.RecordConstructorValue); isRC {
				continue
			}
			violated[tc.name] = true
			if _, known := knownNonRCJoinResultValueDebt[tc.name]; known {
				continue
			}
			t.Errorf("%s: %s result value contains baked ordinal references but is a %T, "+
				"want *values.RecordConstructorValue.\n"+
				"This is the exact precondition of executor.newOrdinalJoinBuild's loud "+
				"\"planner bug\" error: the cursor refuses to construct, so the query fails "+
				"outright at execution. Every shape the planner may legitimately emit for an "+
				"ordinal-build join is an RC — the leg-concat seed, or the wrapper-merge-folded "+
				"projection RC. A bare baked value carries leg-relative ordinals with no leg "+
				"windows to resolve them against.\n  sql: %s\n  result value: %s",
				tc.name, j.kind, j.rv, tc.sql, j.rv.Name())
		}
	}

	// A debt entry that no longer violates must be DELETED, not left standing. An
	// entry nobody has to remove is how a defect list becomes a permanent
	// allowlist, and a fix that lands without deleting its line leaves the next
	// reader believing the shape is still broken.
	for name, reason := range knownNonRCJoinResultValueDebt {
		if !violated[name] {
			t.Errorf("debt entry %q no longer violates the invariant — DELETE its line, and "+
				"the matching arm of TestFDB_CommaJoin3ProjectedExistsWithEquijoins.\n"+
				"  recorded reason: %s\n"+
				"If you fixed it, this is how the fix gets recognized. If the shape stopped "+
				"planning or stopped producing a join, the case above is vacuous and needs a "+
				"replacement that still reaches a join.", name, reason)
		}
	}
}
