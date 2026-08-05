package executor

import (
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// This file pins the binder facts a PLANNER proof rests on, and it exists
// because that proof has no residual to catch an error.
//
// RFC-210's R2 lets a NULL-rejecting scan range license two claims about a
// UNIQUE index on a NULLABLE column: that a DISTINCT over it is redundant, and
// that the scan is strictlySorted. Both are false on any stream containing the
// index's NULL entries — under NULLS DISTINCT one NULL prefix legitimately holds
// arbitrarily many of them. The planner never inspects a key byte; it reads the
// comparison KIND and concludes the range sits above the NULL boundary. Whether
// it actually does is decided here.
//
// So each fact below is a load-bearing structural claim, not an implementation
// detail. If one changes, the corresponding admission in
// cascades/null_rejecting_scan_range.go becomes unsound and silently returns
// wrong rows under a claim nothing re-checks.

// nullBinder answers every parameter with NULL — a prepared statement whose
// placeholder was bound to nil, which is the shape whose emptiness the planner
// cannot constant-fold.
type nullBinder struct{}

func (nullBinder) BindParameter(int, string) (any, bool) { return nil, true }

// TestScanRange_EqualityOverNullIsEmptyNotANullProbe is the fact that resolved
// RFC-210's recorded hazard.
//
// `col = NULL` DOES reach an index scan as a singleton equality range with no
// residual filter — the planner neither folds it nor re-applies it (it is
// admitted by isSargableComparisonForMatch and consumed by ComparisonRange.Merge
// without the operand ever being inspected). The worry was that such a range
// would ENCLOSE the NULL boundary and sweep the index's NULL entries into a
// stream claimed to be tie-free.
//
// It does not, because SQL's three-valued semantics are enforced HERE instead:
// `NULL = x` is UNKNOWN for every row, so the range is EMPTY and the scan reads
// nothing. Zero rows have no ties and no duplicates, which is what makes the
// planner's admission of Equals sound for BOTH consumers.
//
// Delete the guard and the range would seek the [null] entries — the wrong-rows
// bug this test is the sentinel for.
func TestScanRange_EqualityOverNullIsEmptyNotANullProbe(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		operand values.Value
		binder  values.ParameterBinder
	}{
		// A literal NULL: constant-foldable in principle, and not folded.
		{"literal_null", values.LiteralValue(nil), nil},
		// A parameter bound to NULL: NOT knowable at plan time, so the binder is
		// the only place this can be caught.
		{"parameter_bound_to_null", values.NewParameterValue(0), nullBinder{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec, err := bindScanComparisonsToRangeSet(
				[]*predicates.ComparisonRange{
					scanRangeTestComparison(t, predicates.ComparisonEquals, tc.operand),
				},
				[]values.Type{values.NullableString},
				tc.binder, false, "idx:T_EMAIL",
			)
			if err != nil {
				t.Fatal(err)
			}
			if !spec.empty {
				t.Fatal("`col = NULL` bound to a NON-empty range. It now encloses the " +
					"NULL boundary, so RFC-210's admission of ComparisonEquals as a " +
					"NULL-rejecting scan range is unsound: a UNIQUE index's NULL " +
					"entries reach a stream claimed duplicate-free and strictly sorted")
			}
		})
	}
}

// TestScanRange_NullSafeEqualityOverNullSeeksTheNullEntries is the contrast that
// gives the test above its meaning.
//
// IS NOT DISTINCT FROM is classified as a scan-range EQUALITY type by exactly
// the same code path (comparison_range.go:196-203), differs from ordinary
// equality by three lines in this binder, and does the OPPOSITE: it selects the
// one NULL tuple key. It is refused by RFC-210's allow-list, and this pins that
// the refusal is answering a real difference rather than being cautious about an
// imagined one.
func TestScanRange_NullSafeEqualityOverNullSeeksTheNullEntries(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		comparison *predicates.ComparisonRange
	}{
		{"is_null", scanRangeTestComparison(t, predicates.ComparisonIsNull, nil)},
		{"not_distinct_from_null", scanRangeTestComparison(
			t, predicates.ComparisonNotDistinctFrom, values.LiteralValue(nil))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec, err := bindScanComparisonsToRangeSet(
				[]*predicates.ComparisonRange{tc.comparison},
				[]values.Type{values.NullableString},
				nil, false, "idx:T_EMAIL",
			)
			if err != nil {
				t.Fatal(err)
			}
			if spec.empty {
				t.Fatal("expected a range over the NULL entries, got an empty set")
			}
			rng, err := spec.materialize(make([]uint32, len(spec.alternativeCounts)))
			if err != nil {
				t.Fatal(err)
			}
			if len(rng.Low) != 1 || rng.Low[0] != nil {
				t.Fatalf("range low = %+v, want the NULL tuple element — this shape is "+
					"the exempt set itself and must stay refused by the allow-list", rng.Low)
			}
		})
	}
}

// TestScanRange_BareUpperBoundStartsAboveTheNullBoundary is the second
// structural fact, and the one that is least obvious.
//
// `col < 'm'` names only an UPPER bound. Nothing in the predicate says where the
// scan starts, and starting it at the bottom of the key space would be a
// defensible implementation that swept every NULL entry into the result — NULL
// sorts below every value in FDB tuple order. The binder instead installs an
// EXCLUSIVE low at the NULL boundary whenever no lower bound exists
// (`tail.lowIsNullBoundary`), which is Java's
// `Range.greaterThan(NULL_boundary)` (RangeConstraints.java:650-653) arrived at
// from the other side.
//
// That is what licenses LESS_THAN and LESS_THAN_OR_EQUALS on the allow-list.
// Without it those two entries are unsound and IS NOT NULL is the only ordered
// comparison that could stay.
func TestScanRange_BareUpperBoundStartsAboveTheNullBoundary(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name           string
		comparisonType predicates.ComparisonType
	}{
		{"less_than", predicates.ComparisonLessThan},
		{"less_than_or_eq", predicates.ComparisonLessThanOrEq},
		{"is_not_null", predicates.ComparisonIsNotNull},
	} {
		comparisonType := tc.comparisonType
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var operand values.Value
			if comparisonType != predicates.ComparisonIsNotNull {
				operand = values.LiteralValue("m")
			}
			spec, err := bindScanComparisonsToRangeSet(
				[]*predicates.ComparisonRange{
					scanRangeTestComparison(t, comparisonType, operand),
				},
				[]values.Type{values.NullableString},
				nil, false, "idx:T_EMAIL",
			)
			if err != nil {
				t.Fatal(err)
			}
			if spec.empty {
				t.Fatal("expected a non-empty range")
			}
			rng, err := spec.materialize(make([]uint32, len(spec.alternativeCounts)))
			if err != nil {
				t.Fatal(err)
			}
			if len(rng.Low) != 1 || rng.Low[0] != nil {
				t.Fatalf("range low = %+v, want the NULL boundary element", rng.Low)
			}
			if rng.LowEndpoint != recordlayer.EndpointTypeRangeExclusive {
				t.Fatalf("range low endpoint = %v, want EXCLUSIVE. An inclusive NULL "+
					"boundary puts the index's NULL entries back in the scan, and "+
					"RFC-210 admits %v as NULL-rejecting on the strength of their "+
					"exclusion", rng.LowEndpoint, comparisonType)
			}
		})
	}
}
