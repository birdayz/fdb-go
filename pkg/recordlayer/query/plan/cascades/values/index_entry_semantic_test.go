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
