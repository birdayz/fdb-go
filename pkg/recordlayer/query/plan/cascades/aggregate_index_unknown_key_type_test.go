package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestAggregateCandidateWithUnknownGroupingTypeDeclinesEveryBindingFailClosed
// gives the SILENT decline path a witness.
//
// normalizePhysicalKeyTypes returns a fixed-width type vector and pads any
// coordinate metadata did not establish with values.UnknownType. That padding
// is deliberate — it keeps every consumer on the same coordinate system as the
// comparisons — but it is also INVISIBLE: nothing at the construction site
// distinguishes "the caller supplied a short vector" from "the caller supplied
// a type for every column". Production reaches the padded state by at least
// three routes: an index whose backing descriptor is missing, a
// FanType_CONCATENATE key whose element domain is not a scalar column, and two
// record types sharing one index while disagreeing on FLOAT versus DOUBLE.
//
// Downstream, bindingsUseUnknownPhysicalKeyType turns an Unknown coordinate
// into a DECLINE of every binding at it. That is the correct, fail-closed
// answer — an aggregate-index row carries no backing record, so a bound range
// over an unknown tuple-wire domain cannot be repaired by a residual predicate
// above the pre-aggregated stream, and guessing a width would silently omit
// every entry stored with the other one. Declining costs a plan; guessing costs
// rows.
//
// What is dangerous is that the decline is indistinguishable from "this
// candidate simply did not match". An aggregate index that quietly stops being
// sargable for EVERY query looks exactly like one that was never eligible, and
// no test failed when it happened. This test is that witness: it constructs a
// candidate whose grouping type is deliberately Unknown and asserts the decline
// is total, so the fail-closed behaviour is pinned as INTENDED rather than
// rediscovered as a regression. If a future change makes an Unknown coordinate
// bind anything at all, this goes red and names the tradeoff.
func TestAggregateCandidateWithUnknownGroupingTypeDeclinesEveryBindingFailClosed(t *testing.T) {
	t.Parallel()

	newCandidate := func(groupKeyTypes []values.Type) *AggregateIndexMatchCandidate {
		return NewAggregateIndexMatchCandidate(
			"SUM_AMOUNT_BY_CUSTOMER",
			[]string{"ORDERS"},
			[]string{"CUSTOMER_ID"},
			expressions.AggSum,
			"AMOUNT",
			values.UnknownType,
			groupKeyTypes,
			1,
		)
	}

	equalityRange := func(t *testing.T) *predicates.ComparisonRange {
		t.Helper()
		empty := predicates.EmptyComparisonRange()
		cmp := predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(5))
		res := empty.Merge(&cmp)
		if !res.Ok {
			t.Fatal("Empty + EQUALS must merge; the binding below would otherwise be empty " +
				"and skipped by the guard for the WRONG reason")
		}
		return res.Range
	}

	// The control. An established grouping type binds, so the decline below is
	// attributable to the Unknown coordinate and nothing else. Without this the
	// test would pass just as well against a candidate that declines everything
	// unconditionally.
	t.Run("known_grouping_type_binds", func(t *testing.T) {
		t.Parallel()
		known := newCandidate([]values.Type{values.NullableString})
		aliases := known.GetSargableAliases()
		bindings := map[values.CorrelationIdentifier]*predicates.ComparisonRange{
			aliases[0]: equalityRange(t),
		}
		if !known.bindingRangesEligible(bindings) {
			t.Fatal("a candidate whose grouping type IS established must accept an " +
				"equality binding — if this fails, the Unknown assertion below proves " +
				"nothing, because the candidate declines regardless of type")
		}
	})

	// Two routes to the same Unknown coordinate, asserted separately because
	// they arrive differently: a SHORT vector (metadata supplied nothing for
	// this column) and an EXPLICIT UnknownType (metadata supplied a type it
	// could not resolve, e.g. FLOAT/DOUBLE disagreement across record types).
	// normalizePhysicalKeyTypes collapses both to the same padded vector, and
	// that collapse is exactly what makes the state hard to see.
	for _, tc := range []struct {
		name          string
		groupKeyTypes []values.Type
	}{
		{"short_vector_padded_to_unknown", nil},
		{"explicit_unknown_type", []values.Type{values.UnknownType}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			candidate := newCandidate(tc.groupKeyTypes)

			// Precondition: the padding really did produce an Unknown at
			// coordinate 0. Asserting the decline without this would pass even
			// if the constructor had started resolving the type, at which point
			// the decline would be a DIFFERENT bug.
			physical := candidate.GetKeyComponentTypes()
			if len(physical) != 1 {
				t.Fatalf("GetKeyComponentTypes() = %v, want one entry per grouping column", physical)
			}
			if physical[0] == nil || physical[0].Code() != values.TypeCodeUnknown {
				t.Fatalf("grouping type = %v, want UnknownType — normalizePhysicalKeyTypes "+
					"no longer pads this shape, so this test is not exercising the "+
					"silent-decline path it was written for", physical[0])
			}

			aliases := candidate.GetSargableAliases()
			if len(aliases) == 0 {
				t.Fatal("candidate advertises no sargable alias, so no binding can be offered")
			}
			bindings := map[values.CorrelationIdentifier]*predicates.ComparisonRange{
				aliases[0]: equalityRange(t),
			}

			if candidate.bindingRangesEligible(bindings) {
				t.Fatalf("an aggregate candidate whose grouping type padded to UnknownType "+
					"ACCEPTED an equality binding. It must decline: the tuple-wire domain "+
					"is unestablished, an aggregate-index row has no backing record to "+
					"carry a residual predicate, and binding one guessed width silently "+
					"omits every entry stored with the other. Declining loses a plan; "+
					"accepting loses ROWS.\n  grouping types: %v", physical)
			}
		})
	}
}
