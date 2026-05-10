package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// ---------------------------------------------------------------------------
// Compensation interface
// ---------------------------------------------------------------------------

// Compensation is the byproduct of expression DAG matching. When a
// query subgraph Q matches a materialized data set M (e.g. an index),
// M may subsume Q but produce extraneous records. A Compensation
// corrects for that by applying post-operations such as filtering,
// distinct-ing, or reshaping results.
//
// Ports Java's com.apple.foundationdb.record.query.plan.cascades.Compensation.
//
// ForMatchCompensation implements Intersect, Union, and Apply.
// applyFinal requires memoizer infrastructure (not yet ported).
type Compensation interface {
	// IsNeeded reports whether this compensation must be applied.
	// Returns false only for NoCompensation.
	IsNeeded() bool

	// IsImpossible reports whether this compensation cannot be applied.
	// Returns true only for ImpossibleCompensation or a ForMatch with
	// impossible=true.
	IsImpossible() bool

	// IsNeededForFiltering reports whether this compensation needs to
	// be applied for correct filtering. This matters when a caller cares
	// about the correct set of rows but not the result shape (e.g.
	// EXISTS predicates).
	IsNeededForFiltering() bool

	// IsFinalNeeded reports whether final (result-shape) compensation
	// must be applied when this compensation is at the top of a
	// compensation chain.
	IsFinalNeeded() bool

	// CanBeDeferred reports whether this compensation can be combined
	// with subsequent compensations further up the matched DAG or
	// whether it must be applied at the exact position that created it.
	CanBeDeferred() bool
}

// ---------------------------------------------------------------------------
// noCompensation — the "no compensation needed" sentinel
// ---------------------------------------------------------------------------

type noCompensation struct{}

func (noCompensation) IsNeeded() bool             { return false }
func (noCompensation) IsImpossible() bool         { return false }
func (noCompensation) IsNeededForFiltering() bool { return false }
func (noCompensation) IsFinalNeeded() bool        { return false }
func (noCompensation) CanBeDeferred() bool        { return true }
func (noCompensation) String() string             { return "no-compensation" }

// ---------------------------------------------------------------------------
// impossibleCompensation — identity element for the intersection monoid
// ---------------------------------------------------------------------------

type impossibleCompensation struct{}

func (impossibleCompensation) IsNeeded() bool             { return true }
func (impossibleCompensation) IsImpossible() bool         { return true }
func (impossibleCompensation) IsNeededForFiltering() bool { return true }
func (impossibleCompensation) IsFinalNeeded() bool        { return true }
func (impossibleCompensation) CanBeDeferred() bool        { return true }
func (impossibleCompensation) String() string             { return "impossible-compensation" }

// ---------------------------------------------------------------------------
// Sentinel values
// ---------------------------------------------------------------------------

var (
	// NoCompensation indicates that no additional operators need to be
	// injected to compensate for a match. Equivalent to Java's
	// Compensation.NO_COMPENSATION.
	NoCompensation Compensation = noCompensation{}

	// ImpossibleCompensation indicates that compensation is needed but
	// cannot be computed. It is the identity element for the
	// intersection monoid on compensations. Equivalent to Java's
	// Compensation.IMPOSSIBLE_COMPENSATION.
	ImpossibleCompensation Compensation = impossibleCompensation{}
)

// CompensatedResult bundles the results of computing result
// compensation for a partial match. Ports Java's
// Compensation.CompensatedResult.
type CompensatedResult struct {
	Impossible           bool
	ResultCompensationFn *ResultCompensationFunction
	GroupByMappings      *GroupByMappings
}

// ComputeResultCompensation computes the result compensation for the
// top operation's partial match. Ports Java's
// Compensation.computeResultCompensation.
func ComputeResultCompensation(pm PartialMatch, rootOfMatchPullUp *PullUp) *CompensatedResult {
	matchInfo := pm.GetMatchInfo()

	if rootOfMatchPullUp == nil {
		return &CompensatedResult{
			Impossible:           false,
			ResultCompensationFn: NoResultCompensation(),
			GroupByMappings:      EmptyGroupByMappings(),
		}
	}

	mmm := matchInfo.GetRegularMatchInfo().GetMaxMatchMap()
	if mmm == nil {
		return nil
	}
	pulledUp := rootOfMatchPullUp.PullUpValueMaybe(mmm.GetQueryValue())
	if pulledUp == nil {
		return nil
	}

	var rcf *ResultCompensationFunction
	if qov, ok := pulledUp.(*values.QuantifiedObjectValue); ok && qov.Correlation == rootOfMatchPullUp.GetCandidateAlias() {
		rcf = NoResultCompensation()
	} else {
		rcf = ResultCompensationOfValue(pulledUp)
	}

	return &CompensatedResult{
		Impossible:           rcf.IsImpossible(),
		ResultCompensationFn: rcf,
		GroupByMappings:      EmptyGroupByMappings(),
	}
}

