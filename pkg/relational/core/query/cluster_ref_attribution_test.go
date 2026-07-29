package query

// The clustered-outer correlated-scalar path attributes a reference to an outer
// leg by its CORRELATION. Both consumers — the pull-up bake and the outer-ref
// classifier — used to accept a second channel: a childless FieldValue whose
// name was sliced at the first dot, with the prefix taken for a leg alias.
//
// A string cannot carry that fact. `A.B` as a qualified reference and `"A.B"`
// as one quoted column name are the same bytes (the quoted form is supported
// and separately pinned by TestJoinDerivedDottedName_OrdinalUnshifted), so the
// slice cannot tell a reference to leg A from a column the inner's own source
// declares — and the bake, believing it a reference, rebinds it onto leg A's
// row. Nor is the input restricted to references at all: the one childless
// dotted value the classifier actually meets across the FDB suite is a rendered
// aggregate output name, `SUM(AMOUNT+E.REF)`, out of which the first-dot slice
// manufactures the leg alias `SUM(AMOUNT+E`.
//
// These pins are white-box because the defect is invisible from the row side:
// the misattribution needs a column whose name collides with a live leg
// qualifier, and every covered query resolves the same either way.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// dottedNameValue is a lazy childless read of a column whose NAME contains a
// dot — the shape a quoted `"O.ORDER_ID"` identifier produces, and the shape a
// rendered expression name produces.
func dottedNameValue(name string) values.Value {
	return &values.FieldValue{Field: name, Typ: values.NotNullLong}
}

// TestClusterBake_DoesNotAttributeAChildlessDottedName pins that the pull-up
// leaves a childless dotted name alone. "O.ORDER_ID" names leg O's column
// ORDER_ID if read as a qualifier and something else entirely if read as a
// name; with no correlation on the value there is nothing to decide it with, so
// the only safe rewrite is none.
func TestClusterBake_DoesNotAttributeAChildlessDottedName(t *testing.T) {
	t.Parallel()
	tr := newGateTranslator(t)

	pu := tr.buildClusterPullUp(inner(scan("Order", "o"), scan("Customer", "c")))
	if pu == nil {
		t.Fatal("pull-up spine")
	}
	// Guard the fixture: O must really be a leg carrying ORDER_ID, or the test
	// would pass for the wrong reason (nothing to misattribute to).
	leg, isLeg := pu.legByBinding["O"]
	if !isLeg {
		t.Fatal("fixture: O is not a leg — nothing for a sliced qualifier to hit")
	}
	if _, found := leg.typ.FieldIndex("ORDER_ID"); !found {
		t.Fatal("fixture: leg O has no ORDER_ID — nothing for a sliced qualifier to hit")
	}

	in := dottedNameValue("O.ORDER_ID")
	out := pu.bake(in)
	if out != in {
		t.Fatalf("bake rewrote a childless dotted name to %#v — it read `O.ORDER_ID` as a qualified reference to leg O, which is indistinguishable from a column so named", out)
	}
	if pu.missed {
		t.Error("bake flagged missed for a value it cannot attribute — an unattributable value is not a failed leg reference")
	}

	// The structural twin must still bake: the rule is "attribution needs a
	// correlation", not "stop baking".
	structural := values.NewFieldValue(
		values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("o")),
		"ORDER_ID", values.NotNullLong)
	baked, isFV := pu.bake(structural).(*values.FieldValue)
	if !isFV || baked.Resolved == nil {
		t.Fatalf("bake(QOV(o).ORDER_ID) = %#v, want an ordinal bake — the correlation-carrying reference is the one that IS attributable", pu.bake(structural))
	}
}

