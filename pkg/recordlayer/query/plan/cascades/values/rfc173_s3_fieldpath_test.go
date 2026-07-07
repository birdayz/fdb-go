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

// TestRFC173S3_FusedPath_CorrelatedContexts is the S3-W1 review's demanded
// trap pin: evaluateCorrelated is the fused node's PRIMARY eval path (the
// compose rule's only constructible output is fused-over-QOV), and every one
// of its name-keyed arms must start at the ROOT step's key and descend — a
// display-name (last-step) read at the top level returns the planted trap
// value. The node is UNPINNED so the guard does not mask a misread (pinned
// fused nodes were loud-by-accident on these arms pre-fix).
func TestRFC173S3_FusedPath_CorrelatedContexts(t *testing.T) {
	t.Parallel()
	qov := s3NestedQOV(t)
	corr := qov.Correlation
	fused := &FieldValue{
		Field: "Y", Typ: NotNullLong, Child: qov,
		Resolved: NewFieldPathOfSingle("NESTED", 0, false).WithSuffix(NewFieldPathOfSingle("Y", 1, false)),
	}
	// The trap: "Y" present at top level everywhere; the correct answer lives
	// only under NESTED.Y.
	nested := map[string]any{"X": int64(1), "Y": int64(2)}
	row := map[string]any{"NESTED": nested, "Y": int64(99)}
	qualRow := map[string]any{
		"Q.NESTED": nested, "Q.Y": int64(99),
		"NESTED": nested, "Y": int64(99),
	}

	assertReads2 := func(ctxName string, got any, err error) {
		t.Helper()
		if err != nil || got != int64(2) {
			t.Fatalf("%s: fused correlated read = (%v, %v), want (2, nil) — root step key + descent, never the display name", ctxName, got, err)
		}
	}
	got, err := fused.Evaluate(&RowEvalContext{Correlations: &mapBinder{id: corr, m: row}})
	assertReads2("RowEvalContext correlation map binding", got, err)
	got, err = fused.Evaluate(&mapBinder{id: corr, m: row})
	assertReads2("bare CorrelationBinder map binding", got, err)
	got, err = fused.Evaluate(map[CorrelationIdentifier]map[string]any{corr: row})
	assertReads2("per-correlation map", got, err)
	got, err = fused.Evaluate(qualRow)
	assertReads2("qualified-key map (qualKey = root step)", got, err)
	got, err = fused.Evaluate(&RowEvalContext{Datum: qualRow})
	assertReads2("RowEvalContext qualified Datum", got, err)
}

// TestRFC173S3_FusedPath_IdentityHashExplain pins the fused node's identity
// surface post-W3-flip: element-wise ORDINAL-ONLY equality (Java FieldPath
// list-equals over ResolvedAccessor.equals = getOrdinal() alone,
// FieldValue.java:411-420 + :675-689), equal ⟹ same-hash (the baked hash
// folds only the ordinal path — a name-bearing hash would split the
// alias-mapped twins the flip makes equal), and the multi-step Explain
// rendering (every step as name#ordinal, dot-joined, '#'-escaped — rendering
// keeps names; identity does not).
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
	// The W3 flip: a step NAME difference is NOT an identity difference —
	// Java's ResolvedAccessor.equals is ordinal-only (FieldValue.java:675-689).
	// Same ordinals, different display names: EQUAL and hash-equal (this is
	// the alias-mapped-twin dedup the flip exists for; pre-flip these were
	// unequal — the refinement that could only under-dedup).
	diffName := &FieldValue{Field: "X", Child: fused.Child, Resolved: NewFieldPathOfSingle("OTHER", 0, true).WithSuffix(NewFieldPathOfSingle("X", 1, true))}
	if !EqualsWithoutChildren(fused, diffName) {
		t.Fatal("paths differing only in step NAMES must be EQUAL (Java ordinal-only element identity, S3-W3 flip)")
	}
	if SemanticHashCode(fused) != SemanticHashCode(diffName) {
		t.Fatal("name-differing equal paths must hash equal (the baked hash folds ordinals only)")
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

// TestRFC173S4_DupBareNameMemoIdentity pins RFC-173 S4 Rule A's acceptance gate
// (ii): a positional join seed carrying DUPLICATE bare names (two legs each with a
// column "X") keeps them as DISTINCT memo members iff the references are BAKED.
// writeSemanticHash forks FieldValue identity on Resolved (semantic_hash.go:120-139):
// baked → ordinal PATH, lazy → NAME bucket. So join ordinalization is memo-safe as
// long as it emits BAKED dup refs (exactly as producer #2's positional wrap does);
// a LAZY name-only ref is the ONLY way to conflate two logically-different columns
// into one memo member → wrong plans. This pins that mechanism as a regression.
func TestRFC173S4_DupBareNameMemoIdentity(t *testing.T) {
	t.Parallel()
	// DESIGN CONSTRAINT (found here): NewRecordType PANICS on a duplicate field name,
	// so the ordinal join seed for two legs each with column "X" CANNOT carry two bare
	// "X" fields in its type — it must name the seed slots DISTINCTLY (qualified) and
	// address the columns POSITIONALLY. That is precisely why Rule A binds by baked
	// ordinal, not bare name. The two legs' X columns sit at qualified slots A_X (0)
	// and B_X (1).
	seed := NewRecordType("", false, []Field{
		{Name: "A_X", FieldType: NotNullLong, Ordinal: 0},
		{Name: "B_X", FieldType: NotNullLong, Ordinal: 1},
	})
	qov := NewQuantifiedObjectValueOfType(NamedCorrelationIdentifier("q"), seed)

	// BAKED refs to the two X columns — DISTINCT memo members (ordinal-path identity):
	// this is what the join ordinalization must emit.
	bx0, err := NewFieldValueOfOrdinal(qov, 0)
	if err != nil {
		t.Fatalf("bake X#0: %v", err)
	}
	bx1, err := NewFieldValueOfOrdinal(qov, 1)
	if err != nil {
		t.Fatalf("bake X#1: %v", err)
	}
	if SemanticHashCode(bx0) == SemanticHashCode(bx1) {
		t.Fatal("baked dup-bare-name refs at different ordinals must be DISTINCT memo members (gate ii)")
	}
	if ValuesStructurallyEqual(bx0, bx1) {
		t.Fatal("baked X#0 and X#1 must be UNEQUAL (ordinal-path identity, not name)")
	}

	// LAZY (name-only) refs to bare "X" CONFLATE (name-bucket identity) — the exact
	// failure Rule A must avoid: two logically-different columns become one memo member.
	lx0 := NewFieldValue(qov, "X", NotNullLong)
	lx1 := NewFieldValue(qov, "X", NotNullLong)
	if SemanticHashCode(lx0) != SemanticHashCode(lx1) {
		t.Fatal("lazy same-name refs must share name-bucket identity — the conflation gate ii forbids in the seed")
	}
	if !ValuesStructurallyEqual(lx0, lx1) {
		t.Fatal("lazy same-name refs must be structurally EQUAL (name identity) — documents the conflation")
	}

	// Baked and lazy are DISTINCT by contract (different identity prefixes), so a
	// baked seed column never conflates with a stray lazy reference to the same name.
	if SemanticHashCode(bx0) == SemanticHashCode(lx0) {
		t.Fatal("baked and lazy refs to the same column must be distinct by contract")
	}
}
