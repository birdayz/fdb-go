package embedded

// What fieldValueMatchesAggregateGroupKey compares, and what re-arms each
// direction of the fail-close it now relies on.
//
// The matcher used to AND a per-accessor DISPLAY-NAME check onto ordinal
// equality. Java's ResolvedAccessor.equals is ordinal-only
// (FieldValue.java:676-685) and RFC-197 makes that the rule, so the name check
// was a divergence in two directions at once, and both are pinned below.
//
// REFUSING direction (the defect it carried): two references that agree on the
// whole ordinal path in the same layout ARE the same column, whatever display
// name each happens to render. Refusing them turned a legitimate group-key
// reference into the caller's "references a field outside the aggregate output
// contract" error. Latent rather than shipped — hard-wiring the matcher to
// `return false` leaves the whole //pkg/relational/sqldriver suite green, so no
// SQL this repo exercises reaches the relaxation at all — but latent is how the
// original seven shipped.
//
// ACCEPTING direction (what the name was load-bearing for): Java asserts
// ordinal >= 0 at construction (FieldValue.java:651); Go mints Ordinal -1
// name-only accessors for array/unnest descent (cascades_translator.go:1910-1912,
// unnest_seed.go:177, unnest_gather.go:189, index_expansion.go:491). Two of those
// are ordinal-equal BY CONSTRUCTION, so an ordinal-only comparison over them is
// vacuous and would conflate two distinct nested fields. values.SameColumnPath
// declines a negative ordinal outright, which covers the hazard without
// consulting a name — the fail-close, not the display string, is what makes the
// wrong-column bind unreachable now.
//
// N1 — the matcher is DEAD wherever both sides share a child shape. All three
// call sites OR it with values.SemanticEqualsUnderAliasMap, which is ordinal-only
// on baked FieldValues, so both-childless and both-QOV ordinal-equal pairs are
// matched by the OR's first arm before the matcher runs. The matcher exists only
// for the ASYMMETRIC shape (one side childless, one side QOV-qualified) that
// semantic equality rejects.
//
// N2 — production operands carry a domain. Both resolver arms
// (expr.go:277-284 correlated, expr.go:296-297 bare) mint the ordinal and the
// OrdinalDomain from the SAME declared column list in one call to
// sourceColumnOrdinal, so the asymmetric pair the matcher exists for agrees on
// the domain by construction. A fixture without one would be testing a shape the
// resolver does not produce, so every fixture here states its layout.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

func aggkOneSource() *logical.LogicalAggregate {
	return &logical.LogicalAggregate{Input: logical.NewScan("orders", "")}
}

// aggkDomain is the layout a one-source resolution states — the source's
// declared column order, exactly as sourceColumnOrdinal derives it
// (expr.go:334-338).
func aggkDomain(cols ...string) values.OrdinalDomain {
	return values.OrdinalDomainOfColumnNames(cols)
}

// aggkChildless is the source-relative bake shape a bare group-key reference
// carries (expr.go:296-297) — no QuantifiedObjectValue child.
func aggkChildless(name string, ordinal int, domain values.OrdinalDomain) *values.FieldValue {
	return &values.FieldValue{
		Field:    name,
		Resolved: values.NewFieldPathOfSingleInDomain(name, ordinal, false, domain),
	}
}

// aggkQualified is the shape a QUALIFIED reference carries (expr.go:278-284) —
// the same resolved path, in the same domain, under a QOV child.
func aggkQualified(corr, name string, ordinal int, domain values.OrdinalDomain) *values.FieldValue {
	return &values.FieldValue{
		Field:    name,
		Child:    values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(corr)),
		Resolved: values.NewFieldPathOfSingleInDomain(name, ordinal, false, domain),
	}
}

