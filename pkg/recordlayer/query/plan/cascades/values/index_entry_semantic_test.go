package values

import "testing"

// TestSemanticEquals_IndexEntryObject_OrdinalPath is the regression for the bug
// @claude caught in PR #214: SemanticEqualsUnderAliasMap intercepted
// IndexEntryObjectValue and compared ONLY the alias, dropping OrdinalPath — so
// two index-entry values with the same alias but different OrdinalPath were
// "equal" yet hashed differently (the hash folds OrdinalPath), violating
// equal ⟹ same hash. Fix: it falls through to the canonical OrdinalPath
// comparison (alias-invariant).
func TestSemanticEquals_IndexEntryObject_OrdinalPath(t *testing.T) {
	t.Parallel()
	a := NewIndexEntryObjectValue(NamedCorrelationIdentifier("q0"), TupleSourceKey, []int{0}, UnknownType)
	bDiffPath := NewIndexEntryObjectValue(NamedCorrelationIdentifier("q0"), TupleSourceKey, []int{1}, UnknownType)

	// Different OrdinalPath ⇒ NOT equal (was incorrectly equal).
	if SemanticEqualsUnderAliasMap(a, bDiffPath, AliasMap{}) {
		t.Fatal("index-entry values with different OrdinalPath must NOT be equal")
	}
	// And the hash↔equality invariant: unequal here is consistent with the
	// hash, which also differs on OrdinalPath.
	if SemanticHashCode(a) == SemanticHashCode(bDiffPath) {
		t.Fatal("different OrdinalPath must hash differently")
	}

	// Same OrdinalPath, different alias ⇒ equal (alias ignored) + equal hash.
	bDiffAlias := NewIndexEntryObjectValue(NamedCorrelationIdentifier("q1"), TupleSourceKey, []int{0}, UnknownType)
	if !SemanticEqualsUnderAliasMap(a, bDiffAlias, AliasMap{}) {
		t.Fatal("index-entry values differing only by alias must be equal (alias ignored)")
	}
	if SemanticHashCode(a) != SemanticHashCode(bDiffAlias) {
		t.Fatal("alias-variant index-entry values must hash equal")
	}
}

// TestSemanticEquals_IndexEntryObject_Source is the regression for the bug
// Codex flagged on PR #214: Source (KEY vs VALUE) was dropped from BOTH
// EqualsWithoutChildren and SemanticHashCode, so KEY[p] and VALUE[p] with the
// same alias + ordinal path collapsed to one value in the memo. But Evaluate
// reads PrimaryKey() for KEY and IndexValues() for VALUE — different tuples —
// so they are distinct columns (e.g. KEY[0] and VALUE[0] of a KeyWithValue
// covering-index entry). Java keeps them distinct (planHash folds source);
// Go must too. Fix folds Source into equality AND hash (kept consistent:
// equal ⟹ same hash).
func TestSemanticEquals_IndexEntryObject_Source(t *testing.T) {
	t.Parallel()
	key0 := NewIndexEntryObjectValue(NamedCorrelationIdentifier("q0"), TupleSourceKey, []int{0}, UnknownType)
	val0 := NewIndexEntryObjectValue(NamedCorrelationIdentifier("q0"), TupleSourceValue, []int{0}, UnknownType)
	other0 := NewIndexEntryObjectValue(NamedCorrelationIdentifier("q0"), TupleSourceOther, []int{0}, UnknownType)

	// Same alias + path, different Source ⇒ NOT equal (was incorrectly equal).
	if SemanticEqualsUnderAliasMap(key0, val0, AliasMap{}) {
		t.Fatal("KEY[0] and VALUE[0] with same ordinal path must NOT be equal (Source discriminates)")
	}
	if SemanticEqualsUnderAliasMap(key0, other0, AliasMap{}) {
		t.Fatal("KEY[0] and OTHER[0] with same ordinal path must NOT be equal (Source discriminates)")
	}
	// equal⟹same-hash invariant in the distinct direction: distinct Source
	// folds into the hash too, so they do not collide in the memo.
	if SemanticHashCode(key0) == SemanticHashCode(val0) {
		t.Fatal("KEY[0] and VALUE[0] must hash differently")
	}

	// Same Source (and alias differs) ⇒ still equal + same hash: the fix does
	// not over-restrict — Source-equal + path-equal + alias-ignored stays equal.
	val0OtherAlias := NewIndexEntryObjectValue(NamedCorrelationIdentifier("q1"), TupleSourceValue, []int{0}, UnknownType)
	if !SemanticEqualsUnderAliasMap(val0, val0OtherAlias, AliasMap{}) {
		t.Fatal("VALUE[0] values differing only by alias must be equal")
	}
	if SemanticHashCode(val0) != SemanticHashCode(val0OtherAlias) {
		t.Fatal("alias-variant VALUE[0] values must hash equal")
	}
}
