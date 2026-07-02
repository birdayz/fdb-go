package values

import (
	"errors"
	"testing"
)

// rawDupIDRecord builds a RAW RecordType with two same-named fields [ID, ID] —
// the §5 duplicate-name shape. NewRecordType panics on duplicate names, so the
// raw literal is deliberate: Slice 2's 2-way join output (`SELECT * FROM a JOIN
// b` with same-named leg columns) makes this type constructible in prod for the
// first time, and the identity refinement below is what keeps the two columns
// from conflating into one memo member.
func rawDupIDRecord() *RecordType {
	return &RecordType{Fields: []Field{
		{Name: "ID", FieldType: NotNullLong, Ordinal: 0},
		{Name: "ID", FieldType: NotNullLong, Ordinal: 1},
	}}
}

// TestFieldValueBaked_DuplicateNameIdentityPin_RFC173S2 is the §5 duplicate-name
// identity pin (pulled into Slice 2 by the implementation contract): two BAKED
// FieldValues over the same child, with the SAME display name but DIFFERENT
// ordinals, are UNEQUAL and hash differently — name-based identity would intern
// them as one memo member → wrong plans. Also pins the rest of the refinement
// matrix: baked(0) vs baked(0) equal with equal hashes; baked vs lazy same-name
// UNEQUAL (worst case a missed dedup, never a conflation); lazy vs lazy
// name-only (unchanged).
func TestFieldValueBaked_DuplicateNameIdentityPin_RFC173S2(t *testing.T) {
	t.Parallel()
	qov := NewQuantifiedObjectValueOfType(NamedCorrelationIdentifier("q"), rawDupIDRecord())

	baked0, err := NewFieldValueOfOrdinal(qov, 0)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal(qov, 0): %v", err)
	}
	baked1, err := NewFieldValueOfOrdinal(qov, 1)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal(qov, 1): %v", err)
	}
	// Both resolve the SAME display name from the dup-named type…
	if baked0.Field != "ID" || baked1.Field != "ID" {
		t.Fatalf("display names = %q, %q, want ID, ID", baked0.Field, baked1.Field)
	}
	// …but they are different columns: UNEQUAL, different hashes.
	if EqualsWithoutChildren(baked0, baked1) {
		t.Fatal("baked ID#0 vs baked ID#1 must be UNEQUAL — name identity would conflate two different columns")
	}
	if SemanticHashCode(baked0) == SemanticHashCode(baked1) {
		t.Fatal("baked ID#0 and baked ID#1 must hash differently")
	}
	if ValuesStructurallyEqual(baked0, baked1) {
		t.Fatal("baked ID#0 vs baked ID#1 must be structurally UNEQUAL")
	}

	// baked(0) vs baked(0): equal, same hash (equal ⟹ same hash holds).
	baked0b, err := NewFieldValueOfOrdinal(qov, 0)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal(qov, 0) again: %v", err)
	}
	if !EqualsWithoutChildren(baked0, baked0b) || !ValuesStructurallyEqual(baked0, baked0b) {
		t.Fatal("two baked ID#0 nodes over the same child must be EQUAL")
	}
	if SemanticHashCode(baked0) != SemanticHashCode(baked0b) {
		t.Fatal("equal baked nodes must have equal hashes")
	}

	// Baked vs lazy, same name: UNEQUAL in both directions (contract: a missed
	// dedup at worst, never a conflation). Differing hashes are allowed since
	// they are unequal — no hash assertion here.
	lazy := NewFieldValue(qov, "ID", NotNullLong)
	if EqualsWithoutChildren(baked0, lazy) || EqualsWithoutChildren(lazy, baked0) {
		t.Fatal("baked vs lazy same-name must be UNEQUAL (both directions)")
	}

	// Lazy vs lazy: name-only, unchanged behavior.
	lazy2 := NewFieldValue(qov, "ID", NotNullLong)
	if !EqualsWithoutChildren(lazy, lazy2) {
		t.Fatal("lazy vs lazy same-name must stay EQUAL (name-only identity unchanged)")
	}
	if SemanticHashCode(lazy) != SemanticHashCode(lazy2) {
		t.Fatal("equal lazy nodes must have equal hashes (unchanged)")
	}
}

