package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// isProvablePointProbe gained a covering arm with RFC-220, and that flips its
// answer for the shape essentially every index access now takes. Before the
// arm, a covering scan matched no case and fell to the default `return false` —
// "no unique-index point probe is provable" — which is the EXPENSIVE direction:
// a probe bounded to one row gets priced as a bucket scan, and the IN-list
// cardinality shortcut that consults it (cost.go, RecordQueryInJoinPlan) stops
// firing.
//
// The failure is silent in both directions that matter. False-for-everything
// looks like a conservative estimate rather than a missed proof, and a
// true-for-everything regression would look like a cost win. Both halves are
// therefore driven here.

// pointProbeScan builds an index scan on idx_a over key [A] with pk [ID].
// bindAll binds BOTH components, which is what makes the bind FULL; unique sets
// the index's UNIQUE flag, which is what bounds a full bind to a single row.
func pointProbeScan(t *testing.T, unique, bindAll bool) *RecordQueryIndexPlan {
	t.Helper()
	mk := func(v int64) *predicates.ComparisonRange {
		comp := predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: v, Typ: values.NullableLong},
		}
		mr := predicates.EmptyComparisonRange().Merge(&comp)
		if !mr.Ok {
			t.Fatal("premise broken: could not build an equality comparison range")
		}
		return mr.Range
	}
	// The index has TWO key columns. bindAll binds both, which is what makes
	// the equality FULL — binding only the first leaves a prefix, and a prefix
	// is not a point probe however unique the index is. That distinction is the
	// entire unprovable half of the matrix, so the fixture has to be able to
	// express it, and a single-column index cannot.
	comps := []*predicates.ComparisonRange{mk(42)}
	types := []values.Type{values.NullableLong}
	if bindAll {
		comps = append(comps, mk(7))
		types = append(types, values.NullableLong)
	}
	return mustChecked(t, func() (*RecordQueryIndexPlan, error) {
		return NewRecordQueryIndexPlan(
			"idx_ab", comps, []string{"T"}, exactTestRecordType(), false,
		)
	}).
		// Physical key types are what turn a bind into a PROOF: without them the
		// multiplicity is Unknown and nothing is provable, so every cell would
		// agree at false and the pin would be vacuous. The vacuity guard below
		// caught exactly that while this test was being written.
		WithKeyComponentTypes(types).
		WithIndexMetadata([]string{"A", "B"}, []string{"ID"}, unique)
}

// TestCoveringScanPointProbeProvability pins that a covering wrapper neither
// gains nor loses provability relative to the scan it wraps.
//
// Stating it as AGREEMENT with the inner rather than as a fixed expected value
// is deliberate: it keeps the pin correct if the provability rule itself is
// later refined, while still failing the moment the wrapper starts answering
// something its own inner does not.
func TestCoveringScanPointProbeProvability(t *testing.T) {
	t.Parallel()

	var sawTrue, sawFalse bool
	for _, unique := range []bool{true, false} {
		for _, bindAll := range []bool{true, false} {
			inner := pointProbeScan(t, unique, bindAll)
			cov := mustChecked(t, func() (*RecordQueryCoveringIndexPlan, error) {
				return NewRecordQueryCoveringIndexPlan(inner)
			})

			want := isProvablePointProbe(inner)
			got := isProvablePointProbe(cov)
			if got != want {
				t.Errorf("unique=%v bindAll=%v: covering scan reports provable=%v, wrapped "+
					"scan reports %v. The covering wrapper holds its scan as a FIELD, so a "+
					"bare-plan type test misses it and answers FALSE — pricing a bounded "+
					"probe as a bucket scan for every index access in the tree",
					unique, bindAll, got, want)
			}
			if want {
				sawTrue = true
			} else {
				sawFalse = true
			}

			// Through the production wrapper too: the access path emits
			// Fetch(Covering(scan)), and a fetch is transparent 1:1 over its child.
			fetch := mustChecked(t, func() (*RecordQueryFetchFromPartialRecordPlan, error) {
				return NewRecordQueryFetchFromPartialRecordPlan(
					cov, nil, exactTestRecordType(), FetchIndexRecordsPrimaryKey)
			})
			if got := isProvablePointProbe(fetch); got != want {
				t.Errorf("unique=%v bindAll=%v: Fetch(Covering(scan)) reports provable=%v, "+
					"want %v — a fetch is transparent 1:1 over its child and must inherit "+
					"its provability unchanged", unique, bindAll, got, want)
			}
		}
	}

	// Vacuity guard on BOTH populations. If the matrix stopped producing a
	// provable case the agreement assertion would pass while proving nothing;
	// if it stopped producing an unprovable one, a predicate that returns true
	// unconditionally would also pass.
	if !sawTrue {
		t.Error("no cell in the matrix produced a PROVABLE point probe, so the agreement " +
			"assertion never exercised the interesting direction and this test is vacuous")
	}
	if !sawFalse {
		t.Error("no cell in the matrix produced an UNPROVABLE case, so a predicate " +
			"answering true unconditionally would satisfy this test")
	}
}
