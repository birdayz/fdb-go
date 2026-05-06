package embedded

import (
	"testing"
)

// TestFoldConstantProjections_PureConstant pins that an arithmetic
// expression with no row context folds to a precomputed Go value.
// SELECT 1+2 FROM Order — projExprs[0] is `1+2`; after the fold pass
// projConstFolded[0].present is true and the value is int64(3),
// matching ArithmeticValue.Evaluate (int64-only at the seed).
func TestFoldConstantProjections_PureConstant(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	sq := parseSelect(t, "SELECT 1+2 FROM Order")
	foldConstantProjections(sq, md)
	if len(sq.projConstFolded) == 0 {
		t.Fatalf("expected projConstFolded populated, got empty")
	}
	if !sq.projConstFolded[0].present {
		t.Fatalf("expected slot 0 to be folded, got %+v", sq.projConstFolded[0])
	}
	got, ok := sq.projConstFolded[0].value.(int64)
	if !ok {
		t.Fatalf("expected int64, got %T (%v)", sq.projConstFolded[0].value, sq.projConstFolded[0].value)
	}
	if got != 3 {
		t.Fatalf("expected 3, got %d", got)
	}
}

// TestFoldConstantProjections_NestedArith pins partial fold composition:
// (1+2)*4 reaches the constant collapser as a single arithmetic Value
// whose every leaf is a constant after children-fold. Final value is
// int64(12).
func TestFoldConstantProjections_NestedArith(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	sq := parseSelect(t, "SELECT (1+2)*4 FROM Order")
	foldConstantProjections(sq, md)
	if !sq.projConstFolded[0].present {
		t.Fatalf("expected slot 0 folded")
	}
	if got := sq.projConstFolded[0].value.(int64); got != 12 {
		t.Fatalf("expected 12, got %d", got)
	}
}

// TestFoldConstantProjections_ScalarFunction pins UPPER('hi') folding
// — the scalar-function arm of SimplifyValue's whitelist.
func TestFoldConstantProjections_ScalarFunction(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	sq := parseSelect(t, "SELECT UPPER('hi') FROM Order")
	foldConstantProjections(sq, md)
	if !sq.projConstFolded[0].present {
		t.Fatalf("expected slot 0 folded")
	}
	if got := sq.projConstFolded[0].value.(string); got != "HI" {
		t.Fatalf("expected HI, got %q", got)
	}
}

// TestFoldConstantProjections_FieldRefDeclines pins that a projection
// referring to a row column is not folded — IsConstantValue rejects
// FieldValue, so the slot stays unset and per-row evalExpr handles it.
func TestFoldConstantProjections_FieldRefDeclines(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	sq := parseSelect(t, "SELECT price + 1 FROM Order")
	foldConstantProjections(sq, md)
	// The slice is allocated whenever the resolver builds, so it
	// exists; check that slot 0 stayed unset.
	if len(sq.projConstFolded) > 0 && sq.projConstFolded[0].present {
		t.Fatalf("expected slot 0 unset (field ref), got folded value %v", sq.projConstFolded[0].value)
	}
}

// TestFoldConstantProjections_NilMetaData pins the short-circuit on
// nil metadata: nothing folds because no Resolver can be built.
func TestFoldConstantProjections_NilMetaData(t *testing.T) {
	t.Parallel()
	sq := parseSelect(t, "SELECT 1+2 FROM Order")
	foldConstantProjections(sq, nil)
	if len(sq.projConstFolded) != 0 {
		t.Fatalf("expected projConstFolded empty on nil md, got len %d", len(sq.projConstFolded))
	}
}

// TestFoldConstantProjections_PlainColumnUnaffected pins that bare
// columns (which always have a nil projExpr slot) leave the cache slice
// either zero-length or with that slot's `present` false.
func TestFoldConstantProjections_PlainColumnUnaffected(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	sq := parseSelect(t, "SELECT price FROM Order")
	foldConstantProjections(sq, md)
	for i := range sq.projConstFolded {
		if sq.projConstFolded[i].present {
			t.Fatalf("slot %d unexpectedly folded for plain column projection", i)
		}
	}
}

// TestFoldConstantProjections_MixedColumnsAndConsts pins the slot
// alignment: in `SELECT 1+2, price, UPPER('x') FROM Order`, slots 0
// and 2 fold; slot 1 (a bare column) has projExprs[1]==nil and stays
// unset.
func TestFoldConstantProjections_MixedColumnsAndConsts(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	sq := parseSelect(t, "SELECT 1+2, price, UPPER('x') FROM Order")
	foldConstantProjections(sq, md)
	if len(sq.projExprs) != 3 {
		t.Fatalf("expected 3 projExprs, got %d", len(sq.projExprs))
	}
	if !sq.projConstFolded[0].present || sq.projConstFolded[0].value.(int64) != 3 {
		t.Fatalf("slot 0: expected 3, got %+v", sq.projConstFolded[0])
	}
	if sq.projConstFolded[1].present {
		t.Fatalf("slot 1 (bare column) unexpectedly folded: %+v", sq.projConstFolded[1])
	}
	if !sq.projConstFolded[2].present || sq.projConstFolded[2].value.(string) != "X" {
		t.Fatalf("slot 2: expected X, got %+v", sq.projConstFolded[2])
	}
}

// TestFoldConstantProjections_Idempotent pins that calling the fold
// pass twice doesn't re-fold or wipe an existing slot.
func TestFoldConstantProjections_Idempotent(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	sq := parseSelect(t, "SELECT 1+2 FROM Order")
	foldConstantProjections(sq, md)
	foldConstantProjections(sq, md)
	if !sq.projConstFolded[0].present || sq.projConstFolded[0].value.(int64) != 3 {
		t.Fatalf("expected 3, got %+v", sq.projConstFolded[0])
	}
}

// TestFoldConstantProjections_UnsupportedShapeDeclines pins the
// best-effort contract: a projection shape outside the walker's seed
// support (here `?` parameter — walker walks it, but not constant)
// silently leaves the slot unset.
func TestFoldConstantProjections_UnsupportedShapeDeclines(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	// `?` walks to ParameterValue; IsConstantValue returns false for
	// ParameterValue (binding happens at runtime); EvaluateConstant
	// declines. Slot stays unset.
	sq := parseSelect(t, "SELECT ? FROM Order")
	foldConstantProjections(sq, md)
	for i := range sq.projConstFolded {
		if sq.projConstFolded[i].present {
			t.Fatalf("slot %d unexpectedly folded for `?`: %+v", i, sq.projConstFolded[i])
		}
	}
}