// TestFieldValueBaked_ResolveOrdinal_RFC173S2 pins that a BAKED node's ordinal
// is returned BEFORE the lazy child-type derivation: a node whose display name
// would lazily resolve to a DIFFERENT position still yields the baked ordinal,
// and a baked node with no child at all (e.g. after a passthrough copy) still
// resolves.
func TestFieldValueBaked_ResolveOrdinal_RFC173S2(t *testing.T) {
	t.Parallel()
	rt := NewRecordType("", false, []Field{
		{Name: "A", FieldType: NotNullLong, Ordinal: 0},
		{Name: "B", FieldType: NotNullLong, Ordinal: 1},
	})
	qov := NewQuantifiedObjectValueOfType(NamedCorrelationIdentifier("q"), rt)

	// Constructor path: baked ordinal round-trips.
	baked1, err := NewFieldValueOfOrdinal(qov, 1)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal: %v", err)
	}
	if ord, ok := baked1.resolveOrdinal(); !ok || ord != 1 {
		t.Fatalf("baked resolveOrdinal = (%d,%v), want (1,true)", ord, ok)
	}

	// Adversarial: display name "A" (lazy would derive 0), baked ordinal 1 —
	// the marker wins. Hand-built because the constructor never produces a
	// name/ordinal mismatch; a rebuild against a re-typed child can.
	mismatch := &FieldValue{Field: "A", Typ: NotNullLong, Child: qov, Resolved: &ResolvedAccessor{Ordinal: 1}}
	if ord, ok := mismatch.resolveOrdinal(); !ok || ord != 1 {
		t.Fatalf("baked-before-lazy: resolveOrdinal = (%d,%v), want (1,true) — the baked ordinal, not the lazy name derivation", ord, ok)
	}

	// Baked with nil child (a passthrough copy drops Child but keeps the
	// marker): still resolves — the marker precedes the nil-Child decline.
	orphan := &FieldValue{Field: "X", Resolved: &ResolvedAccessor{Ordinal: 2}}
	if ord, ok := orphan.resolveOrdinal(); !ok || ord != 2 {
		t.Fatalf("nil-child baked resolveOrdinal = (%d,%v), want (2,true)", ord, ok)
	}
}

// TestFieldValueBaked_ConstructorErrors_RFC173S2 pins NewFieldValueOfOrdinal's
// loud failures (Java raises — SemanticException FIELD_ACCESS_INPUT_NON_RECORD_TYPE
// / IndexOutOfBounds; no silent fallback): nil child, non-record child, and
// out-of-range ordinals all return *OrdinalBakeError. The success case reads
// display name and Typ from the child type at the ordinal.
func TestFieldValueBaked_ConstructorErrors_RFC173S2(t *testing.T) {
	t.Parallel()
	var obe *OrdinalBakeError

	// nil child.
	if _, err := NewFieldValueOfOrdinal(nil, 0); !errors.As(err, &obe) {
		t.Fatalf("nil child must be a loud OrdinalBakeError, got %v", err)
	}
	// Non-record child.
	prim := NewQuantifiedObjectValueOfType(NamedCorrelationIdentifier("p"), NotNullLong)
	if _, err := NewFieldValueOfOrdinal(prim, 0); !errors.As(err, &obe) {
		t.Fatalf("non-record child must be a loud OrdinalBakeError, got %v", err)
	}
	// Out of range, both ends.
	rt := NewRecordType("", false, []Field{
		{Name: "ID", FieldType: NotNullLong, Ordinal: 0},
		{Name: "V", FieldType: NullableString, Ordinal: 1},
	})
	qov := NewQuantifiedObjectValueOfType(NamedCorrelationIdentifier("q"), rt)
	if _, err := NewFieldValueOfOrdinal(qov, 2); !errors.As(err, &obe) {
		t.Fatalf("ordinal past the last field must be a loud OrdinalBakeError, got %v", err)
	}
	if _, err := NewFieldValueOfOrdinal(qov, -1); !errors.As(err, &obe) {
		t.Fatalf("negative ordinal must be a loud OrdinalBakeError, got %v", err)
	}

	// Success: display name + Typ come from the field at the ordinal.
	baked, err := NewFieldValueOfOrdinal(qov, 1)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal(qov, 1): %v", err)
	}
	if baked.Field != "V" {
		t.Fatalf("display name = %q, want V", baked.Field)
	}
	if baked.Typ != NullableString {
		t.Fatalf("Typ = %v, want NullableString", baked.Typ)
	}
	if baked.Resolved == nil || baked.Resolved.Ordinal != 1 {
		t.Fatalf("Resolved = %+v, want ordinal 1", baked.Resolved)
	}
}