// IntersectCompensations folds a slice of Compensations via the
// intersection monoid. The identity element is ImpossibleCompensation.
// Ports Java's `compensations.stream().reduce(impossibleCompensation, Compensation::intersect)`.
func IntersectCompensations(compensations []Compensation) Compensation {
	result := ImpossibleCompensation
	for _, c := range compensations {
		result = intersectTwo(result, c)
	}
	return result
}

// UnionCompensations folds a slice of Compensations via union.
// The identity element is NoCompensation.
func UnionCompensations(compensations []Compensation) Compensation {
	result := Compensation(NoCompensation)
	for _, c := range compensations {
		result = unionTwo(result, c)
	}
	return result
}

// intersectTwo dispatches intersection between any two Compensation
// values, handling the monoid identities.
func intersectTwo(a, b Compensation) Compensation {
	// ImpossibleCompensation is the identity: impossible ∩ X = X
	if _, ok := a.(impossibleCompensation); ok {
		return b
	}
	if _, ok := b.(impossibleCompensation); ok {
		return a
	}
	// NoCompensation is the absorbing element: none ∩ X = none
	if !a.IsNeeded() || !b.IsNeeded() {
		return NoCompensation
	}
	// Both are ForMatchCompensation — delegate to the full algorithm.
	aFM, aOk := a.(*ForMatchCompensation)
	bFM, bOk := b.(*ForMatchCompensation)
	if aOk && bOk {
		return aFM.Intersect(bFM)
	}
	// Fallback: can't intersect non-ForMatch compensations.
	return ImpossibleCompensation
}

// unionTwo dispatches union between any two Compensation values.
func unionTwo(a, b Compensation) Compensation {
	if !a.IsNeeded() && !b.IsNeeded() {
		return NoCompensation
	}
	if !a.IsNeeded() {
		return b
	}
	if !b.IsNeeded() {
		return a
	}
	aFM, aOk := a.(*ForMatchCompensation)
	bFM, bOk := b.(*ForMatchCompensation)
	if aOk && bOk {
		return aFM.Union(bFM)
	}
	return ImpossibleCompensation
}

// ---------------------------------------------------------------------------
// Placeholder types for ForMatch dependencies
// ---------------------------------------------------------------------------

// PredicateCompensationMap maps query predicates to compensation
// functions using identity-based keying (pointer equality).
// Ports Java's LinkedIdentityMap<QueryPredicate, PredicateCompensationFunction>.
type PredicateCompensationMap struct {
	keys   []predicates.QueryPredicate
	values []PredicateCompensationFunc
}

// NewPredicateCompensationMap creates a PredicateCompensationMap from
// parallel slices of predicates and compensation functions.
func NewPredicateCompensationMap(keys []predicates.QueryPredicate, vals []PredicateCompensationFunc) *PredicateCompensationMap {
	if len(keys) != len(vals) {
		panic("NewPredicateCompensationMap: keys and values must have same length")
	}
	k := make([]predicates.QueryPredicate, len(keys))
	copy(k, keys)
	v := make([]PredicateCompensationFunc, len(vals))
	copy(v, vals)
	return &PredicateCompensationMap{keys: k, values: v}
}

// EmptyPredicateCompensationMap returns an empty predicate
// compensation map.
func EmptyPredicateCompensationMap() *PredicateCompensationMap {
	return &PredicateCompensationMap{}
}

// StubPredicateCompensationMap creates a PredicateCompensationMap with
// N no-op entries. Used by tests that need a non-empty map to drive
// IsNeeded/IsNeededForFiltering without real predicate content.
func StubPredicateCompensationMap(n int) *PredicateCompensationMap {
	if n <= 0 {
		return EmptyPredicateCompensationMap()
	}
	keys := make([]predicates.QueryPredicate, n)
	vals := make([]PredicateCompensationFunc, n)
	for i := 0; i < n; i++ {
		keys[i] = predicates.NewConstantPredicate(predicates.TriTrue)
		vals[i] = NoPredicateCompensationNeeded()
	}
	return &PredicateCompensationMap{keys: keys, values: vals}
}

// Get returns the compensation function for the given predicate key
// using identity (pointer) comparison. Returns nil if not found.
// Mirrors Java's LinkedIdentityMap.get().
func (m *PredicateCompensationMap) Get(key predicates.QueryPredicate) PredicateCompensationFunc {
	if m == nil {
		return nil
	}
	for i, k := range m.keys {
		if k == key { // pointer identity
			return m.values[i]
		}
	}
	return nil
}

// IsEmpty reports whether the map has no entries.
func (m *PredicateCompensationMap) IsEmpty() bool {
	return m == nil || len(m.keys) == 0
}

