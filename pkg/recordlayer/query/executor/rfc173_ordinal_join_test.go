package executor

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// --- fixtures --------------------------------------------------------------

// ojLegTypeAV is leg A's type [ID, V]; ojLegTypeBW is leg B's type [ID, W].
// The duplicated ID across legs is deliberate — it is the §5 duplicate-name
// shape the ordinal join makes constructible, and the axis every wrong-slot
// test below turns on.
func ojLegTypeAV() *values.RecordType {
	return values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "V", FieldType: values.NotNullLong, Ordinal: 1},
	})
}

func ojLegTypeBW() *values.RecordType {
	return values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "W", FieldType: values.NotNullLong, Ordinal: 1},
	})
}

// buildOrdinalJoinRC builds the canonical ordinal join RC over the given typed
// leg QOVs — the exact shape W3b's translator seed will emit: for each leg, in
// order, ofOrdinal(QOV(leg), 0..n-1), field name = the leg column name,
// duplicates across legs preserved verbatim (NewRawRecordConstructorValue).
func buildOrdinalJoinRC(t *testing.T, qovs ...*values.QuantifiedObjectValue) *values.RecordConstructorValue {
	t.Helper()
	var fields []values.RecordConstructorField
	for _, qov := range qovs {
		rt, isRT := qov.Type().(*values.RecordType)
		if !isRT {
			t.Fatalf("buildOrdinalJoinRC: leg %s flows %T, want *RecordType", qov.Correlation, qov.Type())
		}
		for i := range rt.Fields {
			fv, err := values.NewFieldValueOfOrdinal(qov, i)
			if err != nil {
				t.Fatalf("buildOrdinalJoinRC: NewFieldValueOfOrdinal(%s, %d): %v", qov.Correlation, i, err)
			}
			fields = append(fields, values.RecordConstructorField{Name: fv.Field, Value: fv})
		}
	}
	return values.NewRawRecordConstructorValue(fields...)
}

// ojMergedRow hand-builds the merged positional row [A.ID=1, A.V=10, B.ID=2,
// B.W=20] over the given merged type — the fixture every window test reads.
func ojMergedRow(t *testing.T, mergedType *values.RecordType) *PositionalRow {
	t.Helper()
	row := NewPositionalRow(mergedType)
	for i, v := range []any{int64(1), int64(10), int64(2), int64(20)} {
		if !row.Set(i, v) {
			t.Fatalf("ojMergedRow: Set(%d) out of range — merged type has %d fields", i, len(mergedType.Fields))
		}
	}
	return row
}

// stubBinder binds correlations to arbitrary values. A key PRESENT with a nil
// value binds the leg to nil — the sanctioned NULL-leg expression (contract
// ruling #3).
type stubBinder map[values.CorrelationIdentifier]any

func (b stubBinder) GetCorrelationBinding(id values.CorrelationIdentifier) (any, bool) {
	v, found := b[id]
	return v, found
}

// mustPanicRFC173 runs fn and asserts it panics with a message containing
// "RFC-173" (and every extra want substring) — a malformed ordinal seed is a
// planner bug and must be LOUD, never a silent name-model demotion.
func mustPanicRFC173(t *testing.T, fn func(), want ...string) {
	t.Helper()
	defer func() {
		t.Helper()
		r := recover()
		if r == nil {
			t.Fatal("want an RFC-173 panic, got none — a malformed ordinal seed silently passed")
		}
		msg := fmt.Sprint(r)
		if !strings.Contains(msg, "RFC-173") {
			t.Fatalf("panic message %q must contain RFC-173", msg)
		}
		for _, w := range want {
			if !strings.Contains(msg, w) {
				t.Fatalf("panic message %q must contain %q", msg, w)
			}
		}
	}()
	fn()
}

// --- raw RC constructor ------------------------------------------------------

