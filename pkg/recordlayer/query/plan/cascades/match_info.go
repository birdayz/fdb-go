package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// ---------------------------------------------------------------------------
// Placeholder types — full ports pending. These provide enough surface
// for MatchInfo consumers to compile and test against; they'll be
// fleshed out when their own ports land.
// ---------------------------------------------------------------------------

// MaxMatchMap represents the maximum matching between query and
// candidate Value subtrees. Placeholder -- full port pending.
type MaxMatchMap struct {
	mapping        map[values.Value]values.Value
	queryValue     values.Value
	candidateValue values.Value
}

// NewMaxMatchMap constructs a MaxMatchMap.
func NewMaxMatchMap(
	mapping map[values.Value]values.Value,
	queryValue values.Value,
	candidateValue values.Value,
) *MaxMatchMap {
	m := make(map[values.Value]values.Value, len(mapping))
	for k, v := range mapping {
		m[k] = v
	}
	return &MaxMatchMap{
		mapping:        m,
		queryValue:     queryValue,
		candidateValue: candidateValue,
	}
}

// GetMap returns the value-to-value mapping.
func (m *MaxMatchMap) GetMap() map[values.Value]values.Value { return m.mapping }

// GetQueryValue returns the root query value.
func (m *MaxMatchMap) GetQueryValue() values.Value { return m.queryValue }

// GetCandidateValue returns the root candidate value.
func (m *MaxMatchMap) GetCandidateValue() values.Value { return m.candidateValue }

// PredicateMultiMap maps query predicates to candidate predicate
// mappings. Placeholder -- full port pending.
type PredicateMultiMap struct {
	// Will hold map[QueryPredicate][]PredicateMapping when predicates
	// are fully ported.
}

// QueryPlanConstraint captures assumptions under which a match is valid.
// Placeholder -- full port pending.
type QueryPlanConstraint struct{}

// PartialMatch is forward-declared -- full definition will live in
// partial_match.go. Using an interface to avoid circular dependencies.
type PartialMatch interface {
	GetMatchCandidate() MatchCandidate
	GetMatchInfo() MatchInfo
}

// ---------------------------------------------------------------------------
// MatchInfo interface
// ---------------------------------------------------------------------------

// MatchInfo represents the result of matching one expression against
// an expression from a MatchCandidate.
//
// Ports Java's com.apple.foundationdb.record.query.plan.cascades.MatchInfo.
type MatchInfo interface {
	// GetMatchedOrderingParts returns the ordering parts matched by
	// this match info.
	GetMatchedOrderingParts() []*MatchedOrderingPart

	// GetMaxMatchMap returns the maximum match map between query and
	// candidate value subtrees.
	GetMaxMatchMap() *MaxMatchMap

	// IsAdjusted reports whether this MatchInfo was produced by
	// adjusting (wrapping) another MatchInfo.
	IsAdjusted() bool

	// IsRegular reports whether this MatchInfo is a direct
	// (non-adjusted) match.
	IsRegular() bool

	// GetRegularMatchInfo returns the underlying RegularMatchInfo.
	// For a RegularMatchInfo, it returns itself. For an
	// AdjustedMatchInfo, it delegates to the underlying MatchInfo.
	GetRegularMatchInfo() *RegularMatchInfo

	// GetGroupByMappings returns the group-by mappings captured
	// during matching.
	GetGroupByMappings() *GroupByMappings
}

// NewAdjustedBuilder creates an AdjustedBuilder pre-seeded with
// the given MatchInfo's current values. Equivalent to Java's
// MatchInfo.adjustedBuilder() default method.
func NewAdjustedBuilder(mi MatchInfo) *AdjustedBuilder {
	return &AdjustedBuilder{
		underlying:           mi,
		matchedOrderingParts: mi.GetMatchedOrderingParts(),
		maxMatchMap:          mi.GetMaxMatchMap(),
		groupByMappings:      mi.GetGroupByMappings(),
	}
}

// ---------------------------------------------------------------------------
// RegularMatchInfo
// ---------------------------------------------------------------------------

// RegularMatchInfo is the primary implementation of MatchInfo,
// representing a direct match between two expressions.
//
// Ports Java's MatchInfo.RegularMatchInfo.
type RegularMatchInfo struct {
	parameterBindingMap      map[values.CorrelationIdentifier]*predicates.ComparisonRange
	bindingAliasMap          *AliasMap
	predicateMap             *PredicateMultiMap
	matchedOrderingParts     []*MatchedOrderingPart
	maxMatchMap              *MaxMatchMap
	groupByMappings          *GroupByMappings
	rollUpToGroupingValues   []values.Value // nil when not applicable
	additionalPlanConstraint *QueryPlanConstraint
}

