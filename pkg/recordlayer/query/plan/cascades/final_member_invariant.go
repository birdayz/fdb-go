package cascades

import (
	"context"
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ExtractionVerificationReport describes the extraction-path invariant from
// RFC-224. VisitedReferences counts distinct canonical References reached by
// following the member extraction would select. DeadEnds counts reached
// References for which no physical selection exists. MultiFinalReferences is
// diagnostic reach: retaining more than one physical final is legal when each
// retained member is the winner for a required physical property.
//
// Violations contains both dead ends and coherence failures. In particular, a
// stamped winner must still belong to the Reference's final set, and every
// retained non-winner in a multi-final group must be licensed by one of the
// same physical-property partitions OptimizeGroup uses.
type ExtractionVerificationReport struct {
	Violations           []string
	VisitedReferences    int
	DeadEnds             int
	MultiFinalReferences int

	// These sets make the verifier/extractor agreement mutation-testable in
	// this package without exposing memo References as API. The exported
	// counters above are the stable reporting surface.
	visited  map[*expressions.Reference]struct{}
	selected map[*expressions.Reference][]expressions.RelationalExpression
	deadEnds map[*expressions.Reference]struct{}
	multi    map[*expressions.Reference]struct{}
	nilEnds  int
}

// VerifyExtractionIsUnambiguous walks exactly one selected member per reached
// Reference: a compatible stamped winner first, the selector's compatible best
// member next, and the cheapest compatible physical member as the fallback.
// Positional ordinal-layout requirements are derived from the selected parent
// and applied to the corresponding child edge before cost comparison, exactly
// as selector-based extraction does.
//
// The cardinality of FinalMembers is deliberately not an invariant. Go keeps
// one winner per required physical property because parents retain live memo
// edges. For multi-final groups this verifier instead reconstructs the legal
// OptimizeGroup keep set: the global winner, the cheapest member of every
// (distinct-records, stored-record) partition, the cheapest provider of every
// requested ordering, and the cheapest provider of every required ordinal
// layout. A survivor outside that set is a coherence violation; a legitimate
// property-retained alternative is not.
func VerifyExtractionIsUnambiguous(
	root *expressions.Reference,
	selector BestMemberSelector,
	stats properties.StatisticsProvider,
) ExtractionVerificationReport {
	if stats == nil {
		stats = properties.DefaultStatistics{}
	}
	v := extractionVerifier{
		selector: selector,
		stats:    stats,
		report: ExtractionVerificationReport{
			visited:  make(map[*expressions.Reference]struct{}),
			selected: make(map[*expressions.Reference][]expressions.RelationalExpression),
			deadEnds: make(map[*expressions.Reference]struct{}),
			multi:    make(map[*expressions.Reference]struct{}),
		},
		states: make(map[*expressions.Reference][]plans.OrdinalLayoutRequirement),
		stack:  make(map[*expressions.Reference]bool),
	}
	if planner, ok := selector.(*Planner); ok {
		v.constraints = planner.constraintMap
		if planner.costModel != nil {
			v.retentionLess = lessWithHashTieBreak(planner.costModel)
		}
	}
	if v.retentionLess == nil {
		v.retentionLess = physicalFallbackLess(selector, stats)
	}
	v.walk(root, nil, "root")
	v.report.VisitedReferences = len(v.report.visited)
	v.report.DeadEnds = len(v.report.deadEnds) + v.report.nilEnds
	v.report.MultiFinalReferences = len(v.report.multi)
	return v.report
}

type extractionVerifier struct {
	selector      BestMemberSelector
	stats         properties.StatisticsProvider
	constraints   *ConstraintMap
	retentionLess func(a, b expressions.RelationalExpression) bool
	report        ExtractionVerificationReport
	states        map[*expressions.Reference][]plans.OrdinalLayoutRequirement
	stack         map[*expressions.Reference]bool
}

func (v *extractionVerifier) walk(
	ref *expressions.Reference,
	requirement plans.OrdinalLayoutRequirement,
	path string,
) {
	if ref == nil {
		v.report.Violations = append(v.report.Violations,
			fmt.Sprintf("%s: nil child reference", path))
		// nil is not a Reference and therefore cannot enter the distinct
		// dead-end set. Count each malformed nil child edge separately.
		v.report.nilEnds++
		return
	}
	ref = ref.Canonical()
	if v.stateSeen(ref, requirement) {
		return
	}
	v.states[ref] = append(v.states[ref], requirement)
	v.report.visited[ref] = struct{}{}

	// Extraction is cycle-safe: a recursive back-edge is not a missing
	// selection, and it must not recurse forever. Unlike states, this guard is
	// stack-scoped so a shared sub-DAG is still checked under every positional
	// requirement through which it is reached.
	if v.stack[ref] {
		return
	}
	v.stack[ref] = true
	defer delete(v.stack, ref)

	selected, err := v.selectPhysical(ref, requirement)
	if err != nil {
		v.markDeadEnd(ref, path, err.Error())
		return
	}
	if selected == nil {
		v.markDeadEnd(ref, path, "no stamped, selected, or fallback physical member")
		return
	}
	v.report.selected[ref] = append(v.report.selected[ref], selected)
	v.verifyFinalCoherence(ref, selected, path)

	requirements, err := ordinalInputRequirementsOf(selected)
	if err != nil {
		v.markDeadEnd(ref, path,
			fmt.Sprintf("selected %T has incoherent positional requirements: %v", selected, err))
		return
	}
	for i, q := range selected.GetQuantifiers() {
		var childRequirement plans.OrdinalLayoutRequirement
		if len(requirements) != 0 {
			childRequirement = requirements[i]
		}
		v.walk(q.GetRangesOver(), childRequirement, fmt.Sprintf("%s/q%d", path, i))
	}
}

func (v *extractionVerifier) stateSeen(
	ref *expressions.Reference,
	requirement plans.OrdinalLayoutRequirement,
) bool {
	for _, seen := range v.states[ref] {
		if seen == nil && requirement == nil {
			return true
		}
		if seen != nil && requirement != nil && plans.OrdinalLayoutRequirementsEqual(seen, requirement) {
			return true
		}
	}
	return false
}

func (v *extractionVerifier) selectPhysical(
	ref *expressions.Reference,
	requirement plans.OrdinalLayoutRequirement,
) (expressions.RelationalExpression, error) {
	if requirement != nil {
		selected, err := selectedOrdinalCompatiblePhysicalMember(
			context.Background(), ref, requirement, v.selector, v.stats)
		if err != nil {
			return nil, err
		}
		if isPhysicalPlan(selected) {
			return selected, nil
		}
		return nil, nil
	}

	if winner := ref.Winner(); isPhysicalPlan(winner) {
		return winner, nil
	}
	if v.selector != nil && v.selector.HasBestMember(ref) {
		if selected := v.selector.BestMember(ref); isPhysicalPlan(selected) {
			return selected, nil
		}
	}
	return findBestValidPhysicalExpr(ref, physicalFallbackLess(v.selector, v.stats)), nil
}

func (v *extractionVerifier) markDeadEnd(ref *expressions.Reference, path, why string) {
	v.report.deadEnds[ref] = struct{}{}
	v.report.Violations = append(v.report.Violations,
		fmt.Sprintf("%s: extraction dead end: %s", path, why))
}

func (v *extractionVerifier) verifyFinalCoherence(
	ref *expressions.Reference,
	selected expressions.RelationalExpression,
	path string,
) {
	finals := ref.FinalMembers()
	physicalFinals := make([]expressions.RelationalExpression, 0, len(finals))
	finalSet := make(map[expressions.RelationalExpression]struct{}, len(finals))
	for _, member := range finals {
		if !isPhysicalPlan(member) {
			continue
		}
		physicalFinals = append(physicalFinals, member)
		finalSet[member] = struct{}{}
	}

	winner := ref.Winner()
	if winner != nil {
		if !isPhysicalPlan(winner) {
			v.report.Violations = append(v.report.Violations,
				fmt.Sprintf("%s: stamped winner %T is not physical", path, winner))
		} else if _, ok := finalSet[winner]; !ok {
			v.report.Violations = append(v.report.Violations,
				fmt.Sprintf("%s: stamped winner %T is not a final member", path, winner))
		}
	}

	if len(physicalFinals) <= 1 {
		return
	}
	v.report.multi[ref] = struct{}{}

	licensed := make(map[expressions.RelationalExpression]string, len(physicalFinals))
	if _, ok := finalSet[winner]; ok {
		licensed[winner] = "stamped global winner"
	} else if _, ok := finalSet[selected]; ok {
		// A group without a stamp is legal when extraction's physical fallback
		// is total. Only that cheapest fallback receives the global license.
		licensed[selected] = "cheapest physical fallback"
	}

	// Non-ordering interesting-property partitions are retained without an
	// explicit pushed constraint. This is what preserves, for example, both a
	// fetching index member and a costlier stored-record Fetch that enables a
	// cheaper parent Projection rewrite.
	type nonOrderingPartition struct {
		distinct bool
		stored   bool
	}
	partitionWinners := make(map[nonOrderingPartition]expressions.RelationalExpression)
	if planProperties := GetRefPlanPropertiesMap(ref); planProperties != nil {
		for _, member := range physicalFinals {
			memberProperties := planProperties.GetProperties(member)
			if memberProperties == nil {
				continue
			}
			partition := nonOrderingPartition{
				distinct: memberProperties.GetBool(properties.PropDistinctRecords),
				stored:   memberProperties.GetBool(properties.PropStoredRecord),
			}
			best := partitionWinners[partition]
			if best == nil || v.retentionLess(member, best) {
				partitionWinners[partition] = member
			}
		}
	}
	for _, member := range partitionWinners {
		licensed[member] = "distinct/stored property partition winner"
	}

	if orderings, ok := Get(v.constraints, ref, RequestedOrderingConstraintKey); ok {
		for _, ordering := range orderings {
			if ordering == nil || ordering.IsPreserve() {
				continue
			}
			var best expressions.RelationalExpression
			for _, member := range physicalFinals {
				if !memberSatisfiesOrdering(member, ordering) {
					continue
				}
				if best == nil || v.retentionLess(member, best) {
					best = member
				}
			}
			if best != nil {
				licensed[best] = "requested-ordering winner"
			}
		}
	}

	if requirements, ok := Get(v.constraints, ref, OrdinalLayoutConstraintKey); ok {
		for _, requirement := range requirements {
			best, err := bestOrdinalCompatiblePhysicalMemberAmong(
				physicalFinals, requirement, v.retentionLess)
			if err != nil {
				v.report.Violations = append(v.report.Violations,
					fmt.Sprintf("%s: ordinal-layout retention cannot be verified: %v", path, err))
				continue
			}
			if best != nil {
				licensed[best] = "ordinal-layout winner"
			}
		}
	}

	for _, member := range physicalFinals {
		if _, ok := licensed[member]; ok {
			continue
		}
		v.report.Violations = append(v.report.Violations,
			fmt.Sprintf("%s: retained final %T is not the winner for any required physical property", path, member))
	}
}

// SetVerifyExtractionUnambiguous enables RFC-224's post-drain extraction-path
// verification for this planner. It is off by default because it performs an
// additional traversal of the selected memo path.
func (p *Planner) SetVerifyExtractionUnambiguous(on bool) {
	p.verifyExtractionUnambiguous = on
}

// ExtractionVerification returns a defensive copy of the report from the
// last planning run for which verification was enabled.
func (p *Planner) ExtractionVerification() ExtractionVerificationReport {
	return p.extractionVerification.clone()
}

func (r ExtractionVerificationReport) clone() ExtractionVerificationReport {
	clone := r
	clone.Violations = append([]string(nil), r.Violations...)
	clone.visited = cloneReferenceSet(r.visited)
	clone.deadEnds = cloneReferenceSet(r.deadEnds)
	clone.multi = cloneReferenceSet(r.multi)
	if r.selected != nil {
		clone.selected = make(map[*expressions.Reference][]expressions.RelationalExpression, len(r.selected))
		for ref, selections := range r.selected {
			clone.selected[ref] = append([]expressions.RelationalExpression(nil), selections...)
		}
	}
	return clone
}

func cloneReferenceSet(in map[*expressions.Reference]struct{}) map[*expressions.Reference]struct{} {
	if in == nil {
		return nil
	}
	out := make(map[*expressions.Reference]struct{}, len(in))
	for ref := range in {
		out[ref] = struct{}{}
	}
	return out
}
