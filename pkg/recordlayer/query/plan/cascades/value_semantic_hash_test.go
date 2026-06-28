package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestValueSemanticHashCode_AliasInvariant pins the central RFC-040 040.0
// property: correlation-bearing values hash IDENTICALLY regardless of which
// quantifier alias they reference — so the hash is consistent with the
// alias-AWARE ValueSemanticEquals (QOV equality goes through the
// AliasMapValueEquivalence fallback).
func TestValueSemanticHashCode_AliasInvariant(t *testing.T) {
	t.Parallel()
	qa := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("q_a"))
	qb := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("q_b"))
	if values.SemanticHashCode(qa) != values.SemanticHashCode(qb) {
		t.Fatal("QOVs with different aliases must hash equal (alias-invariant)")
	}
	// And they ARE semantically equal under an alias map binding q_a↦q_b —
	// so equal-hash is REQUIRED for hash-gated dedup to ever compare them.
	veq := NewAliasMapValueEquivalence(AliasMapOfAliases(
		values.NamedCorrelationIdentifier("q_a"), values.NamedCorrelationIdentifier("q_b")))
	if !ValueSemanticEquals(qa, qb, veq).IsTrue() {
		t.Fatal("precondition: QOVs must be veq-equal under the alias map")
	}

	// FieldValue over QOV: field path same, alias-bearing child excluded.
	fa := &values.FieldValue{Field: "x", Typ: values.UnknownType, Child: qa}
	fb := &values.FieldValue{Field: "x", Typ: values.UnknownType, Child: qb}
	if values.SemanticHashCode(fa) != values.SemanticHashCode(fb) {
		t.Fatal("FieldValue over alias-variant QOV must hash equal")
	}

	// Negative: different field path ⇒ different hash.
	fc := &values.FieldValue{Field: "y", Typ: values.UnknownType, Child: qa}
	if values.SemanticHashCode(fa) == values.SemanticHashCode(fc) {
		t.Fatal("different field paths must hash differently")
	}
}