// TestFieldValueBaked_EvaluateOrdinalAuthoritative_RFC173S2 pins that a BAKED
// node evaluated over an OrdinalRow reads slot `ordinal` even when the row
// TYPE's names disagree with the display name — the ordinal is authoritative,
// the name is diagnostics. The row deliberately places a column NAMED like the
// display name at a DIFFERENT slot: a name-based read would return the wrong
// value, not merely miss.
func TestFieldValueBaked_EvaluateOrdinalAuthoritative_RFC173S2(t *testing.T) {
	t.Parallel()
	rt := NewRecordType("", false, []Field{
		{Name: "ID", FieldType: NotNullLong, Ordinal: 0},
		{Name: "V", FieldType: NotNullLong, Ordinal: 1},
	})
	corr := UniqueCorrelationIdentifier()
	qov := NewQuantifiedObjectValueOfType(corr, rt)
	baked0, err := NewFieldValueOfOrdinal(qov, 0) // display name "ID", ordinal 0
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal: %v", err)
	}

	// Row names disagree: "ID" sits at slot 1 in the ROW's type. Ordinal 0 must
	// win — a name read would return 20.
	row := &fakeOrdinalRow{names: []string{"V", "ID"}, slots: []any{int64(10), int64(20)}}
	got, err := baked0.Evaluate(row)
	if err != nil {
		t.Fatalf("baked ordinal eval: %v", err)
	}
	if got != int64(10) {
		t.Fatalf("baked ID#0 over renamed row = %v, want 10 (slot 0; a name read would give 20)", got)
	}

	// Same through the correlated binding path (RowEvalContext).
	got, err = baked0.Evaluate(&RowEvalContext{Correlations: &ordEvalBinder{id: corr, bound: row}})
	if err != nil {
		t.Fatalf("correlated baked ordinal eval: %v", err)
	}
	if got != int64(10) {
		t.Fatalf("correlated baked ID#0 = %v, want 10", got)
	}

	// A baked ordinal past the row's slots is a LOUD OrdinalResolutionError,
	// never a silent NULL.
	baked1, err := NewFieldValueOfOrdinal(qov, 1)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal: %v", err)
	}
	short := &fakeOrdinalRow{slots: []any{int64(10)}}
	var ore *OrdinalResolutionError
	if _, err = baked1.Evaluate(short); !errors.As(err, &ore) {
		t.Fatalf("out-of-range baked ordinal must be a loud OrdinalResolutionError, got %v", err)
	}
}