// Len returns the number of entries in the map.
func (m *PredicateCompensationMap) Len() int {
	if m == nil {
		return 0
	}
	return len(m.keys)
}

// Entries returns the predicate→compensation pairs in insertion order.
func (m *PredicateCompensationMap) Entries() ([]predicates.QueryPredicate, []PredicateCompensationFunc) {
	if m == nil {
		return nil, nil
	}
	return m.keys, m.values
}

// ApplyCompensations applies all compensation functions in this map
// via the given translation map and returns the collected residual
// predicates. Ports the iteration in Java's ForMatch.apply().
func (m *PredicateCompensationMap) ApplyCompensations(tm TranslationMap) []predicates.QueryPredicate {
	if m == nil {
		return nil
	}
	var result []predicates.QueryPredicate
	for _, fn := range m.values {
		result = append(result, fn.ApplyCompensationForPredicate(tm)...)
	}
	return result
}

// Amend creates a new PredicateCompensationMap with all compensation
// functions amended. Ports the amend loop in Java's
// Compensation.ForMatch.intersect.
func (m *PredicateCompensationMap) Amend(
	unmatchedAggregateMap *BiMap[values.CorrelationIdentifier, values.Value],
	amendedMatchedAggregateMap map[values.Value]values.Value,
) *PredicateCompensationMap {
	if m == nil || len(m.keys) == 0 {
		return m
	}
	newVals := make([]PredicateCompensationFunc, len(m.values))
	for i, fn := range m.values {
		newVals[i] = fn.Amend(unmatchedAggregateMap, amendedMatchedAggregateMap)
	}
	newKeys := make([]predicates.QueryPredicate, len(m.keys))
	copy(newKeys, m.keys)
	return &PredicateCompensationMap{keys: newKeys, values: newVals}
}

// ResultCompensationFunction handles final result shape
// transformation. Ports Java's
// PredicateMultiMap.ResultCompensationFunction.
type ResultCompensationFunction struct {
	needed     bool
	impossible bool
	resultVal  values.Value
}

// NoResultCompensation returns a ResultCompensationFunction that
// indicates no result compensation is needed. Mirrors Java's
// ResultCompensationFunction.noCompensationNeeded().
func NoResultCompensation() *ResultCompensationFunction {
	return &ResultCompensationFunction{needed: false}
}

// NewResultCompensationFunction creates a ResultCompensationFunction.
func NewResultCompensationFunction(needed bool) *ResultCompensationFunction {
	return &ResultCompensationFunction{needed: needed}
}

// ResultCompensationOfValue creates a ResultCompensationFunction from
// a result Value. When applied, it translates the value through the
// translation map. Ports Java's ResultCompensationFunction.ofValue.
func ResultCompensationOfValue(v values.Value) *ResultCompensationFunction {
	return &ResultCompensationFunction{
		needed:     true,
		impossible: valueContainsUnmatchedAggregates(v),
		resultVal:  v,
	}
}

// valueContainsUnmatchedAggregates reports whether a Value tree
// contains any UnmatchedAggregateValue nodes. Ports Java's
// ResultCompensationFunction.valueContainsUnmatchedValues.
func valueContainsUnmatchedAggregates(v values.Value) bool {
	if v == nil {
		return false
	}
	found := false
	values.WalkValue(v, func(node values.Value) bool {
		if _, ok := node.(*values.UnmatchedAggregateValue); ok {
			found = true
			return false
		}
		return !found
	})
	return found
}

// NewImpossibleResultCompensation creates a ResultCompensationFunction
// that is both needed and impossible.
func NewImpossibleResultCompensation() *ResultCompensationFunction {
	return &ResultCompensationFunction{needed: true, impossible: true}
}

// IsNeeded reports whether result compensation must be applied.
func (f *ResultCompensationFunction) IsNeeded() bool {
	return f != nil && f.needed
}

// IsImpossible reports whether result compensation is impossible.
func (f *ResultCompensationFunction) IsImpossible() bool {
	return f != nil && f.impossible
}

// Amend recreates the result compensation function with updated
// aggregate value mappings. Ports Java's
// ResultCompensationFunction.amend.
func (f *ResultCompensationFunction) Amend(
	unmatchedAggregateMap *BiMap[values.CorrelationIdentifier, values.Value],
	amendedMatchedAggregateMap map[values.Value]values.Value,
) *ResultCompensationFunction {
	if f == nil || !f.needed {
		return f
	}
	if f.resultVal == nil {
		return f
	}
	amended := replaceUnmatchedAggregateValues(
		unmatchedAggregateMap, amendedMatchedAggregateMap, f.resultVal)
	return ResultCompensationOfValue(amended)
}