// TestRFC173S2_RawRecordConstructor_DupNamesVerbatim pins the Graefe ruling's
// dedicated raw RC constructor: duplicate field names survive VERBATIM (the
// ordinal join RC's cross-leg duplicates are positional, not name-addressed),
// while NewRecordConstructorValue — the name-model constructor — still renames
// (ID, ID_2), unchanged.
func TestRFC173S2_RawRecordConstructor_DupNamesVerbatim(t *testing.T) {
	t.Parallel()
	fields := []values.RecordConstructorField{
		{Name: "ID", Value: &values.ConstantValue{Value: int64(1), Typ: values.NotNullLong}},
		{Name: "ID", Value: &values.ConstantValue{Value: int64(2), Typ: values.NotNullLong}},
	}

	raw := values.NewRawRecordConstructorValue(fields...)
	if raw.Fields[0].Name != "ID" || raw.Fields[1].Name != "ID" {
		t.Fatalf("raw RC names = [%q, %q], want [ID, ID] — duplicates must survive verbatim", raw.Fields[0].Name, raw.Fields[1].Name)
	}
	if raw.AnchoredJoin {
		t.Fatal("raw RC must not be born anchored")
	}

	// Control: the name-model constructor still renames.
	named := values.NewRecordConstructorValue(fields...)
	if named.Fields[0].Name != "ID" || named.Fields[1].Name != "ID_2" {
		t.Fatalf("name-model RC names = [%q, %q], want [ID, ID_2] — the _N dedup is name-model behavior and must stay", named.Fields[0].Name, named.Fields[1].Name)
	}
}

// --- ordinalJoinSpans --------------------------------------------------------

// TestRFC173S2_OrdinalJoinSpans_HappyPath pins the strict deriver on a 2+1
// seed: spans (offsets 0/2, widths 2/1) derive from the RC — the single
// authority — and the merged type keeps the concatenated names in order,
// duplicate ID across legs preserved.
func TestRFC173S2_OrdinalJoinSpans_HappyPath(t *testing.T) {
	t.Parallel()
	corrA := values.NamedCorrelationIdentifier("a")
	corrB := values.NamedCorrelationIdentifier("b")
	qovA := values.NewQuantifiedObjectValueOfType(corrA, ojLegTypeAV())
	legB := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
	})
	qovB := values.NewQuantifiedObjectValueOfType(corrB, legB)

	rc := buildOrdinalJoinRC(t, qovA, qovB)
	spans, mergedType, ok := ordinalJoinSpans(rc)
	if !ok {
		t.Fatal("ordinalJoinSpans rejected a well-formed ordinal join RC")
	}
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2", len(spans))
	}
	if spans[0].Alias != corrA || spans[0].Offset != 0 || spans[0].Width != 2 {
		t.Fatalf("leg A span = %+v, want {a, offset 0, width 2}", spans[0])
	}
	if spans[1].Alias != corrB || spans[1].Offset != 2 || spans[1].Width != 1 {
		t.Fatalf("leg B span = %+v, want {b, offset 2, width 1}", spans[1])
	}
	if len(spans[0].LegType.Fields) != 2 || len(spans[1].LegType.Fields) != 1 {
		t.Fatalf("leg types = %d/%d fields, want 2/1", len(spans[0].LegType.Fields), len(spans[1].LegType.Fields))
	}
	wantNames := []string{"ID", "V", "ID"}
	if len(mergedType.Fields) != len(wantNames) {
		t.Fatalf("merged type has %d fields, want %d", len(mergedType.Fields), len(wantNames))
	}
	for i, want := range wantNames {
		f := mergedType.Fields[i]
		if f.Name != want || f.Ordinal != i {
			t.Fatalf("merged field %d = {%q, ord %d}, want {%q, ord %d} — dup names across legs must survive in order", i, f.Name, f.Ordinal, want, i)
		}
		if f.FieldType != values.NotNullLong {
			t.Fatalf("merged field %d type = %v, want the baked FieldValue's type", i, f.FieldType)
		}
	}
}

