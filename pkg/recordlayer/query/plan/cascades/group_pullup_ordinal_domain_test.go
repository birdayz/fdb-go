package cascades

// The group-by pull-up walk rebuilds a decomposed reference step by step. Each
// rebuilt step must state the layout its ordinal indexes, and it can DERIVE
// that layout instead of minting the unknown token: the step reads a field out
// of the value it is being applied to, so the frontier is that value's own
// result type (RFC-197's working rule — derive when the child is typed, store
// only when it is not; the reason Java needs no token at all is that its
// FieldValue.childValue is non-null and typed).
//
// Minting unknown here is not a wrong answer, it is a SILENT one: the rebuilt
// reference fails OrdinalIn forever after, so every downstream domain-checked
// site declines a push it could have proven. That is the shape this file pins.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestGroupPullUpFieldPath_DerivesDomainFromTypedBase(t *testing.T) {
	t.Parallel()

	layout := testRecordRowType("T", "ID", "ADDR", "CITY")
	base := values.NewQuantifiedObjectValueOfType(
		values.NamedCorrelationIdentifier("upper"), layout)

	rebuilt := applyGroupFieldPath(base, []groupFieldPathStep{{
		field:    "CITY",
		ordinal:  2,
		resolved: true,
	}}, values.UnknownType)

	fv, ok := rebuilt.(*values.FieldValue)
	if !ok {
		t.Fatalf("rebuilt reference is %T, want *values.FieldValue", rebuilt)
	}
	if fv.Resolved == nil {
		t.Fatal("a resolved step must rebuild as a BAKED reference")
	}
	ord, answered := fv.OrdinalIn(values.OrdinalDomainOfType(layout))
	if !answered {
		t.Fatalf("the rebuilt reference states no usable layout (%v) — its ordinal indexes the "+
			"base's own type, which is right here in hand, so deriving it is free and minting "+
			"unknown silently disarms every downstream domain check", fv.Resolved.Domain)
	}
	if ord != 2 {
		t.Fatalf("derived ordinal = %d, want 2 (CITY's slot in the base's layout)", ord)
	}
}

func TestGroupPullUpFieldPath_UntypedBaseStillFailsClosed(t *testing.T) {
	t.Parallel()

	// Derivation is not invention: a base with no column order names no
	// layout, so the rebuilt step keeps the UNKNOWN token and OrdinalIn
	// declines it. Stating a layout here would be the ordinal conflation the
	// token exists to prevent, wearing a proof's clothes.
	base := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("upper"))

	rebuilt := applyGroupFieldPath(base, []groupFieldPathStep{{
		field:    "CITY",
		ordinal:  2,
		resolved: true,
	}}, values.UnknownType)

	fv, ok := rebuilt.(*values.FieldValue)
	if !ok {
		t.Fatalf("rebuilt reference is %T, want *values.FieldValue", rebuilt)
	}
	if fv.Resolved == nil {
		t.Fatal("a resolved step must rebuild as a BAKED reference")
	}
	if fv.Resolved.Domain.IsKnown() {
		t.Fatalf("an untyped base must yield the UNKNOWN token, got %v", fv.Resolved.Domain)
	}
	// And it is unanswerable against any layout, including one that happens to
	// place a column at ordinal 2.
	if _, answered := fv.OrdinalIn(values.OrdinalDomainOfType(
		testRecordRowType("T", "ID", "ADDR", "CITY"))); answered {
		t.Fatal("an unknown domain must never answer OrdinalIn")
	}
}

func TestGroupPullUpFieldPath_FusedStepKeepsTheReceiversDomain(t *testing.T) {
	t.Parallel()

	// Fusing an outer step onto an inner baked path keeps the INNER path's
	// root read context, so the derived token on the suffix is deliberately
	// discarded — the root still indexes the layout the inner reference
	// resolved against. Pinned because a fused path must not come out of this
	// walk claiming the nested record's layout for its root ordinal.
	layout := testRecordRowType("T", "ID", "ADDR")
	nested := testRecordRowType("ADDR", "STREET", "CITY")
	base := values.NewQuantifiedObjectValueOfType(
		values.NamedCorrelationIdentifier("upper"), layout)

	inner := values.NewCorrelatedFieldValueWithResolvedOrdinalInDomain(
		base, "ADDR", 1, nested, values.OrdinalDomainOfType(layout))

	rebuilt := applyGroupFieldPath(inner, []groupFieldPathStep{{
		field:    "CITY",
		ordinal:  1,
		resolved: true,
	}}, values.UnknownType)

	fv, ok := rebuilt.(*values.FieldValue)
	if !ok || fv.Resolved == nil {
		t.Fatalf("rebuilt reference is %#v, want a baked *values.FieldValue", rebuilt)
	}
	if len(fv.Resolved.Accessors) != 2 {
		t.Fatalf("fused path has %d accessors, want 2", len(fv.Resolved.Accessors))
	}
	if fv.Resolved.Domain != values.OrdinalDomainOfType(layout) {
		t.Fatalf("fused path's domain = %v, want the RECEIVER's layout %v",
			fv.Resolved.Domain, values.OrdinalDomainOfType(layout))
	}
	// A fused path still declines OrdinalIn — a different arm, and it must not
	// start answering just because the token is now known.
	if _, answered := fv.OrdinalIn(values.OrdinalDomainOfType(layout)); answered {
		t.Fatal("a multi-accessor path must never answer OrdinalIn")
	}
}
