package executor

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// This file pins the RFC-173 Slice 2 W3a-2 wiring: the ordinal-BIRTH state on
// the NLJ/flatMap cursors (dual emission — untouched name Datum + positional
// merged row), the birth-time predicate context, and the downstream
// leg-window dispatch in executeFilter/executeProjection/
// executePredicatesFilter/executeMap. Everything here is DARK-stage: no
// production plan carries an ordinal result value until W3b, so these tests
// hand-build the plans/cursors.

// --- shared fixtures ---------------------------------------------------------

// ojWiringLegs builds the wiring fixtures under UPPER-case aliases A/B (the
// executor-visible alias spelling): leg types A[ID,V] / B[ID,W] (dup ID across
// legs — the §5 axis), their typed QOVs, and the pristine seed RC.
func ojWiringLegs(t *testing.T) (legA, legB *values.RecordType, qovA, qovB *values.QuantifiedObjectValue, seed *values.RecordConstructorValue) {
	t.Helper()
	legA, legB = ojLegTypeAV(), ojLegTypeBW()
	qovA = values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("A"), legA)
	qovB = values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("B"), legB)
	seed = buildOrdinalJoinRC(t, qovA, qovB)
	return legA, legB, qovA, qovB, seed
}

// ojLegQR builds a frontier-shaped leg row: name-keyed Datum AND positional
// row over rt, mirroring each other slot-for-slot (the dual representation a
// scan emits).
func ojLegQR(t *testing.T, rt *values.RecordType, vals ...any) QueryResult {
	t.Helper()
	if len(vals) != len(rt.Fields) {
		t.Fatalf("ojLegQR: %d values for a %d-field type", len(vals), len(rt.Fields))
	}
	pos := NewPositionalRow(rt)
	m := make(map[string]any, len(vals))
	for i, v := range vals {
		pos.Set(i, v)
		m[rt.Fields[i].Name] = v
	}
	return QueryResult{Datum: m, Positional: pos}
}

// ojNameQR builds a NAME-model leg row: Datum only, no positional (an
// aggregate/union box output shape).
func ojNameQR(rt *values.RecordType, vals ...any) QueryResult {
	m := make(map[string]any, len(vals))
	for i, v := range vals {
		m[rt.Fields[i].Name] = v
	}
	return QueryResult{Datum: m}
}

// ojEqPred builds lhs = rhs.
func ojEqPred(lhs, rhs values.Value) predicates.QueryPredicate {
	return &predicates.ComparisonPredicate{
		Operand:    lhs,
		Comparison: predicates.Comparison{Type: predicates.ComparisonEquals, Operand: rhs},
	}
}

// ojAssertSlots asserts a positional row's slots exactly.
func ojAssertSlots(t *testing.T, pos *PositionalRow, want ...any) {
	t.Helper()
	if pos == nil {
		t.Fatal("row carries no positional row — expected an ordinal-birth emission")
	}
	if len(pos.Slots) != len(want) {
		t.Fatalf("positional row has %d slots, want %d", len(pos.Slots), len(want))
	}
	for i, w := range want {
		got, ok := pos.Get(i)
		if !ok || !reflect.DeepEqual(got, w) {
			t.Fatalf("slot %d = (%v, %v), want (%v, true)", i, got, ok, w)
		}
	}
}

// mustNLJCursor builds an nljCursor, failing the test on a birth-probe error.
// The shared constructor for the pre-existing cursor-mechanics tests (nil
// result value = name-model) and the ordinal-birth wiring tests below.
func mustNLJCursor(
	t *testing.T,
	outer recordlayer.RecordCursor[QueryResult],
	innerRows []QueryResult,
	joinType plans.JoinType,
	outerAlias, innerAlias string,
	preds []predicates.QueryPredicate,
	resultValue values.Value,
	evalCtx *EvaluationContext,
	st *recordlayer.ExecuteState,
) *nljCursor {
	t.Helper()
	c, err := newNLJCursor(outer, innerRows, joinType, outerAlias, innerAlias, preds, resultValue, evalCtx, st)
	if err != nil {
		t.Fatalf("newNLJCursor: %v", err)
	}
	return c
}

// --- ordinalJoinBirth constructor ---------------------------------------------

