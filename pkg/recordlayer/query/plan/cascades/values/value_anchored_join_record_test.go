package values

import (
	"strings"
	"testing"
)

// TestNewAnchoredJoinRecord_ComposeResolvesEveryColumn pins RFC-077's name-miss guard: for
// every column the SARG/derivation pulls up, composeFieldOverConstructor over the anchored RC
// must resolve to the leg FieldValue (never nil — the simplifier_value.go silent-nil
// landmine). Unique columns resolve by bare name; cross-leg duplicates by ALIAS.COL.
func TestNewAnchoredJoinRecord_ComposeResolvesEveryColumn(t *testing.T) {
	t.Parallel()
	a := NamedCorrelationIdentifier("A")
	b := NamedCorrelationIdentifier("B")
	legs := []AnchoredJoinLeg{
		{Alias: a, Columns: []Field{{Name: "ID", FieldType: UnknownType}, {Name: "NAME", FieldType: UnknownType}}},
		{Alias: b, Columns: []Field{{Name: "ID", FieldType: UnknownType}, {Name: "AMOUNT", FieldType: UnknownType}}},
	}
	rc := NewAnchoredJoinRecord(legs)

	resolvesToLegField := func(field string) {
		t.Helper()
		got := composeFieldOverConstructor(NewFieldValue(rc, field, UnknownType))
		if got == nil {
			t.Fatalf("field %q must resolve over the anchored RC, got nil (silent-nil landmine)", field)
		}
		fv, ok := got.(*FieldValue)
		if !ok {
			t.Fatalf("field %q: expected a leg *FieldValue, got %T", field, got)
		}
		if _, ok := fv.Child.(*QuantifiedObjectValue); !ok {
			t.Fatalf("field %q: leg FieldValue must be anchored to a QuantifiedObjectValue, got child %T", field, fv.Child)
		}
	}

	// Unique columns resolve by bare name.
	resolvesToLegField("NAME")
	resolvesToLegField("AMOUNT")
	// Unique columns ALSO resolve by their qualified ALIAS.COL name — a qualified
	// reference (e.g. A.NAME) must compose even when the bare name is unique (codex
	// P2: the bare-only emission silently read NULL for such references).
	resolvesToLegField("A.NAME")
	resolvesToLegField("B.AMOUNT")
	// The cross-leg duplicate column ID is disambiguated as ALIAS.COL and each resolves.
	resolvesToLegField("A.ID")
	resolvesToLegField("B.ID")

	// Each duplicate's leg FieldValue is anchored to the RIGHT leg.
	aID := composeFieldOverConstructor(NewFieldValue(rc, "A.ID", UnknownType)).(*FieldValue)
	if qov, _ := aID.Child.(*QuantifiedObjectValue); qov == nil || qov.Correlation != a {
		t.Fatalf("A.ID must anchor to leg A, got %+v", aID.Child)
	}
	bID := composeFieldOverConstructor(NewFieldValue(rc, "B.ID", UnknownType)).(*FieldValue)
	if qov, _ := bID.Child.(*QuantifiedObjectValue); qov == nil || qov.Correlation != b {
		t.Fatalf("B.ID must anchor to leg B, got %+v", bID.Child)
	}
}

// TestNewAnchoredJoinRecord_EvaluatesNameKeyedRow pins that the anchored RC's Evaluate yields
// a column-named row (the SELECT */flow-through case where the RC survives to runtime).
func TestNewAnchoredJoinRecord_EvaluatesNameKeyedRow(t *testing.T) {
	t.Parallel()
	a := NamedCorrelationIdentifier("A")
	legs := []AnchoredJoinLeg{
		{Alias: a, Columns: []Field{{Name: "NAME", FieldType: UnknownType}}},
	}
	rc := NewAnchoredJoinRecord(legs)
	row := rc.Evaluate(staticBinder{a: map[string]any{"NAME": "alice"}})
	m, ok := row.(map[string]any)
	if !ok {
		t.Fatalf("anchored RC Evaluate must yield a name-keyed map, got %T", row)
	}
	if m["NAME"] != "alice" {
		t.Fatalf("expected NAME=alice in the anchored row, got %v", m)
	}
}

