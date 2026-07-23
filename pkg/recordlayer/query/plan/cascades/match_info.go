package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// QueryPlanConstraint captures assumptions under which a plan match is
// valid. Wraps a QueryPredicate that must evaluate to true for the
// match to be applicable.
//
// Ports Java's com.apple.foundationdb.record.query.plan.QueryPlanConstraint.
type QueryPlanConstraint struct {
	predicate predicates.QueryPredicate
}

// Tautology returns a constraint that is always satisfied.
func Tautology() *QueryPlanConstraint {
	return &QueryPlanConstraint{predicate: predicates.NewConstantPredicate(predicates.TriTrue)}
}

// NewQueryPlanConstraint creates a constraint from a predicate.
func NewQueryPlanConstraint(pred predicates.QueryPredicate) *QueryPlanConstraint {
	return &QueryPlanConstraint{predicate: pred}
}

// IsTautology reports whether this constraint is always satisfied.
func (c *QueryPlanConstraint) IsTautology() bool {
	if c == nil || c.predicate == nil {
		return true
	}
	if cp, ok := c.predicate.(*predicates.ConstantPredicate); ok {
		return cp.Value == predicates.TriTrue
	}
	return false
}

// IsConstrained reports whether this constraint is non-trivial.
func (c *QueryPlanConstraint) IsConstrained() bool {
	return !c.IsTautology()
}

// GetPredicate returns the underlying predicate.
func (c *QueryPlanConstraint) GetPredicate() predicates.QueryPredicate {
	if c == nil {
		return nil
	}
	return c.predicate
}

// PartialMatch is forward-declared -- full definition will live in
// partial_match.go. Using an interface to avoid circular dependencies.
type PartialMatch interface {
	GetMatchCandidate() MatchCandidate
	GetMatchInfo() MatchInfo
	GetBoundAliasMap() *AliasMap
	GetQueryRef() *expressions.Reference
	GetQueryExpression() expressions.RelationalExpression
	GetCandidateRef() *expressions.Reference
	GetRegularMatchInfo() *RegularMatchInfo
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
	childPartialMatchMap     map[values.CorrelationIdentifier]PartialMatch
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

// GetConstraint composes the current expression's constraint with predicate
// and child-match constraints. Mirrors Java RegularMatchInfo.getConstraint().
func (r *RegularMatchInfo) GetConstraint() *QueryPlanConstraint {
	constraints := make([]*QueryPlanConstraint, 0, 1+len(r.childPartialMatchMap))
	if r.predicateMap != nil {
		for _, mapping := range r.predicateMap.Values() {
			constraints = append(constraints, mapping.GetConstraint())
		}
	}
	for _, alias := range sortedChildAliases(r.childPartialMatchMap) {
		child := r.childPartialMatchMap[alias]
		if child != nil && child.GetRegularMatchInfo() != nil {
			constraints = append(
				constraints,
				child.GetRegularMatchInfo().GetConstraint(),
			)
		}
	}
	constraints = append(constraints, r.additionalPlanConstraint)
	return composeQueryPlanConstraints(constraints...)
}

// IsAdjusted returns false -- RegularMatchInfo is not adjusted.
// GetChildPartialMatchMaybe returns the child PartialMatch for the
// given quantifier alias, or nil if no child match exists for that
// alias. Ports Java's RegularMatchInfo.getChildPartialMatchMaybe.
func (r *RegularMatchInfo) GetChildPartialMatchMaybe(alias values.CorrelationIdentifier) PartialMatch {
	if r.childPartialMatchMap == nil {
		return nil
	}
	return r.childPartialMatchMap[alias]
}

// SetChildPartialMatch sets the child PartialMatch for a quantifier
// alias. Called by MatchIntermediateRule when building composite matches.
func (r *RegularMatchInfo) SetChildPartialMatch(alias values.CorrelationIdentifier, pm PartialMatch) {
	if r.childPartialMatchMap == nil {
		r.childPartialMatchMap = make(map[values.CorrelationIdentifier]PartialMatch)
	}
	r.childPartialMatchMap[alias] = pm
}

// GetChildPartialMatchMap returns a defensive copy of the selected child
// branch keyed by query-quantifier alias.
func (r *RegularMatchInfo) GetChildPartialMatchMap() map[values.CorrelationIdentifier]PartialMatch {
	result := make(
		map[values.CorrelationIdentifier]PartialMatch,
		len(r.childPartialMatchMap),
	)
	for alias, partialMatch := range r.childPartialMatchMap {
		result[alias] = partialMatch
	}
	return result
}

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

// AdjustGroupByMappings pulls up the GroupByMappings through a
// candidate expression. For each matched grouping/aggregate value
// pair, pulls up the candidate-side value through the candidate
// expression's result value.
//
// Ports Java's MatchInfo.adjustGroupByMappings.
func AdjustGroupByMappings(
	gbm *GroupByMappings,
	candidateAlias values.CorrelationIdentifier,
	candidateExpression expressions.RelationalExpression,
) (*GroupByMappings, bool) {
	if gbm == nil || candidateExpression == nil {
		return nil, false
	}
	candidateCorrelations := expressions.GetCorrelatedToOfExpression(candidateExpression)
	adjustedGroupings, ok := adjustMatchedValueMap(
		gbm.MatchedGroupingsMap(),
		candidateAlias,
		candidateExpression.GetResultValue(),
		candidateCorrelations,
	)
	if !ok {
		return nil, false
	}
	adjustedAggregates, ok := adjustMatchedValueMap(
		gbm.MatchedAggregatesMap(),
		candidateAlias,
		candidateExpression.GetResultValue(),
		candidateCorrelations,
	)
	if !ok {
		return nil, false
	}
	return NewGroupByMappings(
		adjustedGroupings,
		adjustedAggregates,
		gbm.UnmatchedAggregatesMap(),
	), true
}

// onlyReferenceMember is the fail-closed counterpart of Java Reference.get(),
// which requires exactly one member. Go Reference.Get() is intentionally a
// first-member convenience and must not be used where result-value metadata
// would depend on which explored alternative happened to be inserted first.
func onlyReferenceMember(
	ref *expressions.Reference,
) (expressions.RelationalExpression, bool) {
	if ref == nil {
		return nil, false
	}
	members := ref.AllMembers()
	if len(members) != 1 || members[0] == nil {
		return nil, false
	}
	return members[0], true
}

func adjustMatchedValueMap(
	matchedMap *BiMap[values.Value, values.Value],
	candidateAlias values.CorrelationIdentifier,
	candidateResultValue values.Value,
	candidateExpressionCorrelations map[values.CorrelationIdentifier]struct{},
) (*BiMap[values.Value, values.Value], bool) {
	if matchedMap == nil {
		return nil, false
	}
	result := NewValueBiMap()
	ok := true
	matchedMap.Range(func(queryValue, candidateValue values.Value) bool {
		constantAliases := differenceCorrelationSets(
			values.GetCorrelatedToWithoutChildrenOfValue(candidateValue),
			candidateExpressionCorrelations,
		)
		pulledUp, pullOK := pullUpGroupByValue(
			candidateValue,
			candidateResultValue,
			candidateAlias,
			constantAliases,
		)
		if !pullOK {
			ok = false
			return false
		}
		if pulledUp != nil {
			ok = putValueBiMapChecked(result, queryValue, pulledUp)
		}
		return ok
	})
	if !ok {
		return nil, false
	}
	return result, true
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
