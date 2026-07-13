package executor

// RFC-173 W5 commit 2 — executor pins for the span-derivation extension (the
// gathered unnest's MIXED element carried through a partition collapse) and
// the design-ruling Q6 dimension pin (the NLJ birth's nil-legRVs windows).

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
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

// mixedSeedOuter builds a 2-column baked outer leg QOV keyed by the given alias.
func mixedSeedOuter(alias string) *values.QuantifiedObjectValue {
	return values.NewQuantifiedObjectValueOfType(
		values.NamedCorrelationIdentifier(alias),
		values.NewRecordType("", false, []values.Field{
			{Name: alias + "ID", FieldType: values.NotNullLong, Ordinal: 0},
			{Name: alias + "V", FieldType: values.NotNullLong, Ordinal: 1},
		}))
}

// bakeOrdinal bakes a frontier-pinned ofOrdinal field over a leg QOV.
func bakeOrdinal(t *testing.T, qov *values.QuantifiedObjectValue, i int) values.RecordConstructorField {
	t.Helper()
	fv, err := values.NewFieldValueOfOrdinal(qov, i)
	if err != nil {
		t.Fatalf("bake: %v", err)
	}
	return values.RecordConstructorField{Name: fv.Field, Value: fv}
}

// assertSpanWindowAgreement asserts the executor's ordinalJoinSpans and the
// planner's values.OrdinalSeedLegWindows agree BIT-FOR-BIT on the seed rc: same
// accept/decline, and — when accepted — identical leg count, per-leg offset,
// per-leg type (names AND types), and merged type (names AND types). The
// architectural-review exit gate: a fixture that only checks one happy shape by
// NAME lets the two walks drift on the accept boundary and on field types while
// staying green (a sentinel that goes green on a measured divergence is none).
func assertSpanWindowAgreement(t *testing.T, label string, rc *values.RecordConstructorValue, wantAccept bool) {
	t.Helper()
	spans, mergedFromSpans, spansOK := ordinalJoinSpans(rc)
	windows, mergedFromWindows := values.OrdinalSeedLegWindows(rc)
	winOK := windows != nil
	if spansOK != winOK {
		t.Fatalf("%s: ACCEPT DISAGREEMENT — executor spans ok=%v, values windows ok=%v", label, spansOK, winOK)
	}
	if spansOK != wantAccept {
		t.Fatalf("%s: want accept=%v, got %v", label, wantAccept, spansOK)
	}
	if !wantAccept {
		return
	}
	// The windows map keys every ADDRESSABLE name (a box run's SUBS included —
	// the box's name means its rightmost LEAF); spans are RUN-level. Compare
	// per addressable name: a plain run 1:1, a box run per sub-leg.
	// Assumes the LEAF-NAMED box regime (a sub leg carries the run alias, so
	// the values twin REPLACES the run window): a $BOX-minted fixture would
	// retain the whole-run window under the minted key and mis-count here —
	// extend the accounting before adding such a fixture.
	wantWindows := 0
	for _, s := range spans {
		if len(s.LegType.Legs) > 0 {
			wantWindows += len(s.LegType.Legs)
			for _, sub := range s.LegType.Legs {
				w, present := windows[sub.Name]
				if !present {
					t.Fatalf("%s: box-run sub %s in spans' Legs but not windows", label, sub.Name)
				}
				if w.Offset != s.Offset+sub.Start || len(w.Typ.Fields) != sub.Width {
					t.Fatalf("%s: box-run sub %s LAYOUT DISAGREEMENT: window (offset %d, width %d) vs span sub (offset %d, width %d)",
						label, sub.Name, w.Offset, len(w.Typ.Fields), s.Offset+sub.Start, sub.Width)
				}
			}
			continue
		}
		wantWindows++
		w, present := windows[strings.ToUpper(s.Alias.Name())]
		if !present {
			t.Fatalf("%s: leg %s in spans but not windows", label, s.Alias)
		}
		if w.Offset != s.Offset || len(w.Typ.Fields) != s.Width {
			t.Fatalf("%s: leg %s LAYOUT DISAGREEMENT: window (offset %d, width %d) vs span (offset %d, width %d)",
				label, s.Alias, w.Offset, len(w.Typ.Fields), s.Offset, s.Width)
		}
		for i := range s.LegType.Fields {
			sf, wf := s.LegType.Fields[i], w.Typ.Fields[i]
			if !strings.EqualFold(sf.Name, wf.Name) || sf.FieldType.String() != wf.FieldType.String() {
				t.Fatalf("%s: leg %s field %d TYPE DISAGREEMENT: span {%s %s} vs window {%s %s}",
					label, s.Alias, i, sf.Name, sf.FieldType, wf.Name, wf.FieldType)
			}
		}
	}
	if len(windows) != wantWindows {
		t.Fatalf("%s: window count disagreement: %d windows vs %d addressable names from spans", label, len(windows), wantWindows)
	}
	if len(mergedFromSpans.Fields) != len(mergedFromWindows.Fields) {
		t.Fatalf("%s: merged-type width disagreement: %d vs %d", label, len(mergedFromSpans.Fields), len(mergedFromWindows.Fields))
	}
	for i := range mergedFromSpans.Fields {
		sf, wf := mergedFromSpans.Fields[i], mergedFromWindows.Fields[i]
		if sf.Name != wf.Name || sf.FieldType.String() != wf.FieldType.String() {
			t.Fatalf("%s: merged field %d DISAGREEMENT: span {%s %s} vs window {%s %s}",
				label, i, sf.Name, sf.FieldType, wf.Name, wf.FieldType)
		}
	}
	// The merged type's LEGS are the dotted-read bridges' contract — the two
	// derivations must agree on the full boundary list (name, start, width,
	// order). The dimension whose absence let the values twin drop a box
	// run's rightmost-leaf boundary while the executor emitted it.
	if len(mergedFromSpans.Legs) != len(mergedFromWindows.Legs) {
		t.Fatalf("%s: merged-type LEGS count disagreement: spans %v vs windows %v", label, mergedFromSpans.Legs, mergedFromWindows.Legs)
	}
	for i := range mergedFromSpans.Legs {
		if mergedFromSpans.Legs[i] != mergedFromWindows.Legs[i] {
			t.Fatalf("%s: merged-type LEG %d DISAGREEMENT: span %+v vs window %+v", label, i, mergedFromSpans.Legs[i], mergedFromWindows.Legs[i])
		}
	}
}

