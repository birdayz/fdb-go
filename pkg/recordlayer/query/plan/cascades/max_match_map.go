package cascades

import (
	"math"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// MaxMatchMap represents the maximum matching between query and
// candidate Value subtrees. Each entry maps a query sub-value to a
// candidate sub-value (keyed by ExplainValue for structural equality,
// matching Java's BiMap approach).
//
// Ports Java's
// com.apple.foundationdb.record.query.plan.cascades.values.translation.MaxMatchMap.
type MaxMatchMap struct {
	mapping           map[string]maxMatchEntry // ExplainValue(queryValue) → entry
	queryValue        values.Value
	candidateValue    values.Value
	rangedOverAliases map[values.CorrelationIdentifier]struct{}
}

// maxMatchEntry holds both the query and candidate values for a single
// mapping entry.
type maxMatchEntry struct {
	queryValue     values.Value
	candidateValue values.Value
}

// matchResult captures the result of matching a query subtree variant
// to the candidate. valueMap holds the correspondences found, maxDepth
// measures match quality (0 = perfect match at current level,
// math.MaxInt32 = not matched).
//
// Ports Java's MaxMatchMap.MatchResult.
type matchResult struct {
	valueMap map[string]maxMatchEntry
	maxDepth int
}

// notMatched returns a sentinel matchResult indicating failure.
func notMatched() matchResult {
	return matchResult{
		valueMap: nil,
		maxDepth: math.MaxInt32,
	}
}

// isMatched reports whether this result represents a successful match.
func (r matchResult) isMatched() bool {
	return r.maxDepth < math.MaxInt32
}

// matchResultOf constructs a successful matchResult.
func matchResultOf(vm map[string]maxMatchEntry, maxDepth int) matchResult {
	return matchResult{valueMap: vm, maxDepth: maxDepth}
}

// ---------------------------------------------------------------------------
// incrementalValueMatcher
// ---------------------------------------------------------------------------

// incrementalValueMatcher tracks which candidate subtrees could
// structurally match a query value as we descend through the tree.
// It implements a lazy, narrowing search: at each level we filter the
// parent's matches to only those whose child at the descended ordinal
// also matches structurally (sans children).
//
// Ports Java's MaxMatchMap.IncrementalValueMatcher.
type incrementalValueMatcher struct {
	matchingCandidates []values.Value // lazily populated
	computed           bool
	computeFn          func() []values.Value
}

// anyMatches reports whether there are any candidate values that still
// potentially match.
func (m *incrementalValueMatcher) anyMatches() bool {
	return len(m.getMatchingCandidates()) > 0
}

func (m *incrementalValueMatcher) getMatchingCandidates() []values.Value {
	if !m.computed {
		m.matchingCandidates = m.computeFn()
		m.computed = true
	}
	return m.matchingCandidates
}

// initialMatcher creates a root-level matcher. It walks the candidate
// tree in pre-order, descending only into RecordConstructorValue nodes
// (the only "invertible" structure), and collects all reachable
// candidate values where equalsWithoutChildren(queryValue, candidateValue)
// is true.
func initialMatcher(queryValue, candidateRoot values.Value) *incrementalValueMatcher {
	return &incrementalValueMatcher{
		computeFn: func() []values.Value {
			var result []values.Value
			walkReachable(candidateRoot, func(cv values.Value) {
				if cv == candidateRoot {
					// At root level, QuantifiedRecordValue always matches.
					if _, ok := queryValue.(*values.QuantifiedRecordValue); ok {
						result = append(result, cv)
						return
					}
				}
				if values.EqualsWithoutChildren(queryValue, cv) {
					result = append(result, cv)
				}
			})
			return result
		},
	}
}

// descendMatcher creates a child-level matcher by filtering the
// parent's matching candidates: for each parent match, take the child
// at ordinal and check if it structurally equals (sans children) the
// child query value.
func descendMatcher(parent *incrementalValueMatcher, childQueryValue values.Value, ordinal int) *incrementalValueMatcher {
	return &incrementalValueMatcher{
		computeFn: func() []values.Value {
			var result []values.Value
			for _, parentCandidate := range parent.getMatchingCandidates() {
				children := parentCandidate.Children()
				if ordinal >= len(children) {
					continue
				}
				candidateChild := children[ordinal]
				if values.EqualsWithoutChildren(childQueryValue, candidateChild) {
					result = append(result, candidateChild)
				}
			}
			return result
		},
	}
}

// walkReachable walks the candidate value tree in pre-order, only
// descending into RecordConstructorValue nodes. This mirrors Java's
// preOrderIterable(v -> v instanceof RecordConstructorValue) — only
// "invertible" structures (record construction) are considered
// reachable from the root.
func walkReachable(v values.Value, visit func(values.Value)) {
	if v == nil {
		return
	}
	visit(v)
	// Only descend into RecordConstructorValue — other composite types
	// are "destructive" (arithmetic, etc.) and their children are not
	// reachable from the root.
	if _, ok := v.(*values.RecordConstructorValue); ok {
		for _, c := range v.Children() {
			walkReachable(c, visit)
		}
	}
}

// ---------------------------------------------------------------------------
// ComputeMaxMatchMap — entry point
// ---------------------------------------------------------------------------

// ComputeMaxMatchMap creates a MaxMatchMap by finding the maximum
// matching between query and candidate value trees.
//
// Ports Java's MaxMatchMap.compute.
func ComputeMaxMatchMap(
	queryValue values.Value,
	candidateValue values.Value,
	rangedOverAliases map[values.CorrelationIdentifier]struct{},
) *MaxMatchMap {
	if rangedOverAliases == nil {
		rangedOverAliases = map[values.CorrelationIdentifier]struct{}{}
	}

	if queryValue == nil || candidateValue == nil {
		return &MaxMatchMap{
			mapping:           make(map[string]maxMatchEntry),
			queryValue:        queryValue,
			candidateValue:    candidateValue,
			rangedOverAliases: rangedOverAliases,
		}
	}

	// Short-circuit: if the candidate is a QOV over the single
	// rangedOverAlias and the query references that alias, the entire
	// candidate is the match.
	if sc := shortCircuitMaybe(queryValue, candidateValue, rangedOverAliases); sc != nil {
		return sc
	}

	// Run the recursive matching algorithm.
	matchers := make([]*incrementalValueMatcher, 0, 8)
	memoMap := make(map[values.Value]map[values.Value]matchResult) // identity-keyed in Go via pointer
	resultsMap := recurseQueryResultValue(
		queryValue, candidateValue, rangedOverAliases,
		-1, matchers, math.MaxInt32, memoMap,
	)

	// Pick the result with the minimum maxDepth.
	var bestMapping map[string]maxMatchEntry
	var bestQueryValue values.Value
	bestMaxDepth := math.MaxInt32
	for qv, result := range resultsMap {
		if result.maxDepth < bestMaxDepth {
			bestMapping = result.valueMap
			bestQueryValue = qv
			bestMaxDepth = result.maxDepth
		}
	}

	if bestMapping == nil {
		bestMapping = make(map[string]maxMatchEntry)
		bestQueryValue = queryValue
	}

	return &MaxMatchMap{
		mapping:           bestMapping,
		queryValue:        bestQueryValue,
		candidateValue:    candidateValue,
		rangedOverAliases: rangedOverAliases,
	}
}

// shortCircuitMaybe attempts to short-circuit matching when the
// candidate is a simple QuantifiedObjectValue over the single
// rangedOverAlias. In that case the entire candidate IS the flowing
// row, so any query that references that alias is trivially matched.
//
// Ports Java's MaxMatchMap.shortCircuitMaybe.
func shortCircuitMaybe(
	queryValue values.Value,
	candidateValue values.Value,
	rangedOverAliases map[values.CorrelationIdentifier]struct{},
) *MaxMatchMap {
	if len(rangedOverAliases) != 1 {
		return nil
	}

	// Extract the single alias.
	var singularAlias values.CorrelationIdentifier
	for a := range rangedOverAliases {
		singularAlias = a
	}

	qov, ok := candidateValue.(*values.QuantifiedObjectValue)
	if !ok || qov.Correlation != singularAlias {
		return nil
	}

	// Query must reference the alias.
	correlated := values.GetCorrelatedToOfValue(queryValue)
	if _, found := correlated[singularAlias]; !found {
		return nil
	}

	// Ensure no QuantifiedRecordValue lurks in the query (Java checks
	// for non-QOV QuantifiedValue instances).
	hasNonQOV := false
	values.WalkValue(queryValue, func(v values.Value) bool {
		if _, isQRV := v.(*values.QuantifiedRecordValue); isQRV {
			hasNonQOV = true
			return false
		}
		return true
	})
	if hasNonQOV {
		return nil
	}

	// Build identity mapping: candidateValue → candidateValue.
	key := values.ExplainValue(candidateValue)
	mapping := map[string]maxMatchEntry{
		key: {queryValue: candidateValue, candidateValue: candidateValue},
	}
	return &MaxMatchMap{
		mapping:           mapping,
		queryValue:        queryValue,
		candidateValue:    candidateValue,
		rangedOverAliases: rangedOverAliases,
	}
}

// ---------------------------------------------------------------------------
// recurseQueryResultValue — the main recursive algorithm
// ---------------------------------------------------------------------------

// recurseQueryResultValue recursively finds maximal matches between a
// query Value subtree and the candidate Value tree. Returns a map from
// query Value variants to matchResults.
//
// Ports Java's MaxMatchMap.recurseQueryResultValue (excluding the
// Simplification variant-expansion step which is not yet ported).
func recurseQueryResultValue(
	currentQueryValue values.Value,
	candidateValue values.Value,
	rangedOverAliases map[values.CorrelationIdentifier]struct{},
	descendOrdinal int,
	parentMatchers []*incrementalValueMatcher,
	maxDepthBound int,
	memoMap map[values.Value]map[values.Value]matchResult,
) map[values.Value]matchResult {
	if maxDepthBound == 0 {
		return map[values.Value]matchResult{currentQueryValue: notMatched()}
	}

	// Descend all parent matchers and track whether any parent still
	// has potential matches in this subtree.
	var localMatchers []*incrementalValueMatcher
	anyParentsMatching := false
	if descendOrdinal >= 0 {
		localMatchers = make([]*incrementalValueMatcher, 0, len(parentMatchers)+1)
		for _, m := range parentMatchers {
			if m.anyMatches() {
				anyParentsMatching = true
			}
			descended := descendMatcher(m, currentQueryValue, descendOrdinal)
			localMatchers = append(localMatchers, descended)
		}
	} else {
		localMatchers = make([]*incrementalValueMatcher, 0, 1)
	}

	// Memoization: if no parent is matching and we already computed
	// this value, return the cached result.
	if !anyParentsMatching {
		if cached, ok := memoMap[currentQueryValue]; ok {
			return cached
		}
	}

	// Create a matcher for this level.
	currentMatcher := initialMatcher(currentQueryValue, candidateValue)
	localMatchers = append([]*incrementalValueMatcher{currentMatcher}, localMatchers...)
	isCurrentMatching := currentMatcher.anyMatches()

	// bestMatches tracks the best result we've found so far. When
	// no parent is matching we can prune aggressively (local decisions
	// only); otherwise we must keep all variants.
	isPruning := !anyParentsMatching
	var bestValue values.Value
	var bestResult matchResult
	bestMaxDepth := -1
	allResults := make(map[values.Value]matchResult) // used when !isPruning

	putResult := func(v values.Value, r matchResult) {
		if isPruning {
			if bestMaxDepth == -1 || r.maxDepth < bestMaxDepth {
				bestValue = v
				bestResult = r
				bestMaxDepth = r.maxDepth
			}
		} else {
			if bestMaxDepth == -1 || r.maxDepth < bestMaxDepth {
				bestMaxDepth = r.maxDepth
			}
			allResults[v] = r
		}
	}

	toResultMap := func() map[values.Value]matchResult {
		if bestMaxDepth == -1 {
			return map[values.Value]matchResult{currentQueryValue: notMatched()}
		}
		if isPruning {
			return map[values.Value]matchResult{bestValue: bestResult}
		}
		return allResults
	}

	children := currentQueryValue.Children()
	if len(children) == 0 {
		// Leaf value: compute directly.
		result := computeForCurrent(maxDepthBound, currentQueryValue, candidateValue,
			rangedOverAliases, nil)
		putResult(currentQueryValue, result)
	} else {
		// Try direct match if no parents are matching but current level
		// potentially matches. If successful, this is the maximum match
		// (no need to descend further).
		directMatchFound := false
		if !anyParentsMatching && isCurrentMatching {
			if matched, candidateMatch := findMatchingReachableCandidate(
				currentQueryValue, candidateValue); matched {
				vm := map[string]maxMatchEntry{
					values.ExplainValue(currentQueryValue): {
						queryValue:     currentQueryValue,
						candidateValue: candidateMatch,
					},
				}
				putResult(currentQueryValue, matchResultOf(vm, 0))
				directMatchFound = true
			}
		}

		if !directMatchFound {
			// Compute the maxDepthBound for children recursion.
			childrenMaxDepthBound := maxDepthBound
			if maxDepthBound < math.MaxInt32 {
				anyLocalMatching := false
				for _, lm := range localMatchers {
					if lm.anyMatches() {
						anyLocalMatching = true
						break
					}
				}
				if !anyLocalMatching {
					childrenMaxDepthBound = maxDepthBound - 1
				} else {
					childrenMaxDepthBound = math.MaxInt32
				}
			}

			// Recurse into each child, collecting per-child result maps.
			childrenResults := make([][]childResultEntry, len(children))
			for i, child := range children {
				childResultMap := recurseQueryResultValue(
					child, candidateValue, rangedOverAliases,
					i, localMatchers, childrenMaxDepthBound, memoMap,
				)
				entries := make([]childResultEntry, 0, len(childResultMap))
				for v, r := range childResultMap {
					entries = append(entries, childResultEntry{value: v, result: r})
				}
				childrenResults[i] = entries
			}

			// Cross-product of all children's results: for each
			// combination, reconstruct the parent with the variant
			// children and compute the parent-level result.
			crossProductChildren(childrenResults, func(combo []childResultEntry) {
				areAllSame := true
				childValues := make([]values.Value, len(children))
				for i, entry := range combo {
					childValues[i] = entry.value
					if entry.value != children[i] {
						areAllSame = false
					}
				}

				var resultQueryValue values.Value
				if areAllSame {
					resultQueryValue = currentQueryValue
				} else {
					resultQueryValue = values.WithChildren(currentQueryValue, childValues)
				}

				// Build the children result entries in the format
				// computeForCurrent expects.
				childEntries := make([]childResultEntry, len(combo))
				copy(childEntries, combo)

				result := computeForCurrent(maxDepthBound, resultQueryValue,
					candidateValue, rangedOverAliases, childEntries)
				putResult(resultQueryValue, result)
			})
		}
	}

	resultMap := toResultMap()

	// Memoize if appropriate.
	if !anyParentsMatching {
		memoMap[currentQueryValue] = resultMap
	}

	return resultMap
}

// childResultEntry pairs a Value variant with its matchResult, used
// when iterating over children's cross-product.
type childResultEntry struct {
	value  values.Value
	result matchResult
}

// crossProductChildren iterates over the Cartesian product of the
// per-child result lists and calls fn for each combination.
func crossProductChildren(childrenResults [][]childResultEntry, fn func([]childResultEntry)) {
	if len(childrenResults) == 0 {
		fn(nil)
		return
	}

	// Build lists for CrossProduct.
	lists := make([][]childResultEntry, len(childrenResults))
	for i, entries := range childrenResults {
		if len(entries) == 0 {
			return // empty list → no cross product
		}
		lists[i] = entries
	}

	combos := CrossProduct(lists)
	for _, combo := range combos {
		fn(combo)
	}
}

// ---------------------------------------------------------------------------
// computeForCurrent
// ---------------------------------------------------------------------------

// computeForCurrent combines children's matchResults into a parent
// result. First tries a direct match of resultQueryValue against the
// candidate tree. If that fails, merges children's mappings with
// adjusted depth.
//
// Ports Java's MaxMatchMap.computeForCurrent.
func computeForCurrent(
	maxDepthBound int,
	resultQueryValue values.Value,
	candidateValue values.Value,
	rangedOverAliases map[values.CorrelationIdentifier]struct{},
	childEntries []childResultEntry,
) matchResult {
	// Try direct match at this level first.
	if matched, candidateMatch := findMatchingReachableCandidate(
		resultQueryValue, candidateValue); matched {
		vm := map[string]maxMatchEntry{
			values.ExplainValue(resultQueryValue): {
				queryValue:     resultQueryValue,
				candidateValue: candidateMatch,
			},
		}
		return matchResultOf(vm, 0)
	}

	// Merge children's results.
	mergedMap := make(map[string]maxMatchEntry)
	childrenMaxDepth := -1

	for _, entry := range childEntries {
		childValue := entry.value
		childResult := entry.result
		childValueMap := childResult.valueMap

		if len(childValueMap) == 0 {
			// Child has no matches. If the child references any
			// rangedOverAlias (i.e. is not constant), this is a failure.
			childCorrelated := values.GetCorrelatedToOfValue(childValue)
			referencesRanged := false
			for alias := range rangedOverAliases {
				if _, found := childCorrelated[alias]; found {
					referencesRanged = true
					break
				}
			}
			if referencesRanged {
				return notMatched()
			}
			// Child is constant w.r.t. rangedOverAliases.
			if childrenMaxDepth < 0 {
				childrenMaxDepth = 0
			}
		} else {
			if childResult.maxDepth > childrenMaxDepth {
				childrenMaxDepth = childResult.maxDepth
			}
		}

		// Branch-and-bound: if accumulated children depth already
		// exceeds what we need, bail out.
		if maxDepthBound < math.MaxInt32 && childrenMaxDepth >= 0 && maxDepthBound-1 < childrenMaxDepth {
			return notMatched()
		}

		for k, v := range childValueMap {
			mergedMap[k] = v
		}
	}

	if childrenMaxDepth == -1 {
		childrenMaxDepth = math.MaxInt32
	}

	resultDepth := childrenMaxDepth
	if resultDepth < math.MaxInt32 {
		resultDepth = childrenMaxDepth + 1
	}

	return matchResultOf(mergedMap, resultDepth)
}

// ---------------------------------------------------------------------------
// findMatchingReachableCandidate
// ---------------------------------------------------------------------------

// findMatchingReachableCandidate walks the candidate tree (only descending
// into RecordConstructorValue for reachability) and checks if any
// reachable subtree is structurally equal to queryValue.
//
// Returns (true, candidateValue) on match, (false, nil) otherwise.
//
// Ports Java's MaxMatchMap.findMatchingReachableCandidateValue (minus
// the ValueEquivalence and unmatchedHandler extension points).
func findMatchingReachableCandidate(
	queryValue values.Value,
	candidateRoot values.Value,
) (bool, values.Value) {
	var found bool
	var match values.Value

	walkReachable(candidateRoot, func(cv values.Value) {
		if found {
			return // already found, skip
		}
		// At root: QuantifiedRecordValue always matches.
		if cv == candidateRoot {
			if _, ok := queryValue.(*values.QuantifiedRecordValue); ok {
				found = true
				match = cv
				return
			}
		}
		if values.ValuesStructurallyEqual(queryValue, cv) {
			found = true
			match = cv
		}
	})

	return found, match
}

// ---------------------------------------------------------------------------
// NewMaxMatchMap
// ---------------------------------------------------------------------------

// NewMaxMatchMap constructs a MaxMatchMap from a pre-built mapping.
// The mapping is defensively copied. Retained for backwards
// compatibility with existing call sites.
func NewMaxMatchMap(
	mapping map[values.Value]values.Value,
	queryValue values.Value,
	candidateValue values.Value,
) *MaxMatchMap {
	m := make(map[string]maxMatchEntry, len(mapping))
	for k, v := range mapping {
		key := values.ExplainValue(k)
		m[key] = maxMatchEntry{
			queryValue:     k,
			candidateValue: v,
		}
	}
	return &MaxMatchMap{
		mapping:           m,
		queryValue:        queryValue,
		candidateValue:    candidateValue,
		rangedOverAliases: map[values.CorrelationIdentifier]struct{}{},
	}
}

// ---------------------------------------------------------------------------
// MaxMatchMap methods (unchanged from original)
// ---------------------------------------------------------------------------

// GetMap returns the value-to-value mapping in the legacy format
// (map[values.Value]values.Value). This is the query→candidate
// direction.
func (m *MaxMatchMap) GetMap() map[values.Value]values.Value {
	out := make(map[values.Value]values.Value, len(m.mapping))
	for _, entry := range m.mapping {
		out[entry.queryValue] = entry.candidateValue
	}
	return out
}

// GetQueryValue returns the root query value.
func (m *MaxMatchMap) GetQueryValue() values.Value { return m.queryValue }

// GetCandidateValue returns the root candidate value.
func (m *MaxMatchMap) GetCandidateValue() values.Value { return m.candidateValue }

// Size returns the number of entries in the mapping.
func (m *MaxMatchMap) Size() int { return len(m.mapping) }

// TranslateQueryValueMaybe translates the query value so that it can
// be expressed in terms of candidateAlias.
//
// For the identity case (query == candidate structurally, single
// mapping entry), returns a QuantifiedObjectValue(candidateAlias).
//
// For non-identity mappings, uses values.Replace to substitute each
// mapped query subtree with a pulled-up reference through
// candidateAlias, then validates that no rangedOverAliases remain in
// the result.
//
// Returns nil if the translation fails.
//
// Ports Java's MaxMatchMap.translateQueryValueMaybe.
func (m *MaxMatchMap) TranslateQueryValueMaybe(
	candidateAlias values.CorrelationIdentifier,
) values.Value {
	if m.queryValue == nil {
		return nil
	}

	if len(m.mapping) == 0 {
		return nil
	}

	// Build substitution map: ExplainValue(query subtree) → pulled-up value.
	substitutions := make(map[string]values.Value, len(m.mapping))
	for key, entry := range m.mapping {
		// Pull up the candidate value through candidateAlias.
		pulledUp := values.PullUpValue(entry.candidateValue, entry.candidateValue, candidateAlias)
		if pulledUp == nil {
			// Cannot pull up this candidate value — for the identity
			// case (query == candidate), just use a QOV directly.
			if values.ValuesStructurallyEqual(entry.queryValue, entry.candidateValue) {
				pulledUp = &values.QuantifiedObjectValue{
					Correlation: candidateAlias,
					Typ:         entry.candidateValue.Type(),
				}
			} else {
				return nil
			}
		}
		substitutions[key] = pulledUp
	}

	// Replace mapped subtrees in the query value tree.
	result := values.Replace(m.queryValue, func(v values.Value) values.Value {
		key := values.ExplainValue(v)
		if replacement, ok := substitutions[key]; ok {
			return replacement
		}
		return v
	})

	if result == nil {
		return nil
	}

	// Validate: remaining correlations should not include rangedOverAliases.
	if len(m.rangedOverAliases) > 0 {
		remaining := values.GetCorrelatedToOfValue(result)
		for alias := range m.rangedOverAliases {
			if _, found := remaining[alias]; found {
				return nil
			}
		}
	}

	return result
}

// PullUpMaybe creates a TranslationMap that translates queryAlias
// references to the query value expressed through candidateAlias.
//
// Returns (translationMap, true) on success, (nil, false) on failure.
//
// Ports Java's MaxMatchMap.pullUpMaybe.
func (m *MaxMatchMap) PullUpMaybe(
	queryAlias values.CorrelationIdentifier,
	candidateAlias values.CorrelationIdentifier,
) (*RegularTranslationMap, bool) {
	translated := m.TranslateQueryValueMaybe(candidateAlias)
	if translated == nil {
		return nil, false
	}

	// Build a TranslationMap: When(queryAlias).Then(fn that returns
	// the translated value for any leaf referencing queryAlias).
	b := NewTranslationMapBuilder()
	capturedValue := translated // capture for closure
	b.When(queryAlias).Then(func(_ values.CorrelationIdentifier, _ values.LeafValue) values.Value {
		return capturedValue
	})
	return b.Build(), true
}

// AdjustMaybe adjusts this MaxMatchMap through an upper candidate
// level. It translates the query value through upperCandidateAlias,
// then re-computes a MaxMatchMap against upperCandidateResultValue.
//
// Returns (adjustedMap, true) on success, (nil, false) on failure.
//
// Ports Java's MaxMatchMap.adjustMaybe.
func (m *MaxMatchMap) AdjustMaybe(
	upperCandidateAlias values.CorrelationIdentifier,
	upperCandidateResultValue values.Value,
	rangedOverAliases map[values.CorrelationIdentifier]struct{},
) (*MaxMatchMap, bool) {
	translated := m.TranslateQueryValueMaybe(upperCandidateAlias)
	if translated == nil {
		return nil, false
	}

	result := ComputeMaxMatchMap(translated, upperCandidateResultValue, rangedOverAliases)
	return result, true
}
