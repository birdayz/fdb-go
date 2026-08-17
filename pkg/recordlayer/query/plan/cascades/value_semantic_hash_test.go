package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func semanticHashRowType() *values.RecordType {
	nested := values.NewRecordType("semantic_hash_nested", false, []values.Field{
		{Name: "LEAF", FieldType: values.NotNullLong},
	})
	return values.NewRecordType("semantic_hash_row", false, []values.Field{
		{Name: "F", FieldType: values.NotNullLong},
		{Name: "G", FieldType: values.NotNullLong},
		{Name: "NESTED", FieldType: nested},
	})
}

func requireSemanticHashQOV(
	t testing.TB,
	alias values.CorrelationIdentifier,
) values.QuantifiedObjectValue {
	t.Helper()
	qov, err := values.NewQuantifiedObjectValue(alias, semanticHashRowType())
	if err != nil {
		t.Fatalf("construct exact semantic-hash QOV: %v", err)
	}
	return qov
}

func requireSemanticHashField(
	t testing.TB,
	alias values.CorrelationIdentifier,
	path ...string,
) values.Value {
	t.Helper()
	requests := make([]values.FieldRequest, len(path))
	for i, name := range path {
		request, err := values.FieldByName(name)
		if err != nil {
			t.Fatalf("construct semantic-hash field request %q: %v", name, err)
		}
		requests[i] = request
	}
	field, err := values.ResolveFieldAccess(requireSemanticHashQOV(t, alias), requests)
	if err != nil {
		t.Fatalf("resolve exact semantic-hash field %v: %v", path, err)
	}
	return field
}

// TestValueSemanticHashCode_AliasInvariant pins the central RFC-040 040.0
// property: correlation-bearing values hash IDENTICALLY regardless of which
// quantifier alias they reference — so the hash is consistent with the
// alias-AWARE ValueSemanticEquals (QOV equality goes through the
// AliasMapValueEquivalence fallback).
func TestValueSemanticHashCode_AliasInvariant(t *testing.T) {
	t.Parallel()
	aliasA := values.NamedCorrelationIdentifier("q_a")
	aliasB := values.NamedCorrelationIdentifier("q_b")
	qa := requireSemanticHashQOV(t, aliasA)
	qb := requireSemanticHashQOV(t, aliasB)
	if values.SemanticHashCode(qa) != values.SemanticHashCode(qb) {
		t.Fatal("QOVs with different aliases must hash equal (alias-invariant)")
	}
	// And they ARE semantically equal under an alias map binding q_a↦q_b —
	// so equal-hash is REQUIRED for hash-gated dedup to ever compare them.
	veq := NewAliasMapValueEquivalence(AliasMapOfAliases(
		aliasA, aliasB))
	if !ValueSemanticEquals(qa, qb, veq).IsTrue() {
		t.Fatal("precondition: QOVs must be veq-equal under the alias map")
	}

	// Exact FieldValue over QOV: root layout and field path are identical;
	// only the alias-bearing child changes.
	fa := requireSemanticHashField(t, aliasA, "F")
	fb := requireSemanticHashField(t, aliasB, "F")
	if !ValueSemanticEquals(fa, fb, veq).IsTrue() {
		t.Fatal("precondition: exact fields must be veq-equal under the alias map")
	}
	if values.SemanticHashCode(fa) != values.SemanticHashCode(fb) {
		t.Fatal("FieldValue over alias-variant QOV must hash equal")
	}

	// Negative: different field path ⇒ different hash.
	fc := requireSemanticHashField(t, aliasA, "G")
	if values.SemanticHashCode(fa) == values.SemanticHashCode(fc) {
		t.Fatal("different field paths must hash differently")
	}

	// Exact root layout is part of QOV and admitted FieldValue identity. A
	// same-ordinal field from a differently typed row must not share its hash
	// merely because both paths begin at slot zero.
	foreignRow := values.NewRecordType("semantic_hash_foreign", false, []values.Field{
		{Name: "F", FieldType: values.NotNullString},
		{Name: "G", FieldType: values.NotNullLong},
		{Name: "NESTED", FieldType: values.NewRecordType("semantic_hash_nested", false, []values.Field{
			{Name: "LEAF", FieldType: values.NotNullLong},
		})},
	})
	foreignQOV, err := values.NewQuantifiedObjectValue(aliasA, foreignRow)
	if err != nil {
		t.Fatalf("construct foreign exact QOV: %v", err)
	}
	request, err := values.FieldByName("F")
	if err != nil {
		t.Fatalf("construct foreign field request: %v", err)
	}
	foreignField, err := values.ResolveFieldAccess(
		foreignQOV, []values.FieldRequest{request})
	if err != nil {
		t.Fatalf("resolve foreign exact field: %v", err)
	}
	if values.SemanticHashCode(fa) == values.SemanticHashCode(foreignField) {
		t.Fatal("same ordinal in different exact row layouts must hash differently")
	}
}
