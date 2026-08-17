package cascades

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// narrowedDistinctFrom returns the physical distinct the rule yielded, or nil if
// the DISTINCT was elided instead.
func narrowedDistinctFrom(results []expressions.RelationalExpression) *plans.RecordQueryDistinctPlan {
	for _, result := range results {
		if distinct, ok := result.(*plans.RecordQueryDistinctPlan); ok {
			return distinct
		}
	}
	return nil
}

// fireFilteredDistinctForR3 states the filtered projection with exact row
// types on both the logical and physical members of its projection group. The
// older shared helper used an untyped scan as a wildcard implementation; an
// unresolved row can no longer cross an RFC-232 constructor boundary.
func fireFilteredDistinctForR3(
	t *testing.T,
	projected []string,
	preds []predicates.QueryPredicate,
) (retained bool, stampedBy string, fired bool) {
	t.Helper()
	rowType := distinctScanType("T")
	scan := mustDistinctConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"T"}, rowType))
	scanQ := expressions.NamedForEachQuantifier(
		distinctReadAlias("T"), expressions.InitialOf(scan))
	filter := mustDistinctConstruct(expressions.NewLogicalFilterExpression(preds, scanQ))
	filterQ := expressions.NamedForEachQuantifier(
		distinctReadAlias("T"), expressions.InitialOf(filter))
	cols := make([]values.Value, len(projected))
	for i, column := range projected {
		cols[i] = distinctRead("T", column)
	}
	projection := mustDistinctConstruct(expressions.NewLogicalProjectionExpression(cols, filterQ))
	projectionRef := expressions.InitialOf(projection)
	projectionRef.Insert(makeFakePlanWrapperForType(
		"T", projection.GetResultValue().Type(), false))
	distinct := mustDistinctConstruct(expressions.NewLogicalDistinctExpression(
		expressions.ForEachQuantifier(projectionRef)))
	ctx := &indexTestPlanContext{
		candidates:      secondaryUniqueTestCandidates(),
		readableIndexes: AllIndexesReadable(),
	}
	results := mustFireImplementationRuleWithContext(t,
		NewImplementDistinctFinalRule(), expressions.InitialOf(distinct), ctx, nil)
	for _, result := range results {
		fired = true
		if _, ok := result.(*plans.RecordQueryDistinctPlan); ok {
			retained = true
			continue
		}
		if stamped, ok := result.(plans.DistinctProofStamped); ok {
			if name := stamped.GetDistinctProofIndexName(); name != "" {
				stampedBy = name
			}
		}
	}
	return retained, stampedBy, fired
}

// TestDistinctFinal_R3NarrowsWhereItCannotElide is R3's central planner pin.
//
// A UNIQUE index on a NULLABLE column proves nothing in general and nothing here
// — no predicate has cleared the stream of NULLs, so the exempt set may be
// non-empty and the operator must stay. R3's move is that staying is not the end
// of it: the index still guarantees at most one row per NON-exempt key value, so
// the operator is NARROWED to dedup exactly the exempt subset and every other
// row passes through retaining nothing.
//
// This is the shape a user gets from `SELECT DISTINCT email FROM users` on an
// ordinary SQL table — the query R1 could never help and R2 only helps once a
// WHERE clause is written.
func TestDistinctFinal_R3NarrowsWhereItCannotElide(t *testing.T) {
	t.Parallel()

	ctx := &indexTestPlanContext{
		candidates:      secondaryUniqueTestCandidates(),
		readableIndexes: AllIndexesReadable(),
	}
	results := fireDistinctOverT(t, ctx, "NULLABLE_EMAIL")
	if len(results) == 0 {
		t.Fatal("rule did not fire at all")
	}
	distinct := narrowedDistinctFrom(results)
	if distinct == nil {
		t.Fatal("DISTINCT over a NULLABLE unique index was ELIDED with nothing " +
			"rejecting NULL. The index holds (NULL),(NULL),(NULL) legitimately and " +
			"the operator is what collapses them")
	}
	if !distinct.IsNarrowedDedup() {
		t.Fatal("the retained DISTINCT was NOT narrowed. A qualifying unique index " +
			"covers the projection, so every NON-exempt row is provably unique " +
			"already and must pass through without entering the seen-set — the " +
			"narrowed set is a subset of the full one on every input, so there is no " +
			"input on which declining this is right")
	}
	if got := distinct.GetDistinctProofIndexName(); got != "T$nullable_email_unique" {
		t.Fatalf("the narrowed distinct names %q as its proving index, want "+
			"\"T$nullable_email_unique\". R3 reads as unconditional because it "+
			"removes no operator, and that reading is wrong: withdraw the index's "+
			"uniqueness and a non-exempt row can duplicate another non-exempt row, "+
			"which is exactly what the narrowed seen-set no longer catches. The "+
			"dependency is recorded for the same reason a full elision records it",
			got)
	}

	// EXPLAIN must say so. The narrowing changes WHICH ROWS THE OPERATOR
	// RETAINS, so an acceptance criterion has to be able to assert it fired; a
	// narrowed distinct that silently degraded to a full one is otherwise
	// indistinguishable from one that never narrowed.
	explained := distinct.Explain()
	if !strings.Contains(explained, "narrowed-by:T$nullable_email_unique") {
		t.Fatalf("EXPLAIN renders %q, which names no narrowing. The optimization's "+
			"evidence would otherwise be invisible", explained)
	}
}