// ApplyCompensationForResult applies this compensation by translating
// the result value through the translation map. Returns the
// compensated result value.
// Ports Java's ResultCompensationFunction.applyCompensationForResult.
func (f *ResultCompensationFunction) ApplyCompensationForResult(tm TranslationMap) values.Value {
	if f == nil || f.resultVal == nil {
		return nil
	}
	if tm == nil || tm.DefinesOnlyIdentities() {
		return f.resultVal
	}
	return translateValueCorrelations(f.resultVal, tm)
}

// ---------------------------------------------------------------------------
// ForMatchCompensation — the main compensation implementation
// ---------------------------------------------------------------------------

// ForMatchCompensation is the primary compensation implementation for
// matches based on query predicates. It tracks matched/unmatched
// quantifiers, predicate compensation, result compensation, and
// group-by mappings.
//
// Ports Java's Compensation.ForMatch (which implements
// Compensation.WithSelectCompensation).
type ForMatchCompensation struct {
	impossible               bool
	childCompensation        Compensation
	predicateCompensationMap *PredicateCompensationMap
	matchedQuantifiers       []expressions.Quantifier
	unmatchedQuantifiers     []expressions.Quantifier
	compensatedAliases       map[values.CorrelationIdentifier]struct{}
	resultCompensationFn     *ResultCompensationFunction
	groupByMappings          *GroupByMappings

	// Lazily computed set of unmatched ForEach quantifiers.
	unmatchedForEachQuantifiers []expressions.Quantifier
	forEachComputed             bool
}

// NewForMatchCompensation constructs a ForMatchCompensation.
//
// Mirrors Java's Compensation.ForMatch constructor. All collection
// fields are defensively copied.
func NewForMatchCompensation(
	impossible bool,
	childCompensation Compensation,
	predicateCompensationMap *PredicateCompensationMap,
	matchedQuantifiers []expressions.Quantifier,
	unmatchedQuantifiers []expressions.Quantifier,
	compensatedAliases map[values.CorrelationIdentifier]struct{},
	resultCompensationFn *ResultCompensationFunction,
	groupByMappings *GroupByMappings,
) *ForMatchCompensation {
	// Defensive copies.
	mq := make([]expressions.Quantifier, len(matchedQuantifiers))
	copy(mq, matchedQuantifiers)

	uq := make([]expressions.Quantifier, len(unmatchedQuantifiers))
	copy(uq, unmatchedQuantifiers)

	ca := make(map[values.CorrelationIdentifier]struct{}, len(compensatedAliases))
	for k, v := range compensatedAliases {
		ca[k] = v
	}

	return &ForMatchCompensation{
		impossible:               impossible,
		childCompensation:        childCompensation,
		predicateCompensationMap: predicateCompensationMap,
		matchedQuantifiers:       mq,
		unmatchedQuantifiers:     uq,
		compensatedAliases:       ca,
		resultCompensationFn:     resultCompensationFn,
		groupByMappings:          groupByMappings,
	}
}

// IsNeeded reports whether this compensation must be applied. Mirrors
// Java's WithSelectCompensation.isNeeded() default method.
func (c *ForMatchCompensation) IsNeeded() bool {
	return c.childCompensation.IsNeeded() ||
		len(c.GetUnmatchedForEachQuantifiers()) > 0 ||
		!c.predicateCompensationMap.IsEmpty() ||
		c.resultCompensationFn.IsNeeded()
}

// IsImpossible reports whether this compensation is infeasible.
func (c *ForMatchCompensation) IsImpossible() bool {
	return c.impossible
}

// IsNeededForFiltering reports whether this compensation needs to be
// applied for correct filtering. Mirrors Java's
// WithSelectCompensation.isNeededForFiltering() default method.
func (c *ForMatchCompensation) IsNeededForFiltering() bool {
	return c.childCompensation.IsNeededForFiltering() ||
		len(c.GetUnmatchedForEachQuantifiers()) > 0 ||
		!c.predicateCompensationMap.IsEmpty()
}

// IsFinalNeeded reports whether final result-shape compensation is
// needed. Mirrors Java's WithSelectCompensation.isFinalNeeded()
// default method.
func (c *ForMatchCompensation) IsFinalNeeded() bool {
	return c.resultCompensationFn.IsNeeded()
}

// CanBeDeferred reports whether this compensation can be combined with
// subsequent compensations further up the graph. Mirrors Java's
// Compensation.canBeDeferred() default (always returns true).
func (c *ForMatchCompensation) CanBeDeferred() bool {
	return true
}

// GetChildCompensation returns the child (inner) compensation.
func (c *ForMatchCompensation) GetChildCompensation() Compensation {
	return c.childCompensation
}

