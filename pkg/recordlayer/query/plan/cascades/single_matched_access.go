package cascades

import (
	"fmt"
	"strings"
	"sync"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// SingleMatchedAccess wraps a PartialMatch with its computed
// Compensation and metadata about how it can be used for data access.
// It is a value object produced during the prepareMatchesAndCompensations
// phase of AbstractDataAccessRule.
//
// Ports Java's
// com.apple.foundationdb.record.query.plan.cascades.rules.AbstractDataAccessRule.SingleMatchedAccess.
type SingleMatchedAccess struct {
	partialMatch                 PartialMatch
	compensation                 Compensation
	candidateTopAlias            values.CorrelationIdentifier
	reverseScanOrder             bool
	topToTopTranslationMap       TranslationMap
	satisfyingRequestedOrderings []*properties.RequestedOrdering

	// Lazily computed pulled-up group-by mappings. Java uses
	// Suppliers.memoize with adjustGroupByMappings(Quantifier.current(),
	// partialMatch.getCandidateRef().get()). Go calls
	// AdjustGroupByMappings with values.CurrentAlias (matching Java's
	// Quantifier.current()) so the pulled-up values carry the canonical
	// alias used by downstream aggregate index matching.
	pulledUpGroupByMappingsOnce sync.Once
	pulledUpGroupByMappings     *GroupByMappings
}

// NewSingleMatchedAccess constructs a SingleMatchedAccess. The
// satisfyingRequestedOrderings slice is defensively copied.
//
// Mirrors Java's SingleMatchedAccess constructor.
func NewSingleMatchedAccess(
	partialMatch PartialMatch,
	compensation Compensation,
	candidateTopAlias values.CorrelationIdentifier,
	reverseScanOrder bool,
	topToTopTranslationMap TranslationMap,
	satisfyingRequestedOrderings []*properties.RequestedOrdering,
) *SingleMatchedAccess {
	// Defensive copy of orderings.
	orderings := make([]*properties.RequestedOrdering, len(satisfyingRequestedOrderings))
	copy(orderings, satisfyingRequestedOrderings)

	return &SingleMatchedAccess{
		partialMatch:                 partialMatch,
		compensation:                 compensation,
		candidateTopAlias:            candidateTopAlias,
		reverseScanOrder:             reverseScanOrder,
		topToTopTranslationMap:       topToTopTranslationMap,
		satisfyingRequestedOrderings: orderings,
	}
}

// GetPartialMatch returns the partial match this access was derived from.
func (s *SingleMatchedAccess) GetPartialMatch() PartialMatch {
	return s.partialMatch
}

// GetCompensation returns the compensation computed for this match.
func (s *SingleMatchedAccess) GetCompensation() Compensation {
	return s.compensation
}

// GetCandidateTopAlias returns the correlation identifier for the
// candidate's top-level quantifier.
func (s *SingleMatchedAccess) GetCandidateTopAlias() values.CorrelationIdentifier {
	return s.candidateTopAlias
}

// IsReverseScanOrder reports whether the scan should be in reverse order.
func (s *SingleMatchedAccess) IsReverseScanOrder() bool {
	return s.reverseScanOrder
}

// GetTopToTopTranslationMap returns the translation map between query
// and candidate top-level aliases.
func (s *SingleMatchedAccess) GetTopToTopTranslationMap() TranslationMap {
	return s.topToTopTranslationMap
}

// GetSatisfyingRequestedOrderings returns the set of requested orderings
// that this access satisfies.
func (s *SingleMatchedAccess) GetSatisfyingRequestedOrderings() []*properties.RequestedOrdering {
	return s.satisfyingRequestedOrderings
}

// GetPulledUpGroupByMappingsForOrdering returns the pulled-up group-by
// mappings, lazily computed on first access. Mirrors Java's
// getPulledUpGroupByMappingsForOrdering().
func (s *SingleMatchedAccess) GetPulledUpGroupByMappingsForOrdering() *GroupByMappings {
	s.pulledUpGroupByMappingsOnce.Do(func() {
		s.pulledUpGroupByMappings = s.computePulledUpGroupByMappings()
	})
	return s.pulledUpGroupByMappings
}

// computePulledUpGroupByMappings computes the pulled-up group-by
// mappings by adjusting the match info's GroupByMappings through the
// candidate's result value. Ports Java's
// matchInfo.adjustGroupByMappings(Quantifier.current(),
// partialMatch.getCandidateRef().get()).
func (s *SingleMatchedAccess) computePulledUpGroupByMappings() *GroupByMappings {
	gbm := s.partialMatch.GetMatchInfo().GetGroupByMappings()
	candidateRef := s.partialMatch.GetCandidateRef()
	if candidateRef == nil {
		return gbm
	}
	candidateExpr, single := onlyReferenceMember(candidateRef)
	if !single {
		// Java Reference.get() requires a singleton. Picking Go's first
		// member would make aggregate ordering depend on insertion order
		// when alternatives expose different result values.
		return EmptyGroupByMappings()
	}
	if candidateExpr.GetResultValue() == nil {
		return gbm
	}
	adjusted, ok := AdjustGroupByMappings(
		gbm,
		values.CurrentAlias,
		candidateExpr,
	)
	if !ok {
		// Ambiguous group metadata cannot safely contribute aggregate
		// ordering. Returning empty mappings is the bounded optimization
		// miss for this derived property.
		return EmptyGroupByMappings()
	}
	return adjusted
}

// String returns a human-readable representation mirroring Java's
// SingleMatchedAccess.toString(). Note: Java's label for reverseScanOrder
// is inverted (true -> "forward", false -> "reverse"); we match that
// exactly.
func (s *SingleMatchedAccess) String() string {
	scanLabel := "reverse"
	if s.reverseScanOrder {
		scanLabel = "forward"
	}
	var orderings []string
	for _, o := range s.satisfyingRequestedOrderings {
		orderings = append(orderings, fmt.Sprintf("%v", o))
	}
	return fmt.Sprintf("[%v, %v, %s, %s, [%s]]",
		s.partialMatch, s.compensation,
		s.candidateTopAlias.Name(), scanLabel,
		strings.Join(orderings, ", "))
}
