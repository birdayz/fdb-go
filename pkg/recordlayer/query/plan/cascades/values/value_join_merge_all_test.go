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

// TestJoinMergeAllValue_ColumnNameCollision pins the dimension Torvalds flagged:
// two live tables that share a non-PK column name. The QUALIFIED keys (A.NAME,
// B.NAME) must each carry their own table's value — that is how consumers resolve
// a buried column, and it must never return the wrong table's value. The bare key
// is ambiguous (rejected at SQL resolution) and is defined as last-table-wins.
func TestJoinMergeAllValue_ColumnNameCollision(t *testing.T) {
	t.Parallel()
	aQ, bQ := NamedCorrelationIdentifier("A"), NamedCorrelationIdentifier("B")
	out := NewJoinMergeAllValue(aQ, bQ).Evaluate(fakeCorrBinder{rows: map[CorrelationIdentifier]any{
		aQ: map[string]any{"ID": int64(1), "NAME": "from_a"},
		bQ: map[string]any{"ID": int64(2), "NAME": "from_b"},
	}})
	row := out.(map[string]any)
	if row["A.NAME"] != "from_a" {
		t.Errorf("A.NAME = %v, want from_a (wrong table's value)", row["A.NAME"])
	}
	if row["B.NAME"] != "from_b" {
		t.Errorf("B.NAME = %v, want from_b (wrong table's value)", row["B.NAME"])
	}
	if row["A.ID"] != int64(1) || row["B.ID"] != int64(2) {
		t.Errorf("qualified IDs wrong: A.ID=%v B.ID=%v", row["A.ID"], row["B.ID"])
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

// TestJoinMergeAllValue_CanonicalAcrossLegOrder pins Graefe condition 1 (RFC-074)
// at the VALUE level: the merge of a leg-SET is ONE canonical value regardless of
// leg order. Concretely: SemanticEqualsUnderAliasMap is set-based (any permutation
// compares equal) and SemanticHashCode is order-invariant — equal ⟹ same hash.
// This is the value-level property the merge-value collapse establishes (it
// replaces the retired binary JoinMergeResultValue, which — a distinct Go type —
// could never compare equal to the N-ary form for the same leg pair). NOTE: this
// is a VALUE-level guarantee only; it does NOT by itself coalesce equivalent join
// sub-products into one memo Reference (measurement showed the ≥5-way distinctRefs
// are unchanged — that is the separate broad-merge/pruning problem, PR-C2).
func TestJoinMergeAllValue_CanonicalAcrossLegOrder(t *testing.T) {
	t.Parallel()
	a := NamedCorrelationIdentifier("A")
	b := NamedCorrelationIdentifier("B")
	c := NamedCorrelationIdentifier("C")
	d := NamedCorrelationIdentifier("D")
	e := NamedCorrelationIdentifier("E")

	canonical := NewJoinMergeAllValue(a, b, c, d, e)
	// Every permutation of the same leg-set must be SemanticEquals + same hash.
	perms := [][]CorrelationIdentifier{
		{e, d, c, b, a},
		{c, a, e, b, d},
		{b, a, d, c, e},
		{a, b, c, d, e},
	}
	for _, p := range perms {
		other := NewJoinMergeAllValue(p...)
		if !SemanticEqualsUnderAliasMap(canonical, other, AliasMap{}) {
			t.Errorf("permutation %v not SemanticEquals to canonical {A,B,C,D,E} — interning would fork", p)
		}
		if SemanticHashCode(canonical) != SemanticHashCode(other) {
			t.Errorf("permutation %v has a different SemanticHashCode — violates equal⟹same-hash", p)
		}
	}

	// A DIFFERENT leg-set must NOT compare equal (no over-collapse).
	if SemanticEqualsUnderAliasMap(canonical, NewJoinMergeAllValue(a, b, c, d), AliasMap{}) {
		t.Error("{A,B,C,D,E} must not equal {A,B,C,D} (different arity)")
	}
	if SemanticEqualsUnderAliasMap(NewJoinMergeAllValue(a, b), NewJoinMergeAllValue(a, c), AliasMap{}) {
		t.Error("{A,B} must not equal {A,C} (different members)")
	}

	// Provenance is preserved: a translator SEED and a re-enumeration of the SAME
	// leg-set must NOT compare equal (and must hash differently). The retired
	// two-type design never interned them; the Seed bit keeps that — interning them
	// would trigger cross-group merges the two types never did (the STAR budget
	// regression). This is the behavior-preserving guarantee of the collapse.
	seed := NewJoinMergeSeedValue(a, b)
	reenum := NewJoinMergeAllValue(a, b)
	if SemanticEqualsUnderAliasMap(seed, reenum, AliasMap{}) {
		t.Error("a translator SEED must not equal a re-enumeration of the same leg-set (provenance must be preserved)")
	}
	if SemanticHashCode(seed) == SemanticHashCode(reenum) {
		t.Error("seed and re-enumeration of the same leg-set must hash differently (Seed folded into the hash)")
	}
	// A seed's leg ORDER is semantic (joinResultValueIsReversed reads Aliases[0],
	// composeFieldOverJoinMerge reads Aliases[1]), so a seed is compared
	// order-SENSITIVELY (positional), matching the retired JoinMergeResultValue:
	// two seeds over the same leg-set in DIFFERENT orders must NOT intern (codex —
	// otherwise interning could keep the wrong column order for a binary join
	// reached the opposite way). Identical-order seeds DO intern.
	if SemanticEqualsUnderAliasMap(seed, NewJoinMergeSeedValue(b, a), AliasMap{}) {
		t.Error("two seeds in DIFFERENT leg order must NOT compare equal (a seed's order is semantic)")
	}
	if !SemanticEqualsUnderAliasMap(seed, NewJoinMergeSeedValue(a, b), AliasMap{}) {
		t.Error("identical seeds (same legs, same order) must compare equal")
	}
	// Re-enumeration merges, by contrast, ARE order-independent (no order-sensitive
	// consumer reads them; they intern across bipartitions — Graefe condition 1).
	if !SemanticEqualsUnderAliasMap(reenum, NewJoinMergeAllValue(b, a), AliasMap{}) {
		t.Error("two re-enumeration merges over the same leg-set must compare equal regardless of order")
	}

	// Evaluate output is leg-order-independent for the qualified keys (the keys
	// consumers actually resolve): both orders produce A.ID and B.ID identically.
	rows := map[CorrelationIdentifier]any{
		a: map[string]any{"ID": int64(1)},
		b: map[string]any{"ID": int64(2)},
	}
	ab := NewJoinMergeAllValue(a, b).Evaluate(fakeCorrBinder{rows: rows}).(map[string]any)
	ba := NewJoinMergeAllValue(b, a).Evaluate(fakeCorrBinder{rows: rows}).(map[string]any)
	if ab["A.ID"] != int64(1) || ab["B.ID"] != int64(2) || ba["A.ID"] != int64(1) || ba["B.ID"] != int64(2) {
		t.Errorf("qualified keys must be leg-order-independent: ab=%v ba=%v", ab, ba)
	}
}

// TestJoinMergeAllValue_SeedReportsNoCorrelations pins the load-bearing fix
// (RFC-074, @claude review): GetCorrelatedToOfValue must report ZERO correlations
// for a translator SEED (Seed=true), exactly as the retired binary
// JoinMergeResultValue did (it stored aliases as fields the walk never read).
// Reporting them inflated every enclosing select's correlation set (+~32% planner
// tasks, tipping the 4-way STAR past budget). This unit pin catches the
// value_correlation.go `if !q.Seed` guard being removed or inverted — which the
// FDB N-way integration test would only catch indirectly.
func TestJoinMergeAllValue_SeedReportsNoCorrelations(t *testing.T) {
	t.Parallel()
	aQ, bQ := NamedCorrelationIdentifier("A"), NamedCorrelationIdentifier("B")
	if corr := GetCorrelatedToOfValue(NewJoinMergeSeedValue(aQ, bQ)); len(corr) != 0 {
		t.Fatalf("Seed=true must report 0 correlations (load-bearing: reporting them "+
			"tipped the 4-way STAR past budget); got %v", corr)
	}
	// A re-enumeration merge (Seed=false) DOES report its aliases — the live set
	// the partition rule's exact branch reads.
	if corr := GetCorrelatedToOfValue(NewJoinMergeAllValue(aQ, bQ)); len(corr) != 2 {
		t.Fatalf("Seed=false must report its aliases; got %v", corr)
	}
}

// TestJoinMergeAllValue_AliasMapEquality exercises the alias-map-applying branch
// of SemanticEqualsUnderAliasMap for the merge value (@claude review): merge{A,B}
// under A→X must equal merge{X,B}. This is the path the memo uses to intern
// equivalents under quantifier renaming; the leg-order tests use only the empty
// map, leaving this branch otherwise unpinned at the unit level.
func TestJoinMergeAllValue_AliasMapEquality(t *testing.T) {
	t.Parallel()
	a, b, x := NamedCorrelationIdentifier("A"), NamedCorrelationIdentifier("B"), NamedCorrelationIdentifier("X")
	m := AliasMap{a: x}
	// Re-enumeration (order-independent) under A→X.
	if !SemanticEqualsUnderAliasMap(NewJoinMergeAllValue(a, b), NewJoinMergeAllValue(x, b), m) {
		t.Error("re-enum merge{A,B} under A→X should equal merge{X,B}")
	}
	// Seed (order-sensitive) under A→X — same positions, so still equal.
	if !SemanticEqualsUnderAliasMap(NewJoinMergeSeedValue(a, b), NewJoinMergeSeedValue(x, b), m) {
		t.Error("seed merge{A,B} under A→X should equal seed{X,B} (same leg positions)")
	}
	// Without the map, A and X differ.
	if SemanticEqualsUnderAliasMap(NewJoinMergeAllValue(a, b), NewJoinMergeAllValue(x, b), AliasMap{}) {
		t.Error("merge{A,B} must not equal merge{X,B} under the empty map")
	}
}

// TestJoinMergeAllValue_EqualsWithoutChildren_Provenance pins the merge arm of the
// alias-free EqualsWithoutChildren path directly (@claude review): it must agree
// with SemanticEqualsUnderAliasMap on provenance + per-provenance leg-order.
func TestJoinMergeAllValue_EqualsWithoutChildren_Provenance(t *testing.T) {
	t.Parallel()
	a, b := NamedCorrelationIdentifier("A"), NamedCorrelationIdentifier("B")
	// Re-enumeration: order-independent.
	if !EqualsWithoutChildren(NewJoinMergeAllValue(a, b), NewJoinMergeAllValue(b, a)) {
		t.Error("re-enum merges over the same leg-set must be EqualsWithoutChildren regardless of order")
	}
	// Seed: order-sensitive.
	if EqualsWithoutChildren(NewJoinMergeSeedValue(a, b), NewJoinMergeSeedValue(b, a)) {
		t.Error("seeds in different leg order must NOT be EqualsWithoutChildren (order is semantic)")
	}
	// Seed never equals re-enumeration.
	if EqualsWithoutChildren(NewJoinMergeSeedValue(a, b), NewJoinMergeAllValue(a, b)) {
		t.Error("a seed must never be EqualsWithoutChildren to a re-enumeration of the same leg-set")
	}
}