// TestRFC173S2_OrdinalJoinBirth_Constructor pins the once-at-construction
// birth probe: disabled (nil) for nil/lazy result values, enabled with
// windows for the pristine seed, enabled WITHOUT windows for a folded
// projection RC, and a LOUD error for a baked non-RC shape (planner bug —
// never a silent name-model demotion).
func TestRFC173S2_OrdinalJoinBirth_Constructor(t *testing.T) {
	t.Parallel()
	legA, legB, qovA, qovB, seed := ojWiringLegs(t)

	t.Run("nil result value disabled", func(t *testing.T) {
		t.Parallel()
		birth, err := newOrdinalJoinBirth(nil, nil)
		if err != nil || birth != nil {
			t.Fatalf("nil rv = (%v, %v), want (nil, nil)", birth, err)
		}
		if birth.enabled() {
			t.Fatal("nil birth must read as disabled")
		}
	})

	t.Run("lazy RC disabled", func(t *testing.T) {
		t.Parallel()
		lazy := values.NewRecordConstructorValue(
			values.RecordConstructorField{Name: "ID", Value: values.NewFieldValue(qovA, "ID", values.NotNullLong)},
		)
		birth, err := newOrdinalJoinBirth(lazy, nil)
		if err != nil || birth != nil {
			t.Fatalf("lazy RC = (%v, %v), want (nil, nil) — the name model's RCs are not birth sites", birth, err)
		}
	})

	t.Run("pristine seed enabled with windows", func(t *testing.T) {
		t.Parallel()
		birth, err := newOrdinalJoinBirth(seed, nil)
		if err != nil {
			t.Fatalf("seed birth: %v", err)
		}
		if !birth.enabled() || !birth.WindowsOK {
			t.Fatalf("seed birth = enabled %v, windows %v, want both true", birth.enabled(), birth.WindowsOK)
		}
		if len(birth.Spans) != 2 || birth.Spans[0].Offset != 0 || birth.Spans[0].Width != 2 || birth.Spans[1].Offset != 2 || birth.Spans[1].Width != 2 {
			t.Fatalf("seed spans = %+v, want offsets 0/2 widths 2/2", birth.Spans)
		}
		wantNames := []string{"ID", "V", "ID", "W"}
		if len(birth.OutputType.Fields) != len(wantNames) {
			t.Fatalf("output type has %d fields, want %d", len(birth.OutputType.Fields), len(wantNames))
		}
		for i, w := range wantNames {
			if birth.OutputType.Fields[i].Name != w || birth.OutputType.Fields[i].Ordinal != i {
				t.Fatalf("output field %d = {%q, ord %d}, want {%q, ord %d} — dup names preserved verbatim", i, birth.OutputType.Fields[i].Name, birth.OutputType.Fields[i].Ordinal, w, i)
			}
		}
		for _, id := range []values.CorrelationIdentifier{qovA.Correlation, qovB.Correlation} {
			if _, present := birth.LegTypes[id]; !present {
				t.Fatalf("LegTypes missing leg %s", id)
			}
		}
		if got := len(birth.LegTypes[qovA.Correlation].Fields); got != len(legA.Fields) {
			t.Fatalf("leg A type has %d fields, want %d", got, len(legA.Fields))
		}
		if got := len(birth.LegTypes[qovB.Correlation].Fields); got != len(legB.Fields) {
			t.Fatalf("leg B type has %d fields, want %d", got, len(legB.Fields))
		}
	})

	t.Run("folded RC enabled without windows", func(t *testing.T) {
		t.Parallel()
		bakedAV, err := values.NewFieldValueOfOrdinal(qovA, 1)
		if err != nil {
			t.Fatalf("NewFieldValueOfOrdinal: %v", err)
		}
		folded := values.NewRawRecordConstructorValue(
			values.RecordConstructorField{Name: "V", Value: bakedAV},
			values.RecordConstructorField{Name: "C", Value: &values.ConstantValue{Value: int64(7), Typ: values.NotNullLong}},
		)
		birth, err := newOrdinalJoinBirth(folded, nil)
		if err != nil {
			t.Fatalf("folded birth: %v", err)
		}
		if !birth.enabled() || birth.WindowsOK {
			t.Fatalf("folded birth = enabled %v, windows %v, want enabled without windows (plain projection row downstream)", birth.enabled(), birth.WindowsOK)
		}
		if _, present := birth.LegTypes[qovA.Correlation]; !present {
			t.Fatal("folded LegTypes must recover leg A from the baked reference")
		}
		if _, present := birth.LegTypes[qovB.Correlation]; present {
			t.Fatal("a leg folded away entirely must be ABSENT from LegTypes — no reference to it can exist")
		}
		if len(birth.OutputType.Fields) != 2 || birth.OutputType.Fields[0].Name != "V" || birth.OutputType.Fields[1].Name != "C" {
			t.Fatalf("folded output type = %v, want [V C]", typeFieldNames(birth.OutputType))
		}
	})

	t.Run("baked non-RC is a loud error", func(t *testing.T) {
		t.Parallel()
		bakedBare, err := values.NewFieldValueOfOrdinal(qovA, 0)
		if err != nil {
			t.Fatalf("NewFieldValueOfOrdinal: %v", err)
		}
		birth, err := newOrdinalJoinBirth(bakedBare, nil)
		if err == nil || birth != nil {
			t.Fatalf("baked non-RC = (%v, %v), want a loud error — an ordinal result value that is not an RC is a planner bug", birth, err)
		}
		if !strings.Contains(err.Error(), "RFC-173") {
			t.Fatalf("error %q must carry the RFC-173 marker", err)
		}
	})
}

// --- birth.evaluate -------------------------------------------------------------

// TestRFC173S2_OrdinalJoinBirth_Evaluate pins the one-shot birth: positional
// legs flow verbatim into the merged slots; a NIL QueryResult pointer is the
// NULL leg (that side's slots come out NULL — contract ruling #3); a
// NAME-model leg (Datum-only) is adapted by the leg type; a folded RV (baked
// ref + constant) evaluates correctly with leg bindings even without spans.
func TestRFC173S2_OrdinalJoinBirth_Evaluate(t *testing.T) {
	t.Parallel()
	legA, legB, qovA, _, seed := ojWiringLegs(t)
	birth, err := newOrdinalJoinBirth(seed, nil)
	if err != nil {
		t.Fatalf("seed birth: %v", err)
	}
	outerQR := ojLegQR(t, legA, int64(1), int64(10))
	innerQR := ojLegQR(t, legB, int64(2), int64(20))

	t.Run("both legs positional", func(t *testing.T) {
		t.Parallel()
		pos, err := birth.evaluate("A", "B", &outerQR, &innerQR, nil)
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		if pos.Type != birth.OutputType {
			t.Fatal("birthed row must carry the birth's single OutputType")
		}
		ojAssertSlots(t, pos, int64(1), int64(10), int64(2), int64(20))
	})

	t.Run("outer nil is the null leg", func(t *testing.T) {
		t.Parallel()
		pos, err := birth.evaluate("A", "B", nil, &innerQR, nil)
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		ojAssertSlots(t, pos, nil, nil, int64(2), int64(20))
	})

	t.Run("inner nil is the null leg", func(t *testing.T) {
		t.Parallel()
		pos, err := birth.evaluate("A", "B", &outerQR, nil, nil)
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		ojAssertSlots(t, pos, int64(1), int64(10), nil, nil)
	})

	t.Run("name-model leg adapted by leg type", func(t *testing.T) {
		t.Parallel()
		nameInner := ojNameQR(legB, int64(2), int64(20))
		pos, err := birth.evaluate("A", "B", &outerQR, &nameInner, nil)
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		ojAssertSlots(t, pos, int64(1), int64(10), int64(2), int64(20))
	})

	t.Run("folded RV evaluates with leg bindings", func(t *testing.T) {
		t.Parallel()
		bakedAV, err := values.NewFieldValueOfOrdinal(qovA, 1)
		if err != nil {
			t.Fatalf("NewFieldValueOfOrdinal: %v", err)
		}
		folded := values.NewRawRecordConstructorValue(
			values.RecordConstructorField{Name: "V", Value: bakedAV},
			values.RecordConstructorField{Name: "C", Value: &values.ConstantValue{Value: int64(7), Typ: values.NotNullLong}},
		)
		fb, err := newOrdinalJoinBirth(folded, nil)
		if err != nil {
			t.Fatalf("folded birth: %v", err)
		}
		// The inner leg is folded away (no LegTypes entry) AND name-model
		// (no positional row): adaptLegPositional(qr, nil) — a zero-width
		// row nothing references.
		nameInner := ojNameQR(legB, int64(2), int64(20))
		pos, err := fb.evaluate("A", "B", &outerQR, &nameInner, nil)
		if err != nil {
			t.Fatalf("folded evaluate: %v", err)
		}
		ojAssertSlots(t, pos, int64(10), int64(7))
	})
}

// --- nljCursor end-to-end (no FDB) ----------------------------------------------

