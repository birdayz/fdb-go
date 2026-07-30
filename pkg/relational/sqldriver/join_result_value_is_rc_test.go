package sqldriver_test

// PLAN-LEVEL structural gates on the result values the planner hands its join
// nodes, over a whole shape class rather than one query.
//
// The history matters for reading them. This file used to assert
// `ContainsBakedOrdinal ⟹ the result value is a RecordConstructorValue`, derived
// from a construction-time refusal in executor.newOrdinalJoinBuild. That
// invariant was FALSE, and the refusal that motivated it was the bug: Java
// imposes no RC requirement anywhere — ImplementNestedLoopJoinRule.java:187,201,
// 214 hand selectExpression.getResultValue() to RecordQueryFlatMapPlan verbatim
// in all three arms, and PartitionSelectRule.java:281,319 legitimately mints a
// BARE baked result value (a single-live-lower select flows one leg's whole row,
// and a later positional-merge round translates that bare QOV into
// `ofOrdinal(QOV(merge), i)`). The executor now builds that shape; the two gates
// below are what is actually true, and each is red without its fix.
//
// Planning-only, so they need no FDB / Docker.

import (
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/core/embedded"
)

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

// joinShapeBattery centres on the family that surfaced both defects — a comma
// join with a PROJECTED EXISTS — and varies the axes that decide which
// PartitionSelectRule arm builds the intermediate selects:
//
//   - projection ARITY, one column being the only arity for which a bare
//     (non-RC) result value is representable at the top;
//   - join arity, 2-way and 3-way, because only the 3-quantifier path partitions
//     twice and so reaches the merge-of-a-flowed-row composition;
//   - whether the legs are tied by EQUIJOIN predicates, which is what routes the
//     join to the correlated FlatMap arm rather than a materialized NLJ, and —
//     measured — is exactly the axis that separated the broken shapes from the
//     working ones;
//   - whether the EXISTS is PROJECTED or a WHERE filter, since only the projected
//     form folds the existential into the result value.
func joinShapeBattery() []struct{ name, sql string } {
	return []struct{ name, sql string }{
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
		{"comma3_eq_preds_plain", `SELECT A."K", B."K", EEV."VK" FROM A, B, EEV WHERE A."AID" = B."BID" AND B."BID" = EEV."VK"`},
		{"comma3_eq_preds_one_leg", `SELECT A."K" FROM A, B, EEV WHERE A."AID" = B."BID" AND B."BID" = EEV."VK"`},
		{"on_join3_projected_exists", `SELECT A."K", EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") FROM A JOIN B ON B."BID" = A."AID" JOIN EEV ON EEV."VK" = A."AID"`},
		{"on_join3_projected_exists_1col", `SELECT EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") FROM A JOIN B ON B."BID" = A."AID" JOIN EEV ON EEV."VK" = A."AID"`},
		{"leftbox_projected_exists", `SELECT A."K", EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") FROM A LEFT JOIN B ON A."AID" = B."BID"`},
		{"leftbox_projected_exists_1col", `SELECT EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") FROM A LEFT JOIN B ON A."AID" = B."BID"`},
	}
}

// forEachBatteryJoin plans every battery shape and calls visit for each of its
// join nodes. A shape that plans with NO join node is a failure: its place in the
// battery is then vacuous.
func forEachBatteryJoin(t *testing.T, visit func(name, sql string, j joinResultValue)) {
	t.Helper()
	md := existsGatherSchemaMetadata(t)
	for _, tc := range joinShapeBattery() {
		plan, err := embedded.PlanRecordQueryWithMetadata(tc.sql, md, nil)
		if err != nil {
			// A shape that legitimately declines to plan contributes nothing, and this
			// is not the place that decides which shapes must plan —
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
			visit(tc.name, tc.sql, j)
		}
	}
}

