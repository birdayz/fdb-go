package query

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// legRef's exclusion is MACHINERY-OWNERSHIP, and machinery-ownership is the
// FRONTIER PIN. It used to be spelled "FrontierPinned OR multi-accessor", on the
// reading that only the walk's own output is multi-accessor — and that reading
// was refuted by trace: a user-written nested descent (`m.n.sk`) arrives as ONE
// UNPINNED FieldValue with a leg-relative root and the descent in its remaining
// accessors, so the arity half declined exactly the references the walk exists
// to rebase.
//
// legRef is the shared prologue of THREE walks — the bake closure,
// predicateLegAliases and predicateRefsBuriedLeg — so the fix widened all three
// at once. This drives each of them directly rather than inferring the widening
// from the end-to-end query, because two of the three are COUNTERS whose move is
// invisible in a row set: a nested reference now counts toward the cross-leg
// test and toward the buried-leg test, which is what makes the conjunct reach
// the bake at all.
//
// The pinned-decline arms are the other direction and they matter as much: if
// the pin stopped excluding, a re-walk would re-bake an address that already
// indexes the composed row — adding the leg offset a second time. The
// MULTI-ACCESSOR PINNED arm is the one the old spelling could not distinguish
// from a user nested ref, and it is the reason the replacement had to key on the
// pin rather than merely drop the arity clause.
func TestLegRef_AdmitsANestedUnpinnedRefAndStillDeclinesMachineryOutput(t *testing.T) {
	t.Parallel()

	nestedUnpinned := func(corr, root, leaf string) *values.FieldValue {
		return &values.FieldValue{
			Field: root,
			Child: values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(corr)),
			Typ:   values.NotNullLong,
			Resolved: &values.FieldPath{Accessors: []values.ResolvedAccessor{
				{Field: root, Ordinal: 2},
				{Field: leaf, Ordinal: 0},
			}},
		}
	}

	for _, tc := range []struct {
		name    string
		value   values.Value
		wantKey string
		wantOK  bool
	}{
		{
			// The shape the arity clause refused. Two accessors, UNPINNED root:
			// its root ordinal addresses M's own window, so it must be counted
			// and re-baked here exactly like its single-accessor twin.
			name:    "nested user descent over a leg QOV",
			value:   nestedUnpinned("M", "N", "SK"),
			wantKey: "M", wantOK: true,
		},
		{
			// The single-accessor twin, unchanged — the arm that always worked
			// and whose behaviour the widening must not disturb.
			name: "flat baked leg read",
			value: &values.FieldValue{
				Field: "SK",
				Child: values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("M")),
				Typ:   values.NotNullLong,
				Resolved: &values.FieldPath{Accessors: []values.ResolvedAccessor{
					{Field: "SK", Ordinal: 1},
				}},
			},
			wantKey: "M", wantOK: true,
		},
		{
			// The walk's OWN output: NewFieldValueOfOrdinal over a composed
			// frontier. Its ordinal already indexes that row.
			name: "machinery output, single accessor",
			value: &values.FieldValue{
				Field: "SK",
				Child: values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("M")),
				Typ:   values.NotNullLong,
				Resolved: &values.FieldPath{
					FrontierPinned: true,
					Accessors:      []values.ResolvedAccessor{{Field: "SK", Ordinal: 4}},
				},
			},
			wantOK: false,
		},
		{
			// FUSED machinery output. Multi-accessor AND pinned — under the old
			// spelling the arity clause carried this decline, so dropping arity
			// without keying on the pin would have re-baked it.
			name: "machinery output, fused two-step",
			value: &values.FieldValue{
				Field: "SK",
				Child: values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("M")),
				Typ:   values.NotNullLong,
				Resolved: &values.FieldPath{
					FrontierPinned: true,
					Accessors: []values.ResolvedAccessor{
						{Field: "N", Ordinal: 4},
						{Field: "SK", Ordinal: 0},
					},
				},
			},
			wantOK: false,
		},
		{
			// The merged-row qualified channel keeps its own decline: the child
			// names the MERGED row and the dot in the name carries the leg.
			name:   "flat-dotted merged-row read",
			value:  &values.FieldValue{Field: "M.N", Child: values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("S")), Typ: values.NotNullLong},
			wantOK: false,
		},
	} {
		key, ok := legRef(tc.value)
		if ok != tc.wantOK || (tc.wantOK && key != tc.wantKey) {
			t.Errorf("%s: legRef = (%q, %v), want (%q, %v)", tc.name, key, ok, tc.wantKey, tc.wantOK)
		}
	}

	// ---- The two COUNTERS legRef also feeds. Their move is what lets a
	// nested-only conjunct reach the bake instead of staying lazy for pushdown.
	legTyp := &values.RecordType{Fields: []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "SK", FieldType: values.NotNullLong, Ordinal: 1},
		{Name: "N", FieldType: values.NotNullLong, Ordinal: 2},
	}}

	crossLeg := &predicates.ComparisonPredicate{
		Operand: nestedUnpinned("M", "N", "SK"),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: nestedUnpinned("R", "N", "SK"),
		},
	}
	flatLegs := map[string]bakeLegType{
		"M": {typ: legTyp, leafTyp: legTyp},
		"R": {typ: legTyp, leafTyp: legTyp},
	}
	if got := predicateLegAliases(crossLeg, flatLegs); got != 2 {
		t.Errorf("predicateLegAliases over `m.n.sk = r.n.sk` = %d, want 2.\n"+
			"Below 2 the conjunct fails the cross-leg test and stays LAZY — which is how a "+
			"nested ON predicate reached execution still correlated to its leg", got)
	}

	buriedLegs := map[string]bakeLegType{
		// A BURIED leaf: no quantifier of its own, so a lazy reference to it has
		// nowhere to push into and MUST bake even as the only leg named.
		"M": {typ: legTyp, leafTyp: legTyp, bakeCorr: "BOX"},
		"R": {typ: legTyp, leafTyp: legTyp},
	}
	singleBuried := &predicates.ComparisonPredicate{
		Operand: nestedUnpinned("M", "N", "SK"),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(1)},
		},
	}
	if !predicateRefsBuriedLeg(singleBuried, buriedLegs) {
		t.Error("predicateRefsBuriedLeg missed a NESTED reference into a buried leg. " +
			"A buried reference left lazy evaluates QOV(buriedAlias) → unbound → NULL and " +
			"silently drops rows; the single-leg cross-leg test cannot catch it")
	}
}