// TestRFC173S2_NLJCursor_OrdinalBirth_InnerJoin drives an INNER join with the
// ordinal seed RV over two stub legs on the LINEAR path: emitted rows carry
// BOTH the untouched name-model Datum (mergeRows keys, byte-identical to a
// name-model join) and the correct positional merged row; a lazy leg
// predicate evaluates correctly through the birth-time predicate context
// (leg-relative against the adapted leg rows — correct even for the second
// leg); a BAKED predicate works through the leg bindings and is a loud error
// without them.
func TestRFC173S2_NLJCursor_OrdinalBirth_InnerJoin(t *testing.T) {
	t.Parallel()
	legA, legB, qovA, qovB, seed := ojWiringLegs(t)

	outerRows := []QueryResult{
		ojLegQR(t, legA, int64(1), int64(10)),
		ojLegQR(t, legA, int64(2), int64(20)),
	}
	innerRows := []QueryResult{
		ojLegQR(t, legB, int64(1), int64(100)),
		ojLegQR(t, legB, int64(3), int64(300)),
	}
	lazyPred := ojEqPred(
		values.NewFieldValue(qovA, "ID", values.NotNullLong),
		values.NewFieldValue(qovB, "ID", values.NotNullLong),
	)

	t.Run("lazy leg predicate + dual emission", func(t *testing.T) {
		t.Parallel()
		c := mustNLJCursor(t, recordlayer.FromList(outerRows), innerRows, plans.JoinInner,
			"A", "B", []predicates.QueryPredicate{lazyPred}, seed, EmptyEvaluationContext(), nil)
		defer c.Close()
		results := collectCursor(t, c)
		if len(results) != 1 {
			t.Fatalf("got %d rows, want 1 (only A.ID=1 matches B.ID=1)", len(results))
		}
		// The name-model Datum: mergeRows keys, untouched (dark dual emission).
		wantDatum := map[string]any{
			"ID": int64(1), "V": int64(10), "W": int64(100),
			"A.ID": int64(1), "A.V": int64(10),
			"B.ID": int64(1), "B.W": int64(100),
		}
		if !reflect.DeepEqual(results[0].Datum, wantDatum) {
			t.Fatalf("Datum = %v, want the untouched mergeRows map %v", results[0].Datum, wantDatum)
		}
		// The ordinal birth: the merged positional row.
		ojAssertSlots(t, results[0].Positional, int64(1), int64(10), int64(1), int64(100))
	})

	t.Run("baked predicate through leg bindings", func(t *testing.T) {
		t.Parallel()
		bakedBW, err := values.NewFieldValueOfOrdinal(qovB, 1)
		if err != nil {
			t.Fatalf("NewFieldValueOfOrdinal: %v", err)
		}
		bakedPred := ojEqPred(bakedBW, &values.ConstantValue{Value: int64(100), Typ: values.NotNullLong})
		c := mustNLJCursor(t, recordlayer.FromList(outerRows), innerRows, plans.JoinInner,
			"A", "B", []predicates.QueryPredicate{bakedPred}, seed, EmptyEvaluationContext(), nil)
		defer c.Close()
		results := collectCursor(t, c)
		// B.W=100 matches for BOTH outer rows (no leg-A condition).
		if len(results) != 2 {
			t.Fatalf("got %d rows, want 2 (baked B#1=100 matches once per outer row)", len(results))
		}
		ojAssertSlots(t, results[0].Positional, int64(1), int64(10), int64(1), int64(100))
		ojAssertSlots(t, results[1].Positional, int64(2), int64(20), int64(1), int64(100))

		// The red half: the same baked predicate WITHOUT the leg bindings
		// (today's name-model dispatch, a bare combined Datum) is a loud
		// BakedNameContextError — the leg bindings are what make it work.
		combined := mergeRows(outerRows[0], innerRows[0], "A", "B")
		_, perr := passesJoinPredicatesLegs(combined, []predicates.QueryPredicate{bakedPred}, EmptyEvaluationContext(), nil)
		var bnce *values.BakedNameContextError
		if !errors.As(perr, &bnce) {
			t.Fatalf("baked predicate without leg bindings must be a loud *BakedNameContextError, got %v", perr)
		}
	})
}

// TestRFC173S2_NLJCursor_OrdinalBirth_HashPath drives the ordinal birth
// through the HASH join path (≥100 inner rows + a single-column equijoin):
// same dual emission as the linear path.
func TestRFC173S2_NLJCursor_OrdinalBirth_HashPath(t *testing.T) {
	t.Parallel()
	legA, legB, qovA, qovB, seed := ojWiringLegs(t)

	innerRows := make([]QueryResult, 120)
	for i := range innerRows {
		innerRows[i] = ojLegQR(t, legB, int64(i), int64(i*10))
	}
	outerRows := []QueryResult{
		ojLegQR(t, legA, int64(5), int64(50)),
		ojLegQR(t, legA, int64(200), int64(2000)), // no hash match
	}
	pred := ojEqPred(
		values.NewFieldValue(qovA, "ID", values.NotNullLong),
		values.NewFieldValue(qovB, "ID", values.NotNullLong),
	)
	c := mustNLJCursor(t, recordlayer.FromList(outerRows), innerRows, plans.JoinInner,
		"A", "B", []predicates.QueryPredicate{pred}, seed, EmptyEvaluationContext(), nil)
	defer c.Close()
	if c.hashIndex == nil {
		t.Fatal("hash index was not built — this test must exercise the hash path")
	}
	results := collectCursor(t, c)
	if len(results) != 1 {
		t.Fatalf("got %d rows, want 1 (outer ID=5 matches inner ID=5)", len(results))
	}
	ojAssertSlots(t, results[0].Positional, int64(5), int64(50), int64(5), int64(50))
	m, isMap := results[0].Datum.(map[string]any)
	if !isMap || m["A.ID"] != int64(5) || m["B.W"] != int64(50) {
		t.Fatalf("hash-path Datum = %v, want the untouched mergeRows keys", results[0].Datum)
	}
}

