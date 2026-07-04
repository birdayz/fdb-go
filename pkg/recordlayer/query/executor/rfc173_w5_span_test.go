package executor

// RFC-173 W5 commit 2 — executor pins for the span-derivation extension (the
// gathered unnest's MIXED element carried through a partition collapse) and
// the design-ruling Q6 dimension pin (the NLJ birth's nil-legRVs windows).

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestRFC173W5_MixedElementSpanSynthesis pins the terminal synthesis: a
// SINGLE-accessor pinned ref over a merge quantifier whose referenced slot is
// a bare NON-RECORD QOV (the gathered unnest's mixed element after the
// partition collapsed {source, Explode}) resolves to a synthesized 1-field
// element leg — alias = the SLOT QOV's correlation (the AS alias, never the
// merge alias), the sole column named from the enclosing RC field. Without
// legRVs the shape must DECLINE (fail-safe, never mis-windowed); with a
// RECORD slot the merge-leg run resolution is UNCHANGED (the load-bearing
// non-record guard).
func TestRFC173W5_MixedElementSpanSynthesis(t *testing.T) {
	t.Parallel()

	legS := values.NewRecordType("", false, []values.Field{
		{Name: "SID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "ARR", FieldType: values.NotNullLong, Ordinal: 1},
	})
	qovS := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("S"), legS)
	elemQOV := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("EL"), values.NotNullLong)

	mergedType := values.NewRecordType("", false, []values.Field{
		{Name: values.OrdinalFieldName(0), FieldType: legS, Ordinal: 0},
		{Name: values.OrdinalFieldName(1), FieldType: values.NotNullLong, Ordinal: 1},
	})
	mergeQOV := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("m"), mergedType)

	elemRef, err := values.NewFieldValueOfOrdinal(mergeQOV, 1)
	if err != nil {
		t.Fatalf("bake element slot: %v", err)
	}
	top := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "SID", Value: s3FusedRef(t, mergeQOV, 0, 0)},
		values.RecordConstructorField{Name: "ARR", Value: s3FusedRef(t, mergeQOV, 0, 1)},
		values.RecordConstructorField{Name: "EL", Value: elemRef},
	)
	legRVs := map[values.CorrelationIdentifier]values.Value{
		values.NamedCorrelationIdentifier("m"): values.NewRawRecordConstructorValue(
			values.RecordConstructorField{Name: values.OrdinalFieldName(0), Value: qovS},
			values.RecordConstructorField{Name: values.OrdinalFieldName(1), Value: elemQOV},
		),
	}

	spans, _, ok := ordinalJoinSpansOf(top, legRVs)
	if !ok {
		t.Fatal("the mixed-element translated top must derive windows (the W5 span synthesis)")
	}
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2 (the S run + the synthesized element leg)", len(spans))
	}
	if spans[0].Alias.Name() != "S" || spans[0].Width != 2 {
		t.Fatalf("span 0 = %s width %d, want the full S run (width 2)", spans[0].Alias, spans[0].Width)
	}
	el := spans[1]
	if el.Alias.Name() != "EL" {
		t.Fatalf("element span alias = %s, want the SLOT QOV's correlation EL (never the merge alias)", el.Alias)
	}
	if el.Width != 1 || len(el.LegType.Fields) != 1 || el.LegType.Fields[0].Name != "EL" {
		t.Fatalf("element leg = %+v, want the 1-field synthesis named from the enclosing RC field", el.LegType)
	}

	// Fail-safe: no legRVs → the fused outer refs cannot resolve → decline.
	if _, _, ok := ordinalJoinSpansOf(top, nil); ok {
		t.Fatal("the translated top must DECLINE without legRVs")
	}

	// The non-record guard: a RECORD slot referenced single-accessor is a
	// merge-leg run, NOT an element — the pristine positional-merge RC's own
	// consumers keep resolving unchanged (partial coverage here, so the probe
	// declines rather than synthesizing a bogus leg).
	recRef, err := values.NewFieldValueOfOrdinal(mergeQOV, 0)
	if err != nil {
		t.Fatalf("bake record slot: %v", err)
	}
	topRec := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "R", Value: recRef},
		values.RecordConstructorField{Name: "EL", Value: elemRef},
	)
	if _, _, ok := ordinalJoinSpansOf(topRec, legRVs); ok {
		t.Fatal("a RECORD slot must not synthesize an element leg (partial merge-run coverage declines)")
	}
}

