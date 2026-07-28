package embedded

// Why the per-accessor NAME comparison in fieldValueMatchesAggregateGroupKey is
// NOT a live defect, and what re-arms it.
//
// The comparison ANDs ordinal equality with a name check, so on its face it can
// refuse a match between two values that agree on the full ordinal path. Java's
// ResolvedAccessor.equals is ordinal-only (FieldValue.java:675-684) and RFC-197
// makes that the rule, so the site reads like a divergence that can turn a
// legitimate group-key reference into the caller's "references a field outside
// the aggregate output contract" error.
//
// It cannot, for two independent reasons pinned below. The blunt empirical
// check agrees: hard-wiring fieldValueMatchesAggregateGroupKey to `return
// false` leaves the entire //pkg/relational/sqldriver integration suite green,
// and every case in
// pkg/relational/sqldriver/aggregate_group_key_qualifier_alias_binding_test.go
// green. No SQL exercised by this repo reaches the relaxation at all, let
// alone the name check inside it. Only the tests in THIS file fail under that
// mutation, which is why they exist.
//
// N1 — the name check is DEAD wherever both sides share a child shape. All three
// call sites OR the matcher with values.SemanticEqualsUnderAliasMap, which is
// ordinal-only on baked FieldValues. Both-childless and both-QOV pairs that
// agree on ordinals are therefore already matched by the OR's first arm before
// the matcher runs. The matcher exists only for the ASYMMETRIC shape (one side
// childless, one side QOV-qualified) that semantic equality rejects, so that is
// the only arm in which a name can decide anything.
//
// N2 — in that asymmetric arm the matcher requires a single inner source, and
// both operands at every call site are minted by the same
// expr.Resolver.ResolveIdentifier from the same (col.Id.Name(),
// sourceColumnOrdinal(src, name)) pair. Within one source, ordinal -> name is a
// function, so an ordinal-equal pair is name-equal too and the check never
// fires.
//
// The load-bearing half: at Ordinal -1 the name check is the ONLY identity.
// Java asserts ordinal >= 0 at construction (FieldValue.java:651); Go mints
// name-only accessors at Ordinal -1 for array/unnest descent
// (cascades_translator.go:1910-1912, unnest_seed.go:177, unnest_gather.go:189,
// index_expansion.go:491). Two such accessors are ordinal-equal by
// construction. Deleting the name check to make this site ordinal-only would
// convert a correct refusal into a CONFLATION of two different nested fields —
// a wrong-column bind, strictly worse than the missed bind the check can at
// worst cause. RFC-197's migration of this site needs the domain-explicit
// accessor first; a plain deletion is unsafe.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

func aggkOneSource() *logical.LogicalAggregate {
	return &logical.LogicalAggregate{Input: logical.NewScan("orders", "")}
}

// aggkChildless is the source-relative bake shape a bare group-key reference
// carries (expr.go:296) — no QuantifiedObjectValue child.
func aggkChildless(name string, ordinal int) *values.FieldValue {
	return &values.FieldValue{
		Field:    name,
		Resolved: &values.FieldPath{Accessors: []values.ResolvedAccessor{{Field: name, Ordinal: ordinal}}},
	}
}

// aggkQualified is the shape a QUALIFIED reference carries (expr.go:270-276) —
// the same resolved path under a QOV child.
func aggkQualified(corr, name string, ordinal int) *values.FieldValue {
	return &values.FieldValue{
		Field:    name,
		Child:    values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(corr)),
		Resolved: &values.FieldPath{Accessors: []values.ResolvedAccessor{{Field: name, Ordinal: ordinal}}},
	}
}