// TestRFC173S2_NLJCursor_OrdinalBirth_LeftOuterNullLeg pins the
// unmatched-outer LEFT emission: the positional row carries the outer leg
// verbatim and the INNER slots NULL (the nil-pointer NULL leg — contract
// ruling #3's appendNullLeg equivalence), the Datum stays today's
// qualifyOuterRow map.
func TestRFC173S2_NLJCursor_OrdinalBirth_LeftOuterNullLeg(t *testing.T) {
	t.Parallel()
	legA, legB, qovA, qovB, seed := ojWiringLegs(t)

	outerRows := []QueryResult{
		ojLegQR(t, legA, int64(1), int64(10)),
		ojLegQR(t, legA, int64(2), int64(20)), // unmatched
	}
	innerRows := []QueryResult{ojLegQR(t, legB, int64(1), int64(100))}
	pred := ojEqPred(
		values.NewFieldValue(qovA, "ID", values.NotNullLong),
		values.NewFieldValue(qovB, "ID", values.NotNullLong),
	)
	c := mustNLJCursor(t, recordlayer.FromList(outerRows), innerRows, plans.JoinLeftOuter,
		"A", "B", []predicates.QueryPredicate{pred}, seed, EmptyEvaluationContext(), nil)
	defer c.Close()
	results := collectCursor(t, c)
	if len(results) != 2 {
		t.Fatalf("got %d rows, want 2 (matched + null-padded)", len(results))
	}
	ojAssertSlots(t, results[0].Positional, int64(1), int64(10), int64(1), int64(100))
	// The null-padded row: inner slots NULL.
	ojAssertSlots(t, results[1].Positional, int64(2), int64(20), nil, nil)
	// task #38: the null-padded Datum is SCHEMA-COMPLETE — fabricateNullLeg writes the NULL
	// leg (B) columns present-nil (qualified B.ID/B.W always, bare W if-absent so it doesn't
	// clobber A's ID), so a name-keyed read of B.* resolves to SQL NULL instead of tripping
	// the NameMissLoud guard. Bare ID/V stay A's (add-if-absent). Agrees with the Positional
	// null-pad column-for-column.
	wantDatum := map[string]any{
		"ID": int64(2), "V": int64(20),
		"A.ID": int64(2), "A.V": int64(20),
		"B.ID": nil, "B.W": nil,
		"W": nil,
	}
	if !reflect.DeepEqual(results[1].Datum, wantDatum) {
		t.Fatalf("null-padded Datum = %v, want the schema-complete map %v", results[1].Datum, wantDatum)
	}
}

// TestRFC173S2_NLJCursor_OrdinalBirth_FullDrain pins the FULL OUTER drain
// emission: an inner row that matched no outer row births with the OUTER
// slots NULL (the symmetric nil-pointer NULL leg).
func TestRFC173S2_NLJCursor_OrdinalBirth_FullDrain(t *testing.T) {
	t.Parallel()
	legA, legB, qovA, qovB, seed := ojWiringLegs(t)

	outerRows := []QueryResult{ojLegQR(t, legA, int64(1), int64(10))}
	innerRows := []QueryResult{
		ojLegQR(t, legB, int64(1), int64(100)),
		ojLegQR(t, legB, int64(3), int64(300)), // matches no outer row
	}
	pred := ojEqPred(
		values.NewFieldValue(qovA, "ID", values.NotNullLong),
		values.NewFieldValue(qovB, "ID", values.NotNullLong),
	)
	c := mustNLJCursor(t, recordlayer.FromList(outerRows), innerRows, plans.JoinFullOuter,
		"A", "B", []predicates.QueryPredicate{pred}, seed, EmptyEvaluationContext(), nil)
	defer c.Close()
	results := collectCursor(t, c)
	if len(results) != 2 {
		t.Fatalf("got %d rows, want 2 (matched + drained unmatched-inner)", len(results))
	}
	ojAssertSlots(t, results[0].Positional, int64(1), int64(10), int64(1), int64(100))
	// The drain row: outer slots NULL.
	ojAssertSlots(t, results[1].Positional, nil, nil, int64(3), int64(300))
}

// TestRFC173S2_NLJCursor_Oracle pins the §5 oracle switch on the ordinal-birth
// NLJ: with SetNameModelOracle(true) the same construction emits NO positional
// rows and the name-model Datum is EXACTLY the oracle-off Datum (the birth is
// pure addition). Deliberately NOT parallel: it owns the oracle globals for
// its duration (sequential test phase; every parallel test is paused).
func TestRFC173S2_NLJCursor_Oracle(t *testing.T) { //nolint:paralleltest // owns the §5 oracle globals
	legA, legB, qovA, qovB, seed := ojWiringLegs(t)
	outerRows := []QueryResult{
		ojLegQR(t, legA, int64(1), int64(10)),
		ojLegQR(t, legA, int64(2), int64(20)),
	}
	innerRows := []QueryResult{
		ojLegQR(t, legB, int64(1), int64(100)),
		ojLegQR(t, legB, int64(2), int64(200)),
	}
	pred := ojEqPred(
		values.NewFieldValue(qovA, "ID", values.NotNullLong),
		values.NewFieldValue(qovB, "ID", values.NotNullLong),
	)
	run := func(t *testing.T) []QueryResult {
		t.Helper()
		c := mustNLJCursor(t, recordlayer.FromList(outerRows), innerRows, plans.JoinInner,
			"A", "B", []predicates.QueryPredicate{pred}, seed, EmptyEvaluationContext(), nil)
		defer c.Close()
		return collectCursor(t, c)
	}

	oracleOff := run(t)
	if len(oracleOff) != 2 {
		t.Fatalf("oracle-off got %d rows, want 2", len(oracleOff))
	}
	for i, r := range oracleOff {
		if r.Positional == nil {
			t.Fatalf("oracle-off row %d carries no positional — the birth must fire", i)
		}
	}

	SetNameModelOracle(true)
	defer SetNameModelOracle(false)
	oracleOn := run(t)
	if len(oracleOn) != len(oracleOff) {
		t.Fatalf("oracle-on got %d rows, oracle-off %d — the oracle must not change results", len(oracleOn), len(oracleOff))
	}
	for i, r := range oracleOn {
		if r.Positional != nil {
			t.Fatalf("oracle-on row %d carries a positional row — the §5 oracle must suppress every birth", i)
		}
		if !reflect.DeepEqual(r.Datum, oracleOff[i].Datum) {
			t.Fatalf("oracle-on Datum %d = %v, oracle-off = %v — the name model must be untouched by the birth", i, r.Datum, oracleOff[i].Datum)
		}
	}
}