// TestRFC173S2_OrdinalJoinSpans_NameModelDeclines pins ok=false — NOT a panic
// — for everything the name model produces: a non-RC value, a lazy projection
// RC, and a lazy ANCHORED join RC. Those are the coexistence window's normal
// citizens; only a malformed ORDINAL seed is loud.
func TestRFC173S2_OrdinalJoinSpans_NameModelDeclines(t *testing.T) {
	t.Parallel()
	qovA := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("a"), ojLegTypeAV())

	// Non-RC value.
	if _, _, ok := ordinalJoinSpans(&values.ConstantValue{Value: int64(1), Typ: values.NotNullLong}); ok {
		t.Fatal("a non-RC value must not be an ordinal join")
	}
	// Lazy projection RC (zero baked fields).
	lazyRC := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "ID", Value: values.NewFieldValue(qovA, "ID", values.NotNullLong)},
		values.RecordConstructorField{Name: "V", Value: values.NewFieldValue(qovA, "V", values.NotNullLong)},
	)
	if _, _, ok := ordinalJoinSpans(lazyRC); ok {
		t.Fatal("a lazy RC must not be an ordinal join — the name model's RCs land here")
	}
	// Lazy ANCHORED join RC — today's name-model join seed shape.
	anchored := &values.RecordConstructorValue{
		Fields: []values.RecordConstructorField{
			{Name: "ID", Value: values.NewFieldValue(qovA, "ID", values.NotNullLong)},
		},
		AnchoredJoin: true,
	}
	if _, _, ok := ordinalJoinSpans(anchored); ok {
		t.Fatal("a lazy anchored join RC must not be an ordinal join")
	}
}