// TestPositionalMergeRowSlotsAreTyped is the gate on the defect that returned
// ZERO ROWS WITH NO ERROR, which is the worst failure mode a join can have.
//
// A positional-merge result value (`RC(_i: QOV(leg_i))` — PartitionSelectRule's
// collapse of ≥2 live lowers) describes a row of ROWS, and every slot's type is
// the leg's whole row type. That type is not decoration: it is what lets a
// reference INTO the merge bake to a pinned ordinal. A slot left UNKNOWN leaves
// every reference through it source-relative, and a source-relative operand
// pushed into a leg's scan as an equijoin SARG evaluates to NULL against the
// build-bound row — so the scan matches nothing and the join silently returns no
// rows.
//
// Java never has the problem because the quantifier itself carries the type
// (Quantifier.java:801-803: getFlowedObjectValue() is
// `QuantifiedObjectValue.of(alias, getFlowedObjectType())`). Go's
// positionalMergeCase used to scavenge the types out of the select's own value
// surfaces instead, which finds nothing precisely when the select's result value
// is itself an untyped flowed row — the single-live-lower arm's output, and the
// shape that broke.
//
// Planning-only, so it needs no FDB / Docker.
func TestPositionalMergeRowSlotsAreTyped(t *testing.T) {
	t.Parallel()
	merges := 0
	forEachBatteryJoin(t, func(name, sql string, j joinResultValue) {
		if j.rv == nil || !values.IsPositionalMergeRC(j.rv) {
			return
		}
		rc, isRC := j.rv.(*values.RecordConstructorValue)
		if !isRC {
			return
		}
		merges++
		for i, f := range rc.Fields {
			qov, isQOV := f.Value.(*values.QuantifiedObjectValue)
			if !isQOV {
				t.Errorf("%s: %s positional-merge slot %d (%q) is a %T, want a QuantifiedObjectValue "+
					"— the merge row's slots ARE the legs\n  sql: %s", name, j.kind, i, f.Name, f.Value, sql)
				continue
			}
			if _, typed := qov.Type().(*values.RecordType); !typed {
				t.Errorf("%s: %s positional-merge slot %d (leg %s) flows an UNTYPED row (%v).\n"+
					"An untyped merge slot leaves every reference through it SOURCE-RELATIVE: it "+
					"cannot bake to a pinned ordinal, and a source-relative equijoin operand pushed "+
					"into a leg's scan evaluates to NULL against the build-bound row, so the scan "+
					"matches nothing and the join returns ZERO ROWS WITH NO ERROR.\n"+
					"The leg type comes from the QUANTIFIER (Quantifier.GetFlowedObjectValueTyped, "+
					"Java's Quantifier.java:801-803), never from scavenging the select's value "+
					"surfaces — those are empty exactly when the select's own result value is an "+
					"untyped flowed row.\n  sql: %s", name, j.kind, i, qov.Correlation, qov.Type(), sql)
			}
		}
	})
	// A gate over zero merges proves nothing. The 3-way equijoin shapes are the
	// ones that partition twice and collapse, so the battery must keep reaching
	// the arm this gate polices.
	if merges == 0 {
		t.Error("the battery produced NO positional-merge join result value, so this gate is " +
			"vacuous. PartitionSelectRule's ≥2-live-lowers collapse is what it polices; either a " +
			"3-way equijoin shape stopped planning through that arm, or the merge representation " +
			"changed. Find out which before trusting a green run here.")
	}
}

// TestJoinResultValueWithBakedOrdinalsIsRCOrWholeValueReference gates the shape a
// baked NON-RC join result value may take.
//
// It may be one — the whole result value being a single baked reference, which is
// the select flowing ONE value as its entire output (executor's
// ordinalJoinBuild.Bare evaluates it and flows the row or wraps the scalar).
// Anything else — a baked reference buried inside an arithmetic value, a
// comparison, a nested constructor that is not the row — has no defined output
// row shape at a join node, and would reach the build as a value it cannot turn
// into a row.
//
// Planning-only, so it needs no FDB / Docker.
func TestJoinResultValueWithBakedOrdinalsIsRCOrWholeValueReference(t *testing.T) {
	t.Parallel()
	bares := 0
	forEachBatteryJoin(t, func(name, sql string, j joinResultValue) {
		if j.rv == nil || !values.ContainsBakedOrdinal(j.rv) {
			return
		}
		if _, isRC := j.rv.(*values.RecordConstructorValue); isRC {
			return
		}
		bares++
		fv, isFV := j.rv.(*values.FieldValue)
		if !isFV || fv.Resolved == nil || !fv.Resolved.FrontierPinned {
			t.Errorf("%s: %s result value carries baked ordinals and is not an RC, but is a %T "+
				"(%s) rather than a baked reference.\n"+
				"A non-RC join result value is legitimate only as the WHOLE value the select "+
				"flows — a single baked reference the build evaluates and flows. A baked ordinal "+
				"buried in a computed value has no defined output row shape at a join node.\n"+
				"  sql: %s", name, j.kind, j.rv, j.rv.Name(), sql)
			return
		}
		if _, isQOV := fv.Child.(*values.QuantifiedObjectValue); !isQOV {
			t.Errorf("%s: %s bare baked result value reads through a %T rather than a leg "+
				"QuantifiedObjectValue (%s). A baked ordinal resolves against a LEG's bound row; "+
				"with no leg QOV at the base there is nothing to bind.\n  sql: %s",
				name, j.kind, fv.Child, describeAccessors(fv), sql)
		}
	})
	if bares == 0 {
		t.Error("the battery produced NO bare (non-RC) baked join result value, so this gate is " +
			"vacuous. That shape is what PartitionSelectRule.java:281+319 mints and what " +
			"ordinalJoinBuild.Bare exists for; if the planner stopped emitting it, the Bare arm " +
			"has lost its only planner-side witness and TestFDB_CommaJoin3ProjectedExistsWithEquijoins " +
			"is no longer covering it.")
	}
}

// describeAccessors renders a baked FieldValue's ordinal path for a failure
// message.
func describeAccessors(fv *values.FieldValue) string {
	if fv.Resolved == nil {
		return "lazy"
	}
	ords := make([]int, len(fv.Resolved.Accessors))
	for i, a := range fv.Resolved.Accessors {
		ords[i] = a.Ordinal
	}
	return fmt.Sprintf("ordinals %v", ords)
}
