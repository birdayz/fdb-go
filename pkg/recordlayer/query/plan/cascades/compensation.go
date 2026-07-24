package cascades

import (
	"sync"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TranslationMapFunc maps a realized base-quantifier alias to a TranslationMap
// that rebases compensated predicates/result values from the candidate's top
// alias to that realized alias. Ports Java's
// Function<CorrelationIdentifier, TranslationMap> passed to
// Compensation.apply / applyFinal / applyAllNeededCompensations
// (AbstractDataAccessRule: realizedAlias -> TranslationMap.ofAliases(candidateTopAlias, realizedAlias)).
type TranslationMapFunc func(realizedAlias values.CorrelationIdentifier) TranslationMap

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
// ForMatchCompensation implements Intersect, Union, Apply, and ApplyFinal.
type Compensation interface {
	// IsNeeded reports whether this compensation must be applied.
	// A ForMatch compensation can also report false when all of its
	// components are empty.
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

	// The ADJUSTED map, not GetRegularMatchInfo().GetMaxMatchMap(): for an
	// adjusted match (MatchableSort re-anchor) the wrapper's map is the
	// child's map re-expressed through the sort level (AdjustMaybe), and the
	// root pull-up's pull-through lives in that same level's namespace.
	// Reading the underlying regular info's map here fed the pull-up a
	// query value one candidate level too deep — the MaxMatchMap could not
	// bridge the level's alias and EVERY adjusted match's compensation
	// collapsed to Impossible (Java reads matchInfo.getMaxMatchMap() on the
	// wrapper, Compensation.java:466).
	mmm := matchInfo.GetMaxMatchMap()
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
	// Ordinary residual compensation treats NoCompensation as absorbing.
	// Primary-key distinct is a cardinality obligation, however, and combines
	// with OR: a leg that needs it cannot lose it merely because the other leg
	// has no filter or result residual.
	if !a.IsNeeded() {
		return primaryKeyDistinctOnlyCompensation(b)
	}
	if !b.IsNeeded() {
		return primaryKeyDistinctOnlyCompensation(a)
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

// primaryKeyDistinctOnlyCompensation strips filtering and result residuals
// while retaining cardinality-correction obligations at their original match
// levels. This is the distinct-aware result of intersecting a compensation
// with NoCompensation.
func primaryKeyDistinctOnlyCompensation(compensation Compensation) Compensation {
	forMatch, ok := compensation.(*ForMatchCompensation)
	if !ok {
		return NoCompensation
	}

	child := primaryKeyDistinctOnlyCompensation(forMatch.childCompensation)
	if !forMatch.requiresPrimaryKeyDistinct {
		return child
	}

	return NewForMatchCompensationWithPrimaryKeyDistinct(
		child.IsImpossible(),
		child,
		EmptyPredicateCompensationMap(),
		forMatch.matchedQuantifiers,
		nil,
		forMatch.compensatedAliases,
		NoResultCompensation(),
		EmptyGroupByMappings(),
		true,
	)
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

// unionResultCompensation combines the two legs' result-compensation functions
// for a ForMatchCompensation.Union, mirroring Java Compensation.java:617-624:
//   - neither needed → no result compensation;
//   - BOTH needed → either serves (the legs share the result shape); Java picks
//     the first after Verify.verify asserts both;
//   - exactly one needed → the invariant Java asserts against. Return ok=false
//     so the caller declines the union (ImpossibleCompensation) instead of
//     silently emitting the wrong output shape — the fix for RFC-189 C3
//     (finding 12c). Fail-closed rather than panic (library code never panics).
func unionResultCompensation(rcf, otherRcf *ResultCompensationFunction) (*ResultCompensationFunction, bool) {
	switch {
	case !rcf.IsNeeded() && !otherRcf.IsNeeded():
		return NoResultCompensation(), true
	case rcf.IsNeeded() && otherRcf.IsNeeded():
		return rcf, true
	default:
		return nil, false
	}
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
	impossible                 bool
	childCompensation          Compensation
	predicateCompensationMap   *PredicateCompensationMap
	matchedQuantifiers         []expressions.Quantifier
	unmatchedQuantifiers       []expressions.Quantifier
	compensatedAliases         map[values.CorrelationIdentifier]struct{}
	resultCompensationFn       *ResultCompensationFunction
	groupByMappings            *GroupByMappings
	requiresPrimaryKeyDistinct bool

	// Lazily computed set of unmatched ForEach quantifiers (thread-safe).
	unmatchedForEachQuantifiers []expressions.Quantifier
	forEachOnce                 sync.Once
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
	return NewForMatchCompensationWithPrimaryKeyDistinct(
		impossible,
		childCompensation,
		predicateCompensationMap,
		matchedQuantifiers,
		unmatchedQuantifiers,
		compensatedAliases,
		resultCompensationFn,
		groupByMappings,
		false,
	)
}

// NewForMatchCompensationWithPrimaryKeyDistinct constructs a compensation
// that can additionally require primary-key cardinality correction. The
// ordinary constructor remains source-compatible and defaults this obligation
// to false.
func NewForMatchCompensationWithPrimaryKeyDistinct(
	impossible bool,
	childCompensation Compensation,
	predicateCompensationMap *PredicateCompensationMap,
	matchedQuantifiers []expressions.Quantifier,
	unmatchedQuantifiers []expressions.Quantifier,
	compensatedAliases map[values.CorrelationIdentifier]struct{},
	resultCompensationFn *ResultCompensationFunction,
	groupByMappings *GroupByMappings,
	requiresPrimaryKeyDistinct bool,
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

	c := &ForMatchCompensation{
		impossible:                 impossible,
		childCompensation:          childCompensation,
		predicateCompensationMap:   predicateCompensationMap,
		matchedQuantifiers:         mq,
		unmatchedQuantifiers:       uq,
		compensatedAliases:         ca,
		resultCompensationFn:       resultCompensationFn,
		groupByMappings:            groupByMappings,
		requiresPrimaryKeyDistinct: requiresPrimaryKeyDistinct,
	}

	// A compensation that has to be applied must know the single base ForEach
	// alias it rebuilds itself on. Enforcing that centrally means every
	// composition path (derived, intersect, union) marks the match impossible
	// before a data-access caller considers it. Apply checks the invariant
	// again and reports failure; neither layer ever guesses an alias.
	//
	// The predicate is exactly the two paths that go on to ask for that alias.
	// The broader IsNeeded also covers a compensation needed only for a
	// CHILD's final shape, where this level never builds a base quantifier;
	// failing those closed would discard usable matches for nothing.
	if c.IsNeededForFiltering() || c.IsFinalNeeded() || c.requiresPrimaryKeyDistinct {
		if _, ok := c.MatchedForEachAliasMaybe(); !ok {
			c.impossible = true
		}
	}
	return c
}

// IsNeeded reports whether this compensation must be applied. Mirrors
// Java's WithSelectCompensation.isNeeded() default method.
func (c *ForMatchCompensation) IsNeeded() bool {
	return c.childCompensation.IsNeeded() ||
		len(c.GetUnmatchedForEachQuantifiers()) > 0 ||
		!c.predicateCompensationMap.IsEmpty() ||
		c.resultCompensationFn.IsNeeded() ||
		c.requiresPrimaryKeyDistinct
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
	c.forEachOnce.Do(func() {
		var result []expressions.Quantifier
		for _, q := range c.unmatchedQuantifiers {
			if q.Kind() == expressions.QuantifierForEach {
				result = append(result, q)
			}
		}
		c.unmatchedForEachQuantifiers = result
	})
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

// RequiresPrimaryKeyDistinct reports whether this match level must correct
// duplicate cardinality by primary key before applying its final result
// projection. It deliberately does not contribute to
// IsNeededForFiltering: duplicate elimination changes multiplicity, not the
// truth of an existential predicate.
func (c *ForMatchCompensation) RequiresPrimaryKeyDistinct() bool {
	return c.requiresPrimaryKeyDistinct
}

// MatchedForEachAliasMaybe returns the single base ForEach alias this
// compensation rebuilds itself on top of, and whether that alias is
// well-defined.
//
// Applying a compensation means re-introducing a base quantifier over the
// compensated expression and re-pointing the residual predicates and result
// value at it. That has an answer only when exactly one matched quantifier is
// a ForEach supplying the rows: with none there is no alias to rebuild on,
// with several there is no way to choose and the losers' correlations dangle.
// Java asserts this with Iterables.getOnlyElement and crashes; Go reports it
// so callers can drop the match instead of building on a guessed alias.
//
// The compensated aliases must also be exactly the matched ones. That pair of
// sets is what the compensation claims responsibility for; when they disagree
// the compensation is describing a different set of quantifiers than the one
// it is about to rebuild, and composing it (intersect, union) propagates the
// disagreement. Java verifies the equality before selecting the sole ForEach
// for the same reason.
func (c *ForMatchCompensation) MatchedForEachAliasMaybe() (values.CorrelationIdentifier, bool) {
	matchedAliases := make(map[values.CorrelationIdentifier]struct{}, len(c.matchedQuantifiers))
	for _, q := range c.matchedQuantifiers {
		matchedAliases[q.GetAlias()] = struct{}{}
	}
	// Java builds an alias-to-quantifier map here and rejects duplicate keys.
	// A collision is equally invalid in Go: two quantifiers with one alias
	// cannot both be reconstructed or addressed unambiguously.
	if len(matchedAliases) != len(c.matchedQuantifiers) {
		return values.CorrelationIdentifier{}, false
	}
	if len(matchedAliases) != len(c.compensatedAliases) {
		return values.CorrelationIdentifier{}, false
	}
	for alias := range c.compensatedAliases {
		if _, ok := matchedAliases[alias]; !ok {
			return values.CorrelationIdentifier{}, false
		}
	}

	var found values.CorrelationIdentifier
	count := 0
	for _, q := range c.matchedQuantifiers {
		if q.Kind() == expressions.QuantifierForEach {
			found = q.GetAlias()
			count++
		}
	}
	if count != 1 || found.IsZero() {
		return values.CorrelationIdentifier{}, false
	}
	return found, true
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

// isPreFinalNeeded reports whether this compensation chain has work that must
// happen before the top-level result projection. It is intentionally separate
// from IsNeededForFiltering: primary-key distinct changes multiplicity, so an
// existential owner must not mistake it for a truth-affecting filter, while an
// ordinary data-access consumer must still apply it.
func (c *ForMatchCompensation) isPreFinalNeeded() bool {
	return compensationIsPreFinalNeeded(c.childCompensation) ||
		c.isLocalFilteringNeeded() ||
		c.requiresPrimaryKeyDistinct
}

func (c *ForMatchCompensation) isLocalFilteringNeeded() bool {
	return len(c.GetUnmatchedForEachQuantifiers()) > 0 ||
		!c.predicateCompensationMap.IsEmpty()
}

func compensationIsPreFinalNeeded(compensation Compensation) bool {
	if compensation == nil {
		return false
	}
	if forMatch, ok := compensation.(*ForMatchCompensation); ok {
		return forMatch.isPreFinalNeeded()
	}
	return compensation.IsNeededForFiltering()
}

// ---------------------------------------------------------------------------
// Apply / Intersect
// ---------------------------------------------------------------------------

// Apply applies this compensation to a relational expression by wrapping it
// with residual predicate filters. It returns false when the compensation
// cannot be applied faithfully; callers must discard that match.
//
// translationMapFunc, given the realized base-quantifier alias, yields a
// TranslationMap that rebases compensated predicates from the candidate's
// top alias to the realized alias. The realized base quantifier is created
// with the matched query-side ForEach alias (matchedForEachAlias) — exactly
// as Java does — so the compensation expression flows under the SAME alias
// the surrounding query graph already correlates to. Allocating a fresh
// alias here instead orphans the access from outer correlations (the outer
// join's reference to the matched source no longer resolves), which is how
// a dual-correlation join collapsed to Fetch(<nil>) and returned 0 rows.
//
// Ports Java's Compensation.ForMatch.apply — full implementation
// including the else-branch (multi-join compensation with unmatched
// ForEach quantifiers pulled up into a new SelectExpression).
func (c *ForMatchCompensation) Apply(
	expr expressions.RelationalExpression,
	translationMapFunc TranslationMapFunc,
) (expressions.RelationalExpression, bool) {
	// Java verifies this precondition before touching the expression. In Go,
	// infeasibility is an expected planner branch, so report it instead of
	// panicking or silently returning an under-compensated expression.
	if c.IsImpossible() {
		return nil, false
	}

	// A cardinality-only child is not filtering compensation, but it still
	// has to run at its own match level before this level's residuals.
	if compensationIsPreFinalNeeded(c.childCompensation) {
		child, ok := c.childCompensation.(*ForMatchCompensation)
		if !ok {
			return nil, false
		}
		expr, ok = child.Apply(expr, translationMapFunc)
		if !ok {
			return nil, false
		}
	}

	if !c.isLocalFilteringNeeded() && !c.requiresPrimaryKeyDistinct {
		return expr, true
	}

	// Local filtering and distinct both rebuild the stream on the single
	// matched query-side ForEach alias. Validate that alias before making any
	// local change.
	matchedForEachAlias, ok := c.MatchedForEachAliasMaybe()
	if !ok {
		return nil, false
	}

	// matchedForEachAlias is the matched query-side ForEach alias (Java
	// getMatchedForEachAlias). Both the rebase translation map and the
	// realized base quantifier are keyed to it.
	var translationMap TranslationMap = EmptyTranslationMap()
	if !c.predicateCompensationMap.IsEmpty() && translationMapFunc != nil {
		translationMap = translationMapFunc(matchedForEachAlias)
	}

	compensatedPreds := c.predicateCompensationMap.ApplyCompensations(translationMap)

	// Collect correlations referenced by compensated predicates.
	compensatedCorrelations := make(map[values.CorrelationIdentifier]struct{})
	for _, pred := range compensatedPreds {
		for alias := range predicates.GetCorrelatedToOfPredicate(pred) {
			compensatedCorrelations[alias] = struct{}{}
		}
	}

	// Determine which quantifiers must be "pulled up" (re-introduced
	// into the compensation expression).
	var toBePulledUp []expressions.Quantifier

	// Matched existential quantifiers referenced by compensation
	// predicates must be pulled up (partial EXISTS match).
	for _, q := range c.matchedQuantifiers {
		if q.Kind() == expressions.QuantifierExistential {
			if _, referenced := compensatedCorrelations[q.GetAlias()]; referenced {
				toBePulledUp = append(toBePulledUp, q)
			}
		}
	}

	// Unmatched ForEach quantifiers affect cardinality — must be
	// retained. Unmatched existential quantifiers are pulled up only
	// if referenced by compensation predicates.
	for _, q := range c.unmatchedQuantifiers {
		if q.Kind() == expressions.QuantifierForEach {
			toBePulledUp = append(toBePulledUp, q)
		} else if q.Kind() == expressions.QuantifierExistential {
			if _, referenced := compensatedCorrelations[q.GetAlias()]; referenced {
				toBePulledUp = append(toBePulledUp, q)
			}
		}
	}

	if len(compensatedPreds) > 0 || len(toBePulledUp) > 0 {
		// Create the base quantifier over the compensated expression, reusing
		// the matched query-side ForEach alias (Java: Quantifier.forEach(ref,
		// matchedForEachAlias)). The residual predicates already reference this
		// alias for scan-record columns, and the surrounding graph correlates to
		// it, so reusing it keeps both linkages intact.
		newBaseQ := newCompensationBaseQuantifier(matchedForEachAlias, expr)

		if len(toBePulledUp) == 0 {
			// Then-branch: simple filter, no join needed.
			expr = expressions.NewLogicalFilterExpression(compensatedPreds, newBaseQ)
		} else {
			// Else-branch: build a SelectExpression that joins the base scan
			// with the pulled-up quantifiers and applies compensation predicates.
			builder := NewGraphExpansionBuilder()
			builder.AddQuantifier(newBaseQ)
			for _, q := range toBePulledUp {
				builder.AddQuantifier(q)
			}
			for _, pred := range compensatedPreds {
				builder.AddPredicate(pred)
			}
			expansion := builder.Build()
			sealed := expansion.Seal()
			expr = sealed.BuildSelectWithResultValue(newBaseQ.GetFlowedObjectValue())
		}
	}

	if c.requiresPrimaryKeyDistinct {
		// Distinct belongs after every child and local residual filter, but
		// before ApplyFinal reshapes the row and can hide its primary key.
		expr = expressions.NewRequiredLogicalUniqueExpression(
			newCompensationBaseQuantifier(matchedForEachAlias, expr),
		)
	}

	return expr, true
}

// ApplyFinal applies the result (shape) compensation by wrapping the
// expression in a SelectExpression with the translated result value. It
// returns false when a needed final compensation cannot be applied faithfully.
//
// Ports Java's Compensation.WithSelectCompensation.applyFinal which
// uses GraphExpansion.builder().addQuantifier(base).build()
// .buildSelectWithResultValue(resultValue).
func (c *ForMatchCompensation) ApplyFinal(
	expr expressions.RelationalExpression,
	translationMapFunc TranslationMapFunc,
) (expressions.RelationalExpression, bool) {
	if !c.resultCompensationFn.IsNeeded() {
		return expr, true
	}
	if c.IsImpossible() {
		return nil, false
	}
	matchedForEachAlias, ok := c.MatchedForEachAliasMaybe()
	if !ok {
		return nil, false
	}
	var translationMap TranslationMap = EmptyTranslationMap()
	if translationMapFunc != nil {
		translationMap = translationMapFunc(matchedForEachAlias)
	}
	resultVal := c.resultCompensationFn.ApplyCompensationForResult(translationMap)
	if resultVal == nil {
		return nil, false
	}
	// Reuse the matched query-side ForEach alias for the realized base
	// quantifier (Java: Quantifier.forEach(ref, matchedForEachAlias)), so the
	// translated result value (keyed to that alias) resolves against it.
	newBaseQ := newCompensationBaseQuantifier(matchedForEachAlias, expr)
	builder := NewGraphExpansionBuilder()
	builder.AddQuantifier(newBaseQ)
	expansion := builder.Build()
	sealed := expansion.Seal()
	return sealed.BuildSelectWithResultValue(resultVal), true
}

// ApplyAllNeeded applies both filter compensation (Apply) and result
// compensation (ApplyFinal) as needed. This is the primary entry point for
// applying compensation to a plan expression; false makes the data-access
// alternative non-viable.
//
// Ports Java's Compensation.applyAllNeededCompensations.
func (c *ForMatchCompensation) ApplyAllNeeded(
	expr expressions.RelationalExpression,
	translationMapFunc TranslationMapFunc,
) (expressions.RelationalExpression, bool) {
	if c.IsImpossible() {
		return nil, false
	}
	var ok bool
	if c.isPreFinalNeeded() {
		expr, ok = c.Apply(expr, translationMapFunc)
		if !ok {
			return nil, false
		}
	}
	if c.IsFinalNeeded() {
		expr, ok = c.ApplyFinal(expr, translationMapFunc)
		if !ok {
			return nil, false
		}
	}
	return expr, true
}

// newCompensationBaseQuantifier builds the ForEach quantifier that the
// compensation expression ranges over, on the matched query-side ForEach alias
// (Java Quantifier.forEach(ref, matchedForEachAlias)) so the compensated
// predicates and the surrounding query graph keep resolving against the same
// alias.
//
// There is deliberately no fresh-alias fallback. The alias is not a detail the
// compensation may choose — it is the name the rest of the graph already uses
// to reach these rows, so a substitute silently detaches them. Callers get the
// alias from MatchedForEachAliasMaybe and bail when it is not well-defined.
func newCompensationBaseQuantifier(matchedForEachAlias values.CorrelationIdentifier, expr expressions.RelationalExpression) expressions.Quantifier {
	ref := expressions.InitialOf(expr)
	if isPhysical(expr) {
		// Java's compensation memoizer receives a realized plan and memoizes it
		// as a FINAL child. Keeping a physical scan in the exploratory set
		// strands enforcers such as required LogicalUnique: its implementation
		// rule reads physical child partitions/properties, sees none, and the
		// fanout match loses to the fallback table scan. StagePlanned also
		// freezes the exact realized access under the compensation; later
		// exploration must not float the wrapper onto an unrelated sibling.
		ref = expressions.FinalOf(expr)
	}
	return expressions.NamedForEachQuantifier(matchedForEachAlias, ref)
}

// Intersect combines this compensation with another by keeping only
// predicates that appear in both (common residuals for index
// intersections). Returns ImpossibleCompensation if the intersection
// is infeasible.
//
// Ports Java's Compensation.WithSelectCompensation.intersect.
func (c *ForMatchCompensation) Intersect(other *ForMatchCompensation) Compensation {
	// Phase 1: Handle edge cases.
	//
	// NoCompensation is the absorbing element for intersection: if one side
	// needs nothing, there is no residual common to both sides. Do not inherit
	// either ForMatch's aggregate impossible flag here. An intersection can
	// legitimately discard a leg-local impossible residual and retain a
	// possible residual shared by both legs; the algorithm below recomputes
	// impossibility from exactly what survives.
	if !c.IsNeeded() {
		return primaryKeyDistinctOnlyCompensation(other)
	}
	if !other.IsNeeded() {
		return primaryKeyDistinctOnlyCompensation(c)
	}
	requiresPrimaryKeyDistinct := c.requiresPrimaryKeyDistinct ||
		other.requiresPrimaryKeyDistinct

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
			// Java: Verify.verify(both needed). Invariant violation —
			// return impossible instead of panicking.
			return ImpossibleCompensation
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
	if !compensationIsPreFinalNeeded(intersectedChild) &&
		!newResultFn.IsNeeded() &&
		combinedPredMap.IsEmpty() &&
		!requiresPrimaryKeyDistinct {
		return NoCompensation
	}
	if !newResultFn.IsNeeded() &&
		combinedPredMap.IsEmpty() &&
		!requiresPrimaryKeyDistinct {
		return intersectedChild
	}

	// Phase 7: Quantifier intersection.
	// matchedQuantifiers = union of both sides, INSERTION-ORDERED (Java
	// LinkedIdentitySet): ranging a map here leaked iteration order into
	// the compensation's quantifier list — downstream expression trees, and
	// with them plan identity, varied run to run.
	intersectedMatched := unionQuantifiersOrdered(c.matchedQuantifiers, other.matchedQuantifiers)

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

	// Phase 8: Build derived compensation. Both legs must carry the same alias
	// responsibility. Merging different sets manufactures a responsibility
	// neither leg proved and can make an invalid intersection look possible
	// merely because the union also matches the unioned quantifier set.
	if !aliasSetsEqual(c.compensatedAliases, other.compensatedAliases) {
		return ImpossibleCompensation
	}
	compensatedAliases := make(map[values.CorrelationIdentifier]struct{}, len(c.compensatedAliases))
	for k, v := range c.compensatedAliases {
		compensatedAliases[k] = v
	}
	return DerivedCompensationWithPrimaryKeyDistinct(
		intersectedChild,
		isImpossible,
		combinedPredMap,
		intersectedMatched,
		intersectedUnmatched,
		compensatedAliases,
		newResultFn,
		newGroupByMappings,
		requiresPrimaryKeyDistinct,
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
	requiresPrimaryKeyDistinct := c.requiresPrimaryKeyDistinct ||
		other.requiresPrimaryKeyDistinct

	// Check: union of matched quantifiers must have at most one ForEach.
	// Insertion-ordered union (Java LinkedIdentitySet) — see the
	// intersection path for why map iteration order must not leak here.
	unionedMatched := unionQuantifiersOrdered(c.matchedQuantifiers, other.matchedQuantifiers)
	forEachCount := 0
	for _, q := range unionedMatched {
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

	// Result compensation: both legs must share the result shape, so Java
	// (Compensation.java:617-624) ASSERTS both sides are needed (Verify.verify)
	// before picking one. Go previously picked c's rcf UNCONDITIONALLY — even
	// when only other's was needed — yielding the wrong output shape.
	newResultFn, ok := unionResultCompensation(c.resultCompensationFn, other.resultCompensationFn)
	if !ok {
		return ImpossibleCompensation
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
	if !compensationIsPreFinalNeeded(unionedChild) &&
		!newResultFn.IsNeeded() &&
		combinedPredMap.IsEmpty() &&
		!requiresPrimaryKeyDistinct {
		return NoCompensation
	}
	if !newResultFn.IsNeeded() &&
		combinedPredMap.IsEmpty() &&
		!requiresPrimaryKeyDistinct {
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

	return DerivedCompensationWithPrimaryKeyDistinct(
		unionedChild,
		false,
		combinedPredMap,
		unionedMatched,
		nil, // unmatched is empty in union
		mergedAliases,
		newResultFn,
		EmptyGroupByMappings(),
		requiresPrimaryKeyDistinct,
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
	return c.DerivedWithPrimaryKeyDistinct(
		impossible,
		predicateCompensationMap,
		matchedQuantifiers,
		unmatchedQuantifiers,
		compensatedAliases,
		resultCompensationFn,
		groupByMappings,
		false,
	)
}

// DerivedWithPrimaryKeyDistinct creates a derived compensation and explicitly
// records whether this new match level requires primary-key distinct.
func (c *ForMatchCompensation) DerivedWithPrimaryKeyDistinct(
	impossible bool,
	predicateCompensationMap *PredicateCompensationMap,
	matchedQuantifiers []expressions.Quantifier,
	unmatchedQuantifiers []expressions.Quantifier,
	compensatedAliases map[values.CorrelationIdentifier]struct{},
	resultCompensationFn *ResultCompensationFunction,
	groupByMappings *GroupByMappings,
	requiresPrimaryKeyDistinct bool,
) *ForMatchCompensation {
	return NewForMatchCompensationWithPrimaryKeyDistinct(
		impossible,
		c, // this compensation becomes the child
		predicateCompensationMap,
		matchedQuantifiers,
		unmatchedQuantifiers,
		compensatedAliases,
		resultCompensationFn,
		groupByMappings,
		requiresPrimaryKeyDistinct,
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
	return DerivedCompensationWithPrimaryKeyDistinct(
		parent,
		impossible,
		predicateCompensationMap,
		matchedQuantifiers,
		unmatchedQuantifiers,
		compensatedAliases,
		resultCompensationFn,
		groupByMappings,
		false,
	)
}

// DerivedCompensationWithPrimaryKeyDistinct is the explicit cardinality-aware
// form of DerivedCompensation. Existing callers retain the default false mode.
func DerivedCompensationWithPrimaryKeyDistinct(
	parent Compensation,
	impossible bool,
	predicateCompensationMap *PredicateCompensationMap,
	matchedQuantifiers []expressions.Quantifier,
	unmatchedQuantifiers []expressions.Quantifier,
	compensatedAliases map[values.CorrelationIdentifier]struct{},
	resultCompensationFn *ResultCompensationFunction,
	groupByMappings *GroupByMappings,
	requiresPrimaryKeyDistinct bool,
) *ForMatchCompensation {
	// Java uses Verify.verify here (crashes on violation). Go returns
	// an impossible compensation instead of panicking — matches the
	// "never panic in library code" principle while preserving the
	// invariant semantics (an impossible compensation is never applied).
	if !impossible &&
		len(unmatchedQuantifiers) == 0 &&
		predicateCompensationMap.IsEmpty() &&
		!resultCompensationFn.IsNeeded() &&
		!compensationIsPreFinalNeeded(parent) &&
		!requiresPrimaryKeyDistinct {
		impossible = true
	}

	return NewForMatchCompensationWithPrimaryKeyDistinct(
		impossible,
		parent,
		predicateCompensationMap,
		matchedQuantifiers,
		unmatchedQuantifiers,
		compensatedAliases,
		resultCompensationFn,
		groupByMappings,
		requiresPrimaryKeyDistinct,
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

// unionQuantifiersOrdered unions two quantifier lists preserving first-seen
// insertion order, deduplicating by alias — the Go analog of Java's
// LinkedIdentitySet union in Compensation.intersect/union. Determinism here
// is load-bearing: the result feeds compensation expression trees whose
// shape participates in plan identity.
func unionQuantifiersOrdered(a, b []expressions.Quantifier) []expressions.Quantifier {
	seen := make(map[values.CorrelationIdentifier]struct{}, len(a)+len(b))
	out := make([]expressions.Quantifier, 0, len(a)+len(b))
	for _, list := range [][]expressions.Quantifier{a, b} {
		for _, q := range list {
			if _, dup := seen[q.GetAlias()]; dup {
				continue
			}
			seen[q.GetAlias()] = struct{}{}
			out = append(out, q)
		}
	}
	return out
}