// TestAggregateGroupKeyMatcherIsDeadUnderSemanticEquality pins N1: the OR at the
// call sites decides every symmetric shape before the matcher is consulted. If
// semantic equality on baked FieldValues ever stops being ordinal-only, the
// matcher becomes reachable in shapes N2 does not cover.
func TestAggregateGroupKeyMatcherIsDeadUnderSemanticEquality(t *testing.T) {
	t.Parallel()

	am := values.AliasMap{}
	dom := aggkDomain("A", "B")

	// Both childless, same ordinal — semantic equality is ordinal-only, so the
	// caller's OR matches whatever the matcher answers.
	if !values.SemanticEqualsUnderAliasMap(
		aggkChildless("A", 0, dom), aggkChildless("A", 0, dom), am) {
		t.Fatal("semantic equality on baked FieldValues is no longer ordinal-only: " +
			"fieldValueMatchesAggregateGroupKey is now REACHABLE for both-childless pairs")
	}
	// Same for both-QOV pairs.
	if !values.SemanticEqualsUnderAliasMap(
		aggkQualified("ORDERS", "A", 0, dom), aggkQualified("ORDERS", "A", 0, dom), am) {
		t.Fatal("semantic equality no longer covers both-QOV ordinal-equal pairs: " +
			"the matcher is now REACHABLE for qualified/qualified group-key binds")
	}
}

// TestAggregateGroupKeyMatcherDecidesOnlyAsymmetricShape pins that the
// childless-vs-qualified arm — the one shape semantic equality rejects — is the
// matcher's whole reason to exist, and that the production pair binds there.
func TestAggregateGroupKeyMatcherDecidesOnlyAsymmetricShape(t *testing.T) {
	t.Parallel()

	agg := aggkOneSource()
	am := values.AliasMap{}
	dom := aggkDomain("A", "B")

	bare := aggkChildless("A", 0, dom)
	qual := aggkQualified("ORDERS", "A", 0, dom)

	if values.SemanticEqualsUnderAliasMap(bare, qual, am) {
		t.Fatal("semantic equality now accepts the childless-vs-qualified pair; " +
			"fieldValueMatchesAggregateGroupKey's relaxation is redundant — re-derive this file")
	}
	// The real production shape of `SELECT a + 1, COUNT(*) FROM t GROUP BY t.a`.
	if !fieldValueMatchesAggregateGroupKey(bare, qual, agg) {
		t.Fatal("qualified/bare group-key reference over one source no longer binds")
	}
}

// TestAggregateGroupKeyMatcherIgnoresDisplayName is the REFUSING direction: the
// same slot of the same layout, rendered under two different display names, is
// one column. The old per-accessor name check refused this pair — an aliased
// reference to the resolved column it names, reported to the user as a field
// outside the aggregate output contract.
//
// This is the dimension the whole site was unprobed on: every other case here
// holds the name equal, so none of them can tell an ordinal match from a name
// match.
func TestAggregateGroupKeyMatcherIgnoresDisplayName(t *testing.T) {
	t.Parallel()

	agg := aggkOneSource()
	dom := aggkDomain("A", "B")

	// Ordinal 0 of the same layout, two display names.
	if !fieldValueMatchesAggregateGroupKey(
		aggkChildless("ALIASED", 0, dom), aggkQualified("ORDERS", "A", 0, dom), agg) {
		t.Fatal("an ordinal-equal group-key reference was refused because its DISPLAY name " +
			"differs: the leaf name is deciding column identity again (RFC-197). Java's " +
			"ResolvedAccessor.equals is getOrdinal()-only (FieldValue.java:676-685)")
	}
}

// TestAggregateGroupKeyMatcherSeparatesDomains is the other half of the same
// dimension: the same ordinal under two DIFFERENT layouts is two different
// columns, and a name check could never have seen the difference — both sides
// here carry the SAME leaf name, so only the domain keeps them apart.
func TestAggregateGroupKeyMatcherSeparatesDomains(t *testing.T) {
	t.Parallel()

	agg := aggkOneSource()

	// Ordinal 0 named A in two different declared column orders.
	orders := aggkDomain("A", "B")
	lineitems := aggkDomain("A", "C", "D")
	if fieldValueMatchesAggregateGroupKey(
		aggkChildless("A", 0, orders), aggkQualified("ORDERS", "A", 0, lineitems), agg) {
		t.Fatal("ordinal 0 of two DIFFERENT layouts matched: an ordinal is only comparable " +
			"against a stated layout (RFC-197 step 0), and an ordinal conflation reads as " +
			"authoritative where a name conflation reads as suspect")
	}

	// An UNSTATED layout on either side declines rather than assuming.
	unknown := values.OrdinalDomain{}
	if fieldValueMatchesAggregateGroupKey(
		aggkChildless("A", 0, unknown), aggkQualified("ORDERS", "A", 0, orders), agg) {
		t.Fatal("a reference whose layout is unknown matched: an unstated domain must fail closed")
	}
}