// TestFieldValueBaked_CopyPreservesResolved_RFC173S2 pins the marker through
// every FieldValue copy/rebuild site in this package: WithChildren, Replace,
// RebaseValue (all funnel through withChildren), and the pullup/pushdown
// passthrough copies. A dropped marker silently degrades a baked node to lazy —
// the conflation hazard the identity refinement exists to prevent.
func TestFieldValueBaked_CopyPreservesResolved_RFC173S2(t *testing.T) {
	t.Parallel()
	dup := rawDupIDRecord()
	corrQ := NamedCorrelationIdentifier("q")
	corrR := NamedCorrelationIdentifier("r")
	qov := NewQuantifiedObjectValueOfType(corrQ, dup)
	qov2 := NewQuantifiedObjectValueOfType(corrR, dup)

	baked1, err := NewFieldValueOfOrdinal(qov, 1)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal: %v", err)
	}
	assertBaked := func(site string, v Value) {
		t.Helper()
		fv, ok := v.(*FieldValue)
		if !ok {
			t.Fatalf("%s: result is %T, want *FieldValue", site, v)
		}
		if fv.Resolved == nil || fv.Resolved.Ordinal != 1 {
			t.Fatalf("%s DROPPED the baked marker: Resolved = %+v, want ordinal 1 — silent baked→lazy degradation", site, fv.Resolved)
		}
		if fv.Field != "ID" {
			t.Fatalf("%s: display name = %q, want ID", site, fv.Field)
		}
	}

	// WithChildren — the rebuild every Replace/simplifier path funnels through.
	assertBaked("WithChildren", WithChildren(baked1, []Value{qov2}))

	// Replace — swap the child QOV; the rebuilt FieldValue must stay baked.
	replaced := Replace(baked1, func(v Value) Value {
		if q, ok := v.(*QuantifiedObjectValue); ok && q.Correlation == corrQ {
			return qov2
		}
		return v
	})
	assertBaked("Replace", replaced)

	// RebaseValue — alias remap rebuilds through withChildren.
	assertBaked("RebaseValue", RebaseValue(baked1, AliasMap{corrQ: corrR}))

	// Pullup/pushdown passthrough copies (these drop Child by design; the
	// marker must survive — the passthrough flows the identical record, so the
	// baked position stays valid).
	assertBaked("PullUpValue(passthrough)", PullUpValue(baked1, qov, NamedCorrelationIdentifier("up")))
	assertBaked("PushDownValue(passthrough)", PushDownValue(baked1, qov, NamedCorrelationIdentifier("up")))

	// Control: the copies still compare EQUAL to the original under the
	// refined identity (same name, same ordinal).
	if !EqualsWithoutChildren(baked1, WithChildren(baked1, []Value{qov2}).(*FieldValue)) {
		t.Fatal("a preserved copy must stay EQUAL to the original (name, ordinal)")
	}
}

// TestFieldValueBaked_ComposeOverRC_ByOrdinal_RFC173S2 pins the simplifier on
// the duplicate-name axis: composing a BAKED field access over a record
// constructor resolves by ORDINAL (Java's
// ComposeFieldValueOverRecordConstructorRule.findColumn is
// getColumns().get(fieldOrdinal)) — a name-based compose would return the FIRST
// same-named column no matter which one the ordinal denotes. Lazy compose stays
// name-based (unchanged), and a baked ordinal inconsistent with the RC declines
// rather than guessing.
func TestFieldValueBaked_ComposeOverRC_ByOrdinal_RFC173S2(t *testing.T) {
	t.Parallel()
	constA := &ConstantValue{Value: "a", Typ: NullableString}
	constB := &ConstantValue{Value: "b", Typ: NullableString}
	rc := &RecordConstructorValue{Fields: []RecordConstructorField{
		{Name: "ID", Value: constA},
		{Name: "ID", Value: constB},
	}}

	// Baked over the SECOND duplicate: composes to constB, not constA.
	baked1, err := NewFieldValueOfOrdinal(rc, 1)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal over RC: %v", err)
	}
	if got := SimplifyValue(baked1); got != constB {
		t.Fatalf("baked ID#1 over RC(ID:a, ID:b) simplified to %v, want the SECOND column (b) — name compose conflates duplicates", got)
	}
	// And the first, for symmetry.
	baked0, err := NewFieldValueOfOrdinal(rc, 0)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal over RC: %v", err)
	}
	if got := SimplifyValue(baked0); got != constA {
		t.Fatalf("baked ID#0 over RC(ID:a, ID:b) simplified to %v, want a", got)
	}

	// Lazy compose: unchanged name-based first-match.
	lazy := NewFieldValue(rc, "ID", NullableString)
	if got := SimplifyValue(lazy); got != constA {
		t.Fatalf("lazy ID over RC simplified to %v, want first-match a (unchanged)", got)
	}

	// A baked ordinal the node's OWN child RC cannot satisfy is a tree
	// inconsistency — a planner bug that must be LOUD (Java throws
	// IndexOutOfBounds), never a silent decline riding the broken node onward
	// (Torvalds catch on the earlier decline shape).
	stale := &FieldValue{Field: "ID", Typ: NullableString, Child: rc, Resolved: &ResolvedAccessor{Ordinal: 5}}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("out-of-range baked compose must PANIC (tree inconsistent with the bake), got a silent result")
			}
		}()
		SimplifyValue(stale)
	}()
}