// TestRFC173S2_OrdinalJoinSpans_MalformedPanics pins every malformation panic:
// once an RC carries even ONE baked field it must be a well-formed 2-way
// ordinal seed, and anything else is a loud RFC-173 planner bug — never a
// silent name-model demotion.
func TestRFC173S2_OrdinalJoinSpans_MalformedPanics(t *testing.T) {
	t.Parallel()
	newQOV := func(name string, rt *values.RecordType) *values.QuantifiedObjectValue {
		return values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier(name), rt)
	}
	baked := func(qov *values.QuantifiedObjectValue, ord int) values.RecordConstructorField {
		t.Helper()
		fv, err := values.NewFieldValueOfOrdinal(qov, ord)
		if err != nil {
			t.Fatalf("NewFieldValueOfOrdinal(%s, %d): %v", qov.Correlation, ord, err)
		}
		return values.RecordConstructorField{Name: fv.Field, Value: fv}
	}

	t.Run("single run", func(t *testing.T) {
		t.Parallel()
		qovA := newQOV("a", ojLegTypeAV())
		rc := values.NewRawRecordConstructorValue(baked(qovA, 0), baked(qovA, 1))
		mustPanicRFC173(t, func() { ordinalJoinSpans(rc) }, "2-way")
	})

	t.Run("three runs", func(t *testing.T) {
		t.Parallel()
		oneCol := func(name string) *values.RecordType {
			return values.NewRecordType("", false, []values.Field{{Name: name, FieldType: values.NotNullLong, Ordinal: 0}})
		}
		rc := values.NewRawRecordConstructorValue(
			baked(newQOV("a", oneCol("X")), 0),
			baked(newQOV("b", oneCol("Y")), 0),
			baked(newQOV("c", oneCol("Z")), 0),
		)
		mustPanicRFC173(t, func() { ordinalJoinSpans(rc) }, "2-way")
	})

	t.Run("ordinal gap", func(t *testing.T) {
		t.Parallel()
		wide := values.NewRecordType("", false, []values.Field{
			{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
			{Name: "V", FieldType: values.NotNullLong, Ordinal: 1},
			{Name: "X", FieldType: values.NotNullLong, Ordinal: 2},
		})
		qovA := newQOV("a", wide)
		qovB := newQOV("b", ojLegTypeBW())
		rc := values.NewRawRecordConstructorValue(
			baked(qovA, 0), baked(qovA, 2), // gap: 0,2
			baked(qovB, 0), baked(qovB, 1),
		)
		mustPanicRFC173(t, func() { ordinalJoinSpans(rc) })
	})

	t.Run("partial leg coverage", func(t *testing.T) {
		t.Parallel()
		wide := values.NewRecordType("", false, []values.Field{
			{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
			{Name: "V", FieldType: values.NotNullLong, Ordinal: 1},
			{Name: "X", FieldType: values.NotNullLong, Ordinal: 2},
		})
		qovA := newQOV("a", wide)
		qovB := newQOV("b", ojLegTypeBW())
		rc := values.NewRawRecordConstructorValue(
			baked(qovA, 0), baked(qovA, 1), // leg type has 3 fields, run has 2
			baked(qovB, 0), baked(qovB, 1),
		)
		mustPanicRFC173(t, func() { ordinalJoinSpans(rc) })
	})

	t.Run("mixed baked and lazy", func(t *testing.T) {
		t.Parallel()
		qovA := newQOV("a", ojLegTypeAV())
		rc := values.NewRawRecordConstructorValue(
			baked(qovA, 0),
			values.RecordConstructorField{Name: "V", Value: values.NewFieldValue(qovA, "V", values.NotNullLong)},
		)
		mustPanicRFC173(t, func() { ordinalJoinSpans(rc) })
	})
}

// --- legWindowRow ------------------------------------------------------------

// TestRFC173S2_LegWindowRow pins the window mechanics over a hand-built merged
// row: Get is leg-relative (window at offset 2 reads merged slot 2+i),
// out-of-range is (nil,false), GetByName resolves leg-LOCALLY against the LEG
// type even when the merged type carries the same name at a DIFFERENT absolute
// slot (the wrong-slot axis), and TypeNames reports the leg's columns.
func TestRFC173S2_LegWindowRow(t *testing.T) {
	t.Parallel()
	corrA := values.NamedCorrelationIdentifier("a")
	corrB := values.NamedCorrelationIdentifier("b")
	qovA := values.NewQuantifiedObjectValueOfType(corrA, ojLegTypeAV())
	qovB := values.NewQuantifiedObjectValueOfType(corrB, ojLegTypeBW())
	rc := buildOrdinalJoinRC(t, qovA, qovB)
	spans, mergedType, ok := ordinalJoinSpans(rc)
	if !ok {
		t.Fatal("ordinalJoinSpans rejected the fixture RC")
	}
	merged := ojMergedRow(t, mergedType) // [A.ID=1, A.V=10, B.ID=2, B.W=20]

	winB := &legWindowRow{parent: merged, legType: spans[1].LegType, offset: spans[1].Offset, width: spans[1].Width}
	if v, found := winB.Get(0); !found || v != int64(2) {
		t.Fatalf("window B Get(0) = (%v, %v), want (2, true) — leg ordinal 0 is merged slot 2", v, found)
	}
	if v, found := winB.Get(1); !found || v != int64(20) {
		t.Fatalf("window B Get(1) = (%v, %v), want (20, true)", v, found)
	}
	if v, found := winB.Get(2); found || v != nil {
		t.Fatalf("window B Get(2) = (%v, %v), want (nil, false) — past the leg width, never a read into a sibling leg", v, found)
	}
	if v, found := winB.Get(-1); found || v != nil {
		t.Fatalf("window B Get(-1) = (%v, %v), want (nil, false)", v, found)
	}

	// GetByName is leg-LOCAL: "ID" resolves against LEG B's type (ordinal 0 →
	// merged slot 2 → 2), even though the merged type has "ID" at absolute
	// slot 0 holding A's ID=1 — the wrong-slot axis.
	if v, found := winB.GetByName("ID"); !found || v != int64(2) {
		t.Fatalf("window B GetByName(ID) = (%v, %v), want (2, true) — must be B's ID, not merged slot 0 (A's)", v, found)
	}
	if v, found := winB.GetByName("W"); !found || v != int64(20) {
		t.Fatalf("window B GetByName(W) = (%v, %v), want (20, true)", v, found)
	}
	if _, found := winB.GetByName("V"); found {
		t.Fatal("window B GetByName(V) must miss — V is leg A's column, not visible through B's window")
	}

	wantNames := []string{"ID", "W"}
	got := winB.TypeNames()
	if len(got) != len(wantNames) || got[0] != wantNames[0] || got[1] != wantNames[1] {
		t.Fatalf("window B TypeNames = %v, want %v — the LEG's names, for OrdinalResolutionError diagnostics", got, wantNames)
	}
}

// TestRFC173S2_LegWindow_WrongSlotHazard is the red→green pin on the EXACT
// hazard the W3 pre-code ruling exists for (Graefe condition 4): a LAZY leg
// reference FieldValue(QOV(B), "ID") derives a LEG-relative ordinal (0, from
// B's type) — evaluated over the MERGED positional row without windows it
// reads absolute slot 0 and returns A's ID, silently wrong; through the leg
// window binder it reads window B slot 0 = merged slot 2, correct.
func TestRFC173S2_LegWindow_WrongSlotHazard(t *testing.T) {
	t.Parallel()
	corrA := values.NamedCorrelationIdentifier("a")
	corrB := values.NamedCorrelationIdentifier("b")
	qovA := values.NewQuantifiedObjectValueOfType(corrA, ojLegTypeAV())
	qovB := values.NewQuantifiedObjectValueOfType(corrB, ojLegTypeBW())
	rc := buildOrdinalJoinRC(t, qovA, qovB)
	spans, mergedType, ok := ordinalJoinSpans(rc)
	if !ok {
		t.Fatal("ordinalJoinSpans rejected the fixture RC")
	}
	merged := ojMergedRow(t, mergedType) // [A.ID=1, A.V=10, B.ID=2, B.W=20]

	lazyBID := values.NewFieldValue(qovB, "ID", values.NotNullLong)

	// (i) THE HAZARD, proven real: merged row as bare Positional, no leg
	// bindings — the lazy leg-relative ordinal 0 reads absolute slot 0 = A's
	// ID. This assertion is the RED half: if it ever starts returning 2, the
	// window scaffolding has become dead weight and the ruling needs revisit.
	got, err := lazyBID.Evaluate(&values.RowEvalContext{Positional: merged})
	if err != nil {
		t.Fatalf("hazard eval errored: %v", err)
	}
	if got != int64(1) {
		t.Fatalf("lazy B.ID over the bare merged row = %v, want the MISREAD 1 (A's ID at absolute slot 0) — the hazard the leg windows exist for", got)
	}

	// (ii) GREEN: the same node through the leg window binder reads window B
	// slot 0 = merged slot 2 = B's ID.
	binder := &legWindowBinder{spans: spans, row: merged}
	got, err = lazyBID.Evaluate(&values.RowEvalContext{Correlations: binder})
	if err != nil {
		t.Fatalf("windowed eval errored: %v", err)
	}
	if got != int64(2) {
		t.Fatalf("lazy B.ID through the leg window = %v, want 2 (B's ID)", got)
	}

	// A BAKED B#0 through the binder reads the same correct slot.
	bakedBID, err := values.NewFieldValueOfOrdinal(qovB, 0)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal: %v", err)
	}
	got, err = bakedBID.Evaluate(&values.RowEvalContext{Correlations: binder})
	if err != nil {
		t.Fatalf("baked windowed eval errored: %v", err)
	}
	if got != int64(2) {
		t.Fatalf("baked B#0 through the leg window = %v, want 2", got)
	}
}

