package cascades

import (
	"sort"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// ---------------------------------------------------------------------------
// Abstract data access utilities
// ---------------------------------------------------------------------------
//
// This file ports the non-abstract helper methods from Java's
// AbstractDataAccessRule as package-level functions. Concrete rules
// (AggregateDataAccessRule, future WithPrimaryKeyDataAccessRule) call
// these directly -- the Go equivalent of extending the abstract class.
//
// Ports Java's:
//   - prepareMatchesAndCompensations
//   - maximumCoverageMatches
//   - createScansForMatches
//   - dataAccessForMatchPartition
//
// The seed implementations are architecturally correct but omit the
// complex Pareto filtering, full compensation computation, and detailed
// ordering satisfaction checking -- those are refinement work.

// IntersectorFunc is the function type for computing intersections of
// multiple data accesses. Concrete rules provide their own intersection
// logic through this callback, replacing Java's abstract method
// createIntersectionAndCompensation.
type IntersectorFunc func(
	accesses []Vectored[*SingleMatchedAccess],
	requestedOrderings []*RequestedOrdering,
) *IntersectionResult

// PrepareMatchesAndCompensations compensates and sorts partial matches
// by coverage (descending bound predicate count). For each
// PartialMatch:
//   - assigns a unique candidateTopAlias
//   - computes compensation (seed: NoCompensation)
//   - computes satisfying orderings (seed: all orderings satisfy)
//   - creates a forward-scan SingleMatchedAccess with empty translation
//
// Returns the accesses sorted by coverage (highest first).
//
// Ports Java's AbstractDataAccessRule.prepareMatchesAndCompensations.
func PrepareMatchesAndCompensations(
	partialMatches []PartialMatch,
	requestedOrderings []*RequestedOrdering,
	_ PlanContext,
) []*SingleMatchedAccess {
	if len(partialMatches) == 0 {
		return nil
	}

	result := make([]*SingleMatchedAccess, 0, len(partialMatches))
	for _, pm := range partialMatches {
		candidateTopAlias := values.UniqueCorrelationIdentifier()

		var comp Compensation
		if pmi, ok := pm.(*PartialMatchImpl); ok {
			comp = pmi.CompensateCompleteMatch(nil, candidateTopAlias)
		} else {
			comp = NoCompensation
		}

		// Seed: all requested orderings are satisfied. The full
		// satisfiesAnyRequestedOrderings logic checks ordering parts
		// against matched ordering parts; deferred to refinement.
		satisfying := make([]*RequestedOrdering, len(requestedOrderings))
		copy(satisfying, requestedOrderings)

		access := NewSingleMatchedAccess(
			pm,
			comp,
			candidateTopAlias,
			false, // forward scan
			EmptyTranslationMap(),
			satisfying,
		)
		result = append(result, access)
	}

	// Sort by bound predicate count descending (maximum coverage first).
	// In the seed, we use the number of matched ordering parts as a proxy
	// for "bound predicate count" since getBoundPlaceholders is not yet
	// on the PartialMatch interface. This preserves the Java sort
	// contract's intent: higher-coverage matches first.
	sort.SliceStable(result, func(i, j int) bool {
		iCount := len(result[i].GetPartialMatch().GetMatchInfo().GetMatchedOrderingParts())
		jCount := len(result[j].GetPartialMatch().GetMatchInfo().GetMatchedOrderingParts())
		return iCount > jCount
	})

	return result
}

// MaximumCoverageMatches eliminates PartialMatches whose coverage is
// entirely contained in other matches, then wraps survivors in
// Vectored with ascending position indices.
//
// Seed: no Pareto filtering -- every match is kept. Refinement will
// add the findContainingAccess logic that prunes dominated matches
// within the same MatchCandidate.
//
// Ports Java's AbstractDataAccessRule.maximumCoverageMatches.
func MaximumCoverageMatches(
	partialMatches []PartialMatch,
	requestedOrderings []*RequestedOrdering,
	ctx PlanContext,
) []Vectored[*SingleMatchedAccess] {
	accesses := PrepareMatchesAndCompensations(partialMatches, requestedOrderings, ctx)
	if len(accesses) == 0 {
		return nil
	}

	// Seed: wrap every access -- no containment pruning.
	result := make([]Vectored[*SingleMatchedAccess], len(accesses))
	for i, access := range accesses {
		result[i] = NewVectored(access, i)
	}
	return result
}

// CreateScansForMatches converts each SingleMatchedAccess into a
// physical scan plan by calling MatchCandidate.ToScanPlan with the
// bound parameter prefix and reverse flag.
//
// Returns a map from PartialMatch to RecordQueryPlan. The map uses
// the PartialMatch identity (pointer equality) as key.
//
// Ports Java's AbstractDataAccessRule.createScansForMatches.
func CreateScansForMatches(
	accesses []Vectored[*SingleMatchedAccess],
	_ PlanContext,
) map[PartialMatch]plans.RecordQueryPlan {
	result := make(map[PartialMatch]plans.RecordQueryPlan, len(accesses))
	for _, v := range accesses {
		access := v.Value
		pm := access.GetPartialMatch()
		candidate := pm.GetMatchCandidate()

		// Compute the bound parameter prefix from the match info's
		// parameter binding map.
		matchInfo := pm.GetMatchInfo()
		regularInfo := matchInfo.GetRegularMatchInfo()
		bindings := regularInfo.GetParameterBindingMap()
		prefix := candidate.ComputeBoundParameterPrefixMap(bindings)

		plan := candidate.ToScanPlan(prefix, access.IsReverseScanOrder())
		result[pm] = plan
	}
	return result
}

// DataAccessForMatchPartition orchestrates data access planning from
// a collection of PartialMatches. This is the main entry point that
// concrete rules call.
//
// Steps:
//  1. Compute maximum-coverage matches (with Pareto filtering)
//  2. Create scan plans for each viable match
//  3. For single match: wrap as expression with compensation
//  4. For multiple matches: try intersections via the provided
//     intersector callback
//
// The intersector is a function parameter so concrete rules can
// provide their own intersection logic (replaces Java's abstract
// createIntersectionAndCompensation method).
//
// Ports Java's AbstractDataAccessRule.dataAccessForMatchPartition.
func DataAccessForMatchPartition(
	requestedOrderings []*RequestedOrdering,
	partialMatches []PartialMatch,
	ctx PlanContext,
	intersector IntersectorFunc,
) []expressions.RelationalExpression {
	if len(partialMatches) == 0 {
		return nil
	}

	// Step 1: maximum coverage matches.
	bestMatches := MaximumCoverageMatches(partialMatches, requestedOrderings, ctx)
	if len(bestMatches) == 0 {
		return nil
	}

	// Step 2: create scan plans.
	scanMap := CreateScansForMatches(bestMatches, ctx)

	// Step 3: for each match, apply compensation and collect expressions.
	var resultExprs []expressions.RelationalExpression
	for _, v := range bestMatches {
		access := v.Value
		pm := access.GetPartialMatch()
		plan, ok := scanMap[pm]
		if !ok {
			continue
		}

		comp := access.GetCompensation()
		if comp.IsImpossible() {
			continue
		}

		// Wrap the scan plan as a RelationalExpression. In the full
		// implementation this would go through
		// applyCompensationForSingleDataAccessMaybe which applies the
		// compensation chain. For the seed, we wrap the plan directly
		// since compensation is NoCompensation.
		expr := &scanPlanExpression{plan: plan}
		resultExprs = append(resultExprs, expr)
	}

	// Step 4: if multiple matches and an intersector is provided, try
	// intersections.
	if len(bestMatches) > 1 && intersector != nil {
		intersectionResult := intersector(bestMatches, requestedOrderings)
		if intersectionResult != nil && intersectionResult.IsViable() {
			resultExprs = append(resultExprs, intersectionResult.GetExpressions()...)
		}
	}

	return resultExprs
}

// ---------------------------------------------------------------------------
// scanPlanExpression — wraps a RecordQueryPlan as a RelationalExpression
// ---------------------------------------------------------------------------

// scanPlanExpression is a thin wrapper that adapts a RecordQueryPlan to
// the RelationalExpression interface. Used by DataAccessForMatchPartition
// to yield scan plans as expressions into the memo.
//
// This mirrors the role of Java's physicalPlanExpression wrappers but is
// deliberately minimal for the seed -- the full wrapper hierarchy
// (physicalIndexScanWrapper etc.) exists in physical_wrapper.go and
// handles the real planner flow. This type is used only by the abstract
// data access utilities when they need to return expressions.
type scanPlanExpression struct {
	plan plans.RecordQueryPlan
}

func (s *scanPlanExpression) GetResultValue() values.Value {
	return values.NewNullValue(values.UnknownType)
}

func (s *scanPlanExpression) GetQuantifiers() []expressions.Quantifier {
	return nil
}

func (s *scanPlanExpression) CanCorrelate() bool  { return false }
func (s *scanPlanExpression) ChildrenAsSet() bool { return false }
func (s *scanPlanExpression) HashCodeWithoutChildren() uint64 {
	return s.plan.HashCodeWithoutChildren()
}

func (s *scanPlanExpression) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return nil
}

func (s *scanPlanExpression) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*scanPlanExpression)
	if !ok {
		return false
	}
	return s.plan.EqualsWithoutChildren(o.plan)
}

func (s *scanPlanExpression) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return s
}

// GetRecordQueryPlan returns the underlying plan.
func (s *scanPlanExpression) GetRecordQueryPlan() plans.RecordQueryPlan {
	return s.plan
}

// compile-time check
var _ expressions.RelationalExpression = (*scanPlanExpression)(nil)