// TestRFC173W4c_MixedSeedSpanLayoutCrossAgreement pins the cross-agreement
// invariant for the MIXED single-source lateral-unnest seed (a baked outer prefix
// + a trailing bare-QOV whole-object element). The executor's ordinalJoinSpans/
// unnestMixedSeedSpans and the planner's values.OrdinalSeedLegWindows must agree
// BIT-FOR-BIT — accept/decline, leg layout, AND field types. This is the exact
// invariant whose ABSENCE — the executor accepted the mixed seed while the values
// layer declined it — forced the translator to PREDICT the executor's routing and
// cost eight review rounds.
//
// It locks the ACCEPT BOUNDARY, not just one happy shape (the exit-gate
// ruling): the two walks must AGREE on a multi-leg outer prefix (now ACCEPTED
// together — the RFC-173 S4 multi-source gathered seed) and BOTH DECLINE a
// single-leg pristine seed — the shapes where they were measured to drift (values
// accepted, executor declined) before the accept-equivalence was extended/bounded
// in lockstep.
func TestRFC173W4c_MixedSeedSpanLayoutCrossAgreement(t *testing.T) {
	t.Parallel()
	tLeg := mixedSeedOuter("T")

	// ACCEPT: multi-col outer + a bare-QOV scalar element (the whole-object element
	// the seed cannot ofOrdinal-bake), keyed by the AS alias X.
	scalarElem := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("X"), values.NotNullLong)
	assertSpanWindowAgreement(t, "mixed/scalar-element", values.NewRawRecordConstructorValue(
		bakeOrdinal(t, tLeg, 0), bakeOrdinal(t, tLeg, 1),
		values.RecordConstructorField{Name: "X", Value: scalarElem},
	), true)

	// ACCEPT: a STRUCT array element maps to UnknownType (NOT a *RecordType), so it
	// flows the same mixed path as a scalar (unnestArrayElementType / isMixedSeedElement).
	structElem := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("W"), values.UnknownType)
	assertSpanWindowAgreement(t, "mixed/struct-element", values.NewRawRecordConstructorValue(
		bakeOrdinal(t, tLeg, 0), bakeOrdinal(t, tLeg, 1),
		values.RecordConstructorField{Name: "W", Value: structElem},
	), true)

	// ACCEPT: a multi-LEG outer prefix (T then B, BOTH FULLY covered) + a scalar
	// element — the MULTI-SOURCE gathered lateral unnest `FROM T, B, T.arr AS X`, the
	// RFC-173 S4 qualifier-honoring resolution slice's grouped-gather input. Both
	// walks now derive [T-window, B-window, element-window] and MUST agree bit-for-bit
	// (accept, per-leg offsets, field types) — `assertSpanWindowAgreement(true)` proves
	// it. Extending the ONE authority (OrdinalSeedLegWindows) and its executor twin
	// (unnestMixedSeedSpans) TOGETHER, re-pinned here, is how the invariant stays
	// worth guarding: the un-collapse groups over exactly this seed and
	// positionally bakes its group keys via these windows. A DUPLICATE alias in the
	// prefix (a split run) still declines — the pristine-run discipline is unchanged.
	bLeg := mixedSeedOuter("B")
	assertSpanWindowAgreement(t, "accept/multi-leg-prefix", values.NewRawRecordConstructorValue(
		bakeOrdinal(t, tLeg, 0), bakeOrdinal(t, tLeg, 1),
		bakeOrdinal(t, bLeg, 0), bakeOrdinal(t, bLeg, 1),
		values.RecordConstructorField{Name: "X", Value: scalarElem},
	), true)

	// ACCEPT: the ELEMENT-ANYWHERE accepts the generalization opened — the element
	// SPLITS the leg run, distinct legs on BOTH sides. This is the exact layout the
	// un-collapse's `FROM T, T.arr AS X, B GROUP BY X` (+ SUM(T.col)) rests on: the
	// walk windows the leading legs, steps over the element's 1-field slot, then
	// windows the trailing legs. Both walks MUST derive identical [T-window, element,
	// B-window] — this pins the accept boundary at the bit-for-bit unit level where the
	// invariant lives (an E2E test would only incidentally catch a drift here).
	assertSpanWindowAgreement(t, "accept/mid-list-element", values.NewRawRecordConstructorValue(
		bakeOrdinal(t, tLeg, 0), bakeOrdinal(t, tLeg, 1),
		values.RecordConstructorField{Name: "X", Value: scalarElem},
		bakeOrdinal(t, bLeg, 0), bakeOrdinal(t, bLeg, 1),
	), true)
	// ACCEPT: a LEADING element (element at position 0, a leg after) — the other new
	// element-anywhere boundary (`FROM T.arr AS X, B`). Both walks derive [element,
	// B-window].
	assertSpanWindowAgreement(t, "accept/leading-element", values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "X", Value: scalarElem},
		bakeOrdinal(t, bLeg, 0), bakeOrdinal(t, bLeg, 1),
	), true)

	// A single baked leg, no element: ordinalJoinSpansOf bails on len(spans) < 2,
	// unnestMixedSeedSpans on the non-QOV trailing field; values now bails on
	// len(windows) < 2. BOTH decline (a folded projection, not the pristine concat).
	assertSpanWindowAgreement(t, "decline/single-leg-pristine", values.NewRawRecordConstructorValue(
		bakeOrdinal(t, tLeg, 0), bakeOrdinal(t, tLeg, 1),
	), false)

	// A SPLIT RUN — a leg alias RECURS (`[A.0, B.0, A.0]`, 1-field legs). Each
	// 1-field run is trivially full-coverage, so the executor's run-LIST would
	// accept it without the seen-alias reject; values' run-MAP declines on the
	// dup-alias check. This pins the truly-bit-for-bit reject (the last drift
	// residual): BOTH decline now. Unreachable from SQL (a seed is a concat of
	// DISTINCT contiguous legs) but the invariant is that the two walks agree.
	a1 := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("A"),
		values.NewRecordType("", false, []values.Field{{Name: "AID", FieldType: values.NotNullLong, Ordinal: 0}}))
	b1 := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("B"),
		values.NewRecordType("", false, []values.Field{{Name: "BID", FieldType: values.NotNullLong, Ordinal: 0}}))
	assertSpanWindowAgreement(t, "decline/split-run", values.NewRawRecordConstructorValue(
		bakeOrdinal(t, a1, 0), bakeOrdinal(t, b1, 0), bakeOrdinal(t, a1, 0),
	), false)

	// A RECORD-typed trailing bare QOV (the S3 positional-merge RC's shape): the
	// non-record guard (isMixedSeedElement / unnestMixedSeedSpans) excludes it. BOTH decline.
	recElem := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("R"),
		values.NewRecordType("", false, []values.Field{{Name: "RC0", FieldType: values.NotNullLong, Ordinal: 0}}))
	assertSpanWindowAgreement(t, "decline/record-element", values.NewRawRecordConstructorValue(
		bakeOrdinal(t, tLeg, 0), bakeOrdinal(t, tLeg, 1),
		values.RecordConstructorField{Name: "R", Value: recElem},
	), false)

	// A lone element (no baked outer prefix): fewer than 2 fields / empty windows. BOTH decline.
	assertSpanWindowAgreement(t, "decline/lone-element", values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "X", Value: scalarElem},
	), false)
}