// TestRFC173W5_NLJBirthNilLegRVsDimension is the design-ruling Q6 pin: the
// NLJ birth derives its spans WITHOUT legRVs (newOrdinalJoinBirth), so a
// TRANSLATED top (fused post-merge refs) births with WindowsOK=false — the
// windows are recovered downstream (downstreamLegWindows) by consumers, and
// the birth's own Datum stays name-model bare. This dimension is a LOUD
// decline today, pinned as such; per the ruling it PROMOTES to a fix
// immediately if it ever produces a wrong ANSWER instead (the fix — NLJ-side
// legRV recovery like the FlatMap's — is otherwise an S4-rider, since S4 may
// obsolete the windows entirely).
func TestRFC173W5_NLJBirthNilLegRVsDimension(t *testing.T) {
	t.Parallel()

	legS := values.NewRecordType("", false, []values.Field{
		{Name: "SID", FieldType: values.NotNullLong, Ordinal: 0},
	})
	mergedType := values.NewRecordType("", false, []values.Field{
		{Name: values.OrdinalFieldName(0), FieldType: legS, Ordinal: 0},
	})
	mergeQOV := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("m"), mergedType)
	legB := values.NewRecordType("", false, []values.Field{
		{Name: "BID", FieldType: values.NotNullLong, Ordinal: 0},
	})
	qovB := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("B"), legB)
	b0, err := values.NewFieldValueOfOrdinal(qovB, 0)
	if err != nil {
		t.Fatalf("bake B#0: %v", err)
	}
	top := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "SID", Value: s3FusedRef(t, mergeQOV, 0, 0)},
		values.RecordConstructorField{Name: "BID", Value: b0},
	)

	birth, err := newOrdinalJoinBirth(top, nil)
	if err != nil {
		t.Fatalf("birth: %v", err)
	}
	if birth == nil || !birth.Enabled {
		t.Fatal("a baked translated top must birth ordinal (positional authority intact)")
	}
	if birth.WindowsOK {
		t.Fatal("the NLJ birth has no legRVs — a translated top's windows must NOT derive at birth (they are recovered downstream); if this ever flips silently, re-check the Datum name-model reads of this shape")
	}
}

// TestRFC173W5_OracleSpanSpliceOnPristineBirth pins the review-round-2 splice
// fix: recoverOracleDatumSpans must SPLICE even when the birth arrived with
// PRISTINE seed spans. A seed whose leg is a gated-join BOX carries a span
// named after the BOX alias covering the whole concat; without the splice,
// oracleNameDatum qualifies every buried column under the box alias
// ("B.PID") instead of the leaf aliases dotted reads actually name ("P.PID").
// The pre-fix early return (`len(DatumSpans) > 0 → return`) left the box
// span unopened — this test is RED against it. NOT parallel: flips the
// process-global oracle (birthActive must read the oracle ON).
func TestRFC173W5_OracleSpanSpliceOnPristineBirth(t *testing.T) {
	legP := values.NewRecordType("", false, []values.Field{
		{Name: "PID", FieldType: values.NotNullLong, Ordinal: 0},
	})
	legQ := values.NewRecordType("", false, []values.Field{
		{Name: "QID", FieldType: values.NotNullLong, Ordinal: 0},
	})
	legC := values.NewRecordType("", false, []values.Field{
		{Name: "CID", FieldType: values.NotNullLong, Ordinal: 0},
	})
	bake := func(t *testing.T, qov *values.QuantifiedObjectValue, ord int) *values.FieldValue {
		t.Helper()
		fv, err := values.NewFieldValueOfOrdinal(qov, ord)
		if err != nil {
			t.Fatalf("bake: %v", err)
		}
		return fv
	}

	// The BOX: an inner join over P and Q whose RV is the pristine 2-leg seed.
	pQOV := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("P"), legP)
	qQOV := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("Q"), legQ)
	boxRV := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "PID", Value: bake(t, pQOV, 0)},
		values.RecordConstructorField{Name: "QID", Value: bake(t, qQOV, 0)},
	)
	boxPlan := plans.NewRecordQueryNestedLoopJoinPlan(nil, nil, nil, plans.JoinInner, "P", "Q", boxRV)

	// The TOP: a pristine seed whose OUTER leg is the BOX (alias B, flowing
	// the 2-column concat) and whose inner is a plain leg C.
	boxConcat := values.NewRecordType("", false, []values.Field{
		{Name: "PID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "QID", FieldType: values.NotNullLong, Ordinal: 1},
	})
	bQOV := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("B"), boxConcat)
	cQOV := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("C"), legC)
	topRV := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "PID", Value: bake(t, bQOV, 0)},
		values.RecordConstructorField{Name: "QID", Value: bake(t, bQOV, 1)},
		values.RecordConstructorField{Name: "CID", Value: bake(t, cQOV, 0)},
	)

	SetNameModelOracle(true)
	defer SetNameModelOracle(false)

	c, err := newNLJCursor(
		recordlayer.FromList([]QueryResult{}), nil, plans.JoinInner,
		"B", "C", nil, topRV, EmptyEvaluationContext(), nil,
	)
	if err != nil {
		t.Fatalf("newNLJCursor: %v", err)
	}
	if !c.birth.enabled() || c.birthActive {
		t.Fatal("fixture must be an oracle-side (birth enabled, inactive) cursor")
	}
	if len(c.birth.DatumSpans) == 0 {
		t.Fatal("fixture must arrive with PRISTINE seed spans (the pre-fix early-return trigger)")
	}

	c.recoverOracleDatumSpans(boxPlan, nil)

	aliases := make([]string, 0, len(c.birth.DatumSpans))
	for _, s := range c.birth.DatumSpans {
		aliases = append(aliases, s.Alias.Name())
	}
	got := strings.Join(aliases, ",")
	if got != "P,Q,C" {
		t.Fatalf("DatumSpans after recovery = [%s], want the box span OPENED to leaf aliases [P,Q,C] — the pre-fix early return skipped the splice and oracleNameDatum qualified buried columns under the BOX alias", got)
	}
}