// TestRFC173S2_NLJCursor_DualEmissionInvariance is the cursor-level
// row-agreement pin: an ordinal-birth NLJ and an identical NAME-model NLJ
// (lazy anchored RV — today's join seed shape) over the same inputs emit
// deep-equal Datum maps row-for-row; only the ordinal one adds Positional.
func TestRFC173S2_NLJCursor_DualEmissionInvariance(t *testing.T) {
	t.Parallel()
	legA, legB, qovA, qovB, seed := ojWiringLegs(t)
	outerRows := []QueryResult{
		ojLegQR(t, legA, int64(1), int64(10)),
		ojLegQR(t, legA, int64(2), int64(20)),
	}
	innerRows := []QueryResult{
		ojLegQR(t, legB, int64(1), int64(100)),
		ojLegQR(t, legB, int64(2), int64(200)),
	}
	pred := ojEqPred(
		values.NewFieldValue(qovA, "ID", values.NotNullLong),
		values.NewFieldValue(qovB, "ID", values.NotNullLong),
	)
	// Today's name-model join seed: a lazy ANCHORED RC.
	anchored := &values.RecordConstructorValue{
		Fields: []values.RecordConstructorField{
			{Name: "ID", Value: values.NewFieldValue(qovA, "ID", values.NotNullLong)},
			{Name: "V", Value: values.NewFieldValue(qovA, "V", values.NotNullLong)},
			{Name: "ID_2", Value: values.NewFieldValue(qovB, "ID", values.NotNullLong)},
			{Name: "W", Value: values.NewFieldValue(qovB, "W", values.NotNullLong)},
		},
		AnchoredJoin: true,
	}

	collect := func(rv values.Value) []QueryResult {
		c := mustNLJCursor(t, recordlayer.FromList(outerRows), innerRows, plans.JoinInner,
			"A", "B", []predicates.QueryPredicate{pred}, rv, EmptyEvaluationContext(), nil)
		defer c.Close()
		return collectCursor(t, c)
	}
	ordinal := collect(seed)
	name := collect(anchored)
	if len(ordinal) != 2 || len(name) != 2 {
		t.Fatalf("got %d ordinal / %d name rows, want 2/2", len(ordinal), len(name))
	}
	for i := range ordinal {
		if !reflect.DeepEqual(ordinal[i].Datum, name[i].Datum) {
			t.Fatalf("row %d Datum diverged: ordinal %v, name %v — the dark-stage dual emission must keep the name model byte-identical", i, ordinal[i].Datum, name[i].Datum)
		}
		if ordinal[i].Positional == nil {
			t.Fatalf("ordinal row %d missing positional", i)
		}
		// RFC-173 cap: mergeRows is now Positional-NATIVE — a name-model
		// (AnchoredJoin) merge also emits a leg-windowed Positional
		// (concatLegPositionals, built by parallel construction from the leg rows'
		// own Positionals), so the name model can be deleted. The Datum stays
		// byte-identical (asserted above); the Positional is the ordinal sibling.
		// (The former invariant "name-model row carries no positional" was the §5
		// dark-stage gate that retires with the name map.)
		if name[i].Positional == nil {
			t.Fatalf("name-model row %d must now carry a leg-concat positional (RFC-173 cap: merge is Positional-native)", i)
		}
	}
}

// --- flatMapCursor computeResult -------------------------------------------------

// TestRFC173S2_FlatMap_ComputeResult pins the ordinal-birth flatMap: the
// positional row births from the RC with leg bindings; the coexistence Datum
// derives FROM the positional row via datumFromSpans (W3b fallout fix: a
// SEED-shaped RV mirrors the anchored RC's key set — bare keys last-wins on
// dup names, matching RecordConstructorValue.Evaluate's name map, PLUS the
// per-leg ALIAS.COL qualified keys downstream name-model consumers resolve
// dotted references against); the nil inner is the NULL leg.
func TestRFC173S2_FlatMap_ComputeResult(t *testing.T) {
	t.Parallel()
	legA, legB, qovA, qovB, seed := ojWiringLegs(t)
	newCursor := func(t *testing.T) *flatMapCursor {
		t.Helper()
		c, err := newFlatMapCursor(nil, nil, nil, nil, EmptyEvaluationContext(),
			qovA.Correlation, qovB.Correlation, seed, false, recordlayer.ExecuteProperties{})
		if err != nil {
			t.Fatalf("newFlatMapCursor: %v", err)
		}
		return c
	}
	outerQR := ojLegQR(t, legA, int64(1), int64(10))
	innerQR := ojLegQR(t, legB, int64(2), int64(20))

	t.Run("both legs", func(t *testing.T) {
		t.Parallel()
		c := newCursor(t)
		got, err := c.computeResult(outerQR, innerQR)
		if err != nil {
			t.Fatalf("computeResult: %v", err)
		}
		ojAssertSlots(t, got.Positional, int64(1), int64(10), int64(2), int64(20))
		// Datum derives FROM the positional row; bare keys are LAST-WINS on
		// the duplicate ID (B's ID — exactly what evaluating the RC into a
		// name map produces), and the seed shape adds the per-leg qualified
		// keys (datumFromSpans — the anchored RC's key set).
		wantDatum := map[string]any{
			"ID": int64(2), "V": int64(10), "W": int64(20),
			"A.ID": int64(1), "A.V": int64(10), "B.ID": int64(2), "B.W": int64(20),
		}
		if !reflect.DeepEqual(got.Datum, wantDatum) {
			t.Fatalf("Datum = %v, want datumFromSpans' bare+qualified map %v", got.Datum, wantDatum)
		}
		if !reflect.DeepEqual(got.Datum, map[string]any(datumFromSpans(got.Positional, c.birth.Spans))) {
			t.Fatal("Datum must be exactly datumFromSpans(Positional, spans)")
		}
	})

	t.Run("nil inner is the null leg", func(t *testing.T) {
		t.Parallel()
		c := newCursor(t)
		got, err := c.computeResultLegs(outerQR, nil)
		if err != nil {
			t.Fatalf("computeResultLegs: %v", err)
		}
		ojAssertSlots(t, got.Positional, int64(1), int64(10), nil, nil)
		// Last-wins on the dup ID: B's NULL ID wins the name key — the same
		// conflation the name model always had (positional keeps both). The
		// null leg's qualified keys are present with NULL values, exactly as
		// evaluating the anchored RC with an empty inner Datum produced.
		wantDatum := map[string]any{
			"ID": nil, "V": int64(10), "W": nil,
			"A.ID": int64(1), "A.V": int64(10), "B.ID": nil, "B.W": nil,
		}
		if !reflect.DeepEqual(got.Datum, wantDatum) {
			t.Fatalf("null-inner Datum = %v, want %v", got.Datum, wantDatum)
		}
	})
}