// TestRFC173Item3_BoxLegSpanWindowAgreement pins the cross-agreement
// invariant for a seed whose leg is a CLUSTERED BOX (RecordType.Legs carries
// the buried-leaf boundaries): the executor's run-level spans emit EVERY sub
// of the box run into the merged type's Legs, and the values twin must land
// on the identical boundary list — including the RIGHTMOST leaf's entry
// under the box's own name (the box is NAMED by that leaf, so the
// alias-keyed window must be the LEAF's narrow slice, never the whole
// concat: a run-wide window FieldIndexes across the concat and first-matches
// an earlier buried leg's duplicate name). The exact drift the values twin
// shipped: its `leg.Name == alias` skip dropped that boundary while the
// executor emitted it.
func TestRFC173Item3_BoxLegSpanWindowAgreement(t *testing.T) {
	t.Parallel()
	// Plain preserved leg A (2 cols) + a clustered box leg named E whose
	// concat buries B (2 cols, one name DUPLICATING an A column) before its
	// rightmost leaf E (1 col).
	aLeg := mixedSeedOuter("A")
	boxTyp := values.NewRecordType("", false, []values.Field{
		{Name: "BID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "AID", FieldType: values.NotNullLong, Ordinal: 1}, // dup of A's column name
		{Name: "EID", FieldType: values.NotNullLong, Ordinal: 2},
	})
	boxTyp.Legs = []values.RecordTypeLeg{
		{Name: "B", Start: 0, Width: 2},
		{Name: "E", Start: 2, Width: 1},
	}
	eBox := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("E"), boxTyp)
	seed := values.NewRawRecordConstructorValue(
		bakeOrdinal(t, aLeg, 0), bakeOrdinal(t, aLeg, 1),
		bakeOrdinal(t, eBox, 0), bakeOrdinal(t, eBox, 1), bakeOrdinal(t, eBox, 2),
	)
	assertSpanWindowAgreement(t, "box-leg", seed, true)

	// The alias-keyed window is the rightmost LEAF's slice.
	windows, merged := values.OrdinalSeedLegWindows(seed)
	if w := windows["E"]; w.Offset != 4 || len(w.Typ.Fields) != 1 || w.Typ.Fields[0].Name != "EID" {
		t.Fatalf("windows[E] = (offset %d, %v) — want the rightmost LEAF window (offset 4, [EID])", w.Offset, w.Typ.Fields)
	}
	if w := windows["B"]; w.Offset != 2 || len(w.Typ.Fields) != 2 {
		t.Fatalf("windows[B] = (offset %d, width %d) — want the buried sub-window (offset 2, width 2)", w.Offset, len(w.Typ.Fields))
	}
	wantLegs := []values.RecordTypeLeg{{Name: "A", Start: 0, Width: 2}, {Name: "B", Start: 2, Width: 2}, {Name: "E", Start: 4, Width: 1}}
	if len(merged.Legs) != len(wantLegs) {
		t.Fatalf("merged Legs = %v, want %v", merged.Legs, wantLegs)
	}
	for i, want := range wantLegs {
		if merged.Legs[i] != want {
			t.Fatalf("merged Legs[%d] = %+v, want %+v", i, merged.Legs[i], want)
		}
	}
}
