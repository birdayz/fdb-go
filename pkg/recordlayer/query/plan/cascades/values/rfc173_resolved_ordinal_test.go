package values

import (
	"errors"
	"testing"
)

// TestFieldValue_ResolvedOrdinal_RFC173 pins the plan-time-resolved ordinal
// accessor (values.NewFieldValueWithResolvedOrdinal — Java's
// FieldValue.ofOrdinalNumber model), added for the recursive-CTE
// leg-normalization wrap (review P2 on PR #446): a leg emitting DUPLICATE
// output aliases (`SELECT a + 1 AS x, b + 1 AS x` — two slots both named X)
// is only readable positionally; any name-based resolution collapses the two
// columns (OrdinalRow.GetByName is first-match, the name-keyed Datum is
// last-wins). It pins:
//
//	(1) on an OrdinalRow, the resolved ordinal is authoritative — two reads
//	    of the same duplicate name hit their OWN slots;
//	(2) an out-of-range resolved ordinal is a LOUD OrdinalResolutionError
//	    (no name fallback — reviewer's rule);
//	(3) on a name-keyed row the ordinal is inert and the field NAME reads,
//	    exactly like a flat FieldValue (both-model coexistence);
//	(4) the ordinal is part of the Value's semantic identity (equality and
//	    hash) — Java: distinct ofOrdinalNumber ordinals are distinct
//	    FieldPaths — so the memo can never merge reads of two duplicate-named
//	    columns.
func TestFieldValue_ResolvedOrdinal_RFC173(t *testing.T) {
	t.Parallel()

	// (1) Duplicate-named row [X, X] with slots [2, 11]: name resolution
	// (GetByName) is first-match and returns slot 0 for BOTH; resolved-ordinal
	// reads hit each slot.
	dupRow := &fakeOrdinalRow{names: []string{"X", "X"}, slots: []any{int64(2), int64(11)}}
	read0 := NewFieldValueWithResolvedOrdinal("X", 0, UnknownType)
	read1 := NewFieldValueWithResolvedOrdinal("X", 1, UnknownType)
	v0, err := read0.Evaluate(dupRow)
	if err != nil || v0 != int64(2) {
		t.Fatalf("ordinal 0 read: got (%v, %v), want (2, nil)", v0, err)
	}
	v1, err := read1.Evaluate(dupRow)
	if err != nil || v1 != int64(11) {
		t.Fatalf("ordinal 1 read: got (%v, %v), want (11, nil) — first-match name collapse would give 2", v1, err)
	}
	// Control: a flat (name) read of the duplicate name collapses to slot 0 —
	// the review-P2 silent-wrong this accessor exists to prevent.
	flat := NewFlatFieldValue("X", UnknownType)
	vf, err := flat.Evaluate(dupRow)
	if err != nil || vf != int64(2) {
		t.Fatalf("flat control read: got (%v, %v), want (2, nil)", vf, err)
	}

	// (2) Out-of-range resolved ordinal: loud, never a silent NULL.
	readOOR := NewFieldValueWithResolvedOrdinal("X", 5, UnknownType)
	if _, err := readOOR.Evaluate(dupRow); err == nil {
		t.Fatal("out-of-range resolved ordinal must be a loud OrdinalResolutionError, got nil")
	}

	// (3) Non-ordinal context: a resolved-ordinal read has no name model to fall
	// back to (retired at the cap). The retired silent-NULL is now a loud
	// *UnboundEvalContextError — an unrecognized non-ordinal context is a
	// nothing-matched eval, not a sanctioned off-frontier NULL.
	if _, err := read1.Evaluate(map[string]any{"X": int64(7)}); err == nil {
		t.Fatal("resolved-ordinal over a non-ordinal context must be a loud *UnboundEvalContextError, got nil")
	} else {
		var uce *UnboundEvalContextError
		if !errors.As(err, &uce) {
			t.Fatalf("resolved-ordinal over a non-ordinal context = %v, want loud *UnboundEvalContextError", err)
		}
	}

	// (4) Semantic identity: distinct ordinals are distinct FieldPaths.
	if EqualsWithoutChildren(read0, read1) {
		t.Fatal("reads of ordinals 0 and 1 must NOT compare equal (Java: distinct FieldPaths)")
	}
	if SemanticHashCode(read0) == SemanticHashCode(read1) {
		t.Fatal("reads of ordinals 0 and 1 must not hash equal")
	}
	if !EqualsWithoutChildren(read0, NewFieldValueWithResolvedOrdinal("X", 0, UnknownType)) {
		t.Fatal("same field+ordinal must compare equal")
	}
	// An ordinal-carrying read and a plain flat read of the same name are
	// different accessors (one is positional, one is name-resolved).
	if EqualsWithoutChildren(read0, flat) {
		t.Fatal("resolved-ordinal read must not compare equal to a flat name read")
	}
}

// TestFieldValue_ExplainOrdinalEscape_RFC173 pins the '#'-escape that keeps
// ExplainValue injective over (field text, ordinal) — an EXPLAIN-FORMAT pin
// since RFC-176 P2/P3 (plan identity is semantic; renderings carry no
// identity): a quoted identifier may legally contain '#' (DOUBLE_QUOTE_ID
// accepts any non-quote character), so without the escape a plain name-read
// of a field literally named "X#0" would render identically to an ordinal
// read of X at slot 0 — debugging output collapsing two DIFFERENT reads.
// The same doubling keeps writeSemanticHash's FieldValue discriminator
// injective (a collision there is only a shared hash bucket — equality is
// structural and stays sound — but injective is strictly better). Raw field
// text has '#' DOUBLED; only an ordinal read ends in an unpaired '#'+digits.
// Historic origin: review round-3 on PR #446, when identity WAS keyed on
// these renderings.
func TestFieldValue_ExplainOrdinalEscape_RFC173(t *testing.T) {
	t.Parallel()

	ordinalRead := NewFieldValueWithResolvedOrdinal("X", 0, UnknownType)
	literalField := NewFlatFieldValue("X#0", UnknownType)

	if got := ExplainValue(ordinalRead); got != "X#0" {
		t.Fatalf("ordinal read rendering = %q, want X#0", got)
	}
	if got := ExplainValue(literalField); got != "X##0" {
		t.Fatalf("literal '#'-field rendering = %q, want X##0 (escaped)", got)
	}
	if ExplainValue(ordinalRead) == ExplainValue(literalField) {
		t.Fatal("ordinal read of X@0 and a field literally named X#0 must render distinctly")
	}
	// Hash discriminators must not collide either (same escape).
	if SemanticHashCode(ordinalRead) == SemanticHashCode(literalField) {
		t.Fatal("hash discriminators of ordinal read vs literal '#'-field must differ")
	}
	// An ordinal read of a field that itself contains '#': field text escaped,
	// ordinal suffix stays a single '#'.
	if got := ExplainValue(NewFieldValueWithResolvedOrdinal("X#0", 1, UnknownType)); got != "X##0#1" {
		t.Fatalf("ordinal read of '#'-field rendering = %q, want X##0#1", got)
	}
	// Datum-key naming contract untouched: a plain field read keys by Field
	// verbatim (no escape leaks into names).
	if got := ProjectionColumnName(literalField); got != "X#0" {
		t.Fatalf("ProjectionColumnName must stay verbatim, got %q", got)
	}
}