// GetMatchedQuantifiers returns the set of quantifiers that were
// matched during matching.
func (c *ForMatchCompensation) GetMatchedQuantifiers() []expressions.Quantifier {
	return c.matchedQuantifiers
}

// GetUnmatchedQuantifiers returns the set of quantifiers that were
// NOT matched during matching.
func (c *ForMatchCompensation) GetUnmatchedQuantifiers() []expressions.Quantifier {
	return c.unmatchedQuantifiers
}

// GetUnmatchedForEachQuantifiers returns the subset of unmatched
// quantifiers that are ForEach quantifiers. The result is lazily
// computed and cached.
//
// Mirrors Java's ForMatch.computeUnmatchedForEachQuantifiers() with
// Suppliers.memoize.
func (c *ForMatchCompensation) GetUnmatchedForEachQuantifiers() []expressions.Quantifier {
	if !c.forEachComputed {
		var result []expressions.Quantifier
		for _, q := range c.unmatchedQuantifiers {
			if q.Kind() == expressions.QuantifierForEach {
				result = append(result, q)
			}
		}
		c.unmatchedForEachQuantifiers = result
		c.forEachComputed = true
	}
	return c.unmatchedForEachQuantifiers
}

// GetCompensatedAliases returns the set of aliases this compensation
// is responsible for. When applied, the caller can be assured that the
// match replacement plus this compensation can replace the quantifiers
// identified by these aliases.
func (c *ForMatchCompensation) GetCompensatedAliases() map[values.CorrelationIdentifier]struct{} {
	return c.compensatedAliases
}

// GetResultCompensationFunction returns the result compensation
// function.
func (c *ForMatchCompensation) GetResultCompensationFunction() *ResultCompensationFunction {
	return c.resultCompensationFn
}

// GetPredicateCompensationMap returns the predicate compensation map.
func (c *ForMatchCompensation) GetPredicateCompensationMap() *PredicateCompensationMap {
	return c.predicateCompensationMap
}

// GetGroupByMappings returns the group-by mappings.
func (c *ForMatchCompensation) GetGroupByMappings() *GroupByMappings {
	return c.groupByMappings
}

// String returns a human-readable representation of this compensation.
// Mirrors Java's ForMatch.toString().
func (c *ForMatchCompensation) String() string {
	if c.IsNeeded() {
		if c.IsImpossible() {
			return "needed; impossible"
		}
		return "needed; possible"
	}
	return "not needed; possible"
}

// ---------------------------------------------------------------------------
// Apply / Intersect
// ---------------------------------------------------------------------------

// Apply applies this compensation to a relational expression by
// wrapping it with residual predicate filters. Returns the original
// expression if no compensation is needed.
//
// translationMap maps matched correlation identifiers to realized
// (physical plan) identifiers. Compensated predicates are translated
// through this map before injection.
//
// Ports Java's Compensation.ForMatch.apply.
func (c *ForMatchCompensation) Apply(
	expr expressions.RelationalExpression,
	translationMap TranslationMap,
) expressions.RelationalExpression {
	if c.childCompensation != nil && c.childCompensation.IsNeededForFiltering() {
		if child, ok := c.childCompensation.(*ForMatchCompensation); ok {
			expr = child.Apply(expr, translationMap)
		}
	}

	compensatedPreds := c.predicateCompensationMap.ApplyCompensations(translationMap)
	if len(compensatedPreds) == 0 {
		return expr
	}

	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(expr))
	return expressions.NewLogicalFilterExpression(compensatedPreds, innerQ)
}

// ApplyFinal applies the result (shape) compensation by wrapping the
// expression in a map that reshapes the output. Returns the original
// expression if no result compensation is needed.
//
// Ports Java's Compensation.WithSelectCompensation.applyFinal.
func (c *ForMatchCompensation) ApplyFinal(
	expr expressions.RelationalExpression,
	translationMap TranslationMap,
) expressions.RelationalExpression {
	if !c.resultCompensationFn.IsNeeded() {
		return expr
	}
	resultVal := c.resultCompensationFn.ApplyCompensationForResult(translationMap)
	if resultVal == nil {
		return expr
	}
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(expr))
	return expressions.NewLogicalProjectionExpression([]values.Value{resultVal}, innerQ)
}

// ApplyAllNeeded applies both filter compensation (Apply) and result
// compensation (ApplyFinal) as needed. This is the primary entry point
// for applying compensation to a plan expression.
//
// Ports Java's Compensation.applyAllNeededCompensations.
func (c *ForMatchCompensation) ApplyAllNeeded(
	expr expressions.RelationalExpression,
	translationMap TranslationMap,
) expressions.RelationalExpression {
	if c.IsNeededForFiltering() {
		expr = c.Apply(expr, translationMap)
	}
	if c.IsFinalNeeded() {
		expr = c.ApplyFinal(expr, translationMap)
	}
	return expr
}