// TestRFC173S2_FlatMap_Oracle pins the flatMap oracle fallback: with
// SetNameModelOracle(true) computeResult takes TODAY'S Evaluate path — the
// baked RC resolves by display name via values.OracleBakedNameFallback over
// the name-keyed leg bindings — emitting the same (last-wins) Datum and NO
// positional row. Deliberately NOT parallel: owns the oracle globals.
func TestRFC173S2_FlatMap_Oracle(t *testing.T) { //nolint:paralleltest // owns the §5 oracle globals
	legA, legB, qovA, qovB, seed := ojWiringLegs(t)
	c, err := newFlatMapCursor(nil, nil, nil, nil, EmptyEvaluationContext(),
		qovA.Correlation, qovB.Correlation, seed, false, recordlayer.ExecuteProperties{})
	if err != nil {
		t.Fatalf("newFlatMapCursor: %v", err)
	}
	outerQR := ojLegQR(t, legA, int64(1), int64(10))
	innerQR := ojLegQR(t, legB, int64(2), int64(20))

	ordinalRow, err := c.computeResult(outerQR, innerQR)
	if err != nil {
		t.Fatalf("oracle-off computeResult: %v", err)
	}

	SetNameModelOracle(true)
	defer SetNameModelOracle(false)
	oracleRow, err := c.computeResult(outerQR, innerQR)
	if err != nil {
		t.Fatalf("oracle-on computeResult: %v", err)
	}
	if oracleRow.Positional != nil {
		t.Fatal("oracle-on flatMap must not birth a positional row")
	}
	if !reflect.DeepEqual(oracleRow.Datum, ordinalRow.Datum) {
		t.Fatalf("oracle-on Datum = %v, ordinal Datum = %v — the two paths must produce the same name map", oracleRow.Datum, ordinalRow.Datum)
	}
}

// --- downstream dispatch -----------------------------------------------------------

// TestRFC173S2_DownstreamLegWindows pins the construction-time spans probe:
// a direct ordinal-seed NLJ/FlatMap input yields spans; each enumerated
// PASSTHROUGH wrapper (sort, in-memory sort, limit, distinct, type filter,
// filter, predicates filter — the cursors that re-emit input rows verbatim)
// unwraps to the join; a non-join input, a folded-RV join, and a
// deliberately-EXCLUDED wrapper (FirstOrDefault fabricates a default row)
// yield false.
func TestRFC173S2_DownstreamLegWindows(t *testing.T) {
	t.Parallel()
	_, _, qovA, qovB, seed := ojWiringLegs(t)
	nlj := plans.NewRecordQueryNestedLoopJoinPlan(nil, nil, nil, plans.JoinInner, "A", "B", seed)

	assertSpans := func(t *testing.T, label string, p plans.RecordQueryPlan) {
		t.Helper()
		spans, ok := downstreamLegWindows(p)
		if !ok {
			t.Fatalf("%s: downstreamLegWindows(%T) declined, want the join's spans", label, p)
		}
		if len(spans) != 2 || spans[0].Offset != 0 || spans[0].Width != 2 || spans[1].Offset != 2 || spans[1].Width != 2 {
			t.Fatalf("%s: spans = %+v, want offsets 0/2 widths 2/2", label, spans)
		}
	}

	t.Run("direct NLJ", func(t *testing.T) {
		t.Parallel()
		assertSpans(t, "nlj", nlj)
	})
	t.Run("direct FlatMap", func(t *testing.T) {
		t.Parallel()
		assertSpans(t, "flatmap", plans.NewRecordQueryFlatMapPlan(nil, nil, qovA.Correlation, qovB.Correlation, seed, false))
	})
	t.Run("passthrough wrappers", func(t *testing.T) {
		t.Parallel()
		wrappers := map[string]plans.RecordQueryPlan{
			"sort":              plans.NewRecordQuerySortPlan(nil, nlj),
			"in-memory sort":    plans.NewRecordQueryInMemorySortPlan(nlj, nil),
			"limit":             plans.NewRecordQueryLimitPlan(nlj, 10, 0),
			"distinct":          plans.NewRecordQueryDistinctPlan(nlj),
			"type filter":       plans.NewRecordQueryTypeFilterPlan(nil, nlj),
			"filter":            plans.NewRecordQueryFilterPlan(nil, nlj),
			"predicates filter": plans.NewRecordQueryPredicatesFilterPlan(nlj, nil),
		}
		for name, w := range wrappers {
			assertSpans(t, name, w)
		}
		// Nested passthroughs unwrap transitively.
		assertSpans(t, "limit(distinct(sort))", plans.NewRecordQueryLimitPlan(plans.NewRecordQueryDistinctPlan(plans.NewRecordQuerySortPlan(nil, nlj)), 5, 0))
	})
	t.Run("non-join input declines", func(t *testing.T) {
		t.Parallel()
		if _, ok := downstreamLegWindows(plans.NewRecordQueryMapPlan(nlj, seed)); ok {
			t.Fatal("a Map input must decline — it rewrites rows (and is itself a dispatch site)")
		}
		if _, ok := downstreamLegWindows(nil); ok {
			t.Fatal("a nil input must decline")
		}
	})
	t.Run("folded-RV join declines", func(t *testing.T) {
		t.Parallel()
		bakedAV, err := values.NewFieldValueOfOrdinal(qovA, 1)
		if err != nil {
			t.Fatalf("NewFieldValueOfOrdinal: %v", err)
		}
		folded := values.NewRawRecordConstructorValue(
			values.RecordConstructorField{Name: "V", Value: bakedAV},
		)
		foldedNLJ := plans.NewRecordQueryNestedLoopJoinPlan(nil, nil, nil, plans.JoinInner, "A", "B", folded)
		if _, ok := downstreamLegWindows(foldedNLJ); ok {
			t.Fatal("a folded-RV join must decline — its output is a plain projection row, no leg windows")
		}
	})
	t.Run("excluded wrapper declines", func(t *testing.T) {
		t.Parallel()
		if _, ok := downstreamLegWindows(plans.NewRecordQueryFirstOrDefaultPlan(nlj, nil)); ok {
			t.Fatal("FirstOrDefault is deliberately NOT a passthrough (it fabricates a default row) — must decline")
		}
	})
}

