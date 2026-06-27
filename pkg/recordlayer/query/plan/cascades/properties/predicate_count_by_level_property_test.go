package properties

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

func TestEvaluatePredicateCountByLevel_Nil(t *testing.T) {
	t.Parallel()
	info := EvaluatePredicateCountByLevel(nil)
	if info.GetHighestLevel() != -1 {
		t.Fatalf("highest level for nil = %d, want -1", info.GetHighestLevel())
	}
}

func TestEvaluatePredicateCountByLevel_NoPredicates(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
	info := EvaluatePredicateCountByLevel(scan)
	// Leaf node is level 0, with 0 predicates.
	if info.GetHighestLevel() != 0 {
		t.Fatalf("highest level = %d, want 0", info.GetHighestLevel())
	}
	if info.LevelToPredicateCount[0] != 0 {
		t.Fatalf("level 0 count = %d, want 0", info.LevelToPredicateCount[0])
	}
}

func TestEvaluatePredicateCountByLevel_FilterWithPredicates(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
	ref := expressions.InitialOf(scan)
	inner := expressions.ForEachQuantifier(ref)
	p1 := predicates.NewConstantPredicate(predicates.TriTrue)
	p2 := predicates.NewConstantPredicate(predicates.TriFalse)
	filter := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{p1, p2}, inner)

	info := EvaluatePredicateCountByLevel(filter)
	// scan at level 0 (0 predicates), filter at level 1 (2 predicates).
	if info.GetHighestLevel() != 1 {
		t.Fatalf("highest level = %d, want 1", info.GetHighestLevel())
	}
	if info.LevelToPredicateCount[0] != 0 {
		t.Fatalf("level 0 = %d, want 0", info.LevelToPredicateCount[0])
	}
	if info.LevelToPredicateCount[1] != 2 {
		t.Fatalf("level 1 = %d, want 2", info.LevelToPredicateCount[1])
	}
}

func TestComparePredicateCountByLevel_Equal(t *testing.T) {
	t.Parallel()
	a := PredicateCountByLevelInfo{LevelToPredicateCount: map[int]int{0: 2, 1: 1}}
	b := PredicateCountByLevelInfo{LevelToPredicateCount: map[int]int{0: 2, 1: 1}}
	if cmp := ComparePredicateCountByLevel(a, b); cmp != 0 {
		t.Fatalf("compare equal = %d, want 0", cmp)
	}
}

func TestComparePredicateCountByLevel_AFewerAtDeeper(t *testing.T) {
	t.Parallel()
	a := PredicateCountByLevelInfo{LevelToPredicateCount: map[int]int{0: 1, 1: 3}}
	b := PredicateCountByLevelInfo{LevelToPredicateCount: map[int]int{0: 2, 1: 1}}
	// a has fewer at level 0 (deeper) than b, so a < b.
	if cmp := ComparePredicateCountByLevel(a, b); cmp >= 0 {
		t.Fatalf("compare = %d, want negative", cmp)
	}
}

func TestComparePredicateCountByLevel_FewerLevels(t *testing.T) {
	t.Parallel()
	a := PredicateCountByLevelInfo{LevelToPredicateCount: map[int]int{0: 1}}
	b := PredicateCountByLevelInfo{LevelToPredicateCount: map[int]int{0: 1, 1: 1}}
	// a has fewer levels (highest=0) than b (highest=1), so a < b.
	if cmp := ComparePredicateCountByLevel(a, b); cmp >= 0 {
		t.Fatalf("compare = %d, want negative", cmp)
	}
}