// Intersect combines this compensation with another by keeping only
// predicates that appear in both (common residuals for index
// intersections). Returns ImpossibleCompensation if the intersection
// is infeasible.
//
// Ports Java's Compensation.WithSelectCompensation.intersect.
func (c *ForMatchCompensation) Intersect(other *ForMatchCompensation) Compensation {
	// Phase 1: Handle edge cases.
	if c.IsImpossible() || other.IsImpossible() {
		return ImpossibleCompensation
	}
	if !c.IsNeeded() && !other.IsNeeded() {
		return NoCompensation
	}
	if !c.IsNeeded() {
		return other
	}
	if !other.IsNeeded() {
		return c
	}

	// Phase 2: Intersect child compensations.
	// Java: childCompensation.intersect(other.getChildCompensation())
	// Uses interface dispatch. In Go, ForMatchCompensation.Intersect
	// handles the impossible check; for non-ForMatch types, use
	// intersectTwo which handles the monoid identities.
	var intersectedChild Compensation
	if childFM, ok := c.childCompensation.(*ForMatchCompensation); ok {
		if otherChildFM, ok2 := other.childCompensation.(*ForMatchCompensation); ok2 {
			intersectedChild = childFM.Intersect(otherChildFM)
		} else {
			intersectedChild = intersectTwo(c.childCompensation, other.childCompensation)
		}
	} else {
		intersectedChild = intersectTwo(c.childCompensation, other.childCompensation)
	}
	if intersectedChild.IsImpossible() || !intersectedChild.CanBeDeferred() {
		return ImpossibleCompensation
	}

	// Phase 3: Merge GroupByMappings.
	// Matched groupings: union of both sides.
	newMatchedGroupings := c.groupByMappings.MatchedGroupingsMap().Copy()
	other.groupByMappings.MatchedGroupingsMap().Range(func(k, v values.Value) bool {
		if _, ok := newMatchedGroupings.Get(k); !ok {
			newMatchedGroupings.Put(k, v)
		}
		return true
	})

	// Matched aggregates: union of both sides.
	newMatchedAggregates := c.groupByMappings.MatchedAggregatesMap().Copy()
	other.groupByMappings.MatchedAggregatesMap().Range(func(k, v values.Value) bool {
		if _, ok := newMatchedAggregates.Get(k); !ok {
			newMatchedAggregates.Put(k, v)
		}
		return true
	})

	// Unmatched aggregates: filter out those that are now matched.
	newUnmatchedAggregates := NewCorrValueBiMap()
	unmatchedAggMap := c.groupByMappings.UnmatchedAggregatesMap()
	unmatchedAggMap.Range(func(k values.CorrelationIdentifier, v values.Value) bool {
		if _, matched := newMatchedAggregates.Get(v); !matched {
			newUnmatchedAggregates.Put(k, v)
		}
		return true
	})
	other.groupByMappings.UnmatchedAggregatesMap().Range(func(k values.CorrelationIdentifier, v values.Value) bool {
		if _, matched := newMatchedAggregates.Get(v); !matched {
			if _, alreadyIn := unmatchedAggMap.GetInverse(v); !alreadyIn {
				newUnmatchedAggregates.Put(k, v)
			}
		}
		return true
	})
	newGroupByMappings := NewGroupByMappings(newMatchedGroupings, newMatchedAggregates, newUnmatchedAggregates)

	// Phase 4: Result compensation.
	// Build the amended matched-aggregates map for Amend calls.
	amendedMatchedAggMap := make(map[values.Value]values.Value)
	newMatchedAggregates.Range(func(k, v values.Value) bool {
		amendedMatchedAggMap[k] = v
		return true
	})

	isImpossible := false
	var newResultFn *ResultCompensationFunction
	rcf := c.resultCompensationFn
	otherRcf := other.resultCompensationFn
	if !rcf.IsNeeded() && !otherRcf.IsNeeded() {
		newResultFn = NoResultCompensation()
	} else {
		if !rcf.IsNeeded() || !otherRcf.IsNeeded() {
			panic("Compensation.Intersect: both sides must have result compensation or neither")
		}
		newResultFn = rcf.Amend(unmatchedAggMap, amendedMatchedAggMap)
		isImpossible = isImpossible || newResultFn.IsImpossible()
	}

	// Phase 5: Predicate map intersection — keep only predicates in BOTH maps.
	otherPredMap := other.predicateCompensationMap
	var combinedKeys []predicates.QueryPredicate
	var combinedVals []PredicateCompensationFunc
	predKeys, predVals := c.predicateCompensationMap.Entries()
	for i, key := range predKeys {
		otherFn := otherPredMap.Get(key)
		if otherFn != nil {
			newFn := predVals[i].Amend(unmatchedAggMap, amendedMatchedAggMap)
			combinedKeys = append(combinedKeys, key)
			combinedVals = append(combinedVals, newFn)
			isImpossible = isImpossible || newFn.IsImpossible()
		}
	}
	combinedPredMap := NewPredicateCompensationMap(combinedKeys, combinedVals)

	// Phase 6: Early returns.
	if !intersectedChild.IsNeededForFiltering() && !newResultFn.IsNeeded() && combinedPredMap.IsEmpty() {
		return NoCompensation
	}
	if !newResultFn.IsNeeded() && combinedPredMap.IsEmpty() {
		return intersectedChild
	}

	// Phase 7: Quantifier intersection.
	// matchedQuantifiers = union of both sides.
	matchedSet := make(map[values.CorrelationIdentifier]expressions.Quantifier)
	for _, q := range c.matchedQuantifiers {
		matchedSet[q.GetAlias()] = q
	}
	for _, q := range other.matchedQuantifiers {
		matchedSet[q.GetAlias()] = q
	}
	intersectedMatched := make([]expressions.Quantifier, 0, len(matchedSet))
	for _, q := range matchedSet {
		intersectedMatched = append(intersectedMatched, q)
	}

	// unmatchedQuantifiers = intersection of both sides.
	otherUnmatchedSet := make(map[values.CorrelationIdentifier]struct{})
	for _, q := range other.unmatchedQuantifiers {
		otherUnmatchedSet[q.GetAlias()] = struct{}{}
	}
	var intersectedUnmatched []expressions.Quantifier
	unmatchedAliases := make(map[values.CorrelationIdentifier]struct{})
	for _, q := range c.unmatchedQuantifiers {
		if _, ok := otherUnmatchedSet[q.GetAlias()]; ok {
			intersectedUnmatched = append(intersectedUnmatched, q)
			unmatchedAliases[q.GetAlias()] = struct{}{}
		}
	}

	// Check if any combined predicate references an unmatched quantifier.
	if !isImpossible {
		for _, key := range combinedKeys {
			correlated := predicates.GetCorrelatedToOfPredicate(key)
			for alias := range correlated {
				if _, unmatched := unmatchedAliases[alias]; unmatched {
					isImpossible = true
					break
				}
			}
			if isImpossible {
				break
			}
		}
	}

	// Phase 8: Build derived compensation.
	return DerivedCompensation(
		intersectedChild,
		isImpossible,
		combinedPredMap,
		intersectedMatched,
		intersectedUnmatched,
		c.compensatedAliases, // both sides should be identical
		newResultFn,
		newGroupByMappings,
	)
}

