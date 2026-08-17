package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestDistinctKeyColumns_WrapperOverProjection pins the consumer arm that
// RFC-226's type change flips, and that no corpus in this repo reaches.
//
// WHY IT IS DRIVEN DIRECTLY RATHER THAN THROUGH SQL. distinctKeyColumns
// short-circuits on a DIRECT projection inner (it prefers GetProjections, the
// richer answer), so the arm below is only reachable when a WRAPPER sits between
// the Distinct and the projection.
//
// WHAT IS MEASURED, stated separately from what is inferred, because the two
// were conflated in an earlier revision. MEASURED: the whole sqldriver FDB
// corpus reaches this site 36 times and never with a projection under a wrapper
// — 31 over a FlatMap, and 5 resolved over a Scan, a PredicatesFilter and an
// Index. Note the PredicatesFilter reads LOOK like the wrapper shape and are
// not: they sit over a Scan, so they would refute the claim if read carelessly.
//
// INFERRED, not measured: that the shape is unreachable from SQL in general.
// That rests on the three enumerated forms below, each of which puts the
// projection immediately under the Distinct:
//
//	SELECT DISTINCT a FROM (SELECT a FROM t LIMIT 10) x
//	  -> Distinct(Project(Project(Limit(Scan))))
//	SELECT DISTINCT * FROM (SELECT a FROM t LIMIT 10) x
//	  -> Distinct(Project(Limit(Scan)))
//	SELECT DISTINCT x.a FROM (SELECT a FROM t) x WHERE x.a > 5
//	  -> Distinct(Project(PredicatesFilter(Project(Scan))))
//
// Either way no corpus covers the arm, which is exactly why it gets a unit pin:
// an arm no corpus reaches is an arm whose first real firing would be read as a
// finding rather than as untested code.
//
// WHAT CHANGED. Before RFC-226 a projection answered UnknownType, so a wrapper
// above one forwarded UnknownType, the RecordType assertion below failed and
// this returned nil — DISTINCT stayed on the hash-set. Now the projection states
// its row, the wrapper forwards a real RecordType, and the dedup key is derived
// with resolved ordinals, enabling streaming DISTINCT. This test drives that
// flip so it is a decision on the record rather than a side effect.
//
// THE COLUMNS MUST BE THE PROJECTION'S, NOT THE SCAN'S. That is the assertion
// that would catch the failure mode this whole RFC is about: if the wrapper
// forwarded the scan's row instead, the dedup key would name columns the
// projection does not output, and DISTINCT would dedup on the wrong slots.
func TestDistinctKeyColumns_WrapperOverProjection(t *testing.T) {
	t.Parallel()

	scanRow := values.NewRecordType("", true, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "A", FieldType: values.NotNullLong, Ordinal: 1},
		{Name: "B", FieldType: values.NotNullString, Ordinal: 2},
	})
	scan, err := plans.NewRecordQueryScanPlan([]string{"T"}, scanRow, false)
	scan = mustConstruct(t, scan, err)
	projectedA, err := values.ResolveFieldOrdinals(scan.GetResultValue(), []int{1})
	projectedA = mustConstruct(t, projectedA, err)

	// A projection narrowing 3 columns to 1, renamed. If any consumer reads the
	// SCAN's row instead of the projection's, it sees 3 fields named ID/A/B.
	proj, err := plans.NewRecordQueryProjectionPlanWithAliases(
		[]values.Value{projectedA},
		[]string{"RENAMED"}, scan)
	proj = mustConstruct(t, proj, err)

	// The wrapper. A PredicatesFilter changes no row shape and DOES forward its
	// inner's type, which is what makes it the probe: anything it reports is the
	// projection's answer verbatim. (RecordQueryLimitPlan would NOT work here — it
	// is still a GetResultType stub answering UnknownType, so a Limit above a
	// projection swallows the row the projection now states. That is a real gap,
	// booked as CQ-101, and it is why this pin uses a forwarding wrapper: pinning
	// the flip requires a wrapper that can transmit it.)
	wrapper, err := plans.NewRecordQueryPredicatesFilterPlan(proj, nil)
	wrapper = mustConstruct(t, wrapper, err)

	// Fixture guard: if the wrapper stopped forwarding a stated row, this test
	// would "pass" the nil arm below while proving nothing about the flip.
	wrapperRow, stated := wrapper.GetResultType().(*values.RecordType)
	if !stated {
		t.Fatalf("fixture: the wrapper states %T, not a RecordType.\n"+
			"  Either the wrapper stopped forwarding its inner's type, or the "+
			"projection under it stopped stating a row — in which case RFC-226 regressed "+
			"and this pin is measuring nothing.", wrapper.GetResultType())
	}
	if len(wrapperRow.Fields) != 1 || wrapperRow.Fields[0].Name != "RENAMED" {
		t.Fatalf("fixture: the wrapper forwards %v, want the PROJECTION's 1-field row "+
			"named RENAMED — a 3-field ID/A/B row means the scan's row leaked past the "+
			"projection", wrapperRow)
	}

	cols := distinctKeyColumns(wrapper)
	if len(cols) == 0 {
		t.Fatal("distinctKeyColumns returned no dedup key for a wrapper over a projection.\n" +
			"  That is the PRE-RFC-226 behaviour: the projection answered UnknownType, the " +
			"wrapper forwarded it, and DISTINCT fell back to the hash-set. If this is " +
			"deliberate, say why a stated row is not usable here; if it is a regression, " +
			"the projection stopped stating its row.")
	}
	if len(cols) != 1 {
		t.Fatalf("dedup key has %d columns, want 1 — the key must be the PROJECTION's "+
			"output (1 column), not the scan's row (3). A 3-column key dedups on slots "+
			"the projection does not emit, which is a WRONG-ROW bug, not a slow one: "+
			"got %v", len(cols), cols)
	}
	fv, isField := values.AsFieldValue(cols[0])
	if !isField {
		t.Fatalf("dedup key column is a %T, want an exact FieldValue — "+
			"orderingSatisfiesGroupingKeys compares against the inner ordering's keys in "+
			"that representation, so a non-FieldValue silently never matches and streaming "+
			"is never proved", cols[0])
	}
	if got := values.OutputColumnName(fv, ""); got != "RENAMED" {
		t.Errorf("dedup key column is named %q, want %q — the projection's OUTPUT name "+
			"is the authority, not the underlying column's", got, "RENAMED")
	}
}

