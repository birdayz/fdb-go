package properties

import (
	"sort"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
)

// PredicateCountByLevelInfo holds the number of predicates at each
// level of a RelationalExpression tree. Level 0 is the leaf level;
// higher levels are closer to the root. Matches Java's
// PredicateCountByLevelProperty.PredicateCountByLevelInfo.
type PredicateCountByLevelInfo struct {
	// LevelToPredicateCount maps level number to predicate count.
	LevelToPredicateCount map[int]int
}

// GetHighestLevel returns the maximum level recorded, or -1 if empty.
func (info PredicateCountByLevelInfo) GetHighestLevel() int {
	if len(info.LevelToPredicateCount) == 0 {
		return -1
	}
	max := -1
	for k := range info.LevelToPredicateCount {
		if k > max {
			max = k
		}
	}
	return max
}

// ComparePredicateCountByLevel compares two PredicateCountByLevelInfo
// instances level by level, starting from the deepest (leaf) level.
// Returns negative if a has fewer predicates at a deeper level (or
// fewer levels), positive if more, and zero if equal at all levels.
// Matches Java's PredicateCountByLevelInfo.compare.
func ComparePredicateCountByLevel(a, b PredicateCountByLevelInfo) int {
	// Collect and sort all levels from a.
	levels := make([]int, 0, len(a.LevelToPredicateCount))
	for k := range a.LevelToPredicateCount {
		levels = append(levels, k)
	}
	sort.Ints(levels)

	for _, level := range levels {
		aCount := a.LevelToPredicateCount[level]
		bCount := 0
		if v, ok := b.LevelToPredicateCount[level]; ok {
			bCount = v
		}
		if aCount != bCount {
			if aCount < bCount {
				return -1
			}
			return 1
		}
	}
	aHigh := a.GetHighestLevel()
	bHigh := b.GetHighestLevel()
	if aHigh < bHigh {
		return -1
	}
	if aHigh > bHigh {
		return 1
	}
	return 0
}

// combinePredicateCountInfos merges multiple PredicateCountByLevelInfo
// by summing predicate counts at the same level. Matches Java's
// PredicateCountByLevelInfo.combine.
func combinePredicateCountInfos(infos []PredicateCountByLevelInfo) PredicateCountByLevelInfo {
	result := make(map[int]int)
	for _, info := range infos {
		for level, count := range info.LevelToPredicateCount {
			result[level] += count
		}
	}
	return PredicateCountByLevelInfo{LevelToPredicateCount: result}
}

// EvaluatePredicateCountByLevel walks the expression tree and counts
// predicates at each level. Level 0 corresponds to leaves; the root
// gets the highest level number. Matches Java's
// PredicateCountByLevelProperty.evaluate.
func EvaluatePredicateCountByLevel(expr expressions.RelationalExpression) PredicateCountByLevelInfo {
	if expr == nil {
		return PredicateCountByLevelInfo{LevelToPredicateCount: map[int]int{}}
	}

	// Collect child results.
	var childInfos []PredicateCountByLevelInfo
	for _, q := range expr.GetQuantifiers() {
		ref := q.GetRangesOver()
		if ref == nil {
			continue
		}
		// Java's evaluateAtRef picks the single member (Verify
		// memberResults.size() == 1). Go picks the first member.
		members := ref.Members()
		if len(members) > 0 {
			childInfos = append(childInfos, EvaluatePredicateCountByLevel(members[0]))
		}
	}

	combined := combinePredicateCountInfos(childInfos)

	// Current level is one above the highest child level.
	currentLevel := 0
	for _, ci := range childInfos {
		h := ci.GetHighestLevel()
		if h+1 > currentLevel {
			currentLevel = h + 1
		}
	}

	// Count predicates at this expression.
	predicateCount := 0
	if wp, ok := expr.(expressions.RelationalExpressionWithPredicates); ok {
		predicateCount = len(wp.GetPredicates())
	}

	result := make(map[int]int, len(combined.LevelToPredicateCount)+1)
	for k, v := range combined.LevelToPredicateCount {
		result[k] = v
	}
	result[currentLevel] = predicateCount

	return PredicateCountByLevelInfo{LevelToPredicateCount: result}
}