// Union combines this compensation with another by merging predicate
// maps from both sides. Used when multiple partial matches combine
// their compensations (e.g. union of data access matches).
//
// Ports Java's Compensation.WithSelectCompensation.union.
func (c *ForMatchCompensation) Union(other *ForMatchCompensation) Compensation {
	if c.IsImpossible() || other.IsImpossible() {
		return ImpossibleCompensation
	}
	if !c.IsNeeded() && !other.IsNeeded() {
		return NoCompensation
	}
	if !c.IsNeeded() {
		return other
	}
	if !other.IsNeeded() {
		return c
	}

	// Check: union of matched quantifiers must have at most one ForEach.
	matchedSet := make(map[values.CorrelationIdentifier]expressions.Quantifier)
	for _, q := range c.matchedQuantifiers {
		matchedSet[q.GetAlias()] = q
	}
	for _, q := range other.matchedQuantifiers {
		matchedSet[q.GetAlias()] = q
	}
	forEachCount := 0
	var unionedMatched []expressions.Quantifier
	for _, q := range matchedSet {
		unionedMatched = append(unionedMatched, q)
		if q.Kind() == expressions.QuantifierForEach {
			forEachCount++
		}
	}
	if forEachCount > 1 {
		return ImpossibleCompensation
	}

	// If either side has unmatched ForEach quantifiers, union is impossible.
	if len(c.GetUnmatchedForEachQuantifiers()) > 0 || len(other.GetUnmatchedForEachQuantifiers()) > 0 {
		return ImpossibleCompensation
	}

	// Union child compensations.
	var unionedChild Compensation
	if childFM, ok := c.childCompensation.(*ForMatchCompensation); ok {
		if otherChildFM, ok2 := other.childCompensation.(*ForMatchCompensation); ok2 {
			unionedChild = childFM.Union(otherChildFM)
		} else {
			unionedChild = unionTwo(c.childCompensation, other.childCompensation)
		}
	} else {
		unionedChild = unionTwo(c.childCompensation, other.childCompensation)
	}
	if unionedChild.IsImpossible() || !unionedChild.CanBeDeferred() {
		return ImpossibleCompensation
	}

	// Result compensation: pick one side (same shape guaranteed).
	var newResultFn *ResultCompensationFunction
	rcf := c.resultCompensationFn
	otherRcf := other.resultCompensationFn
	if !rcf.IsNeeded() && !otherRcf.IsNeeded() {
		newResultFn = NoResultCompensation()
	} else {
		newResultFn = rcf
	}

	// Predicate map union: merge both sides. Java throws on duplicates;
	// Go uses identity (pointer) comparison so duplicates shouldn't happen
	// unless the same predicate pointer appears in both maps.
	var combinedKeys []predicates.QueryPredicate
	var combinedVals []PredicateCompensationFunc

	predKeys, predVals := c.predicateCompensationMap.Entries()
	combinedKeys = append(combinedKeys, predKeys...)
	combinedVals = append(combinedVals, predVals...)

	otherKeys, otherVals := other.predicateCompensationMap.Entries()
	existingSet := make(map[predicates.QueryPredicate]struct{})
	for _, k := range predKeys {
		existingSet[k] = struct{}{}
	}
	for i, k := range otherKeys {
		if _, dup := existingSet[k]; dup {
			return ImpossibleCompensation
		}
		combinedKeys = append(combinedKeys, k)
		combinedVals = append(combinedVals, otherVals[i])
	}
	combinedPredMap := NewPredicateCompensationMap(combinedKeys, combinedVals)

	// Early returns.
	if !unionedChild.IsNeededForFiltering() && !newResultFn.IsNeeded() && combinedPredMap.IsEmpty() {
		return NoCompensation
	}
	if !newResultFn.IsNeeded() && combinedPredMap.IsEmpty() {
		return unionedChild
	}

	// Merge compensated aliases.
	mergedAliases := make(map[values.CorrelationIdentifier]struct{})
	for k, v := range c.compensatedAliases {
		mergedAliases[k] = v
	}
	for k, v := range other.compensatedAliases {
		mergedAliases[k] = v
	}

	return DerivedCompensation(
		unionedChild,
		false,
		combinedPredMap,
		unionedMatched,
		nil, // unmatched is empty in union
		mergedAliases,
		newResultFn,
		EmptyGroupByMappings(),
	)
}