// TestDistinctFinal_R3DoesNotNarrowWithoutAQualifyingIndex is the negative half,
// and without it the assertion above is satisfiable by narrowing everything.
// Narrowing a distinct with no unique index behind it drops rows: nothing
// guarantees the non-exempt values are unique, and they would sail past the
// seen-set.
func TestDistinctFinal_R3DoesNotNarrowWithoutAQualifyingIndex(t *testing.T) {
	t.Parallel()

	ctx := &indexTestPlanContext{
		candidates:      secondaryUniqueTestCandidates(),
		readableIndexes: AllIndexesReadable(),
	}
	// TAGS is refused by the base-record-cardinality clause: UNIQUE on a fan-out
	// index constrains index ENTRIES, and a record with an empty repeated field
	// produces no entry at all. Nothing about base-row uniqueness follows, so
	// there is no residual to narrow to either.
	for _, column := range []string{"TAGS", "SCORE", "SPARSE_EMAIL"} {
		distinct := narrowedDistinctFrom(fireDistinctOverT(t, ctx, column))
		if distinct == nil {
			t.Fatalf("%s: DISTINCT was elided; the fixture arm is not observing R3", column)
		}
		if distinct.IsNarrowedDedup() {
			t.Fatalf("%s: the DISTINCT was NARROWED over an index that fails clauses "+
				"1-7. R3 rests on the index's uniqueness over base rows exactly as a "+
				"full elision does — where that does not hold, narrowing DROPS ROWS "+
				"that the full seen-set would have caught", column)
		}
		if got := distinct.GetDistinctProofIndexName(); got != "" {
			t.Fatalf("%s: an un-narrowed distinct recorded a dependency on %q", column, got)
		}
	}
}

// TestDistinctFinal_R3YieldsToFullElision pins the strength order. R2 discharges
// the obligation outright on a NULL-rejecting stream, and where it does the
// operator must be REMOVED rather than narrowed — narrowing a provably redundant
// operator is a slower correct answer, and it would also record the dependency
// on a plan shape the criteria assert has no operator at all.
func TestDistinctFinal_R3YieldsToFullElision(t *testing.T) {
	t.Parallel()

	retained, stampedBy, fired := fireFilteredDistinctForR3(t,
		[]string{"NULLABLE_EMAIL"},
		[]predicates.QueryPredicate{predicates.NewComparisonPredicate(
			distinctRead("T", "NULLABLE_EMAIL"),
			predicates.Comparison{Type: predicates.ComparisonIsNotNull})})
	if !fired {
		t.Fatal("rule did not fire")
	}
	if retained {
		t.Fatal("the operator was NARROWED on a stream R2 proved clear of NULLs. " +
			"The exempt set is empty here, so the residual dedup has nothing to " +
			"dedup and the elision is available — strength order is R1, R2, then R3")
	}
	if stampedBy != "T$nullable_email_unique" {
		t.Fatalf("full elision stamped %q, want \"T$nullable_email_unique\"", stampedBy)
	}
}

// TestDistinctFinal_R3NarrowingIsPlanIdentity is a wrong-rows pin rather than a
// plan-quality one, and it is the reason the flag is in structuralKey.
//
// A narrowed operator's seen-set holds ONLY the exempt keys; a full one holds
// every key. If the memo collapsed the two, a re-plan could flip narrowed↔full
// while a DistinctHashContinuation was being resumed — and a full operator
// rebuilding its set from a narrowed serialization believes it has already seen
// every key the narrowing omitted, so it DROPS rows it must emit.
func TestDistinctFinal_R3NarrowingIsPlanIdentity(t *testing.T) {
	t.Parallel()

	full := mustDistinctConstruct(plans.NewRecordQueryDistinctPlan(makeFakePlanWrapper("T")))
	narrowed := full.WithNarrowedDedup("T$nullable_email_unique", []int{0})

	if full.EqualsPlanWithoutChildren(narrowed) {
		t.Fatal("a narrowed distinct and a full one compare EQUAL without children, " +
			"so the memo may collapse them into one group member. The survivor could " +
			"then be either, and a continuation minted under one discipline would be " +
			"resumed under the other — which drops rows, not merely plan quality")
	}
	if full.HashCodeWithoutChildren() == narrowed.HashCodeWithoutChildren() {
		t.Fatal("a narrowed distinct hashes identically to a full one")
	}

	// The exempt SLOTS are identity too: two narrowings aimed at different key
	// positions retain different key sets, so their continuations are equally
	// incompatible.
	otherSlots := full.WithNarrowedDedup("T$nullable_email_unique", []int{1})
	if narrowed.EqualsPlanWithoutChildren(otherSlots) {
		t.Fatal("two narrowings testing DIFFERENT slot positions compare equal. They " +
			"retain different subsets, so their continuations do not interchange " +
			"either")
	}

	// And the un-narrowed plan must still be renderable and comparable as
	// before: the empty stamp is the overwhelmingly common case.
	if strings.Contains(full.Explain(), "narrowed-by") {
		t.Fatalf("an un-narrowed distinct renders a narrowing: %q", full.Explain())
	}
}