// TestRFC173S2_LegWindowBinder_Delegation pins the binder's non-span arms: an
// unknown alias delegates to base when non-nil, else (nil, false).
func TestRFC173S2_LegWindowBinder_Delegation(t *testing.T) {
	t.Parallel()
	corrA := values.NamedCorrelationIdentifier("a")
	outer := values.NamedCorrelationIdentifier("outer")
	spans := []legSpan{{Alias: corrA, LegType: ojLegTypeAV(), Offset: 0, Width: 2}}
	row := NewPositionalRow(ojLegTypeAV())

	// Span alias: a window.
	b := &legWindowBinder{spans: spans, row: row}
	bound, found := b.GetCorrelationBinding(corrA)
	if !found {
		t.Fatal("span alias must bind")
	}
	if _, isWin := bound.(*legWindowRow); !isWin {
		t.Fatalf("span alias bound to %T, want *legWindowRow", bound)
	}
	// Unknown alias, nil base: unbound.
	if _, found := b.GetCorrelationBinding(outer); found {
		t.Fatal("unknown alias with nil base must be unbound")
	}
	// Unknown alias, base present: delegates (the outer-correlation path).
	withBase := &legWindowBinder{base: stubBinder{outer: int64(42)}, spans: spans, row: row}
	bound, found = withBase.GetCorrelationBinding(outer)
	if !found || bound != int64(42) {
		t.Fatalf("base delegation = (%v, %v), want (42, true)", bound, found)
	}
}