// TestFieldValueBaked_PushDownThroughRC_ByOrdinal_RFC173S2 pins PushDownValue's
// record-constructor arm on the same duplicate-name axis as the compose rule: a
// BAKED output reference pushed through RC(ID:a, ID:b) resolves by ORDINAL to
// the column it denotes; a name lookup would always return the first. Lazy
// push-down stays name-based (unchanged); an out-of-range baked ordinal
// declines (nil) rather than guessing.
func TestFieldValueBaked_PushDownThroughRC_ByOrdinal_RFC173S2(t *testing.T) {
	t.Parallel()
	constA := &ConstantValue{Value: "a", Typ: NullableString}
	constB := &ConstantValue{Value: "b", Typ: NullableString}
	rc := &RecordConstructorValue{Fields: []RecordConstructorField{
		{Name: "ID", Value: constA},
		{Name: "ID", Value: constB},
	}}
	upper := NamedCorrelationIdentifier("up")

	baked1 := &FieldValue{Field: "ID", Typ: NullableString, Resolved: &ResolvedAccessor{Ordinal: 1}}
	if got := PushDownValue(baked1, rc, upper); got != constB {
		t.Fatalf("baked ID#1 pushed through RC(ID:a, ID:b) = %v, want the SECOND column (b)", got)
	}
	baked0 := &FieldValue{Field: "ID", Typ: NullableString, Resolved: &ResolvedAccessor{Ordinal: 0}}
	if got := PushDownValue(baked0, rc, upper); got != constA {
		t.Fatalf("baked ID#0 pushed through RC(ID:a, ID:b) = %v, want a", got)
	}

	// Lazy: unchanged name-based first-match.
	lazy := NewFlatFieldValue("ID", NullableString)
	if got := PushDownValue(lazy, rc, upper); got != constA {
		t.Fatalf("lazy ID pushed through RC = %v, want first-match a (unchanged)", got)
	}

	// Out-of-range baked ordinal: decline. Unlike the compose rule (where the
	// RC is the node's OWN child, so a mismatch is a tree inconsistency),
	// PushDownValue pairs the node with an EXTERNAL result value — nil is the
	// generic can't-push-down answer.
	stale := &FieldValue{Field: "ID", Typ: NullableString, Resolved: &ResolvedAccessor{Ordinal: 7}}
	if got := PushDownValue(stale, rc, upper); got != nil {
		t.Fatalf("out-of-range baked push-down must DECLINE (nil), got %v", got)
	}
}

