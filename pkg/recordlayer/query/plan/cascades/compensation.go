package cascades

import (
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
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
// Methods that depend on types not yet ported (apply, applyFinal,
// intersect, union) are omitted and will be added when their
// dependencies land.
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

// ---------------------------------------------------------------------------
// Placeholder types for ForMatch dependencies
// ---------------------------------------------------------------------------

// PredicateCompensationMap maps query predicates to compensation
// functions. Placeholder — full port pending; will hold
// LinkedIdentityMap<QueryPredicate, PredicateCompensationFunction>.
type PredicateCompensationMap struct {
	// empty indicates whether the map has entries. Used to drive
	// IsNeeded/IsNeededForFiltering without full predicate support.
	entries int
}

// NewPredicateCompensationMap creates a PredicateCompensationMap.
// Pass the number of entries to indicate whether compensation
// predicates exist.
func NewPredicateCompensationMap(entries int) *PredicateCompensationMap {
	return &PredicateCompensationMap{entries: entries}
}

// EmptyPredicateCompensationMap returns an empty predicate
// compensation map.
func EmptyPredicateCompensationMap() *PredicateCompensationMap {
	return &PredicateCompensationMap{entries: 0}
}

// IsEmpty reports whether the map has no entries.
func (m *PredicateCompensationMap) IsEmpty() bool {
	return m == nil || m.entries == 0
}

// Len returns the number of entries in the map.
func (m *PredicateCompensationMap) Len() int {
	if m == nil {
		return 0
	}
	return m.entries
}

// ResultCompensationFunction handles final result shape
// transformation. Placeholder — full port pending.
//
// Ports Java's PredicateMultiMap.ResultCompensationFunction.
type ResultCompensationFunction struct {
	needed     bool
	impossible bool
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
	// Verify preconditions (mirrors Java's Verify.verify in derived()).
	if !impossible &&
		len(unmatchedQuantifiers) == 0 &&
		predicateCompensationMap.IsEmpty() &&
		!resultCompensationFn.IsNeeded() &&
		!parent.IsNeededForFiltering() {
		panic(fmt.Sprintf("DerivedCompensation: at least one of impossible, unmatched quantifiers, predicate compensation, result compensation, or child filtering must be needed"))
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
