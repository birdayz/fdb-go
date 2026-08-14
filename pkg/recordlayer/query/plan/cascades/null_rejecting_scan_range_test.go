package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// rangeOf builds the ComparisonRange an index scan carries for one key column.
func rangeOf(t *testing.T, comparisons ...predicates.Comparison) *predicates.ComparisonRange {
	t.Helper()
	cr := predicates.EmptyComparisonRange()
	for i := range comparisons {
		result := cr.Merge(&comparisons[i])
		if !result.Ok {
			t.Fatalf("Merge(%v) refused; the fixture does not build the range under test",
				comparisons[i].Type)
		}
		cr = result.Range
	}
	return cr
}

func unaryComparison(t predicates.ComparisonType) predicates.Comparison {
	return predicates.Comparison{Type: t}
}

// TestNullRejectedByScanRange_PerComparisonKind is R2's scan-range route stated
// one comparison kind at a time.
//
// The two REFUSED kinds are the reason the route needs its own file. IS NULL and
// IS NOT DISTINCT FROM are classified as scan-range EQUALITY types by
// comparison_range.go:196-203 exactly as ordinary Equals is, and the binder gives
// each of them `component.alternatives = []any{nil}` — a probe that seeks the
// NULL entries. A route that read "is an equality range" as "excludes NULL"
// would license a strict-ordering claim over precisely the ties the claim denies.
func TestNullRejectedByScanRange_PerComparisonKind(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		rng      *predicates.ComparisonRange
		rejected bool
	}{
		{
			"equals_non_null_literal",
			rangeOf(t, predicates.NewLiteralComparison(predicates.ComparisonEquals, "a")), true,
		},
		// The recorded hazard's exact shape. It is admitted, and the binder is
		// why: a NULL-valued equality becomes an EMPTY range, never a [null]
		// probe (scan_range_binding.go:321-325, pinned in the executor package).
		{
			"equals_null_literal",
			rangeOf(t, predicates.NewLiteralComparison(predicates.ComparisonEquals, nil)), true,
		},
		{
			"is_not_null",
			rangeOf(t, unaryComparison(predicates.ComparisonIsNotNull)), true,
		},
		{
			"less_than",
			rangeOf(t, predicates.NewLiteralComparison(predicates.ComparisonLessThan, "m")), true,
		},
		{
			"less_than_or_eq",
			rangeOf(t, predicates.NewLiteralComparison(predicates.ComparisonLessThanOrEq, "m")), true,
		},
		{
			"greater_than",
			rangeOf(t, predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, "m")), true,
		},
		{
			"greater_than_eq",
			rangeOf(t, predicates.NewLiteralComparison(predicates.ComparisonGreaterThanEq, "m")), true,
		},
		{
			"two_sided_inequality",
			rangeOf(t,
				predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, "a"),
				predicates.NewLiteralComparison(predicates.ComparisonLessThan, "m")), true,
		},
		{
			"starts_with",
			rangeOf(t, predicates.NewLiteralComparison(predicates.ComparisonStartsWith, "a")), true,
		},

		// THE TRAP, both halves. Each is an equality-typed range that ENCLOSES
		// the NULL boundary.
		{
			"is_null",
			rangeOf(t, unaryComparison(predicates.ComparisonIsNull)), false,
		},
		{
			"not_distinct_from_null",
			rangeOf(t, predicates.NewLiteralComparison(
				predicates.ComparisonNotDistinctFrom, nil)), false,
		},
		{
			"not_distinct_from_value",
			rangeOf(t, predicates.NewLiteralComparison(
				predicates.ComparisonNotDistinctFrom, "a")), false,
		},

		// An unconstrained coordinate states nothing.
		{"empty_range", predicates.EmptyComparisonRange(), false},
		{"nil_range", nil, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := nullRejectedByScanRange([]*predicates.ComparisonRange{tc.rng}, 1)
			if len(got) != 1 {
				t.Fatalf("nullRejectedByScanRange returned %d positions, want 1", len(got))
			}
			if got[0] != tc.rejected {
				t.Fatalf("nullRejectedByScanRange(%s) = %v, want %v",
					tc.name, got[0], tc.rejected)
			}
		})
	}
}