// TestAggregateGroupKeyMatcherFailsClosedAtNegativeOrdinal pins the direction
// the deleted name check was load-bearing for. Java forbids a negative ordinal
// (FieldValue.java:651); Go's name-only accessors carry -1, so ordinal equality
// is vacuous there. The matcher now declines every such path — INCLUDING the
// same-named one, which the name check used to accept.
//
// Declining the same-named pair costs a missed relaxation on a shape no covered
// query reaches; accepting the different-named pair would be a wrong-column
// bind. That is the fail-closed trade, and both halves are pinned so neither can
// be "fixed" back without re-deriving it.
func TestAggregateGroupKeyMatcherFailsClosedAtNegativeOrdinal(t *testing.T) {
	t.Parallel()

	agg := aggkOneSource()
	dom := aggkDomain("A", "B")

	// Two DIFFERENT name-only accessors. Ordinals are equal (-1 == -1), so
	// nothing but the fail-close keeps them apart.
	if fieldValueMatchesAggregateGroupKey(
		aggkChildless("X", -1, dom), aggkQualified("ORDERS", "Y", -1, dom), agg) {
		t.Fatal("two name-only (Ordinal -1) accessors matched: ordinal equality is vacuous " +
			"at -1, so this conflates two distinct nested fields and binds a group-key " +
			"reference to the wrong column. values.SameColumnPath must decline a negative " +
			"ordinal — that decline is what replaced the accessor name check")
	}
	// The same-named pair declines too: the matcher no longer has a name to
	// tell it apart with, and guessing is the failure this fail-close prevents.
	if fieldValueMatchesAggregateGroupKey(
		aggkChildless("X", -1, dom), aggkQualified("ORDERS", "X", -1, dom), agg) {
		t.Fatal("a name-only (Ordinal -1) path matched on its display name: the migration " +
			"replaced the name check with a fail-close, so this pair must DECLINE")
	}
}

// TestAggregateGroupKeyMatcherComparesEveryAccessor pins that a nested path is
// compared at every step, not just at the leaf — the multi-accessor shape the
// old loop covered and the new one must keep covering.
func TestAggregateGroupKeyMatcherComparesEveryAccessor(t *testing.T) {
	t.Parallel()

	agg := aggkOneSource()
	dom := aggkDomain("ADDR", "SHIPADDR")

	nested := func(rootOrd, leafOrd int) *values.FieldValue {
		return &values.FieldValue{
			Field: "CITY",
			Resolved: &values.FieldPath{
				Accessors: []values.ResolvedAccessor{
					{Field: "ADDR", Ordinal: rootOrd},
					{Field: "CITY", Ordinal: leafOrd},
				},
				Domain: dom,
			},
		}
	}
	qualNested := func(rootOrd, leafOrd int) *values.FieldValue {
		v := nested(rootOrd, leafOrd)
		v.Child = values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("ORDERS"))
		return v
	}

	if !fieldValueMatchesAggregateGroupKey(nested(0, 3), qualNested(0, 3), agg) {
		t.Fatal("an identical two-accessor path no longer binds")
	}
	// Same leaf ordinal, different ROOT: addr.city vs shipaddr.city.
	if fieldValueMatchesAggregateGroupKey(nested(0, 3), qualNested(1, 3), agg) {
		t.Fatal("two nested paths differing only at the ROOT accessor matched: every step " +
			"of the path is the identity, not just the leaf")
	}
}
