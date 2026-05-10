package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// MaxMatchMap represents the maximum matching between query and
// candidate Value subtrees. Each entry maps a query sub-value to a
// candidate sub-value (keyed by ExplainValue for structural equality,
// matching Java's BiMap approach).
//
// Ports Java's
// com.apple.foundationdb.record.query.plan.cascades.MatchInfo.MaxMatchMap.
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

// ComputeMaxMatchMap creates a MaxMatchMap by finding the maximum
// matching between query and candidate value trees.
//
// Seed implementation: structural equality at the root level.
// If queryValue and candidateValue are structurally equal, creates a
// single-entry mapping. Otherwise, tries to match children
// recursively. Falls back to an empty mapping if no match is found.
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

	mapping := make(map[string]maxMatchEntry)

	if queryValue != nil && candidateValue != nil {
		computeMapping(queryValue, candidateValue, mapping)
	}

	return &MaxMatchMap{
		mapping:           mapping,
		queryValue:        queryValue,
		candidateValue:    candidateValue,
		rangedOverAliases: rangedOverAliases,
	}
}

// computeMapping recursively finds maximal matches between query and
// candidate value trees. If root-level structural equality holds, one
// entry is added. Otherwise, children are matched pairwise.
func computeMapping(queryValue, candidateValue values.Value, mapping map[string]maxMatchEntry) {
	if values.ValuesStructurallyEqual(queryValue, candidateValue) {
		key := values.ExplainValue(queryValue)
		mapping[key] = maxMatchEntry{
			queryValue:     queryValue,
			candidateValue: candidateValue,
		}
		return
	}

	// Try to match children pairwise. This is a simplified version of
	// Java's full Cartesian-product variant generation — good enough
	// for the common cases where children line up positionally.
	qChildren := queryValue.Children()
	cChildren := candidateValue.Children()
	if len(qChildren) == len(cChildren) && len(qChildren) > 0 {
		for i := range qChildren {
			computeMapping(qChildren[i], cChildren[i], mapping)
		}
	}
}

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