// TestNullRejectedByScanRange_MixedInequalityFailsClosed pins that an inequality
// range is judged over ALL its members. A range is a conjunction, so a member
// this file has not reasoned about can only widen what it encloses; requiring
// every member to be independently NULL-excluding is the direction that stays
// correct when a new member kind appears.
func TestNullRejectedByScanRange_MixedInequalityFailsClosed(t *testing.T) {
	t.Parallel()

	// IS NOT NULL merged with a LESS_THAN is the ordinary two-member tail and is
	// admitted; the refusal case has to be built from a kind the allow-list does
	// not carry, which Merge only accepts on the inequality path.
	admitted := rangeOf(t,
		unaryComparison(predicates.ComparisonIsNotNull),
		predicates.NewLiteralComparison(predicates.ComparisonLessThan, "m"))
	if got := nullRejectedByScanRange([]*predicates.ComparisonRange{admitted}, 1); !got[0] {
		t.Fatalf("IS NOT NULL AND < 'm' must reject NULL, got %v", got[0])
	}

	refused := rangeOf(t,
		predicates.NewLiteralComparison(predicates.ComparisonLessThan, "m"),
		predicates.NewLiteralComparison(predicates.ComparisonIsDistinctFrom, "a"))
	if got := nullRejectedByScanRange([]*predicates.ComparisonRange{refused}, 1); got[0] {
		t.Fatal("an inequality range holding IS DISTINCT FROM — which admits NULL — " +
			"must not be credited with rejecting NULL")
	}
}

// TestNullRejectedByScanRange_PositionalPerKeyColumn pins that coverage is never
// rounded up across key columns. A composite UNIQUE (a, b) with only `a`
// constrained still admits (1, NULL) and (1, NULL).
func TestNullRejectedByScanRange_PositionalPerKeyColumn(t *testing.T) {
	t.Parallel()

	comps := []*predicates.ComparisonRange{
		rangeOf(t, predicates.NewLiteralComparison(predicates.ComparisonEquals, "a")),
		predicates.EmptyComparisonRange(),
	}
	got := nullRejectedByScanRange(comps, 2)
	if len(got) != 2 || !got[0] || got[1] {
		t.Fatalf("nullRejectedByScanRange = %v, want [true false]", got)
	}

	// Fewer scan comparisons than key columns: the uncovered tail stays false.
	short := nullRejectedByScanRange(comps[:1], 2)
	if len(short) != 2 || !short[0] || short[1] {
		t.Fatalf("nullRejectedByScanRange (short comps) = %v, want [true false]", short)
	}
}

func uniqueIndexScanRowType(column string) values.Type {
	return &values.RecordType{
		RecordName: "T",
		Fields: []values.Field{
			{Name: column, FieldType: values.NullableString, Ordinal: 0},
			{Name: "OTHER", FieldType: values.NullableString, Ordinal: 1},
		},
	}
}

// scanRowRead is the filter route's operand: a BARE, top-level field reference
// whose ordinal provably indexes the SCAN's row (RFC-197's boundary rule). A
// reference whose domain the layout does not state contributes nothing, which is
// the fail-closed direction the route depends on.
func scanRowRead(t *testing.T, keyColumn, column string) values.Value {
	t.Helper()
	id, ok := values.OrdinalOfNameIn(uniqueIndexScanRowType(keyColumn), column)
	if !ok {
		t.Fatalf("scan row declares no column %s", column)
	}
	root, rootErr := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("scan_row_"+keyColumn),
		uniqueIndexScanRowType(keyColumn),
	)
	root = mustConstruct(t, root, rootErr)
	resolved, err := values.ResolveFieldOrdinals(root, []int{id.Ordinal})
	return mustConstruct(t, resolved, err)
}