// NewRegularMatchInfo constructs a RegularMatchInfo. All collection
// fields are defensively copied.
func NewRegularMatchInfo(
	parameterBindingMap map[values.CorrelationIdentifier]*predicates.ComparisonRange,
	bindingAliasMap *AliasMap,
	predicateMap *PredicateMultiMap,
	matchedOrderingParts []*MatchedOrderingPart,
	maxMatchMap *MaxMatchMap,
	groupByMappings *GroupByMappings,
	rollUpToGroupingValues []values.Value,
	additionalPlanConstraint *QueryPlanConstraint,
) *RegularMatchInfo {
	// Defensive copy of parameterBindingMap.
	pbm := make(map[values.CorrelationIdentifier]*predicates.ComparisonRange, len(parameterBindingMap))
	for k, v := range parameterBindingMap {
		pbm[k] = v
	}

	// Defensive copy of matchedOrderingParts.
	mop := make([]*MatchedOrderingPart, len(matchedOrderingParts))
	copy(mop, matchedOrderingParts)

	// Defensive copy of rollUpToGroupingValues (preserving nil).
	var rug []values.Value
	if rollUpToGroupingValues != nil {
		rug = make([]values.Value, len(rollUpToGroupingValues))
		copy(rug, rollUpToGroupingValues)
	}

	return &RegularMatchInfo{
		parameterBindingMap:      pbm,
		bindingAliasMap:          bindingAliasMap,
		predicateMap:             predicateMap,
		matchedOrderingParts:     mop,
		maxMatchMap:              maxMatchMap,
		groupByMappings:          groupByMappings,
		rollUpToGroupingValues:   rug,
		additionalPlanConstraint: additionalPlanConstraint,
	}
}

// GetParameterBindingMap returns the parameter binding map.
func (r *RegularMatchInfo) GetParameterBindingMap() map[values.CorrelationIdentifier]*predicates.ComparisonRange {
	return r.parameterBindingMap
}

// GetBindingAliasMap returns the alias map used for binding.
func (r *RegularMatchInfo) GetBindingAliasMap() *AliasMap {
	return r.bindingAliasMap
}

// GetPredicateMap returns the predicate multi-map.
func (r *RegularMatchInfo) GetPredicateMap() *PredicateMultiMap {
	return r.predicateMap
}

// GetMatchedOrderingParts returns the matched ordering parts.
func (r *RegularMatchInfo) GetMatchedOrderingParts() []*MatchedOrderingPart {
	return r.matchedOrderingParts
}

// GetMaxMatchMap returns the maximum match map.
func (r *RegularMatchInfo) GetMaxMatchMap() *MaxMatchMap {
	return r.maxMatchMap
}

// GetGroupByMappings returns the group-by mappings.
func (r *RegularMatchInfo) GetGroupByMappings() *GroupByMappings {
	return r.groupByMappings
}

// GetRollUpToGroupingValues returns the roll-up-to-grouping values,
// or nil if not applicable.
func (r *RegularMatchInfo) GetRollUpToGroupingValues() []values.Value {
	return r.rollUpToGroupingValues
}

// GetAdditionalPlanConstraint returns the additional plan constraint.
func (r *RegularMatchInfo) GetAdditionalPlanConstraint() *QueryPlanConstraint {
	return r.additionalPlanConstraint
}

// IsAdjusted returns false -- RegularMatchInfo is not adjusted.
func (r *RegularMatchInfo) IsAdjusted() bool { return false }

// IsRegular returns true -- RegularMatchInfo is a regular match.
func (r *RegularMatchInfo) IsRegular() bool { return true }

// GetRegularMatchInfo returns itself.
func (r *RegularMatchInfo) GetRegularMatchInfo() *RegularMatchInfo { return r }

// ---------------------------------------------------------------------------
// AdjustedMatchInfo
// ---------------------------------------------------------------------------

