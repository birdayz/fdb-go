package values

import (
	"errors"
	"testing"
)

// rawDupIDRecord builds a RAW RecordType with two same-named fields [ID, ID] —
// the duplicate-name shape. NewRecordType panics on duplicate names, so the
// raw literal is deliberate: a 2-way join output (`SELECT * FROM a JOIN b`
// with same-named leg columns) makes this type constructible in production,
// and the identity refinement below is what keeps the two columns from
// conflating into one memo member.
func rawDupIDRecord() *RecordType {
	return &RecordType{Fields: []Field{
		{Name: "ID", FieldType: NotNullLong, Ordinal: 0},
		{Name: "ID", FieldType: NotNullLong, Ordinal: 1},
	}}
}

// TestFieldValueBaked_DuplicateNameIdentityPin is the duplicate-name identity
// pin: two BAKED FieldValues over the same child, with the SAME display name
// but DIFFERENT ordinals, are UNEQUAL and hash differently — name-based
// identity would intern them as one memo member → wrong plans. Also pins the
// rest of the refinement matrix: baked(0) vs baked(0) equal with equal
// hashes; baked vs lazy same-name UNEQUAL (worst case a missed dedup, never a
// conflation); lazy vs lazy name-only (unchanged).
func TestFieldValueBaked_DuplicateNameIdentityPin(t *testing.T) {
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

// TestFieldValueBaked_ResolveOrdinal pins that a BAKED node's ordinal is
// returned BEFORE the lazy child-type derivation: a node whose display name
// would lazily resolve to a DIFFERENT position still yields the baked ordinal,
// and a baked node with no child at all (e.g. after a passthrough copy) still
// resolves.
func TestFieldValueBaked_ResolveOrdinal(t *testing.T) {
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
	mismatch := &FieldValue{Field: "A", Typ: NotNullLong, Child: qov, Resolved: NewFieldPathOfSingle("A", 1, true)}
	if ord, ok := mismatch.resolveOrdinal(); !ok || ord != 1 {
		t.Fatalf("baked-before-lazy: resolveOrdinal = (%d,%v), want (1,true) — the baked ordinal, not the lazy name derivation", ord, ok)
	}

	// Baked with nil child (a passthrough copy drops Child but keeps the
	// marker): still resolves — the marker precedes the nil-Child decline.
	orphan := &FieldValue{Field: "X", Resolved: NewFieldPathOfSingle("X", 2, true)}
	if ord, ok := orphan.resolveOrdinal(); !ok || ord != 2 {
		t.Fatalf("nil-child baked resolveOrdinal = (%d,%v), want (2,true)", ord, ok)
	}
}

// TestFieldValueBaked_ConstructorErrors pins NewFieldValueOfOrdinal's loud
// failures (Java raises — SemanticException FIELD_ACCESS_INPUT_NON_RECORD_TYPE
// / IndexOutOfBounds; no silent fallback): nil child, non-record child, and
// out-of-range ordinals all return *OrdinalBakeError. The success case reads
// display name and Typ from the child type at the ordinal.
func TestFieldValueBaked_ConstructorErrors(t *testing.T) {
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
	if baked.Resolved == nil || baked.Resolved.Root().Ordinal != 1 {
		t.Fatalf("Resolved = %+v, want ordinal 1", baked.Resolved)
	}
}

// TestFieldValueBaked_EvaluateOrdinalAuthoritative pins that a BAKED node
// evaluated over an OrdinalRow reads slot `ordinal` even when the row TYPE's
// names disagree with the display name — the ordinal is authoritative, the
// name is diagnostics. The row deliberately places a column NAMED like the
// display name at a DIFFERENT slot: a name-based read would return the wrong
// value, not merely miss.
func TestFieldValueBaked_EvaluateOrdinalAuthoritative(t *testing.T) {
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

// TestFieldValueBaked_CopyPreservesResolved pins the marker through every
// FieldValue copy/rebuild site in this package: WithChildren, Replace,
// RebaseValue (all funnel through withChildren), and the pullup/pushdown
// passthrough copies. A dropped marker silently degrades a baked node to lazy —
// the conflation hazard the identity refinement exists to prevent.
func TestFieldValueBaked_CopyPreservesResolved(t *testing.T) {
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
		if fv.Resolved == nil || fv.Resolved.Root().Ordinal != 1 {
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

// TestFieldValueBaked_ComposeOverRC_ByOrdinal pins the simplifier on the
// duplicate-name axis: composing a BAKED field access over a record
// constructor resolves by ORDINAL (Java's
// ComposeFieldValueOverRecordConstructorRule.findColumn is
// getColumns().get(fieldOrdinal)) — a name-based compose would return the FIRST
// same-named column no matter which one the ordinal denotes. Lazy compose stays
// name-based (unchanged), and a baked ordinal inconsistent with the RC declines
// rather than guessing.
func TestFieldValueBaked_ComposeOverRC_ByOrdinal(t *testing.T) {
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

	// Lazy compose over an AMBIGUOUS (duplicated) name: DECLINE — no fold, the
	// node rides unchanged (there is no defensible first match). Over a
	// UNIQUE name the fold is unchanged.
	lazy := NewFieldValue(rc, "ID", NullableString)
	if got := SimplifyValue(lazy); got != lazy {
		t.Fatalf("lazy ID over dup-named RC simplified to %v, want DECLINE (node unchanged) — no defensible first match", got)
	}
	cleanRC := &RecordConstructorValue{Fields: []RecordConstructorField{
		{Name: "ID", Value: constA},
		{Name: "X", Value: constB},
	}}
	lazyClean := NewFieldValue(cleanRC, "ID", NullableString)
	if got := SimplifyValue(lazyClean); got != constA {
		t.Fatalf("lazy ID over clean RC simplified to %v, want a (unchanged)", got)
	}

	// A baked ordinal the node's OWN child RC cannot satisfy is a tree
	// inconsistency — a planner bug that must be LOUD (Java throws
	// IndexOutOfBounds), never a silent decline riding the broken node onward.
	stale := &FieldValue{Field: "ID", Typ: NullableString, Child: rc, Resolved: NewFieldPathOfSingle("ID", 5, true)}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("out-of-range baked compose must PANIC (tree inconsistent with the bake), got a silent result")
			}
		}()
		SimplifyValue(stale)
	}()
}

// TestFieldValueBaked_PushDownThroughRC_ByOrdinal pins PushDownValue's
// record-constructor arm on the same duplicate-name axis as the compose rule: a
// BAKED output reference pushed through RC(ID:a, ID:b) resolves by ORDINAL to
// the column it denotes; a name lookup would always return the first. Lazy
// push-down stays name-based (unchanged); an out-of-range baked ordinal
// declines (nil) rather than guessing.
func TestFieldValueBaked_PushDownThroughRC_ByOrdinal(t *testing.T) {
	t.Parallel()
	constA := &ConstantValue{Value: "a", Typ: NullableString}
	constB := &ConstantValue{Value: "b", Typ: NullableString}
	rc := &RecordConstructorValue{Fields: []RecordConstructorField{
		{Name: "ID", Value: constA},
		{Name: "ID", Value: constB},
	}}
	upper := NamedCorrelationIdentifier("up")

	baked1 := &FieldValue{Field: "ID", Typ: NullableString, Resolved: NewFieldPathOfSingle("ID", 1, false)}
	if got := PushDownValue(baked1, rc, upper); got != constB {
		t.Fatalf("baked ID#1 pushed through RC(ID:a, ID:b) = %v, want the SECOND column (b)", got)
	}
	baked0 := &FieldValue{Field: "ID", Typ: NullableString, Resolved: NewFieldPathOfSingle("ID", 0, false)}
	if got := PushDownValue(baked0, rc, upper); got != constA {
		t.Fatalf("baked ID#0 pushed through RC(ID:a, ID:b) = %v, want a", got)
	}

	// Lazy over an AMBIGUOUS name: DECLINE (nil) — same rationale as the
	// compose arm. Over a unique name: unchanged.
	lazy := NewFlatFieldValue("ID", NullableString)
	if got := PushDownValue(lazy, rc, upper); got != nil {
		t.Fatalf("lazy ID pushed through dup-named RC = %v, want DECLINE (nil)", got)
	}
	cleanRC := &RecordConstructorValue{Fields: []RecordConstructorField{
		{Name: "ID", Value: constA},
		{Name: "X", Value: constB},
	}}
	if got := PushDownValue(lazy, cleanRC, upper); got != constA {
		t.Fatalf("lazy ID pushed through clean RC = %v, want a (unchanged)", got)
	}

	// Out-of-range baked ordinal: decline. Unlike the compose rule (where the
	// RC is the node's OWN child, so a mismatch is a tree inconsistency),
	// PushDownValue pairs the node with an EXTERNAL result value — nil is the
	// generic can't-push-down answer.
	stale := &FieldValue{Field: "ID", Typ: NullableString, Resolved: NewFieldPathOfSingle("ID", 7, false)}
	if got := PushDownValue(stale, rc, upper); got != nil {
		t.Fatalf("out-of-range baked push-down must DECLINE (nil), got %v", got)
	}
}

// TestFieldValueBaked_LoudOnNameContext pins the guard that a FrontierPinned
// BAKED node evaluated against a non-positional (name-keyed / unrecognized)
// context is a loud *BakedNameContextError at every tail — never a silent
// display-name read (which would return the FIRST of duplicate same-named
// columns) and never a silent NULL that would hide a frontier bug. A LAZY
// node carries no plan-time ordinal, so it too fails loud over an ordinal row
// (*OrdinalResolutionError — there is no runtime name resolution) and over a
// non-positional context (*UnboundEvalContextError); only a bare nil context
// stays NULL.
func TestFieldValueBaked_LoudOnNameContext(t *testing.T) {
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

	// A baked node over a non-positional context is always LOUD, never a silent
	// NULL. The error kind depends on what happened: a
	// correlation that MATCHED a name-keyed value → *BakedNameContextError (pinned
	// hit a name context); NOTHING matched (unrecognized context / unbound
	// correlation) → *UnboundEvalContextError. Either proves non-silence.
	assertLoud := func(site string, got any, err error) {
		t.Helper()
		var bnce *BakedNameContextError
		var uce *UnboundEvalContextError
		if !errors.As(err, &bnce) && !errors.As(err, &uce) {
			t.Fatalf("%s: baked node over a non-positional context must be LOUD (*BakedNameContextError or *UnboundEvalContextError), got (%v, %v)", site, got, err)
		}
	}

	// Correlated arms (Child is a QOV, so evaluateCorrelated dispatches).
	got, err := baked.Evaluate(&RowEvalContext{})
	assertLoud("RowEvalContext (no Positional)", got, err)
	got, err = baked.Evaluate(&RowEvalContext{Correlations: &mapBinder{id: corr, m: nameRow}})
	assertLoud("RowEvalContext correlation map binding", got, err)
	got, err = baked.Evaluate(&mapBinder{id: corr, m: nameRow})
	assertLoud("bare CorrelationBinder map binding", got, err)
	got, err = baked.Evaluate(map[CorrelationIdentifier]map[string]any{corr: nameRow})
	assertLoud("per-correlation map", got, err)
	got, err = baked.Evaluate(nameRow)
	assertLoud("qualified-key map", got, err)

	// Non-correlated map arm (childless PINNED baked copy, e.g. a seed ref
	// post-passthrough — pullup strips Child but shares the accessor, so the
	// FrontierPinned contract survives and the guard stays loud).
	orphan := &FieldValue{Field: "ID", Typ: NotNullLong, Resolved: NewFieldPathOfSingle("ID", 0, true)}
	got, err = orphan.Evaluate(nameRow)
	assertLoud("plain map arm", got, err)

	// nil context stays NULL (the appendNullLeg / nil-binding path — the null
	// extension falls out, not an error).
	if v, err := orphan.Evaluate(nil); v != nil || err != nil {
		t.Fatalf("baked over nil context = (%v, %v), want (nil, nil)", v, err)
	}

	// An UNRECOGNIZED non-nil context is loud for a baked node — a silent
	// NULL there would hide a frontier bug — at BOTH tails: Evaluate's
	// (childless orphan) and evaluateCorrelated's (QOV child, the COMMON
	// baked shape). It is loud for LAZY/unpinned nodes too: a nothing-matched
	// eval against an unrecognized non-nil context is an
	// *UnboundEvalContextError, never a silent NULL. A bare nil context still
	// stays NULL through the correlated path.
	type weirdCtx struct{}
	got, err = orphan.Evaluate(weirdCtx{})
	assertLoud("unrecognized context: Evaluate tail (orphan)", got, err)
	got, err = baked.Evaluate(weirdCtx{})
	assertLoud("unrecognized context: evaluateCorrelated tail (QOV child)", got, err)
	var uce *UnboundEvalContextError
	if _, err := NewFlatFieldValue("ID", NotNullLong).Evaluate(weirdCtx{}); !errors.As(err, &uce) {
		t.Fatalf("lazy orphan over unrecognized context = %v, want loud *UnboundEvalContextError (no silent lazy NULL)", err)
	}
	if _, err := NewFieldValue(qov, "ID", NotNullLong).Evaluate(weirdCtx{}); !errors.As(err, &uce) {
		t.Fatalf("lazy correlated over unrecognized context = %v, want loud *UnboundEvalContextError (no silent lazy NULL)", err)
	}
	if v, err := baked.Evaluate(nil); v != nil || err != nil {
		t.Fatalf("baked correlated over NIL context = (%v, %v), want (nil, nil)", v, err)
	}

	// Lazy node over an ORDINAL row: a lazy reference carries no plan-time
	// ordinal and there is no runtime name->ordinal resolution, so it fails
	// LOUD (*OrdinalResolutionError) rather than reading its column by name.
	// Only a BAKED accessor reads positionally.
	lazy := NewFieldValue(qov, "ID", NotNullLong)
	var lazyORE *OrdinalResolutionError
	if _, err := lazy.Evaluate(&fakeOrdinalRow{names: []string{"ID"}, slots: []any{int64(7)}}); !errors.As(err, &lazyORE) {
		t.Fatalf("lazy over ordinal row = %v, want loud *OrdinalResolutionError (unbaked ref does not resolve by name)", err)
	}
}

// TestFieldValue_UnpinnedNonOrdinalBinding_IsSilent pins the negative counterpart
// to the FrontierPinned loud guard: an UNPINNED
// correlated FieldValue whose correlation resolves to a non-nil, non-OrdinalRow
// binding is NOT loud — it returns that bound value raw (an unpinned node
// carries no frontier contract, so there is no off-frontier value to guard
// against). Only a FrontierPinned node goes loud there (see the garbage-leg
// pin in the executor's ordinal-join tests). This freezes the pinned/unpinned
// asymmetry so a future change to the `return bound, nil` arm is a
// deliberate red->green edit, not a silent drift.
func TestFieldValue_UnpinnedNonOrdinalBinding_IsSilent(t *testing.T) {
	t.Parallel()
	corr := NamedCorrelationIdentifier("q")
	lazy := NewFieldValue(NewQuantifiedObjectValue(corr), "COL", UnknownType) // UNPINNED (no Resolved bake)
	type garbage struct{ x int }
	g := garbage{x: 9}
	got, err := lazy.Evaluate(&ordEvalBinder{id: corr, bound: g})
	if err != nil {
		t.Fatalf("unpinned node over a non-ordinal binding must be silent, got loud: %v", err)
	}
	if got != any(g) {
		t.Fatalf("unpinned node returns the bound value raw: got %v, want %v", got, g)
	}
}

// TestContainsBakedOrdinal pins the drift-assert probe itself: a
// baked node found at depth, a pure-lazy tree reporting false, and nil
// safety — the SelectMergeRule assert keys on this walk, so a walk bug would
// silently kill the assert.
func TestContainsBakedOrdinal(t *testing.T) {
	t.Parallel()
	rt := NewRecordType("", false, []Field{
		{Name: "ID", FieldType: NotNullLong, Ordinal: 0},
	})
	qov := NewQuantifiedObjectValueOfType(NamedCorrelationIdentifier("q"), rt)
	baked, err := NewFieldValueOfOrdinal(qov, 0)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal: %v", err)
	}
	lazy := NewFieldValue(qov, "ID", NotNullLong)

	// Baked node buried two levels deep inside an RC.
	deep := NewRecordConstructorValue(
		RecordConstructorField{Name: "OUT", Value: NewRecordConstructorValue(
			RecordConstructorField{Name: "INNER", Value: baked},
		)},
	)
	if !ContainsBakedOrdinal(deep) {
		t.Fatal("ContainsBakedOrdinal missed a deep baked node — the drift assert would be dead")
	}
	if !ContainsBakedOrdinal(baked) {
		t.Fatal("ContainsBakedOrdinal(baked leaf) = false")
	}
	lazyTree := NewRecordConstructorValue(RecordConstructorField{Name: "OUT", Value: lazy})
	if ContainsBakedOrdinal(lazyTree) {
		t.Fatal("ContainsBakedOrdinal reports true for a pure-lazy tree — the assert would false-positive")
	}
	if ContainsBakedOrdinal(nil) {
		t.Fatal("ContainsBakedOrdinal(nil) must be false")
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

// TestFieldValueBaked_PullUpThroughRC_Bakes pins an RC name-lookup consumer:
// pullUpThroughRecordConstructor re-frames the matched value as a reference
// to the RC's OUTPUT column, so the emitted node carries the matched ordinal
// BAKED whenever it matters — a baked input (bakedness survives pull-up) or
// a dup-named RC (where a lazy name node would later resolve to the FIRST
// same-named column no matter which matched). A lazy input over a
// clean-named RC stays lazy (production behavior unchanged).
func TestFieldValueBaked_PullUpThroughRC_Bakes(t *testing.T) {
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
	if fv.Resolved == nil || fv.Resolved.Root().Ordinal != 1 {
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
	if fv.Resolved == nil || fv.Resolved.Root().Ordinal != 1 || fv.Field != "OUT1" {
		t.Fatalf("baked input pull-up = {Field:%s Resolved:%+v}, want OUT1 baked at 1", fv.Field, fv.Resolved)
	}

	// LAZY input over a clean-named RC: unchanged — lazy in, lazy out.
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

// TestIsOrdinalFieldName pins OrdinalFieldName's inverse, DIGITS-ONLY: the
// producer never emits signs, so Atoi-style acceptance of `_-1`/`_+0` would
// let a bare-name lookup match strings the planner never writes.
func TestIsOrdinalFieldName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{OrdinalFieldName(0), true},
		{OrdinalFieldName(12), true},
		{"_007", true}, // digits-only, even non-canonical
		{"_-1", false},
		{"_+0", false},
		{"_", false},
		{"", false},
		{"X", false},
		{"_A", false},
		{"0", false},
		{"_1x", false},
	} {
		if got := IsOrdinalFieldName(tc.in); got != tc.want {
			t.Errorf("IsOrdinalFieldName(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
