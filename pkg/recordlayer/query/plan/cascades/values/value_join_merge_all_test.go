package values

import (
	"testing"
)

// fakeCorrBinder implements CorrelationBinder over a fixed alias→row map for tests.
type fakeCorrBinder struct {
	rows map[CorrelationIdentifier]any
}

func (b fakeCorrBinder) GetCorrelationBinding(a CorrelationIdentifier) (any, bool) {
	v, ok := b.rows[a]
	return v, ok
}

// TestJoinMergeAllValue_ChainsQualifiedKeys is the load-bearing unit test for
// RFC-043: a chain of binary/N-ary merges accumulates EVERY table's columns up a
// join spine. It pins the property that makes a buried table's columns (e.g. a
// middle-table projection with no terminal decomposition) survive: Evaluate
// preserves already-qualified keys verbatim and qualifies only bare keys, so the
// outer's accumulated A.*/B.* keys pass through untouched while the inner table
// is freshly qualified.
func TestJoinMergeAllValue_ChainsQualifiedKeys(t *testing.T) {
	t.Parallel()

	aQ := NamedCorrelationIdentifier("A")
	bQ := NamedCorrelationIdentifier("B")
	subQ := NamedCorrelationIdentifier("$m_A_B") // a re-enumerated merge quantifier

	// Level 1: merge base tables A and B (bare-keyed rows).
	m1 := NewJoinMergeAllValue(aQ, bQ)
	level1 := m1.Evaluate(fakeCorrBinder{rows: map[CorrelationIdentifier]any{
		aQ: map[string]any{"ID": int64(1), "VAL": "alpha"},
		bQ: map[string]any{"ID": int64(10), "A_REF": int64(1)},
	}})
	row1, ok := level1.(map[string]any)
	if !ok {
		t.Fatalf("level1 not a map: %T", level1)
	}
	// Both tables' columns present, qualified.
	for k, want := range map[string]any{"A.ID": int64(1), "A.VAL": "alpha", "B.ID": int64(10), "B.A_REF": int64(1)} {
		if row1[k] != want {
			t.Errorf("level1[%q] = %v, want %v", k, row1[k], want)
		}
	}

	// Level 2: merge the level-1 MERGED row (under sub-quantifier $m_A_B,
	// carrying qualified A.*/B.* keys) with a fresh base table C. A.VAL — the
	// deeply-buried column — must SURVIVE the second merge untouched.
	cQ := NamedCorrelationIdentifier("C")
	m2 := NewJoinMergeAllValue(subQ, cQ)
	level2 := m2.Evaluate(fakeCorrBinder{rows: map[CorrelationIdentifier]any{
		subQ: row1, // the merged row from level 1
		cQ:   map[string]any{"ID": int64(100), "B_REF": int64(10)},
	}})
	row2, ok := level2.(map[string]any)
	if !ok {
		t.Fatalf("level2 not a map: %T", level2)
	}
	// The buried A.VAL and A.ID survive the nested merge (the bug RFC-043 fixes).
	if row2["A.VAL"] != "alpha" {
		t.Errorf("level2[A.VAL] = %v, want alpha — buried column LOST through nested merge", row2["A.VAL"])
	}
	if row2["A.ID"] != int64(1) || row2["B.ID"] != int64(10) || row2["C.ID"] != int64(100) {
		t.Errorf("level2 missing accumulated keys: %v", row2)
	}
}

// TestJoinMergeAllValue_NilWhenUnbound returns nil when no alias is bound (the
// row simply does not exist), distinct from an empty-but-present row.
func TestJoinMergeAllValue_NilWhenUnbound(t *testing.T) {
	t.Parallel()
	m := NewJoinMergeAllValue(NamedCorrelationIdentifier("A"))
	if got := m.Evaluate(fakeCorrBinder{rows: map[CorrelationIdentifier]any{}}); got != nil {
		t.Errorf("Evaluate with no binding = %v, want nil", got)
	}
}

// TestJoinMergeAllValue_Correlations reports exactly the listed aliases, so the
// partition rule's liveness closure propagates through a merge result value.
func TestJoinMergeAllValue_Correlations(t *testing.T) {
	t.Parallel()
	aQ, bQ := NamedCorrelationIdentifier("A"), NamedCorrelationIdentifier("B")
	corr := GetCorrelatedToOfValue(NewJoinMergeAllValue(aQ, bQ))
	if len(corr) != 2 {
		t.Fatalf("corr = %v, want {A,B}", corr)
	}
	if _, ok := corr[aQ]; !ok {
		t.Errorf("missing A in %v", corr)
	}
	if _, ok := corr[bQ]; !ok {
		t.Errorf("missing B in %v", corr)
	}
}
