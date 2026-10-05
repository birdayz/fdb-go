package cascades

import (
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestCheckInUnionChildOrdering pins the extraction check of RFC-257 WS-F 4.3
// item 3: an in-union's extracted child must provide its comparison keys in the
// merge direction, and a child that does not is a planner invariant violation
// carrying an InUnionChildOrderingError, never an accepted plan.
func TestCheckInUnionChildOrdering(t *testing.T) {
	t.Parallel()
	index := mustInRuleConstruct(plans.NewRecordQueryIndexPlan(
		"IDX_A", nil, []string{"T"}, inRuleRowType(), false)).
		WithKeyComponentTypes([]values.Type{values.NotNullLong}).
		WithIndexMetadata([]string{"a"}, []string{"x"}, false).
		WithPrimaryKeyComponentTypes([]values.Type{values.NotNullLong}).
		WithDistinctRecordsSignal(false)
	keys := index.HintRichOrdering().GetKeys()
	if len(keys) != 2 {
		t.Fatalf("fixture ordering keys = %v, want (a, x)", keys)
	}
	inUnion := func(keys []values.Value, reverse bool) *plans.RecordQueryInUnionPlan {
		return mustInRuleConstruct(plans.NewRecordQueryInUnionPlan(
			index, []string{"b"}, keys, reverse, plans.UnboundedInUnionSize))
	}

	if err := checkInUnionChildOrdering(inUnion(keys, false)); err != nil {
		t.Fatalf("an ascending (a, x) child merged ascending on (a, x): %v", err)
	}
	if err := checkInUnionChildOrdering(inUnion(nil, false)); err != nil {
		t.Fatalf("an in-union with no comparison keys: %v", err)
	}
	for _, c := range []struct {
		name    string
		keys    []values.Value
		reverse bool
	}{
		{"merged against the child's direction", keys, true},
		{"a key the child does not order by", []values.Value{inRuleField(inRuleQOV(values.UniqueCorrelationIdentifier(), inRuleRowType()), 3)}, false},
	} {
		err := checkInUnionChildOrdering(inUnion(c.keys, c.reverse))
		var invariant *PlannerInvariantViolationError
		var ordering *InUnionChildOrderingError
		if !errors.As(err, &invariant) || !errors.As(err, &ordering) {
			t.Fatalf("%s: error = %v, want a PlannerInvariantViolationError carrying InUnionChildOrderingError", c.name, err)
		}
		if ordering.Reverse != c.reverse || len(ordering.ComparisonKeys) != len(c.keys) || ordering.ChildOrdering == "[]" {
			t.Fatalf("%s: error fields = %+v", c.name, ordering)
		}
	}
}