// ---------------------------------------------------------------------------
// Derived factory
// ---------------------------------------------------------------------------

// Derived creates a new ForMatchCompensation with this compensation as
// its child. This mirrors Java's Compensation.derived() default
// method.
func (c *ForMatchCompensation) Derived(
	impossible bool,
	predicateCompensationMap *PredicateCompensationMap,
	matchedQuantifiers []expressions.Quantifier,
	unmatchedQuantifiers []expressions.Quantifier,
	compensatedAliases map[values.CorrelationIdentifier]struct{},
	resultCompensationFn *ResultCompensationFunction,
	groupByMappings *GroupByMappings,
) *ForMatchCompensation {
	return NewForMatchCompensation(
		impossible,
		c, // this compensation becomes the child
		predicateCompensationMap,
		matchedQuantifiers,
		unmatchedQuantifiers,
		compensatedAliases,
		resultCompensationFn,
		groupByMappings,
	)
}

// DerivedCompensation creates a new ForMatchCompensation with `parent`
// as its child compensation. This is the package-level equivalent of
// Java's Compensation.derived() default method, usable with any
// Compensation (not just ForMatchCompensation).
func DerivedCompensation(
	parent Compensation,
	impossible bool,
	predicateCompensationMap *PredicateCompensationMap,
	matchedQuantifiers []expressions.Quantifier,
	unmatchedQuantifiers []expressions.Quantifier,
	compensatedAliases map[values.CorrelationIdentifier]struct{},
	resultCompensationFn *ResultCompensationFunction,
	groupByMappings *GroupByMappings,
) *ForMatchCompensation {
	// Java uses Verify.verify here (crashes on violation). Go returns
	// an impossible compensation instead of panicking — matches the
	// "never panic in library code" principle while preserving the
	// invariant semantics (an impossible compensation is never applied).
	if !impossible &&
		len(unmatchedQuantifiers) == 0 &&
		predicateCompensationMap.IsEmpty() &&
		!resultCompensationFn.IsNeeded() &&
		!parent.IsNeededForFiltering() {
		impossible = true
	}

	return NewForMatchCompensation(
		impossible,
		parent,
		predicateCompensationMap,
		matchedQuantifiers,
		unmatchedQuantifiers,
		compensatedAliases,
		resultCompensationFn,
		groupByMappings,
	)
}

// ---------------------------------------------------------------------------
// Compile-time interface satisfaction
// ---------------------------------------------------------------------------

var (
	_ Compensation = noCompensation{}
	_ Compensation = impossibleCompensation{}
	_ Compensation = (*ForMatchCompensation)(nil)
)