// TestClusterOuterRefs_DoesNotAttributeAChildlessDottedName pins the classifier
// half. Its verdict decides whether a query is declined loudly as
// non-rightmost-correlated, so an invented leg alias turns into a rejected
// query that has no outer correlation at all.
func TestClusterOuterRefs_DoesNotAttributeAChildlessDottedName(t *testing.T) {
	t.Parallel()

	outer := map[string]struct{}{"C": {}, "E": {}}
	skip := map[string]struct{}{"SQ": {}}

	for _, tc := range []struct {
		name  string
		field string
	}{
		// A quoted column name that happens to start with an outer alias.
		{"quoted dotted column name", "C.CUSTOMER_ID"},
		// The shape measured in production: a rendered aggregate output name.
		// Its first dot sits inside the operand, so the sliced "qualifier" is
		// `SUM(AMOUNT+E` — not any alias, which is luck, not a guard.
		{"rendered aggregate output name", "SUM(AMOUNT+E.REF)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			op := logical.NewFilterWithPredicate(scan("Order", "SQ"),
				&predicates.ComparisonPredicate{
					Operand: dottedNameValue(tc.field),
					Comparison: predicates.Comparison{
						Type:    predicates.ComparisonEquals,
						Operand: &values.ConstantValue{Value: int64(1)},
					},
				}, "")
			refs, exhaustive := collectClusterOuterRefs(op, outer, skip)
			if !exhaustive {
				t.Fatal("fixture: the carrier must be enumerated, or the assertion below is vacuous")
			}
			if len(refs) != 0 {
				t.Fatalf("collectClusterOuterRefs attributed %v to an outer leg from a childless name — a leg alias read out of display text", refs)
			}
		})
	}

	// And the structural reference is still collected: removing the string
	// channel must not blind the classifier to a real outer-leg reference,
	// which is the direction that would cost a loud decline and yield silent
	// wrong rows instead.
	structuralOp := logical.NewFilterWithPredicate(scan("Order", "SQ"),
		&predicates.ComparisonPredicate{
			Operand: values.NewFieldValue(
				values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("c")),
				"CUSTOMER_ID", values.NotNullLong),
			Comparison: predicates.Comparison{
				Type:    predicates.ComparisonEquals,
				Operand: &values.ConstantValue{Value: int64(1)},
			},
		}, "")
	refs, _ := collectClusterOuterRefs(structuralOp, outer, skip)
	if _, hit := refs["C"]; !hit {
		t.Fatalf("collectClusterOuterRefs(%v) lost the correlation-carrying outer-leg reference — the decline guard would go silent", refs)
	}
}

// TestLegRef_DeclinesAMergedRowQualifiedRead pins the OTHER direction of the
// same question, on the one dot-probe that must stay.
//
// `legRef` answers "does this value read leg L directly?" and it asks the
// value's QuantifiedObjectValue child — the correct channel. But it first
// declines any value whose name contains a dot, and that guard is load-bearing
// rather than defensive: the merged-row channel mints
// `FieldValue{Field:"A.ID", Child:QOV(S)}`, where the child names the MERGED
// row S and the name carries the leg A. Ask the child alone and the answer is
// S — the merged correlation reported as a direct leg read, which is the
// conflation this whole workstream exists to remove.
//
// So the dot probe is not a reader to convert; it is the mitigation for a
// producer that packs a leg into a string, and it can only be deleted after
// that producer is gone. Nothing pinned it, which meant "remove the last dot
// probes" read as safe cleanup at exactly the site where it silently misbinds.
// The producer conversion is booked; this is what re-arms if the guard goes
// first.
func TestLegRef_DeclinesAMergedRowQualifiedRead(t *testing.T) {
	t.Parallel()

	mergedQOV := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("S"))

	// The merged-row shape: name says leg A, child says merged row S.
	merged := &values.FieldValue{Field: "A.ID", Child: mergedQOV, Typ: values.NotNullLong}
	if key, ok := legRef(merged); ok {
		t.Fatalf("legRef attributed the merged-row read {Field:%q, Child:QOV(S)} to leg %q — "+
			"the child names the MERGED row, not a leg, so any non-empty answer here is the "+
			"merged correlation misreported as a direct leg reference", merged.Field, key)
	}

	// The shape it MUST still accept, so the guard is not merely a blanket
	// refusal: an unqualified read whose child names the leg itself.
	direct := &values.FieldValue{
		Field: "ID",
		Child: values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("A")),
		Typ:   values.NotNullLong,
	}
	key, ok := legRef(direct)
	if !ok || key != "A" {
		t.Fatalf("legRef(%q over QOV(A)) = (%q, %v), want (\"A\", true) — a guard that also "+
			"declines genuine leg reads is not a mitigation, it is a hole", direct.Field, key, ok)
	}
}
