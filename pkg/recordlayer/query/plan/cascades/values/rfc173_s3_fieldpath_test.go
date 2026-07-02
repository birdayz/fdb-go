package values

import (
	"errors"
	"strings"
	"testing"
)

// RFC-173 S3-W1 pins — the multi-accessor FieldPath substrate (Java
// FieldValue.FieldPath, FieldValue.java:373) and the baked-gated compose rule
// (ComposeFieldValueOverFieldValueRule.java:57-69). W1 is DARK by the staging
// ruling: no production shape changes — multi-accessor paths are constructible
// only through the compose rule, which is gated to fully-baked chains, and no
// baked-over-baked chain exists in the S2 wedge. These pins exercise the
// machinery the S3-W2/W3 flip will make live.

// s3NestedQOV builds q over {NESTED record{X,Y}, W long} — the shape whose
// baked-over-baked chain the compose rule fuses.
func s3NestedQOV(t *testing.T) *QuantifiedObjectValue {
	t.Helper()
	inner := NewRecordType("", false, []Field{
		{Name: "X", FieldType: NotNullLong, Ordinal: 0},
		{Name: "Y", FieldType: NotNullLong, Ordinal: 1},
	})
	outer := NewRecordType("", false, []Field{
		{Name: "NESTED", FieldType: inner, Ordinal: 0},
		{Name: "W", FieldType: NotNullLong, Ordinal: 1},
	})
	return NewQuantifiedObjectValueOfType(NamedCorrelationIdentifier("q"), outer)
}

// bakedChain builds the canonical fusable chain: ofOrdinal(ofOrdinal(q, 0), 1)
// — outer reads Y (ordinal 1) off the NESTED record (ordinal 0) of q.
func bakedChain(t *testing.T) (inner, outer *FieldValue) {
	t.Helper()
	qov := s3NestedQOV(t)
	in, err := NewFieldValueOfOrdinal(qov, 0)
	if err != nil {
		t.Fatalf("inner bake: %v", err)
	}
	out, err := NewFieldValueOfOrdinal(in, 1)
	if err != nil {
		t.Fatalf("outer bake: %v", err)
	}
	return in, out
}

// TestRFC173S3_WithSuffix_ImmutableFuse pins Java FieldPath.withSuffix
// (FieldValue.java:525-534): a NEW path with both accessor lists, neither
// input mutated, and the frontier pin taken from the RECEIVER (the fused
// node's root read context is the inner path's).
func TestRFC173S3_WithSuffix_ImmutableFuse(t *testing.T) {
	t.Parallel()
	p1 := NewFieldPathOfSingle("A", 0, true)
	p2 := NewFieldPathOfSingle("B", 1, false)

	fused := p1.WithSuffix(p2)
	if len(fused.Accessors) != 2 ||
		fused.Accessors[0] != (ResolvedAccessor{Field: "A", Ordinal: 0}) ||
		fused.Accessors[1] != (ResolvedAccessor{Field: "B", Ordinal: 1}) {
		t.Fatalf("fused accessors = %+v, want [(A,0),(B,1)]", fused.Accessors)
	}
	if !fused.FrontierPinned {
		t.Fatal("fused pin must come from the RECEIVER (pinned)")
	}
	if unpinnedFused := p2.WithSuffix(p1); unpinnedFused.FrontierPinned {
		t.Fatal("fused pin must come from the RECEIVER (unpinned)")
	}
	// Immutability: the inputs are untouched, the result is a fresh path.
	if len(p1.Accessors) != 1 || len(p2.Accessors) != 1 {
		t.Fatalf("WithSuffix mutated an input: p1=%+v p2=%+v", p1.Accessors, p2.Accessors)
	}
	if fused == p1 || fused == p2 {
		t.Fatal("WithSuffix must return a NEW path")
	}
	// Empty-suffix identity: the receiver itself comes back.
	if p1.WithSuffix(&FieldPath{}) != p1 || p1.WithSuffix(nil) != p1 {
		t.Fatal("empty suffix must return the receiver unchanged")
	}
}