// uniqueIndexScanOn builds a covering unique index scan on one NULLABLE STRING
// key column — the shape every SQL-expressible secondary unique index has, and
// therefore the shape on which R1 is false and only R2 can license the claim.
func uniqueIndexScanOn(
	t testing.TB,
	column string, comps []*predicates.ComparisonRange,
) *plans.RecordQueryIndexPlan {
	t.Helper()
	rowType := uniqueIndexScanRowType(column)
	scan, err := plans.NewRecordQueryIndexPlan("T$"+column+"_unique", comps, []string{"T"}, rowType, false)
	return mustConstruct(t, scan, err).
		WithIndexMetadata([]string{column}, []string{"ID"}, true).
		WithKeyComponentTypes([]values.Type{values.NullableString})
}

// TestStrictlyOrderedIfUnique_ScanRangeRouteLicensesTheClaim is item 4's core
// wiring pin and the reachability half of RFC-210 §5.7.
//
// A UNIQUE index over a NULLABLE column proves nothing in general — under NULLS
// DISTINCT it legitimately holds (NULL,pk=1),(NULL,pk=2),(NULL,pk=3), three
// entries whose claimed sort key is identical, which is the upstream bug Java
// ships at RemoveSortRule.java:153. The claim becomes TRUE, and this function
// must make it, exactly when the scan cannot reach those entries.
//
// The scan-range route rather than the filter route is what fires on the SQL:
// IS NOT NULL is admitted by isSargableComparisonForMatch, so the planner pushes
// it INTO the range and leaves no residual filter for the other route to read.
func TestStrictlyOrderedIfUnique_ScanRangeRouteLicensesTheClaim(t *testing.T) {
	t.Parallel()

	unconstrained := uniqueIndexScanOn(t, "EMAIL", nil)
	if strictlyOrderedIfUnique(unconstrained, 1) {
		t.Fatal("an unconstrained scan of a UNIQUE index on a NULLABLE column has " +
			"genuine ties among its NULL entries; the claim must be declined")
	}

	notNull := uniqueIndexScanOn(t, "EMAIL", []*predicates.ComparisonRange{
		rangeOf(t, unaryComparison(predicates.ComparisonIsNotNull)),
	})
	if !strictlyOrderedIfUnique(notNull, 1) {
		t.Fatal("WHERE email IS NOT NULL empties the index's exempt set on this " +
			"stream, so the scan IS strictly sorted and the claim must be made")
	}

	// The trap, end to end: IS NULL is an equality-typed scan range that seeks
	// the NULL entries, which is the one stream where the ties are ALL that is
	// left.
	isNull := uniqueIndexScanOn(t, "EMAIL", []*predicates.ComparisonRange{
		rangeOf(t, unaryComparison(predicates.ComparisonIsNull)),
	})
	if strictlyOrderedIfUnique(isNull, 1) {
		t.Fatal("WHERE email IS NULL scans exactly the exempt entries; claiming " +
			"strict sorting over them is the soundness bug this route exists to avoid")
	}
}

// TestStrictlyOrderedIfUnique_EqualsNullRangeIsAdmittedBecauseItIsEmpty pins the
// hazard's resolution at the consumer.
//
// `email = NULL` reaches an index scan as a singleton equality range with NO
// residual filter — the planner neither folds it nor re-applies it. The claim
// over it is nonetheless sound, and for a reason that is a fact about the BINDER
// rather than about the planner: a NULL-valued equality binds to an EMPTY range
// (scan_range_binding.go:321-325), so the scan reads nothing and zero rows have
// no ties. If that guard is ever removed, this admission becomes unsound — which
// is why the guard itself is pinned in the executor package.
func TestStrictlyOrderedIfUnique_EqualsNullRangeIsAdmittedBecauseItIsEmpty(t *testing.T) {
	t.Parallel()

	equalsNull := uniqueIndexScanOn(t, "EMAIL", []*predicates.ComparisonRange{
		rangeOf(t, predicates.NewLiteralComparison(predicates.ComparisonEquals, nil)),
	})
	if !strictlyOrderedIfUnique(equalsNull, 1) {
		t.Fatal("`email = NULL` binds to an empty range, so the scan emits no rows " +
			"and the ordering claim over it is vacuously true")
	}

	// The sibling that is NOT empty: null-safe equality against NULL selects the
	// NULL tuple key, which is the whole tie class.
	notDistinct := uniqueIndexScanOn(t, "EMAIL", []*predicates.ComparisonRange{
		rangeOf(t, predicates.NewLiteralComparison(predicates.ComparisonNotDistinctFrom, nil)),
	})
	if strictlyOrderedIfUnique(notDistinct, 1) {
		t.Fatal("`email IS NOT DISTINCT FROM NULL` selects the NULL entries; the " +
			"claim must be declined")
	}
}