// TestRFC173S2_LegWindowRowContext pins the REAL downstream context builder —
// the downstream half of the W3a-1 wrong-slot hazard: a lazy SECOND-leg
// reference over the merged positional row resolves correctly through the leg
// windows, and an outer correlation still resolves via the base
// EvaluationContext binder.
func TestRFC173S2_LegWindowRowContext(t *testing.T) {
	t.Parallel()
	_, _, _, qovB, seed := ojWiringLegs(t)
	spans, mergedType, ok := ordinalJoinSpans(seed)
	if !ok {
		t.Fatal("ordinalJoinSpans rejected the seed")
	}
	merged := ojMergedRow(t, mergedType) // [A.ID=1, A.V=10, B.ID=2, B.W=20]

	outerID := values.NamedCorrelationIdentifier("OUT")
	ec := EmptyEvaluationContext().WithBinding(outerID, map[string]any{"X": int64(42)})
	rowCtx := legWindowRowContext(merged, ec, spans)

	// The hazard pin, through the REAL builder: lazy B.W's leg-relative
	// ordinal is 1 — the bare merged row would misread absolute slot 1
	// (A.V=10); the window reads merged slot 3 = 20.
	lazyBW := values.NewFieldValue(qovB, "W", values.NotNullLong)
	got, err := lazyBW.Evaluate(rowCtx)
	if err != nil {
		t.Fatalf("lazy B.W through legWindowRowContext: %v", err)
	}
	if got != int64(20) {
		t.Fatalf("lazy B.W = %v, want 20 (B's W through the leg window, not A's V at absolute slot 1)", got)
	}
	// A baked second-leg reference reads the same correct slot.
	bakedBID, err := values.NewFieldValueOfOrdinal(qovB, 0)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal: %v", err)
	}
	if got, err := bakedBID.Evaluate(rowCtx); err != nil || got != int64(2) {
		t.Fatalf("baked B#0 = (%v, %v), want (2, nil)", got, err)
	}
	// An OUTER correlation delegates through the base EvaluationContext.
	outerRef := values.NewFieldValue(values.NewQuantifiedObjectValue(outerID), "X", values.NotNullLong)
	if got, err := outerRef.Evaluate(rowCtx); err != nil || got != int64(42) {
		t.Fatalf("outer correlation OUT.X = (%v, %v), want (42, nil) — must resolve via the base binder", got, err)
	}
}

// TestRFC173S2_SpanAware_BareDupName_DivergencePin (review W3b-1 ACK note):
// a flat BARE name over a dup-named merged row resolves FIRST-MATCH through
// spanAwareRow (merged-type FieldIndex) but LAST-WINS through the coexistence
// Datum (map writes) — a real model divergence that is UNREACHABLE in
// production only because the resolver rejects ambiguous bare references
// (42702) before planning. This pin freezes both behaviors so the divergence
// stays unconstructible: if either side changes, or a resolver change ever
// lets an ambiguous bare reference through, this is the axis to re-examine.
func TestRFC173S2_SpanAware_BareDupName_DivergencePin(t *testing.T) {
	t.Parallel()
	corrA := values.NamedCorrelationIdentifier("a")
	corrB := values.NamedCorrelationIdentifier("b")
	legA := &values.RecordType{Fields: []values.Field{{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0}}}
	legB := &values.RecordType{Fields: []values.Field{{Name: "ID", FieldType: values.NotNullLong, Ordinal: 1}}}
	qovA := values.NewQuantifiedObjectValueOfType(corrA, legA)
	qovB := values.NewQuantifiedObjectValueOfType(corrB, legB)
	bA, err := values.NewFieldValueOfOrdinal(qovA, 0)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal: %v", err)
	}
	bB, err := values.NewFieldValueOfOrdinal(qovB, 0)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal: %v", err)
	}
	rc := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "ID", Value: bA},
		values.RecordConstructorField{Name: "ID", Value: bB},
	)
	spans, mergedType, ok := ordinalJoinSpans(rc)
	if !ok {
		t.Fatal("fixture RC rejected by ordinalJoinSpans")
	}
	row := NewPositionalRow(mergedType)
	row.Set(0, int64(1)) // a.id
	row.Set(1, int64(2)) // b.id

	sa := &spanAwareRow{parent: row, spans: spans}
	if v, found := sa.GetByName("ID"); !found || v != int64(1) {
		t.Fatalf("spanAwareRow bare ID = (%v, %v), want FIRST-match (1)", v, found)
	}
	if m := datumFromSpans(row, spans); m["ID"] != int64(2) {
		t.Fatalf("datumFromSpans bare ID = %v, want LAST-wins (2)", m["ID"])
	}
}

// TestRFC173S2_NLJ_FoldedRVDroppedLeg_PredTypes pins the PR-447 review P1: a
// FOLDED result value can DROP a leg entirely while a baked cross-leg ON
// predicate still references it — LegTypes derived from the RV alone misses
// the dropped leg, so a NAME-model (Datum-only) row for that leg adapted with
// a nil leg type became a ZERO-WIDTH binding and the baked predicate blew up
// (loud OrdinalResolutionError on a legitimate plan). LegTypes must be
// collected from the result value AND the predicates.
func TestRFC173S2_NLJ_FoldedRVDroppedLeg_PredTypes(t *testing.T) {
	t.Parallel()
	legA, legB, qovA, qovB, _ := ojWiringLegs(t)

	// Folded RV: only leg A appears ({A.V baked, const}) — leg B dropped.
	bakedAV, err := values.NewFieldValueOfOrdinal(qovA, 1)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal: %v", err)
	}
	foldedRV := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "V", Value: bakedAV},
		values.RecordConstructorField{Name: "_1", Value: &values.ConstantValue{Value: int64(1), Typ: values.NotNullLong}},
	)

	// Baked cross-leg ON predicate: A.ID = B.ID — references the DROPPED leg.
	bakedAID, err := values.NewFieldValueOfOrdinal(qovA, 0)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal: %v", err)
	}
	bakedBID, err := values.NewFieldValueOfOrdinal(qovB, 0)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal: %v", err)
	}
	pred := ojEqPred(bakedAID, bakedBID)

	outerRows := []QueryResult{
		ojLegQR(t, legA, int64(1), int64(10)),
		ojLegQR(t, legA, int64(2), int64(20)),
	}
	// Leg B rows are NAME-model (Datum only — an aggregate-box shape): the
	// adapter must synthesize them from the PREDICATE-derived leg type.
	innerRows := []QueryResult{
		ojNameQR(legB, int64(1), int64(100)),
		ojNameQR(legB, int64(3), int64(300)),
	}

	c := mustNLJCursor(t, recordlayer.FromList(outerRows), innerRows, plans.JoinInner,
		"A", "B", []predicates.QueryPredicate{pred}, foldedRV, EmptyEvaluationContext(), nil)
	defer c.Close()
	results := collectCursor(t, c)
	if len(results) != 1 {
		t.Fatalf("got %d rows, want 1 (only A.ID=1 matches B.ID=1) — a dropped-leg zero-width binding misfilters or errors", len(results))
	}
	// The folded output row: [A.V, 1].
	ojAssertSlots(t, results[0].Positional, int64(10), int64(1))
}

