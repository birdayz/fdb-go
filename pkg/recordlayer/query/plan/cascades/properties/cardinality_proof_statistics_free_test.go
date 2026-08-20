package properties

import (
	"reflect"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
)

// THE SAFETY ARGUMENT FOR COLLECTED STATISTICS, PINNED (RFC-236 §6).
//
// Statistics may be stale. A stale count can only make the planner pick a worse
// plan — never return wrong rows — and the reason is structural: the ESTIMATE
// side (Cost) consumes a StatisticsProvider, and the PROOF side (Cardinalities)
// does not. A proven [min, max] interval is derived from plan shape alone, so a
// rule that drops a DISTINCT because "max is 1", or elides a sort because "max
// is 1", is reasoning from structure and cannot be misled by a number.
//
// That separation is currently a fact about signatures, which is exactly the
// kind of fact that erodes without anyone deciding to erode it: adding a
// StatisticsProvider parameter to a proof function compiles, passes every
// existing test, and silently converts "wrong plan" into "wrong rows". A stale
// count would then be able to prove max=1 for a table that has since grown, and
// the DISTINCT that gets dropped is not recoverable downstream.
//
// fkChainCardinalityCap is the one place a statistic becomes something the code
// calls a bound. It reaches properties.Cost only; its doc comment carries the
// other half of this note.

// statisticsProviderName is matched against parameter and result types by name
// rather than by identity so an alias, a named wrapper, or a struct embedding
// the interface is caught too — the point is that no route exists, not that one
// particular spelling is absent.
const statisticsProviderName = "StatisticsProvider"

// mentionsStatistics reports whether t is, contains, or is built from anything
// named like a statistics provider.
func mentionsStatistics(t reflect.Type, depth int) bool {
	if t == nil || depth > 4 {
		return false
	}
	if strings.Contains(t.Name(), statisticsProviderName) {
		return true
	}
	switch t.Kind() {
	case reflect.Ptr, reflect.Slice, reflect.Array, reflect.Chan:
		return mentionsStatistics(t.Elem(), depth+1)
	case reflect.Map:
		return mentionsStatistics(t.Key(), depth+1) || mentionsStatistics(t.Elem(), depth+1)
	case reflect.Func:
		for i := 0; i < t.NumIn(); i++ {
			if mentionsStatistics(t.In(i), depth+1) {
				return true
			}
		}
		for i := 0; i < t.NumOut(); i++ {
			if mentionsStatistics(t.Out(i), depth+1) {
				return true
			}
		}
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			if mentionsStatistics(t.Field(i).Type, depth+1) {
				return true
			}
		}
	case reflect.Interface:
		for i := 0; i < t.NumMethod(); i++ {
			if mentionsStatistics(t.Method(i).Type, depth+1) {
				return true
			}
		}
	}
	return false
}

func TestCardinalityProofTakesNoStatistics(t *testing.T) {
	t.Parallel()

	// THE PROOF PRODUCERS. Each of these DERIVES a Cardinalities, so none may
	// see a statistic: a bound is a claim about plan structure, and a number
	// cannot participate in deriving one.
	//
	// BoundedCostHinter and CostWithinBounds are deliberately NOT here. They are
	// where the two sides MEET — they take the proven bounds AND the statistics
	// provider — and that is legitimate precisely because they return a Cost.
	// The invariant for them is the direction of flow, asserted separately below:
	// a proof may inform an estimate, never the reverse.
	subjects := []struct {
		name string
		typ  reflect.Type
	}{
		{"provenCardinalities", reflect.TypeOf(provenCardinalities)},
		{"ClampCardinality", reflect.TypeOf(ClampCardinality)},
		{"CardinalityProver", reflect.TypeOf((*CardinalityProver)(nil)).Elem()},
		{"IntersectCardinalities", reflect.TypeOf(IntersectCardinalities)},
		{"UnionCardinalities", reflect.TypeOf(UnionCardinalities)},
		{"WeakenCardinalities", reflect.TypeOf(WeakenCardinalities)},
	}

	for _, s := range subjects {
		if mentionsStatistics(s.typ, 0) {
			t.Errorf("%s now mentions a %s: %s\n"+
				"  The proof side must derive bounds from plan STRUCTURE alone. Once a\n"+
				"  statistic can reach a proven interval, a stale count can prove max=1 for a\n"+
				"  table that has since grown — and the DISTINCT or sort a rule then drops on\n"+
				"  that proof cannot be recovered downstream. A stale count must be able to\n"+
				"  cost a plan badly and nothing more.\n"+
				"  If a statistics-derived bound is genuinely wanted, it belongs on the Cost\n"+
				"  side, the way fkChainCardinalityCap does it.",
				s.name, statisticsProviderName, s.typ)
		}
	}

	// Vacuity guard: the detector must actually detect. Without this the test
	// above passes just as happily when mentionsStatistics is broken, which is
	// the failure mode it exists to prevent.
	costLike := reflect.TypeOf(func(StatisticsProvider) Cost { return Cost{} })
	if !mentionsStatistics(costLike, 0) {
		t.Fatal("mentionsStatistics failed to flag a function that plainly takes a " +
			"StatisticsProvider — the assertions above prove nothing")
	}
	// And it must not flag the proof side's own types, or every subject would
	// "pass" for the wrong reason.
	if mentionsStatistics(reflect.TypeOf(Cardinalities{}), 0) {
		t.Fatal("mentionsStatistics flags Cardinalities itself — the detector is too broad")
	}
}

