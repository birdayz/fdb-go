package query

// The aggregate half of groupByOutputOrdinals is a LAST-WINS map keyed by the
// rendered aggregate name, so two aggregates that render alike are one slot.
//
// This is the reader side of the same fact
// pkg/recordlayer/query/plan/cascades/expressions pins on the producer side: the
// map is what turns a rendering collision into a LOST OUTPUT SLOT, and it is the
// reason a naming authority must not drop a qualifier. Both halves are needed —
// the producer test alone proves two strings differ, which is only interesting
// because this map would otherwise have merged them.
//
// The collapse is guarded on purpose for the KEY half of the same map
// (addKeyAlias maintains keyStrippedAmbig and refuses an ambiguous alias) and
// unguarded for the aggregate half, whose comment sanctions last-wins for the
// one case it was written for: a canonical/alias collision that means a
// DUPLICATE aggregate, computing the identical value. Two aggregates over
// different quantifiers' same-leaf columns are NOT that case, and before the
// operand-name conversion they minted exactly that collision.
//
// NOT reachable from SQL today, and the test says so rather than implying
// coverage: the sole production AggregateSpec mint always carries the operand's
// parse text, which renders qualified. This holds the plan-internal shape so the
// invariant survives any producer that does not.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestGroupByOutputOrdinals_SameLeafAggregateOperandsKeepSeparateSlots(t *testing.T) {
	t.Parallel()

	operand := func(alias, leaf string) values.Value {
		return &values.FieldValue{
			Field: leaf,
			Child: values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(alias)),
			Typ:   values.UnknownType,
		}
	}

	gb := expressions.NewGroupByExpression(
		nil,
		[]expressions.AggregateSpec{
			{Function: expressions.AggSum, Operand: operand("T", "V")},
			{Function: expressions.AggSum, Operand: operand("U", "V")},
		},
		expressions.Quantifier{},
	)

	_, aggs := groupByOutputOrdinals(gb)

	if len(aggs) != 2 {
		t.Fatalf("groupByOutputOrdinals produced %d aggregate key(s) for TWO aggregates: %v\n\n"+
			"SUM(T.V) and SUM(U.V) are different columns at different output "+
			"ordinals. This map is last-wins, so a shared rendering deletes one of "+
			"the two slots outright and every post-aggregate reference to it binds "+
			"to the other aggregate's value. The rendering authority "+
			"(expressions.AggregateResultColumnName) must not reduce a qualified "+
			"operand to its leaf.", len(aggs), aggs)
	}

	seen := map[int]string{}
	for name, ord := range aggs {
		if prev, dup := seen[ord]; dup {
			t.Fatalf("aggregate output ordinal %d is claimed by both %q and %q — "+
				"two names for one slot is the same conflation seen from the other "+
				"side", ord, prev, name)
		}
		seen[ord] = name
	}
}