// TestRFC173W4Left_SpanLayoutCrossAgreement pins the impl-review condition:
// the executor's span derivation (ordinalJoinSpans — the birth's layout
// authority) and the values-level layout (values.OrdinalSeedLegWindows —
// the planner's existential-rebase authority) must agree per leg on
// (offset, width, leg type) for pristine seeds. Independent walks drift,
// and layout drift between the rebase and the birth is wrong-offset
// wrong-rows.
func TestRFC173W4Left_SpanLayoutCrossAgreement(t *testing.T) {
	t.Parallel()
	mk := func(alias string, cols ...string) *values.QuantifiedObjectValue {
		fields := make([]values.Field, len(cols))
		for i, c := range cols {
			fields[i] = values.Field{Name: c, FieldType: values.NotNullLong, Ordinal: i}
		}
		return values.NewQuantifiedObjectValueOfType(
			values.NamedCorrelationIdentifier(alias),
			values.NewRecordType("", false, fields))
	}
	bake := func(qov *values.QuantifiedObjectValue, i int) values.RecordConstructorField {
		fv, err := values.NewFieldValueOfOrdinal(qov, i)
		if err != nil {
			t.Fatalf("bake: %v", err)
		}
		return values.RecordConstructorField{Name: fv.Field, Value: fv}
	}
	a := mk("A", "AID", "AV")
	b := mk("B", "BID")
	c := mk("C", "CID", "CV", "CW")
	rc := values.NewRawRecordConstructorValue(
		bake(a, 0), bake(a, 1), bake(b, 0), bake(c, 0), bake(c, 1), bake(c, 2),
	)

	spans, mergedFromSpans, ok := ordinalJoinSpans(rc)
	if !ok {
		t.Fatal("executor spans must derive for the pristine 3-leg seed")
	}
	windows, mergedFromWindows := values.OrdinalSeedLegWindows(rc)
	if windows == nil {
		t.Fatal("values windows must derive for the pristine 3-leg seed")
	}
	if len(spans) != len(windows) {
		t.Fatalf("leg count disagreement: %d spans vs %d windows", len(spans), len(windows))
	}
	for _, s := range spans {
		w, present := windows[strings.ToUpper(s.Alias.Name())]
		if !present {
			t.Fatalf("leg %s in spans but not windows", s.Alias)
		}
		if w.Offset != s.Offset || len(w.Typ.Fields) != s.Width {
			t.Fatalf("leg %s LAYOUT DISAGREEMENT: window (offset %d, width %d) vs span (offset %d, width %d) — the rebase and the birth would read different slots",
				s.Alias, w.Offset, len(w.Typ.Fields), s.Offset, s.Width)
		}
	}
	if len(mergedFromSpans.Fields) != len(mergedFromWindows.Fields) {
		t.Fatalf("merged-type width disagreement: %d vs %d", len(mergedFromSpans.Fields), len(mergedFromWindows.Fields))
	}
	for i := range mergedFromSpans.Fields {
		if mergedFromSpans.Fields[i].Name != mergedFromWindows.Fields[i].Name {
			t.Fatalf("merged field %d name disagreement: %q vs %q",
				i, mergedFromSpans.Fields[i].Name, mergedFromWindows.Fields[i].Name)
		}
	}

	// Both must DECLINE the same non-pristine shape (a translated top whose
	// ref FUSED to a multi-accessor path through a merge quantifier).
	mergedType := values.NewRecordType("", false, []values.Field{
		{Name: values.OrdinalFieldName(0), FieldType: a.Typ, Ordinal: 0},
	})
	mergeQOV := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("m"), mergedType)
	nonPristine := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "AID", Value: s3FusedRef(t, mergeQOV, 0, 0)},
		values.RecordConstructorField{Name: "AV", Value: s3FusedRef(t, mergeQOV, 0, 1)},
	)
	_, _, spansOK := ordinalJoinSpans(nonPristine)
	winDeclined, _ := values.OrdinalSeedLegWindows(nonPristine)
	if spansOK || winDeclined != nil {
		t.Fatalf("both authorities must DECLINE the fused top: spans ok=%v windows derived=%v", spansOK, winDeclined != nil)
	}
}
