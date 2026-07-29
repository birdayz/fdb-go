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
// pinned below. All three were LATENT — the switch to the shared decider left the
// sqldriver suite, the explaindiff corpus and plan_shape.golden unchanged — and
// latent is the state the original seven RFC-197 bugs shipped in, which is why
// the drift gets pinned rather than argued away.
//
// "Latent" here means the drift produced no observable difference on the queries
// this repo runs. It does NOT mean the decider is unobservable in principle: one
// of its two wrong answers moves a filter across the aggregate on a multi-source
// grouped HAVING, and the other is invisible end-to-end everywhere. Which is
// which, and how it was measured, is at
// TestHavingPushdownDeciderMovesTheFilterAcrossTheAggregate below.
//
// The three drift cases are unit-level on purpose: the drift lives between two
// deciders, not in any one query's result, so the falsifiable claim is about what
// the decider ANSWERS. Delegating to cascades.PredicatePushesBelowGroupBy is what
// makes all three cases answer correctly at once.

import (
	"strings"
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

// TestHavingPushdownDeciderMovesTheFilterAcrossTheAggregate is the end-to-end
// half, and it exists because the FDB probes cannot supply it.
//
// The decider can be wrong in two directions and they are NOT symmetric in how
// observable they are. Both were measured by hard-wiring the decider to each
// constant answer and running the whole //pkg/relational tree:
//
//	says-NO when it should say YES — OBSERVABLE. The pure group-key HAVING below
//	  stops being pushed and the filter jumps from below the aggregate to above
//	  it. This test is that observation.
//
//	says-YES when it should say NO — NOT observable anywhere end-to-end. With the
//	  decider hard-wired true the entire //pkg/relational tree stays green — the
//	  sqldriver FDB suite, the explaindiff corpus, plan_shape.golden, rowdiff,
//	  plandiff, memoinvariant and yamsql — and the ONLY reds are the three unit
//	  tests above. For that direction those unit tests are not a supplement to
//	  end-to-end coverage, they are the whole of it.
//
// The grouped-unnest FDB probes in array_unnest_ordinality_fdb_test.go observe
// NEITHER direction: their physical plans are byte-identical under both constant
// answers, so no assertion there — over rows or over the plan — can see the
// decider at all. That is stated at those probes so nobody adds one believing it.
//
// The shape here is MULTI-SOURCE on purpose. A single-source grouped HAVING is
// not sensitive; `po` and `pi` both declaring `id` is what makes the rebased bare
// reference behave differently from the qualified one at the rule.
func TestHavingPushdownDeciderMovesTheFilterAcrossTheAggregate(t *testing.T) {
	t.Parallel()

	const ddl = "CREATE TABLE po (id BIGINT NOT NULL, PRIMARY KEY (id)) " +
		"CREATE TABLE pi (id BIGINT NOT NULL, po_id BIGINT, PRIMARY KEY (id))"

	// A pure group-key comparison. The rule pushes it below the GroupBy, so the
	// reference must stay QUALIFIED, and the plan must keep the filter under the
	// aggregate's input — never wrapped around the aggregate.
	plan, err := PlanQueryForTest(
		"SELECT po.id, pi.id, COUNT(*) FROM po, pi WHERE pi.po_id = po.id "+
			"GROUP BY po.id, pi.id HAVING po.id > 1 ORDER BY po.id, pi.id", ddl, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !strings.Contains(plan, "PredicatesFilter(StreamingAgg(") {
		t.Fatalf("the HAVING filter is no longer ABOVE the streaming aggregate.\n  plan: %s\n"+
			"havingPredicatePushesBelowAggregate answered that this predicate pushes BELOW the "+
			"GroupBy, so the reference was left in its pre-aggregate qualified form and the rule "+
			"then pushed the filter under the aggregate. The decider and "+
			"PushFilterThroughGroupByRule have drifted apart, which binds the HAVING reference "+
			"against the wrong row — not a lost optimization.", plan)
	}

	// The complement, so the assertion above cannot be satisfied by a plan that
	// simply never pushes anything: a comparand that references an aggregate is
	// refused by the rule for its OWN reason, and lands in the same place.
	aggPlan, err := PlanQueryForTest(
		"SELECT po.id, pi.id, COUNT(*) FROM po, pi WHERE pi.po_id = po.id "+
			"GROUP BY po.id, pi.id HAVING po.id >= COUNT(*) ORDER BY po.id, pi.id", ddl, nil)
	if err != nil {
		t.Fatalf("plan (aggregate comparand): %v", err)
	}
	if !strings.Contains(aggPlan, "PredicatesFilter(StreamingAgg(") {
		t.Fatalf("a HAVING whose comparand is an aggregate must stay ABOVE the aggregate.\n"+
			"  plan: %s", aggPlan)
	}
}