// TestProvenCardinalitiesSignatureIsExact pins the shape rather than only the
// absence, so a parameter ADDED to the proof dispatch fails here even if it is
// not a statistics provider — the next thing to be routed in will not be named
// StatisticsProvider.
func TestProvenCardinalitiesSignatureIsExact(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeOf(provenCardinalities)
	want := []reflect.Type{
		reflect.TypeOf((*expressions.RelationalExpression)(nil)).Elem(),
		reflect.TypeOf([]Cardinalities(nil)),
	}
	if got := typ.NumIn(); got != len(want) {
		t.Fatalf("provenCardinalities takes %d parameters, want %d — a new input to the\n"+
			"proof dispatch is a change to what a proven bound is allowed to depend on,\n"+
			"and it needs the RFC-236 §6 argument re-made before it lands.", got, len(want))
	}
	for i, w := range want {
		if got := typ.In(i); got != w {
			t.Errorf("provenCardinalities parameter %d is %s, want %s", i, got, w)
		}
	}
	if typ.NumOut() != 1 || typ.Out(0) != reflect.TypeOf(Cardinalities{}) {
		t.Errorf("provenCardinalities returns %v, want a single Cardinalities", typ)
	}
}

// TestProofInformsEstimateNotTheReverse pins the direction of flow at the one
// place the two sides meet.
//
// BoundedCostHinter exists because some Cost terms are a function of the
// operator's own OUTPUT cardinality, so the formula must see the proven
// interval. That is a proof informing an estimate, which is sound. The reverse
// — a statistic informing a proof — is what must never exist, and the check
// that catches it is not "does this take stats" (it legitimately does) but
// "what does it hand back". A signature returning Cardinalities from a
// stats-taking function is the exact shape of the mistake.
func TestProofInformsEstimateNotTheReverse(t *testing.T) {
	t.Parallel()
	cardinalitiesType := reflect.TypeOf(Cardinalities{})

	boundary := []struct {
		name string
		typ  reflect.Type
	}{
		{
			"BoundedCostHinter.HintCostWithin",
			reflect.TypeOf((*BoundedCostHinter)(nil)).Elem().Method(0).Type,
		},
		{"CostWithinBounds", reflect.TypeOf(CostWithinBounds)},
	}
	for _, b := range boundary {
		// Each must still take a statistics provider and the proven bounds —
		// otherwise this test is guarding a boundary that has moved, and its
		// passing says nothing.
		if !mentionsStatistics(b.typ, 0) {
			t.Errorf("%s no longer takes a %s. This test guards the boundary between the\n"+
				"  proof and the estimate; if the boundary moved, the guard belongs where it\n"+
				"  went, not here.", b.name, statisticsProviderName)
		}
		takesBounds := false
		for i := 0; i < b.typ.NumIn(); i++ {
			if b.typ.In(i) == cardinalitiesType {
				takesBounds = true
			}
		}
		if !takesBounds {
			t.Errorf("%s no longer takes the proven Cardinalities — the clamp it exists to\n"+
				"  apply cannot be applied without them.", b.name)
		}
		// The assertion that matters: it hands back an estimate, never a proof.
		for i := 0; i < b.typ.NumOut(); i++ {
			if b.typ.Out(i) == cardinalitiesType {
				t.Errorf("%s returns a Cardinalities while also taking a %s.\n"+
					"  That is a statistic producing a proven bound. A stale count could then\n"+
					"  prove max=1 for a table that has grown, and a rule dropping a DISTINCT on\n"+
					"  that proof returns wrong rows — the one failure collected statistics are\n"+
					"  designed to be incapable of.", b.name, statisticsProviderName)
			}
		}
	}
}