// TestRFC173S3_ComposeGate pins the W1 staging ruling: the compose rule fuses
// FULLY-BAKED chains only. A lazy chain — today's canonical nested access,
// corpus-wide — must come back UNCHANGED from simplification (firing there
// would rewrite memo identity and Explain renderings everywhere: W1 would not
// be dark). Mixed chains (lazy over baked, baked over lazy) stay unfused too.
func TestRFC173S3_ComposeGate(t *testing.T) {
	t.Parallel()
	qov := s3NestedQOV(t)

	// Lazy chain: field(field(q, "NESTED"), "Y") — unchanged.
	lazyInner := NewFieldValue(qov, "NESTED", nil)
	lazyOuter := NewFieldValue(lazyInner, "Y", NotNullLong)
	if got := SimplifyValue(lazyOuter); got != lazyOuter {
		t.Fatalf("lazy chain simplified to %v — the compose gate must leave lazy chains untouched in W1", got)
	}

	// Baked over lazy / lazy over baked: unfused.
	bakedInner, err := NewFieldValueOfOrdinal(qov, 0)
	if err != nil {
		t.Fatalf("bake: %v", err)
	}
	lazyOverBaked := NewFieldValue(bakedInner, "Y", NotNullLong)
	if got := SimplifyValue(lazyOverBaked); got != lazyOverBaked {
		t.Fatalf("lazy-over-baked fused to %v — gate requires BOTH ends baked", got)
	}

	// Fully-baked chain: fuses into ONE node with the concatenated path,
	// child = the chain's base, pin inherited from the INNER path.
	inner, outer := bakedChain(t)
	fused, ok := SimplifyValue(outer).(*FieldValue)
	if !ok || fused == outer {
		t.Fatalf("baked chain did not fuse (got %T %v)", fused, fused)
	}
	if fused.Child != inner.Child {
		t.Fatalf("fused child = %v, want the chain's base QOV", fused.Child)
	}
	if fused.Resolved == nil || len(fused.Resolved.Accessors) != 2 ||
		fused.Resolved.Accessors[0] != (ResolvedAccessor{Field: "NESTED", Ordinal: 0}) ||
		fused.Resolved.Accessors[1] != (ResolvedAccessor{Field: "Y", Ordinal: 1}) {
		t.Fatalf("fused path = %+v, want [(NESTED,0),(Y,1)]", fused.Resolved)
	}
	if !fused.Resolved.FrontierPinned {
		t.Fatal("fused pin must inherit from the inner (seed-baked) path")
	}
	if fused.Field != "Y" {
		t.Fatalf("fused display = %q, want the LAST step's name Y (Java getLastFieldName)", fused.Field)
	}
}

// TestRFC173S3_FusedPath_Evaluate pins the fused node's runtime semantics
// against the chain it replaced: root read by ordinal on a positional row,
// nested steps descend name-keyed Datum records by their per-step names
// (exactly what the outer chained node did), nested positional rows by
// ordinal, NULL propagation on a nil nested record, and the frontier guard
// still keyed at the ROOT context.
func TestRFC173S3_FusedPath_Evaluate(t *testing.T) {
	t.Parallel()
	_, outer := bakedChain(t)
	fused := SimplifyValue(outer).(*FieldValue)

	// Positional root, name-keyed nested record.
	row := &fakeOrdinalRow{
		names: []string{"NESTED", "W"},
		slots: []any{map[string]any{"X": int64(1), "Y": int64(2)}, int64(9)},
	}
	got, err := fused.Evaluate(row)
	if err != nil || got != int64(2) {
		t.Fatalf("fused eval over positional root = (%v, %v), want (2, nil)", got, err)
	}
	// The LAZY chain (what nested access is corpus-wide today) computes the
	// same value — the fused node preserves the access's semantics. The BAKED
	// chain itself cannot evaluate at all: its pinned outer sees the nested
	// record as a bare name-keyed map and the frontier guard is LOUD — fusion
	// is what makes a baked nested access executable, which is why Java has
	// no chained form.
	qovBase := fused.Child.(*QuantifiedObjectValue)
	lazyChain := NewFieldValue(NewFieldValue(qovBase, "NESTED", nil), "Y", NotNullLong)
	lazyGot, err := lazyChain.Evaluate(row)
	if err != nil || lazyGot != got {
		t.Fatalf("lazy chain eval = (%v, %v) — the fused node must compute what the access always computed", lazyGot, err)
	}
	var chainBNCE *BakedNameContextError
	if _, err = outer.Evaluate(row); !errors.As(err, &chainBNCE) {
		t.Fatalf("UNfused baked chain eval = %v, want loud *BakedNameContextError (the intermediate record is a name-keyed map to the pinned outer)", err)
	}

	// Nested positional row: descend by ordinal.
	nested := &fakeOrdinalRow{names: []string{"X", "Y"}, slots: []any{int64(3), int64(4)}}
	got, err = fused.Evaluate(&fakeOrdinalRow{names: []string{"NESTED", "W"}, slots: []any{nested, int64(9)}})
	if err != nil || got != int64(4) {
		t.Fatalf("fused eval over nested positional = (%v, %v), want (4, nil)", got, err)
	}

	// NULL propagation: nil nested record yields NULL (the chained-lazy
	// nil-context arm's behavior).
	got, err = fused.Evaluate(&fakeOrdinalRow{names: []string{"NESTED", "W"}, slots: []any{nil, int64(9)}})
	if err != nil || got != nil {
		t.Fatalf("fused eval over nil nested = (%v, %v), want (nil, nil)", got, err)
	}

	// A non-record nested value under a PINNED path is loud, never a quiet
	// NULL (frontier bug hiding).
	_, err = fused.Evaluate(&fakeOrdinalRow{names: []string{"NESTED", "W"}, slots: []any{int64(7), int64(9)}})
	var ore *OrdinalResolutionError
	if !errors.As(err, &ore) {
		t.Fatalf("pinned descent into a non-record = %v, want loud *OrdinalResolutionError", err)
	}

	// The frontier guard keys at the ROOT context: a pinned fused node on a
	// name-keyed map is loud.
	_, err = fused.Evaluate(map[string]any{"NESTED": map[string]any{"Y": int64(2)}})
	var bnce *BakedNameContextError
	if !errors.As(err, &bnce) {
		t.Fatalf("pinned fused on name-keyed row = %v, want *BakedNameContextError", err)
	}

	// An UNPINNED fused path (hand-built — no production constructor yet)
	// name-reads its ROOT step's key and descends: reading the display name
	// (the LAST step, "Y") at the top level would be wrong.
	unpinned := &FieldValue{
		Field: "Y", Typ: NotNullLong,
		Resolved: NewFieldPathOfSingle("NESTED", 0, false).WithSuffix(NewFieldPathOfSingle("Y", 1, false)),
	}
	got, err = unpinned.Evaluate(map[string]any{
		"NESTED": map[string]any{"Y": int64(5)},
		"Y":      int64(99), // the trap: a display-name root read returns this
	})
	if err != nil || got != int64(5) {
		t.Fatalf("unpinned fused name read = (%v, %v), want (5, nil) — root step's key, then descend", got, err)
	}
}