// --- evaluateOrdinalJoinRow ---------------------------------------------------

// TestRFC173S2_EvaluateOrdinalJoinRow pins the merged-row birth primitive:
// both legs bound to leg-local positional rows concatenate into the merged
// slots; a leg bound to nil — (nil, true), the sanctioned appendNullLeg
// expression — yields NULL for exactly that leg's slots (contract ruling #3:
// appendNullLeg ≡ evaluating the merged RC with the leg QOV bound to nil).
func TestRFC173S2_EvaluateOrdinalJoinRow(t *testing.T) {
	t.Parallel()
	corrA := values.NamedCorrelationIdentifier("a")
	corrB := values.NamedCorrelationIdentifier("b")
	legA, legB := ojLegTypeAV(), ojLegTypeBW()
	qovA := values.NewQuantifiedObjectValueOfType(corrA, legA)
	qovB := values.NewQuantifiedObjectValueOfType(corrB, legB)
	rc := buildOrdinalJoinRC(t, qovA, qovB)
	_, mergedType, ok := ordinalJoinSpans(rc)
	if !ok {
		t.Fatal("ordinalJoinSpans rejected the fixture RC")
	}

	rowA := NewPositionalRow(legA)
	rowA.Set(0, int64(1))
	rowA.Set(1, int64(10))
	rowB := NewPositionalRow(legB)
	rowB.Set(0, int64(2))
	rowB.Set(1, int64(20))

	t.Run("both legs bound", func(t *testing.T) {
		t.Parallel()
		merged, err := evaluateOrdinalJoinRow(rc, mergedType, stubBinder{corrA: rowA, corrB: rowB})
		if err != nil {
			t.Fatalf("evaluateOrdinalJoinRow: %v", err)
		}
		if merged.Type != mergedType {
			t.Fatal("merged row must carry the single merged type")
		}
		want := []any{int64(1), int64(10), int64(2), int64(20)}
		for i, w := range want {
			if got, found := merged.Get(i); !found || got != w {
				t.Fatalf("merged slot %d = (%v, %v), want (%v, true) — legs concatenate", i, got, found, w)
			}
		}
	})

	t.Run("null leg B", func(t *testing.T) {
		t.Parallel()
		// The NULL-leg equivalence pin at the primitive level: leg B bound to
		// nil (present!) — the baked nodes' `return bound, nil` arm yields nil
		// and B's slots fall out NULL, A's intact. No per-row type, no
		// special-case appendNullLeg arm.
		merged, err := evaluateOrdinalJoinRow(rc, mergedType, stubBinder{corrA: rowA, corrB: nil})
		if err != nil {
			t.Fatalf("evaluateOrdinalJoinRow with null leg: %v", err)
		}
		want := []any{int64(1), int64(10), nil, nil}
		for i, w := range want {
			if got, found := merged.Get(i); !found || got != w {
				t.Fatalf("null-leg merged slot %d = (%v, %v), want (%v, true)", i, got, found, w)
			}
		}
	})

	t.Run("name-keyed leg binding is loud", func(t *testing.T) {
		t.Parallel()
		// A leg bound to a NAME-keyed map is the conflation RFC-173 kills: the
		// baked node's map arm raises *BakedNameContextError, and the primitive
		// propagates it — error propagation proven end-to-end.
		_, err := evaluateOrdinalJoinRow(rc, mergedType, stubBinder{corrA: rowA, corrB: map[string]any{"ID": int64(2), "W": int64(20)}})
		var bnce *values.BakedNameContextError
		if !errors.As(err, &bnce) {
			t.Fatalf("name-keyed leg binding must be a loud *BakedNameContextError, got %v", err)
		}
	})

	t.Run("garbage leg binding pins current behavior", func(t *testing.T) {
		t.Parallel()
		// OPEN DECISION (Torvalds W3 borderline, do not change in W3a): a leg
		// bound to a non-nil, non-OrdinalRow, non-map value hits
		// evaluateCorrelated's `return bound, nil` arm, which returns the RAW
		// bound object into the slot — no error, no NULL. This pin freezes the
		// CURRENT behavior so the W3b decision (loud error vs. keep) is a
		// deliberate red→green edit here, not a silent drift. values.go is
		// deliberately untouched this stage.
		type garbage struct{ x int }
		g := garbage{x: 9}
		merged, err := evaluateOrdinalJoinRow(rc, mergedType, stubBinder{corrA: rowA, corrB: g})
		if err != nil {
			t.Fatalf("garbage leg binding errored (behavior changed — update the open-decision pin): %v", err)
		}
		if got, _ := merged.Get(2); got != g {
			t.Fatalf("garbage-bound leg slot = %v, want the raw bound object (current `return bound, nil` behavior)", got)
		}
	})

	t.Run("merged type not derived from RC panics", func(t *testing.T) {
		t.Parallel()
		mustPanicRFC173(t, func() {
			_, _ = evaluateOrdinalJoinRow(rc, legA, stubBinder{corrA: rowA, corrB: rowB})
		})
	})
}