// TestStrictlyOrderedIfUnique_NumKeysStillBounds pins that R2 widens the
// NULLABILITY clause and nothing else: an index whose key is wider than the
// requested ordering still declines, however thoroughly NULL is rejected.
func TestStrictlyOrderedIfUnique_NumKeysStillBounds(t *testing.T) {
	t.Parallel()

	rowType := &values.RecordType{
		RecordName: "T",
		Fields: []values.Field{
			{Name: "A", FieldType: values.NullableString, Ordinal: 0},
			{Name: "B", FieldType: values.NullableString, Ordinal: 1},
		},
	}
	comps := []*predicates.ComparisonRange{
		rangeOf(t, unaryComparison(predicates.ComparisonIsNotNull)),
		rangeOf(t, unaryComparison(predicates.ComparisonIsNotNull)),
	}
	scanBase, scanErr := plans.NewRecordQueryIndexPlan("T$ab_unique", comps, []string{"T"}, rowType, false)
	scan := mustConstruct(t, scanBase, scanErr).
		WithIndexMetadata([]string{"A", "B"}, []string{"ID"}, true).
		WithKeyComponentTypes([]values.Type{values.NullableString, values.NullableString})

	if strictlyOrderedIfUnique(scan, 1) {
		t.Fatal("one requested sort key cannot cover a two-column unique key")
	}
	if !strictlyOrderedIfUnique(scan, 2) {
		t.Fatal("both key columns NULL-rejected and both covered: the claim holds")
	}

	// Partial NULL rejection over a composite key: (1, NULL) and (1, NULL) are
	// two legitimate entries, so `A IS NOT NULL` alone is not enough.
	partialBase, partialErr := plans.NewRecordQueryIndexPlan("T$ab_unique",
		[]*predicates.ComparisonRange{comps[0], predicates.EmptyComparisonRange()},
		[]string{"T"}, rowType, false)
	partial := mustConstruct(t, partialBase, partialErr).
		WithIndexMetadata([]string{"A", "B"}, []string{"ID"}, true).
		WithKeyComponentTypes([]values.Type{values.NullableString, values.NullableString})
	if strictlyOrderedIfUnique(partial, 2) {
		t.Fatal("a composite UNIQUE (A,B) with only A NULL-rejected still admits " +
			"(1,NULL),(1,NULL); coverage must not be rounded up")
	}
}