// AdjustedMatchInfo wraps an underlying MatchInfo with adjusted
// ordering parts, max match map, and/or group-by mappings. Created
// by the AdjustMatchRule when an existing match is refined by walking
// up the Traversal on the candidate side.
//
// Ports Java's MatchInfo.AdjustedMatchInfo.
type AdjustedMatchInfo struct {
	underlying           MatchInfo
	matchedOrderingParts []*MatchedOrderingPart
	maxMatchMap          *MaxMatchMap
	groupByMappings      *GroupByMappings
}

// NewAdjustedMatchInfo constructs an AdjustedMatchInfo.
func NewAdjustedMatchInfo(
	underlying MatchInfo,
	matchedOrderingParts []*MatchedOrderingPart,
	maxMatchMap *MaxMatchMap,
	groupByMappings *GroupByMappings,
) *AdjustedMatchInfo {
	return &AdjustedMatchInfo{
		underlying:           underlying,
		matchedOrderingParts: matchedOrderingParts,
		maxMatchMap:          maxMatchMap,
		groupByMappings:      groupByMappings,
	}
}

// GetUnderlying returns the wrapped MatchInfo.
func (a *AdjustedMatchInfo) GetUnderlying() MatchInfo {
	return a.underlying
}

// GetMatchedOrderingParts returns the adjusted ordering parts.
func (a *AdjustedMatchInfo) GetMatchedOrderingParts() []*MatchedOrderingPart {
	return a.matchedOrderingParts
}

// GetMaxMatchMap returns the adjusted max match map.
func (a *AdjustedMatchInfo) GetMaxMatchMap() *MaxMatchMap {
	return a.maxMatchMap
}

// GetGroupByMappings returns the adjusted group-by mappings.
func (a *AdjustedMatchInfo) GetGroupByMappings() *GroupByMappings {
	return a.groupByMappings
}

// IsAdjusted returns true -- AdjustedMatchInfo is always adjusted.
func (a *AdjustedMatchInfo) IsAdjusted() bool { return true }

// IsRegular returns false -- AdjustedMatchInfo is not regular.
func (a *AdjustedMatchInfo) IsRegular() bool { return false }

// GetRegularMatchInfo delegates to the underlying MatchInfo.
func (a *AdjustedMatchInfo) GetRegularMatchInfo() *RegularMatchInfo {
	return a.underlying.GetRegularMatchInfo()
}

// ---------------------------------------------------------------------------
// AdjustedBuilder
// ---------------------------------------------------------------------------

// AdjustedBuilder builds an AdjustedMatchInfo from an underlying
// MatchInfo, allowing selective override of ordering parts,
// max match map, and group-by mappings.
//
// Ports Java's MatchInfo.AdjustedBuilder.
type AdjustedBuilder struct {
	underlying           MatchInfo
	matchedOrderingParts []*MatchedOrderingPart
	maxMatchMap          *MaxMatchMap
	groupByMappings      *GroupByMappings
}

// GetMatchedOrderingParts returns the builder's current ordering parts.
func (b *AdjustedBuilder) GetMatchedOrderingParts() []*MatchedOrderingPart {
	return b.matchedOrderingParts
}

// SetMatchedOrderingParts overrides the ordering parts.
func (b *AdjustedBuilder) SetMatchedOrderingParts(parts []*MatchedOrderingPart) *AdjustedBuilder {
	b.matchedOrderingParts = parts
	return b
}

// GetMaxMatchMap returns the builder's current max match map.
func (b *AdjustedBuilder) GetMaxMatchMap() *MaxMatchMap {
	return b.maxMatchMap
}

// SetMaxMatchMap overrides the max match map.
func (b *AdjustedBuilder) SetMaxMatchMap(m *MaxMatchMap) *AdjustedBuilder {
	b.maxMatchMap = m
	return b
}

// GetGroupByMappings returns the builder's current group-by mappings.
func (b *AdjustedBuilder) GetGroupByMappings() *GroupByMappings {
	return b.groupByMappings
}

// SetGroupByMappings overrides the group-by mappings.
func (b *AdjustedBuilder) SetGroupByMappings(g *GroupByMappings) *AdjustedBuilder {
	b.groupByMappings = g
	return b
}

// Build constructs the AdjustedMatchInfo.
func (b *AdjustedBuilder) Build() *AdjustedMatchInfo {
	return &AdjustedMatchInfo{
		underlying:           b.underlying,
		matchedOrderingParts: b.matchedOrderingParts,
		maxMatchMap:          b.maxMatchMap,
		groupByMappings:      b.groupByMappings,
	}
}