// --- adaptLegPositional --------------------------------------------------------

// TestRFC173S2_AdaptLegPositional pins the row-FORMAT adapter at the join-input
// boundary: positional passthrough, name-model Datum synthesis by leg-type
// names (missing key → nil slot), and nil / non-map Datum → all-nil row.
func TestRFC173S2_AdaptLegPositional(t *testing.T) {
	t.Parallel()
	legA := ojLegTypeAV()

	t.Run("positional passthrough", func(t *testing.T) {
		t.Parallel()
		pos := NewPositionalRow(legA)
		pos.Set(0, int64(7))
		got := adaptLegPositional(QueryResult{Positional: pos, Datum: map[string]any{"ID": int64(999)}}, legA)
		if got != values.OrdinalRow(pos) {
			t.Fatal("a leg that already carries a positional row must flow it through untouched")
		}
	})

	t.Run("datum synthesis by leg-type names", func(t *testing.T) {
		t.Parallel()
		got := adaptLegPositional(QueryResult{Datum: map[string]any{"ID": int64(7)}}, legA) // V missing
		if v, found := got.Get(0); !found || v != int64(7) {
			t.Fatalf("synthesized slot 0 = (%v, %v), want (7, true)", v, found)
		}
		if v, found := got.Get(1); !found || v != nil {
			t.Fatalf("missing-key slot 1 = (%v, %v), want (nil, true) — SQL NULL, in range", v, found)
		}
		row, isRow := got.(*PositionalRow)
		if !isRow || row.Type != legA {
			t.Fatalf("synthesized row = %T (type %v), want *PositionalRow over the LEG type", got, legA)
		}
	})

	t.Run("nil datum", func(t *testing.T) {
		t.Parallel()
		got := adaptLegPositional(QueryResult{}, legA)
		for i := range legA.Fields {
			if v, found := got.Get(i); !found || v != nil {
				t.Fatalf("nil-datum slot %d = (%v, %v), want (nil, true)", i, v, found)
			}
		}
	})

	t.Run("non-map datum", func(t *testing.T) {
		t.Parallel()
		got := adaptLegPositional(QueryResult{Datum: "not a row"}, legA)
		for i := range legA.Fields {
			if v, found := got.Get(i); !found || v != nil {
				t.Fatalf("non-map-datum slot %d = (%v, %v), want (nil, true)", i, v, found)
			}
		}
	})
}

// --- real-PositionalRow eval pin (owed from W1) ---------------------------------