// TestStrictlyOrderedIfUnique_ResidualFilterEvidenceIsRefused pins the SECOND
// soundness finding of RFC-210's item 4, and it is a refusal rather than a gap.
//
// R2's filter route is valid evidence for the DISTINCT consumer and INVALID here,
// and the asymmetry is structural rather than a matter of care. The DISTINCT node
// sits ABOVE the filter, so a conjunct's guarantee is a guarantee about the very
// stream the elision is decided on. This consumer has nowhere to put the same
// conclusion: `strictlySorted` is a field on RecordQueryIndexPlan, and
// ordering.go:1222 hands it to NewRichOrdering as that scan's own DISTINCTNESS.
// Marking the scan BELOW a filter therefore asserts tie-freedom of a stream that
// still contains every NULL entry the index holds, and the false claim is
// readable from the bare scan node by any consumer that never sees the filter.
//
// This was measured, not reasoned about: an earlier revision of this change did
// descend through the filter, and `SELECT n FROM t WHERE n <> 'z' ORDER BY n`
// then planned `PredicatesFilter(IndexScan(U, [*]))` whose scan reported
// `HintRichOrdering().IsDistinct() == true` over a FULL scan.
//
// So only evidence that is a property of the SCAN ITSELF may license the claim.
// The SQL-path twin of this refusal — the same query, asserted through the real
// planner — is in secondary_unique_proof_reachability_test.go.
func TestStrictlyOrderedIfUnique_ResidualFilterEvidenceIsRefused(t *testing.T) {
	t.Parallel()

	// NOT_EQUALS is on R2's allow-list (NULL <> x is UNKNOWN, not TRUE) and is in
	// neither isScanRangeCompatible nor isSargableComparisonForMatch, so it can
	// only ever arrive as a residual filter. It is the strongest case for reading
	// a filter here, and it is still refused.
	unconstrained := uniqueIndexScanOn(t, "EMAIL", nil)
	legacyFilter, legacyFilterErr := plans.NewRecordQueryFilterPlan(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				scanRowRead(t, "EMAIL", "EMAIL"),
				predicates.NewLiteralComparison(predicates.ComparisonNotEquals, "zzz")),
		}, unconstrained)
	legacyFilter = mustConstruct(t, legacyFilter, legacyFilterErr)
	predicatesFilter, predicatesFilterErr := plans.NewRecordQueryPredicatesFilterPlan(
		unconstrained,
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				scanRowRead(t, "EMAIL", "EMAIL"),
				predicates.NewLiteralComparison(predicates.ComparisonNotEquals, "zzz")),
		})
	predicatesFilter = mustConstruct(t, predicatesFilter, predicatesFilterErr)
	for _, tc := range []struct {
		name string
		expr expressions.RelationalExpression
	}{
		{"record_query_filter", legacyFilter},
		// The shape the SQL planner actually produces. A refusal that knew only
		// the type above would be satisfied by no real query.
		{"predicates_filter", predicatesFilter},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if strictlyOrderedIfUnique(tc.expr, 1) {
				t.Fatal("a residual filter's NULL rejection was credited to the scan " +
					"below it. The scan still emits the index's NULL entries, and its " +
					"own RichOrdering now claims distinctness over a stream with " +
					"genuine ties — readable by any consumer that never sees the filter")
			}
		})
	}
}

// TestMakeStrictlySorted_MarksOnlyWhatTheWalkAdmits pins the pairing between the
// two enumerations from the other side. They must agree: a shape the walk admits
// but the marker leaves unchanged is a proved claim dropped on the floor, and a
// shape the marker rebuilds but the walk refuses is a claim made without a proof.
func TestMakeStrictlySorted_MarksOnlyWhatTheWalkAdmits(t *testing.T) {
	t.Parallel()

	scan := uniqueIndexScanOn(t, "EMAIL", []*predicates.ComparisonRange{
		rangeOf(t, unaryComparison(predicates.ComparisonIsNotNull)),
	})

	markedExpr, err := makeStrictlySorted(scan)
	if err != nil {
		t.Fatalf("makeStrictlySorted(indexScan) error = %v", err)
	}
	marked, ok := markedExpr.(*plans.RecordQueryIndexPlan)
	if !ok || !marked.IsStrictlySorted() {
		t.Fatalf("makeStrictlySorted(indexScan) = %T, not marked", marked)
	}
	if scan.IsStrictlySorted() {
		t.Fatal("makeStrictlySorted mutated the original scan instead of copying")
	}

	// The walk refuses a filter, so the marker must leave one alone — otherwise a
	// later relaxation of the walk would silently arm the false claim above.
	filteredBase, filteredErr := plans.NewRecordQueryPredicatesFilterPlan(scan,
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				scanRowRead(t, "EMAIL", "OTHER"),
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "x")),
		})
	filtered := mustConstruct(t, filteredBase, filteredErr)
	got, err := makeStrictlySorted(filtered)
	if err != nil {
		t.Fatalf("makeStrictlySorted(filter) error = %v", err)
	}
	if got != expressions.RelationalExpression(filtered) {
		t.Fatalf("makeStrictlySorted rebuilt a filter the walk refuses (%T); the two "+
			"enumerations have drifted apart", got)
	}
}