// TestRFC173S2_FlatMap_FoldedRVDroppedLeg_PlanTypes pins the FlatMap half of
// the PR-447 review P1 (@claude final-pass catch): the correlated FlatMap
// implementation pushes the gated join's baked ON references INTO the inner
// plan, so a folded result value that DROPS the outer leg leaves the birth
// typeless for it even though the inner plan still references it — a
// Datum-only outer row (aggregate-box shape) then bound zero-width and the
// baked SARG died loudly on a legitimate plan. newFlatMapCursor must widen
// LegTypes from the inner plan's predicate surfaces.
func TestRFC173S2_FlatMap_FoldedRVDroppedLeg_PlanTypes(t *testing.T) {
	t.Parallel()
	legA, _, qovA, qovB, _ := ojWiringLegs(t)
	outerCorr := values.NamedCorrelationIdentifier("A")

	// Folded RV: only leg B appears — the OUTER leg A dropped.
	bakedBW, err := values.NewFieldValueOfOrdinal(qovB, 1)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal: %v", err)
	}
	foldedRV := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "W", Value: bakedBW},
	)

	// The inner plan carries the baked ON reference to the dropped OUTER leg
	// (a residual PredicatesFilter — the correlated-implementation shape).
	bakedAID, err := values.NewFieldValueOfOrdinal(qovA, 0)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal: %v", err)
	}
	bakedBID, err := values.NewFieldValueOfOrdinal(qovB, 0)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal: %v", err)
	}
	innerPlan := plans.NewRecordQueryPredicatesFilterPlan(
		plans.NewRecordQueryScanPlan(nil, values.UnknownType, false),
		[]predicates.QueryPredicate{ojEqPred(bakedBID, bakedAID)},
	)

	c, err := newFlatMapCursor(nil, nil, innerPlan, nil, EmptyEvaluationContext(),
		outerCorr, values.NamedCorrelationIdentifier("B"), foldedRV, false,
		recordlayer.ExecuteProperties{})
	if err != nil {
		t.Fatalf("newFlatMapCursor: %v", err)
	}
	// The birth must know the dropped OUTER leg's type from the inner plan's
	// baked reference…
	outerType := c.birth.legType(outerCorr)
	if outerType == nil {
		t.Fatal("LegTypes missing the RV-dropped OUTER leg — the inner plan's baked SARG/residual references it (the FlatMap P1 half)")
	}
	if len(outerType.Fields) != len(legA.Fields) {
		t.Fatalf("outer leg type has %d fields, want %d", len(outerType.Fields), len(legA.Fields))
	}
	// …so a Datum-only outer row (aggregate-box shape) adapts to a full-width
	// binding the baked reference can read — not the zero-width death row.
	adapted, aerr := adaptLegPositional(ojNameQR(legA, int64(7), int64(70)), outerType)
	if aerr != nil {
		t.Fatalf("adaptLegPositional: %v", aerr)
	}
	if v, ok := adapted.Get(0); !ok || v != int64(7) {
		t.Fatalf("adapted outer slot 0 = (%v, %v), want (7, true) — zero-width binding means the widening failed", v, ok)
	}
}

// TestRFC173Item3_SpanAwareRow_BoxAliasReadsLeaf pins the dotted-read bridge's
// box-span precedence (RFC-173 item 3): a BOX span is NAMED by its rightmost
// LEAF, so an alias-qualified read through the box name must window that
// LEAF's slice — never the whole concat, whose FieldIndex would first-match
// an earlier BURIED leg's duplicate column name (silently the wrong column).
// The fixture's buried leg B deliberately carries an "ID" column ahead of the
// leaf's own "ID": pre-fix "E.ID" read B's.
func TestRFC173Item3_SpanAwareRow_BoxAliasReadsLeaf(t *testing.T) {
	t.Parallel()
	corrA := values.NamedCorrelationIdentifier("A")
	corrE := values.NamedCorrelationIdentifier("E")
	legA := &values.RecordType{Fields: []values.Field{{Name: "AID", FieldType: values.NotNullLong, Ordinal: 0}}}
	boxTyp := &values.RecordType{
		Fields: []values.Field{
			{Name: "BID", FieldType: values.NotNullLong, Ordinal: 0},
			{Name: "ID", FieldType: values.NotNullLong, Ordinal: 1}, // buried B's ID — the first-match trap
			{Name: "ID", FieldType: values.NotNullLong, Ordinal: 2}, // the leaf E's own ID
		},
		Legs: []values.RecordTypeLeg{{Name: "B", Start: 0, Width: 2}, {Name: "E", Start: 2, Width: 1}},
	}
	qovA := values.NewQuantifiedObjectValueOfType(corrA, legA)
	qovE := values.NewQuantifiedObjectValueOfType(corrE, boxTyp)
	var fields []values.RecordConstructorField
	for i := 0; i < 1; i++ {
		fv, err := values.NewFieldValueOfOrdinal(qovA, i)
		if err != nil {
			t.Fatalf("bake A#%d: %v", i, err)
		}
		fields = append(fields, values.RecordConstructorField{Name: fv.Field, Value: fv})
	}
	for i := 0; i < 3; i++ {
		fv, err := values.NewFieldValueOfOrdinal(qovE, i)
		if err != nil {
			t.Fatalf("bake E#%d: %v", i, err)
		}
		fields = append(fields, values.RecordConstructorField{Name: fv.Field, Value: fv})
	}
	rc := values.NewRawRecordConstructorValue(fields...)
	spans, mergedType, ok := ordinalJoinSpans(rc)
	if !ok {
		t.Fatal("fixture RC rejected by ordinalJoinSpans")
	}
	row := NewPositionalRow(mergedType)
	row.Set(0, int64(10)) // a.aid
	row.Set(1, int64(20)) // b.bid
	row.Set(2, int64(21)) // b.id — the trap value
	row.Set(3, int64(30)) // e.id — the correct read
	sa := &spanAwareRow{parent: row, spans: spans}
	if v, found := sa.GetByName("E.ID"); !found || v != int64(30) {
		t.Fatalf("E.ID = (%v, %v), want the rightmost LEAF's column (30), not the buried dup (21)", v, found)
	}
	if v, found := sa.GetByName("B.ID"); !found || v != int64(21) {
		t.Fatalf("B.ID = (%v, %v), want the buried leg's column (21)", v, found)
	}
	if v, found := sa.GetByName("B.BID"); !found || v != int64(20) {
		t.Fatalf("B.BID = (%v, %v), want 20", v, found)
	}
	if v, found := sa.GetByName("A.AID"); !found || v != int64(10) {
		t.Fatalf("A.AID = (%v, %v), want 10", v, found)
	}
}