// TestFieldValueBaked_LoudOnNameContext_RFC173S2 pins the guard Torvalds'
// review demanded: a BAKED node evaluated against a NAME-keyed row context is
// a loud *BakedNameContextError at every name-read arm — never a silent
// display-name read (which would return the FIRST of duplicate same-named
// columns). Lazy nodes on the same contexts keep the name model unchanged.
func TestFieldValueBaked_LoudOnNameContext_RFC173S2(t *testing.T) {
	t.Parallel()
	rt := NewRecordType("", false, []Field{
		{Name: "ID", FieldType: NotNullLong, Ordinal: 0},
	})
	corr := NamedCorrelationIdentifier("q")
	qov := NewQuantifiedObjectValueOfType(corr, rt)
	baked, err := NewFieldValueOfOrdinal(qov, 0)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal: %v", err)
	}
	nameRow := map[string]any{"ID": int64(7), "Q.ID": int64(7)}

	assertLoud := func(site string, got any, err error) {
		t.Helper()
		var bnce *BakedNameContextError
		if !errors.As(err, &bnce) {
			t.Fatalf("%s: baked node over a name-keyed context must be a loud *BakedNameContextError, got (%v, %v)", site, got, err)
		}
	}

	// Correlated arms (Child is a QOV, so evaluateCorrelated dispatches).
	got, err := baked.Evaluate(&RowEvalContext{Datum: nameRow})
	assertLoud("RowEvalContext.Datum", got, err)
	got, err = baked.Evaluate(&RowEvalContext{Correlations: &mapBinder{id: corr, m: nameRow}})
	assertLoud("RowEvalContext correlation map binding", got, err)
	got, err = baked.Evaluate(&mapBinder{id: corr, m: nameRow})
	assertLoud("bare CorrelationBinder map binding", got, err)
	got, err = baked.Evaluate(map[CorrelationIdentifier]map[string]any{corr: nameRow})
	assertLoud("per-correlation map", got, err)
	got, err = baked.Evaluate(nameRow)
	assertLoud("qualified-key map", got, err)

	// Non-correlated map arm (childless baked copy, e.g. post-passthrough).
	orphan := &FieldValue{Field: "ID", Typ: NotNullLong, Resolved: &ResolvedAccessor{Ordinal: 0}}
	got, err = orphan.Evaluate(nameRow)
	assertLoud("plain map arm", got, err)

	// nil context stays NULL (the appendNullLeg / nil-binding path, contract
	// ruling #3 — the null extension falls out, not an error).
	if v, err := orphan.Evaluate(nil); v != nil || err != nil {
		t.Fatalf("baked over nil context = (%v, %v), want (nil, nil)", v, err)
	}

	// An UNRECOGNIZED non-nil context (Evaluate's tail fall-through) is loud
	// for a baked node — a silent NULL there would hide a frontier bug. Lazy
	// keeps the historical silent NULL.
	type weirdCtx struct{}
	got, err = orphan.Evaluate(weirdCtx{})
	assertLoud("unrecognized context tail", got, err)
	if v, err := NewFlatFieldValue("ID", NotNullLong).Evaluate(weirdCtx{}); v != nil || err != nil {
		t.Fatalf("lazy over unrecognized context = (%v, %v), want silent (nil, nil) — unchanged", v, err)
	}

	// Lazy node over the same contexts: unchanged name model.
	lazy := NewFieldValue(qov, "ID", NotNullLong)
	if v, err := lazy.Evaluate(&RowEvalContext{Datum: nameRow}); err != nil || v != int64(7) {
		t.Fatalf("lazy over name context = (%v, %v), want (7, nil) — name model unchanged", v, err)
	}
}

// mapBinder is a test CorrelationBinder that binds one correlation to a
// name-keyed map row.
type mapBinder struct {
	id CorrelationIdentifier
	m  map[string]any
}

func (b *mapBinder) GetCorrelationBinding(id CorrelationIdentifier) (any, bool) {
	if id == b.id {
		return b.m, true
	}
	return nil, false
}

// TestFieldValueBaked_OracleNameBridge_RFC173S2 pins the §5 differential's
// sanctioned exception: with OracleBakedNameFallback set (test-only, travels
// with executor.DisablePositionalEmission), a baked node reads its display
// name against name-keyed contexts — recreating the pre-RFC-173 name model the
// oracle needs. NOT t.Parallel(): flips a process-global; the flag is restored
// before returning (same discipline as the dualwindow phase barrier).
func TestFieldValueBaked_OracleNameBridge_RFC173S2(t *testing.T) { //nolint:paralleltest
	rt := NewRecordType("", false, []Field{
		{Name: "ID", FieldType: NotNullLong, Ordinal: 0},
	})
	corr := NamedCorrelationIdentifier("q")
	qov := NewQuantifiedObjectValueOfType(corr, rt)
	baked, err := NewFieldValueOfOrdinal(qov, 0)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal: %v", err)
	}

	OracleBakedNameFallback = true
	defer func() { OracleBakedNameFallback = false }()

	if v, err := baked.Evaluate(&RowEvalContext{Datum: map[string]any{"Q.ID": int64(9)}}); err != nil || v != int64(9) {
		t.Fatalf("oracle-bridged baked eval = (%v, %v), want (9, nil) — the name-model read", v, err)
	}
}