// TestNewAnchoredJoinRecord_NamingParityWithOpaqueMerge pins Graefe's binding
// condition 2 (RFC-077 v3): on a duplicate-bare-name multi-way join, the anchored
// RC's key set is EXACTLY the opaque JoinMergeAllValue's Evaluate key set — every
// bare key (last-leg-wins on a shared name) AND every qualified ALIAS.COL. The
// bare-dup key is NOT excluded: a quantifier OVER an inner join reuses the inner
// right leg's alias (sourceAlias(join) = right-leg alias), so a qualified predicate
// reads the join's merged row by the BARE key — which the opaque merge wrote and
// the RC must too, or 3+-way joins return 0 rows. Emitting bare-only-when-unique
// (an earlier cut) dropped exactly these keys; this is the swap-safety proof that
// every key the old merge produced still resolves over the anchored RC.
func TestNewAnchoredJoinRecord_NamingParityWithOpaqueMerge(t *testing.T) {
	t.Parallel()
	a := NamedCorrelationIdentifier("A")
	b := NamedCorrelationIdentifier("B")
	// A and B both carry a PRICE column (duplicate bare name) plus a unique key.
	aRow := map[string]any{"ID": int64(1), "PRICE": int64(10)}
	bRow := map[string]any{"CUSTOMER_ID": int64(2), "PRICE": int64(20)}

	// Opaque merge Evaluate key set (the behavior the anchored RC must match).
	mergeRow := NewJoinMergeAllValue(a, b).Evaluate(fakeCorrBinder{rows: map[CorrelationIdentifier]any{
		a: aRow, b: bRow,
	}}).(map[string]any)
	mergeKeys := map[string]bool{}
	for k := range mergeRow {
		mergeKeys[strings.ToUpper(k)] = true
	}

	// The anchored RC's key set: every field name (each must compose to a non-nil
	// leg FieldValue — the silent-nil landmine).
	legs := []AnchoredJoinLeg{
		{Alias: a, Columns: []Field{{Name: "ID"}, {Name: "PRICE"}}},
		{Alias: b, Columns: []Field{{Name: "CUSTOMER_ID"}, {Name: "PRICE"}}},
	}
	rc := NewAnchoredJoinRecord(legs)
	rcKeys := map[string]bool{}
	for _, f := range rc.Fields {
		if composeFieldOverConstructor(NewFieldValue(rc, f.Name, UnknownType)) == nil {
			t.Fatalf("anchored RC field %q does not compose (silent-nil landmine)", f.Name)
		}
		rcKeys[strings.ToUpper(f.Name)] = true
	}

	// The two key sets must be IDENTICAL (exact parity).
	for k := range mergeKeys {
		if !rcKeys[k] {
			t.Errorf("merge produces key %q but the anchored RC does not (a consumer read would silently break)", k)
		}
	}
	for k := range rcKeys {
		if !mergeKeys[k] {
			t.Errorf("anchored RC has key %q the merge never produced (spurious key)", k)
		}
	}
	// Sanity: the expected exact set, spelled out — qualified ALIAS.COL always,
	// bare always (PRICE = last-leg-wins = B's).
	want := map[string]bool{
		"A.ID": true, "ID": true,
		"A.PRICE": true, "B.PRICE": true, "PRICE": true, // dup bare PRICE present, last-wins
		"B.CUSTOMER_ID": true, "CUSTOMER_ID": true,
	}
	if len(rcKeys) != len(want) {
		t.Errorf("anchored RC key count = %d, want %d: got %v", len(rcKeys), len(want), rcKeys)
	}
	for k := range want {
		if !rcKeys[k] {
			t.Errorf("anchored RC missing expected key %q", k)
		}
	}
	// The bare PRICE (last-leg-wins) resolves to B's PRICE (the last leg) — the
	// last-wins semantics the opaque merge has.
	if got := composeFieldOverConstructor(NewFieldValue(rc, "PRICE", UnknownType)); got == nil {
		t.Error("bare PRICE must resolve (last-leg-wins, parity with the merge)")
	} else if fv, ok := got.(*FieldValue); !ok || fv.Child.(*QuantifiedObjectValue).Correlation != b {
		t.Errorf("bare PRICE must anchor to the LAST leg B (last-wins), got %+v", got)
	}
}

// TestNewAnchoredJoinRecord_DottedColumnPropagatesVerbatim pins the NESTED-join
// naming rule (RFC-077): an already-qualified (dotted) leg column propagates
// VERBATIM — the field name stays "A.ID", NOT re-qualified to "PARENT.A.ID", and
// the value reads it off the parent leg by that dotted key. This mirrors
// JoinMergeAllValue.Evaluate's "preserve already-qualified keys verbatim".
func TestNewAnchoredJoinRecord_DottedColumnPropagatesVerbatim(t *testing.T) {
	t.Parallel()
	parent := NamedCorrelationIdentifier("M") // a merge-quantifier parent leg
	c := NamedCorrelationIdentifier("C")
	legs := []AnchoredJoinLeg{
		// M is itself a join leg exposing already-qualified A.ID / B.ID.
		{Alias: parent, Columns: []Field{{Name: "A.ID"}, {Name: "B.ID"}}},
		{Alias: c, Columns: []Field{{Name: "CID"}}},
	}
	rc := NewAnchoredJoinRecord(legs)

	byName := map[string]*FieldValue{}
	for _, f := range rc.Fields {
		if fv, ok := f.Value.(*FieldValue); ok {
			byName[f.Name] = fv
		}
	}

	// "A.ID" must be present VERBATIM, not "M.A.ID".
	if _, ok := byName["A.ID"]; !ok {
		t.Fatalf("dotted column A.ID must propagate verbatim; got fields %v", fieldNames(rc))
	}
	if _, bad := byName["M.A.ID"]; bad {
		t.Error("dotted column must NOT be re-qualified to M.A.ID")
	}
	// Its value reads "A.ID" off the parent leg M.
	fv := byName["A.ID"]
	if fv.Field != "A.ID" {
		t.Errorf("A.ID value reads field %q, want A.ID (verbatim dotted key)", fv.Field)
	}
	if qov, _ := fv.Child.(*QuantifiedObjectValue); qov == nil || qov.Correlation != parent {
		t.Errorf("A.ID value must anchor to parent leg M, got %+v", fv.Child)
	}
	// A dotted column gets NO extra bare form (mirrors the merge: dotted keys are
	// written verbatim only). "ID" must not appear as a bare key.
	if _, bad := byName["ID"]; bad {
		t.Error("a dotted leg column must not also produce a bare ID key")
	}
	// The plain leg column C.CID is qualified + bare-unique as usual.
	if _, ok := byName["C.CID"]; !ok {
		t.Error("plain leg column should still get its qualified C.CID form")
	}
	if _, ok := byName["CID"]; !ok {
		t.Error("plain unique leg column should still get its bare CID form")
	}
}

func fieldNames(rc *RecordConstructorValue) []string {
	out := make([]string, len(rc.Fields))
	for i, f := range rc.Fields {
		out[i] = f.Name
	}
	return out
}

// staticBinder is a minimal CorrelationBinder for the Evaluate test.
type staticBinder map[CorrelationIdentifier]map[string]any

func (s staticBinder) GetCorrelationBinding(id CorrelationIdentifier) (any, bool) {
	v, ok := s[id]
	return v, ok
}
