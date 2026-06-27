package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// GroupByMappings tracks matched groupings, matched aggregates, and
// unmatched aggregates during aggregate index matching. It is a data
// holder with immutable semantics — all maps are defensively copied at
// construction time.
//
// Ports Java's
// com.apple.foundationdb.record.query.plan.cascades.GroupByMappings.
//
// Fields:
//   - matchedGroupingsMap:   query grouping Value -> candidate grouping Value (bijective)
//   - matchedAggregatesMap:  query aggregate Value -> candidate aggregate Value (bijective)
//   - unmatchedAggregatesMap: CorrelationIdentifier -> query aggregate Value not yet matched (bijective)
type GroupByMappings struct {
	matchedGroupingsMap    *BiMap[values.Value, values.Value]
	matchedAggregatesMap   *BiMap[values.Value, values.Value]
	unmatchedAggregatesMap *BiMap[values.CorrelationIdentifier, values.Value]
}

// NewGroupByMappings creates a GroupByMappings from the three maps.
// All maps are defensively copied so mutations to the originals don't
// affect this instance. Mirrors Java's GroupByMappings.of().
func NewGroupByMappings(
	matchedGroupingsMap *BiMap[values.Value, values.Value],
	matchedAggregatesMap *BiMap[values.Value, values.Value],
	unmatchedAggregatesMap *BiMap[values.CorrelationIdentifier, values.Value],
) *GroupByMappings {
	return &GroupByMappings{
		matchedGroupingsMap:    matchedGroupingsMap.Copy(),
		matchedAggregatesMap:   matchedAggregatesMap.Copy(),
		unmatchedAggregatesMap: unmatchedAggregatesMap.Copy(),
	}
}

// EmptyGroupByMappings returns a GroupByMappings with all three maps
// empty. Mirrors Java's GroupByMappings.empty().
func EmptyGroupByMappings() *GroupByMappings {
	return &GroupByMappings{
		matchedGroupingsMap:    NewValueBiMap(),
		matchedAggregatesMap:   NewValueBiMap(),
		unmatchedAggregatesMap: NewCorrValueBiMap(),
	}
}

// MatchedGroupingsMap returns the bidirectional map from query grouping
// Values to candidate grouping Values.
func (g *GroupByMappings) MatchedGroupingsMap() *BiMap[values.Value, values.Value] {
	return g.matchedGroupingsMap
}

// MatchedAggregatesMap returns the bidirectional map from query aggregate
// Values to candidate aggregate Values.
func (g *GroupByMappings) MatchedAggregatesMap() *BiMap[values.Value, values.Value] {
	return g.matchedAggregatesMap
}

// UnmatchedAggregatesMap returns the bidirectional map from
// CorrelationIdentifiers to query aggregate Values that don't have a
// candidate match.
func (g *GroupByMappings) UnmatchedAggregatesMap() *BiMap[values.CorrelationIdentifier, values.Value] {
	return g.unmatchedAggregatesMap
}