// TestAggregateGroupKeyNameCheckIsDeadUnderSemanticEquality pins N1: the OR at
// the call sites decides every symmetric shape before the name check is
// consulted. If semantic equality on baked FieldValues ever stops being
// ordinal-only, the name check stops being dead and becomes reachable in
// shapes N2 does not cover.
func TestAggregateGroupKeyNameCheckIsDeadUnderSemanticEquality(t *testing.T) {
	t.Parallel()

	agg := aggkOneSource()
	am := values.AliasMap{}

	// Both childless, same ordinal, DIFFERENT names. The matcher refuses...
	if fieldValueMatchesAggregateGroupKey(aggkChildless("A", 0), aggkChildless("B", 0), agg) {
		t.Fatal("matcher unexpectedly ignored the accessor name; this test's premise is stale")
	}
	// ...but semantic equality is ordinal-only, so the caller's OR still matches
	// and the refusal is unobservable.
	if !values.SemanticEqualsUnderAliasMap(aggkChildless("A", 0), aggkChildless("B", 0), am) {
		t.Fatal("semantic equality on baked FieldValues is no longer ordinal-only: " +
			"the name check at the fieldValueMatchesAggregateGroupKey accessor loop is now REACHABLE " +
			"for both-childless pairs and can refuse an ordinal-equal group-key bind")
	}

	// Same for both-QOV pairs.
	if !values.SemanticEqualsUnderAliasMap(
		aggkQualified("ORDERS", "A", 0), aggkQualified("ORDERS", "B", 0), am) {
		t.Fatal("semantic equality no longer covers both-QOV ordinal-equal pairs: " +
			"the accessor name check is now REACHABLE for qualified/qualified group-key binds")
	}

	// A multi-accessor nested path likewise compares ordinal-only.
	nx := &values.FieldValue{Field: "X", Resolved: &values.FieldPath{Accessors: []values.ResolvedAccessor{
		{Field: "REC", Ordinal: 0}, {Field: "X", Ordinal: -1},
	}}}
	ny := &values.FieldValue{Field: "Y", Resolved: &values.FieldPath{Accessors: []values.ResolvedAccessor{
		{Field: "REC", Ordinal: 0}, {Field: "Y", Ordinal: -1},
	}}}
	if !values.SemanticEqualsUnderAliasMap(nx, ny, am) {
		t.Fatal("semantic equality no longer covers multi-accessor ordinal-equal pairs")
	}
}

// TestAggregateGroupKeyNameCheckDecidesOnlyAsymmetricShape pins that the
// childless-vs-qualified arm — the one shape semantic equality rejects — is
// where the name check is the sole arbiter, and that a real-ordinal pair there
// is name-equal by construction (N2), so it resolves to a MATCH in practice.
func TestAggregateGroupKeyNameCheckDecidesOnlyAsymmetricShape(t *testing.T) {
	t.Parallel()

	agg := aggkOneSource()
	am := values.AliasMap{}

	bare := aggkChildless("A", 0)
	qual := aggkQualified("ORDERS", "A", 0)

	// Semantic equality rejects the asymmetric pair — this is the matcher's
	// whole reason to exist.
	if values.SemanticEqualsUnderAliasMap(bare, qual, am) {
		t.Fatal("semantic equality now accepts the childless-vs-qualified pair; " +
			"fieldValueMatchesAggregateGroupKey's relaxation is redundant and its name check " +
			"is fully dead — re-derive this file's reasoning")
	}
	// The matcher accepts it, because the resolver mints both from the same
	// (name, ordinal) pair over one source. This is the real production shape
	// of `SELECT a + 1, COUNT(*) FROM t GROUP BY t.a`.
	if !fieldValueMatchesAggregateGroupKey(bare, qual, agg) {
		t.Fatal("qualified/bare group-key reference over one source no longer binds")
	}
}

// TestAggregateGroupKeyNameCheckIsLoadBearingAtNegativeOrdinal pins the reason
// this site must NOT be migrated to ordinal-only by deletion. Java forbids a
// negative ordinal (FieldValue.java:651); Go's name-only accessors carry -1, so
// ordinal equality is vacuous there and the name is the only discriminator.
func TestAggregateGroupKeyNameCheckIsLoadBearingAtNegativeOrdinal(t *testing.T) {
	t.Parallel()

	agg := aggkOneSource()

	// Two DIFFERENT name-only accessors. Their ordinals are equal (-1 == -1),
	// so only the name keeps them apart.
	if fieldValueMatchesAggregateGroupKey(
		aggkChildless("X", -1), aggkQualified("ORDERS", "Y", -1), agg) {
		t.Fatal("two name-only (Ordinal -1) accessors with DIFFERENT names matched: " +
			"ordinal equality is vacuous at -1, so this is a conflation of two distinct " +
			"nested fields and binds a group-key reference to the wrong column. " +
			"If the accessor name check was removed to satisfy RFC-197, it must be replaced " +
			"by a domain-explicit accessor that rejects negative ordinals, not deleted")
	}
	// Same name at -1 still matches, so the check discriminates rather than
	// blanket-refusing every name-only path.
	if !fieldValueMatchesAggregateGroupKey(
		aggkChildless("X", -1), aggkQualified("ORDERS", "X", -1), agg) {
		t.Fatal("identical name-only accessors no longer match")
	}
}