// TestRFC173S3_FusedPath_IdentityHashExplain pins the fused node's identity
// surface: element-wise (Field, Ordinal) equality (Java FieldPath list-equals,
// FieldValue.java:411-420), equal ⟹ same-hash, and the multi-step Explain
// rendering (every step as name#ordinal, dot-joined, '#'-escaped) feeding the
// ExplainValue-keyed projection-plan identity.
func TestRFC173S3_FusedPath_IdentityHashExplain(t *testing.T) {
	t.Parallel()
	_, outer := bakedChain(t)
	fused := SimplifyValue(outer).(*FieldValue)

	// Explain: child prefix + NESTED#0.Y#1.
	if r := ExplainValue(fused); !strings.HasSuffix(r, "NESTED#0.Y#1") {
		t.Fatalf("fused Explain = %q, want …NESTED#0.Y#1", r)
	}
	// '#'-escape stays per-step injective: a step named "X#0" doubles.
	tricky := &FieldValue{Field: "X#0", Resolved: NewFieldPathOfSingle("A", 0, false).WithSuffix(NewFieldPathOfSingle("X#0", 1, false))}
	if r := ExplainValue(tricky); !strings.HasSuffix(r, "A#0.X##0#1") {
		t.Fatalf("escaped fused Explain = %q, want …A#0.X##0#1", r)
	}

	// Identity: element-wise. Same elements equal (and hash-equal); prefix
	// alone, different last ordinal, and different step name are all unequal.
	same := &FieldValue{Field: "Y", Child: fused.Child, Resolved: NewFieldPathOfSingle("NESTED", 0, true).WithSuffix(NewFieldPathOfSingle("Y", 1, true))}
	if !EqualsWithoutChildren(fused, same) {
		t.Fatal("equal fused paths must be EQUAL (pin excluded)")
	}
	if SemanticHashCode(fused) != SemanticHashCode(same) {
		t.Fatal("equal fused paths (same child) must hash equal")
	}
	prefixOnly := &FieldValue{Field: "NESTED", Resolved: NewFieldPathOfSingle("NESTED", 0, true)}
	if EqualsWithoutChildren(fused, prefixOnly) {
		t.Fatal("a path and its proper prefix must be UNEQUAL")
	}
	diffOrd := &FieldValue{Field: "Y", Resolved: NewFieldPathOfSingle("NESTED", 0, true).WithSuffix(NewFieldPathOfSingle("Y", 0, true))}
	if EqualsWithoutChildren(fused, diffOrd) {
		t.Fatal("paths differing in a step ordinal must be UNEQUAL")
	}
	diffName := &FieldValue{Field: "X", Resolved: NewFieldPathOfSingle("NESTED", 0, true).WithSuffix(NewFieldPathOfSingle("X", 1, true))}
	if EqualsWithoutChildren(fused, diffName) {
		t.Fatal("paths differing in a step name must be UNEQUAL (coexistence-window element identity)")
	}
}

// TestRFC173S3_SeedAssert_RejectsFusedPath pins the S2 seed contract under the
// path substrate: the ordinal-join seed is SINGLE-accessor by construction
// (contract ruling #2) — a fused path in a seed RC means the compose gate
// failed, and AssertOrdinalJoinSeed panics rather than letting a mis-shaped
// seed reach the executor.
func TestRFC173S3_SeedAssert_RejectsFusedPath(t *testing.T) {
	t.Parallel()
	_, outer := bakedChain(t)
	fused := SimplifyValue(outer).(*FieldValue)
	rc := NewRawRecordConstructorValue(RecordConstructorField{Name: fused.Field, Value: fused})
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("AssertOrdinalJoinSeed must panic on a multi-accessor seed field")
		}
		if msg, ok := r.(string); !ok || !strings.Contains(msg, "single-accessor") {
			t.Fatalf("panic = %v, want the single-accessor seed message", r)
		}
	}()
	AssertOrdinalJoinSeed(rc)
}