// TestFieldValueBaked_PullUpThroughRC_Bakes_RFC173S2 pins the THIRD RC
// name-lookup consumer (Torvalds catch): pullUpThroughRecordConstructor
// re-frames the matched value as a reference to the RC's OUTPUT column, so the
// emitted node carries the matched ordinal BAKED whenever it matters — a baked
// input (bakedness survives pull-up) or a dup-named RC (where a lazy name node
// would later resolve to the FIRST same-named column no matter which matched).
// A lazy input over a clean-named RC stays lazy (dark: prod behavior
// unchanged).
func TestFieldValueBaked_PullUpThroughRC_Bakes_RFC173S2(t *testing.T) {
	t.Parallel()
	corr := NamedCorrelationIdentifier("q")
	up := NamedCorrelationIdentifier("up")
	constA := &ConstantValue{Value: "a", Typ: NullableString}
	constB := &ConstantValue{Value: "b", Typ: NullableString}

	// Dup-named RC, LAZY input matching the SECOND column: the pulled-up
	// reference must be baked at ordinal 1 (a lazy "ID" would resolve to 0).
	dupRC := &RecordConstructorValue{Fields: []RecordConstructorField{
		{Name: "ID", Value: constA},
		{Name: "ID", Value: constB},
	}}
	got := PullUpValue(constB, dupRC, up)
	fv, ok := got.(*FieldValue)
	if !ok {
		t.Fatalf("pull-up through dup RC = %T, want *FieldValue", got)
	}
	if fv.Resolved == nil || fv.Resolved.Ordinal != 1 {
		t.Fatalf("pull-up of the SECOND dup column: Resolved = %+v, want baked ordinal 1 — a lazy name node conflates to the first", fv.Resolved)
	}

	// BAKED input over a clean-named RC: bakedness survives, re-framed to the
	// OUTPUT ordinal.
	legRT := NewRecordType("", false, []Field{
		{Name: "X", FieldType: NullableString, Ordinal: 0},
		{Name: "Y", FieldType: NullableString, Ordinal: 1},
	})
	legQOV := NewQuantifiedObjectValueOfType(corr, legRT)
	bakedY, err := NewFieldValueOfOrdinal(legQOV, 1)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal: %v", err)
	}
	cleanRC := &RecordConstructorValue{Fields: []RecordConstructorField{
		{Name: "OUT0", Value: constA},
		{Name: "OUT1", Value: bakedY},
	}}
	got = PullUpValue(bakedY, cleanRC, up)
	fv, ok = got.(*FieldValue)
	if !ok {
		t.Fatalf("pull-up of baked input = %T, want *FieldValue", got)
	}
	if fv.Resolved == nil || fv.Resolved.Ordinal != 1 || fv.Field != "OUT1" {
		t.Fatalf("baked input pull-up = {Field:%s Resolved:%+v}, want OUT1 baked at 1", fv.Field, fv.Resolved)
	}

	// LAZY input over a clean-named RC: unchanged — lazy out (dark stage).
	lazyIn := NewFieldValue(legQOV, "X", NullableString)
	lazyRC := &RecordConstructorValue{Fields: []RecordConstructorField{
		{Name: "OUT0", Value: lazyIn},
	}}
	got = PullUpValue(lazyIn, lazyRC, up)
	fv, ok = got.(*FieldValue)
	if !ok {
		t.Fatalf("lazy pull-up = %T, want *FieldValue", got)
	}
	if fv.Resolved != nil {
		t.Fatalf("lazy input over clean RC must stay LAZY (prod behavior unchanged), got baked %+v", fv.Resolved)
	}
}