// TestRFC173S2_BakedEval_RealPositionalRow closes the W1 debt: a BAKED
// FieldValue evaluated directly against a real executor *PositionalRow (not a
// values-package test fake) reads the right slot through the OrdinalRow arm,
// and an out-of-range bake is a loud values.OrdinalResolutionError carrying
// the row's names — never a silent NULL.
func TestRFC173S2_BakedEval_RealPositionalRow(t *testing.T) {
	t.Parallel()
	legB := ojLegTypeBW()
	qovB := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("b"), legB)
	row := NewPositionalRow(legB)
	row.Set(0, int64(2))
	row.Set(1, int64(20))

	baked1, err := values.NewFieldValueOfOrdinal(qovB, 1)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal: %v", err)
	}
	got, err := baked1.Evaluate(row)
	if err != nil {
		t.Fatalf("baked eval over a real PositionalRow: %v", err)
	}
	if got != int64(20) {
		t.Fatalf("baked B#1 over a real PositionalRow = %v, want 20", got)
	}

	// Out-of-range: bake against a WIDER type than the runtime row (the bake
	// itself is legal; the row is short) — loud OrdinalResolutionError with
	// the row's names in the diagnostics.
	wide := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "W", FieldType: values.NotNullLong, Ordinal: 1},
		{Name: "X", FieldType: values.NotNullLong, Ordinal: 2},
	})
	qovWide := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("wb"), wide)
	baked2, err := values.NewFieldValueOfOrdinal(qovWide, 2)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal: %v", err)
	}
	_, err = baked2.Evaluate(row)
	var ore *values.OrdinalResolutionError
	if !errors.As(err, &ore) {
		t.Fatalf("out-of-range baked eval over a real PositionalRow must be a loud *OrdinalResolutionError, got %v", err)
	}
	if ore.Ordinal != 2 {
		t.Fatalf("OrdinalResolutionError.Ordinal = %d, want 2", ore.Ordinal)
	}
	if len(ore.Available) != 2 || ore.Available[0] != "ID" || ore.Available[1] != "W" {
		t.Fatalf("OrdinalResolutionError.Available = %v, want the row's names [ID W] (TypeNames diagnostics)", ore.Available)
	}
}

// TestRFC173S2_LegWindow_OutOfRangeIsLoud pins that a leg window's
// out-of-range miss surfaces through the EXISTING eval machinery as a loud
// OrdinalResolutionError enriched with the LEG's names — condition 3's
// "no new eval arm, loud-miss preserved" made observable.
func TestRFC173S2_LegWindow_OutOfRangeIsLoud(t *testing.T) {
	t.Parallel()
	corrA := values.NamedCorrelationIdentifier("a")
	corrB := values.NamedCorrelationIdentifier("b")
	qovA := values.NewQuantifiedObjectValueOfType(corrA, ojLegTypeAV())
	qovB := values.NewQuantifiedObjectValueOfType(corrB, ojLegTypeBW())
	rc := buildOrdinalJoinRC(t, qovA, qovB)
	spans, mergedType, ok := ordinalJoinSpans(rc)
	if !ok {
		t.Fatal("ordinalJoinSpans rejected the fixture RC")
	}
	merged := ojMergedRow(t, mergedType)
	binder := &legWindowBinder{spans: spans, row: merged}

	// Hand-built baked node past leg B's width (the constructor would refuse
	// the bake, so this models a stale/corrupted plan node).
	stale := &values.FieldValue{Field: "W", Typ: values.NotNullLong, Child: qovB, Resolved: &values.ResolvedAccessor{Ordinal: 5}}
	_, err := stale.Evaluate(&values.RowEvalContext{Correlations: binder})
	var ore *values.OrdinalResolutionError
	if !errors.As(err, &ore) {
		t.Fatalf("out-of-window baked eval must be a loud *OrdinalResolutionError, got %v", err)
	}
	if len(ore.Available) != 2 || ore.Available[0] != "ID" || ore.Available[1] != "W" {
		t.Fatalf("OrdinalResolutionError.Available = %v, want LEG B's names [ID W] — the window's TypeNames diagnostics", ore.Available)
	}
}
