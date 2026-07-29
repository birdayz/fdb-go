package embedded

// The HAVING rebase and the Cascades push-down rule must answer ONE question the
// SAME way, because they are two halves of one binding decision: a HAVING
// reference the rule pushes below the GroupBy evaluates on the PRE-aggregate row
// and must keep its qualified binding, while one that stays above evaluates on
// the aggregate's OUTPUT row and must be rebased onto it. A disagreement is not a
// lost optimization — it is a reference bound against the wrong row.
//
// havingPredicatePushesBelowAggregate used to be a hand-rolled MIRROR of
// PushFilterThroughGroupByRule.predicateReferencesOnlyKeys carrying a comment
// saying the two "cannot drift". They had drifted in three ways, and each is
// pinned below. All three were LATENT: the whole sqldriver suite, the explaindiff
// corpus and plan_shape.golden are unchanged by the switch, and the three FDB
// probes in array_unnest_ordinality_fdb_test.go ("HAVING group key compared
// against an aggregate") pass on both sides. Latent is the state the original
// seven RFC-197 bugs shipped in, which is why the drift gets pinned rather than
// argued away.
//
// These are unit-level on purpose: the drift lives between two deciders, not in
// any one query's result, so the falsifiable claim is about what the decider
// ANSWERS. Delegating to cascades.PredicatePushesBelowGroupBy is what makes all
// three cases answer correctly at once.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

func hpdAgg(keys ...values.Value) *logical.LogicalAggregate {
	gks := make([]logical.GroupKey, 0, len(keys))
	for _, k := range keys {
		gks = append(gks, logical.GroupKey{Value: k})
	}
	return &logical.LogicalAggregate{Input: logical.NewScan("t", ""), GroupKeys: gks}
}

// hpdBare is a top-level column reference: one accessor, name only.
func hpdBare(name string) *values.FieldValue {
	return &values.FieldValue{Field: name, Typ: values.UnknownType}
}

// hpdNested is `root.leaf` as a STRUCTURED path — the shape the rule's group-key
// set distinguishes from a top-level column of the same leaf name.
func hpdNested(root, leaf string) *values.FieldValue {
	return &values.FieldValue{
		Field: leaf,
		Typ:   values.UnknownType,
		Child: &values.FieldValue{Field: root, Typ: values.UnknownType},
	}
}

func hpdCmp(operand values.Value, rhs values.Value) predicates.QueryPredicate {
	return predicates.NewComparisonPredicate(operand, predicates.Comparison{
		Type: predicates.ComparisonGreaterThan, Operand: rhs,
	})
}

func hpdConst() values.Value { return &values.ConstantValue{Value: int64(1), Typ: values.UnknownType} }

// TestHavingPushdownDeciderAgreesOnTheTrivialCase is the positive control: a
// plain group-key comparison against a constant IS pushed below, and must stay
// so — every negative case below is only meaningful if this one is true.
func TestHavingPushdownDeciderAgreesOnTheTrivialCase(t *testing.T) {
	t.Parallel()

	agg := hpdAgg(hpdBare("CITY"))
	if !havingPredicatePushesBelowAggregate(hpdCmp(hpdBare("CITY"), hpdConst()), agg) {
		t.Fatal("a bare group-key comparison against a constant no longer pushes below the " +
			"GroupBy; the HAVING rebase would now rebase it onto the aggregate output row " +
			"while the rule still pushes it pre-aggregate")
	}
}

// TestHavingPushdownDeciderComparesTheWholePath pins drift 1: the leaf name is
// not the key. Both directions are covered, because a decider that keys on the
// leaf is wrong in both — it would claim a nested key answers to a top-level
// reference AND that a top-level key answers to a nested one.
func TestHavingPushdownDeciderComparesTheWholePath(t *testing.T) {
	t.Parallel()

	// Group by the NESTED addr.city; the HAVING names a top-level CITY.
	nestedKey := hpdAgg(hpdNested("ADDR", "CITY"))
	if havingPredicatePushesBelowAggregate(hpdCmp(hpdBare("CITY"), hpdConst()), nestedKey) {
		t.Fatal("a top-level CITY reference was matched against a nested ADDR.CITY grouping " +
			"key on their shared LEAF NAME: PushFilterThroughGroupByRule keys its group-key " +
			"set by the whole accessor path, so it will NOT push this predicate, and the " +
			"HAVING rebase must not assume it will")
	}

	// The mirror image: group by top-level CITY, HAVING names ADDR.CITY.
	bareKey := hpdAgg(hpdBare("CITY"))
	if havingPredicatePushesBelowAggregate(hpdCmp(hpdNested("ADDR", "CITY"), hpdConst()), bareKey) {
		t.Fatal("a nested ADDR.CITY reference was matched against a top-level CITY grouping key")
	}

	// And the genuine nested match still pushes.
	if !havingPredicatePushesBelowAggregate(hpdCmp(hpdNested("ADDR", "CITY"), hpdConst()), nestedKey) {
		t.Fatal("a nested reference no longer matches the identical nested grouping key")
	}
}

// TestHavingPushdownDeciderChecksTheComparand pins drift 2: the rule refuses to
// push a predicate whose RHS is not itself key-only — `key > other_column`
// evaluated below the GroupBy would read a column the aggregation input has but
// the decision was never made about. The mirror looked only at the LHS.
func TestHavingPushdownDeciderChecksTheComparand(t *testing.T) {
	t.Parallel()

	agg := hpdAgg(hpdBare("CITY"))
	if havingPredicatePushesBelowAggregate(hpdCmp(hpdBare("CITY"), hpdBare("POP")), agg) {
		t.Fatal("a group-key comparison whose COMPARAND is a non-key column was reported as " +
			"pushable: comparandReferencesOnlyKeys refuses it, so the rule leaves the " +
			"predicate above the GroupBy and the HAVING reference must be rebased onto the " +
			"aggregate output row")
	}
	// A comparand that IS a grouping key is fine.
	two := hpdAgg(hpdBare("CITY"), hpdBare("POP"))
	if !havingPredicatePushesBelowAggregate(hpdCmp(hpdBare("CITY"), hpdBare("POP")), two) {
		t.Fatal("a comparison between two grouping keys no longer pushes")
	}
}

// TestHavingPushdownDeciderDisablesOnAnUnidentifiableKey pins drift 3: the rule
// builds NO group-key set at all when any grouping key's identity cannot be
// established (a flat-dotted lazy name is ambiguous between a nested path and an
// alias-qualified leaf, so AccessorNamePathKey declines it), which disables
// pushdown for EVERY predicate. The mirror kept matching the other keys.
func TestHavingPushdownDeciderDisablesOnAnUnidentifiableKey(t *testing.T) {
	t.Parallel()

	agg := hpdAgg(hpdBare("CITY"), hpdBare("V.V"))
	if havingPredicatePushesBelowAggregate(hpdCmp(hpdBare("CITY"), hpdConst()), agg) {
		t.Fatal("pushdown was reported for a GroupBy one of whose keys has no establishable " +
			"identity: buildGroupKeySet returns nil there and the rule pushes nothing, so " +
			"every HAVING reference stays above and must be rebased")
	}
	// A nil grouping-key value is the same answer for the same reason.
	if havingPredicatePushesBelowAggregate(hpdCmp(hpdBare("CITY"), hpdConst()), hpdAgg(hpdBare("CITY"), nil)) {
		t.Fatal("pushdown was reported for a GroupBy with a nil grouping-key value")
	}
}