// TestTypeUnstated_CatchesTheNonNullableUnknown pins the predicate that replaced
// a pointer-identity test against the UnknownType singleton, and pins the ONE
// value where the two actually differ.
//
// SAYING PRECISELY WHAT THE DIFFERENCE IS, because an earlier version of this
// test asserted a difference that does not exist. It listed "a NULLABLE unknown"
// as a distinct case on the belief that it is a different pointer. It is not:
// values.UnknownType is declared nullable and values.WithNullability returns its
// argument unchanged when the nullability already matches, so
// `WithNullability(UnknownType, true) == UnknownType` — that row was literally
// the row above it, and a four-case table with three cases reads as more
// coverage than it has.
//
// The real discriminator is the NON-nullable unknown: a different pointer that
// still means "nothing to say". No production site mints one today, so
// rule_push_set_operation_through_fetch.go's switch to this predicate is a no-op
// on current inputs — uniformity with the sibling site, not a bug fix. This test
// is what would notice if a producer for that value ever appeared.
func TestTypeUnstated_CatchesTheNonNullableUnknown(t *testing.T) {
	t.Parallel()

	// The premise, asserted rather than assumed: if this ever stops holding, the
	// case table below silently loses its discriminating row.
	if values.WithNullability(values.UnknownType, true) != values.UnknownType {
		t.Fatal("WithNullability(UnknownType, true) is no longer the UnknownType " +
			"singleton. The nullable-unknown case is now DISTINCT from the singleton and " +
			"belongs back in the table below — and the comment in " +
			"rule_push_set_operation_through_fetch.go, which says this edit is a no-op on " +
			"today's inputs, stops being true.")
	}

	for _, tc := range []struct {
		name string
		typ  values.Type
		want bool
	}{
		{"nil", nil, true},
		{"the UnknownType singleton", values.UnknownType, true},
		{
			"a NON-NULLABLE unknown (the one pointer identity misses)",
			values.WithNullability(values.UnknownType, false), true,
		},
		{"a real type", values.NotNullLong, false},
	} {
		if got := typeUnstated(tc.typ); got != tc.want {
			t.Errorf("typeUnstated(%s) = %v, want %v.\n"+
				"  An unstated type that reads as STATED gets threaded through as a real "+
				"type; a stated one that reads as UNSTATED gets silently replaced by a "+
				"leg's type. Both are wrong-row risks, which is why this is a predicate "+
				"and not a pointer comparison.", tc.name, got, tc.want)
		}
	}
}
