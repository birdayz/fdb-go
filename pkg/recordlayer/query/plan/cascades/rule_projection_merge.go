package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// ProjectionMergeRule collapses two nested LogicalProjections into one
// by COMPOSING the outer list through the inner:
//
//	Projection(P1) over Projection(P2) over X
//	→
//	Projection(P1 ∘ P2) over X
//
// Each outer slot that reads inner output k is substituted with P2[k]
// (baked-ordinal reads by ordinal; a lazy read carries no ordinal and
// declines). The pre-composition version kept P1 verbatim on
// the name-era contract that "the projection list is a side channel;
// rows pass through" — FALSE under the ordinal model, where P1's reads
// are baked against P2's OUTPUT row: dropping P2 reshaped the row
// underneath the baked ordinals (see OnMatch). A shape composition
// cannot prove declines the merge — both projections simply remain.
//
// This is more aggressive than Java's planner (which lets the cost
// model + cardinality estimates decide): no dedicated Java rule
// exists — the Memo's cost preference for fewer Projection operators
// emerges from physical plan choice (a fetch-only plan with the wider
// column list is cheaper than two stacked Projection operators). Go
// gets the same shape via this static rewrite.
type ProjectionMergeRule struct {
	matcher matching.BindingMatcher
}

// NewProjectionMergeRule constructs the rule.
func NewProjectionMergeRule() *ProjectionMergeRule {
	return &ProjectionMergeRule{
		matcher: NewExpressionMatcher[*expressions.LogicalProjectionExpression]("logical_projection"),
	}
}

// Matcher returns the pattern.
func (r *ProjectionMergeRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires when the inner Quantifier ranges over another
// LogicalProjection; yields a flat projection wrapping the inner's
// inner — with the outer's values COMPOSED through the inner's list.
//
// The original rule kept P1 verbatim, on the name-era contract that the
// projection list was "a side channel; rows pass through". Under the
// ordinal model that contract is FALSE: P1's values are BAKED by ordinal
// against the INNER projection's output row, so dropping the inner
// reshapes the row underneath the baked ordinals. `Project([Y#0],
// Project([COUNT(*)#1], StreamingAgg(keys=[COL1],…)))` merged to
// `Project([Y#0], StreamingAgg(…))` — reading the GROUP KEY instead of
// the count (SUM over a grouped UNION ALL branch returned key-sums;
// yamsql union_aggregate_java, RFC-180). Composition is the fix the
// rule's own caveat predicted: [#0]∘[#1] = [#1] — each outer slot k
// substitutes the inner's projected value at the ordinal it reads.
// A shape composition cannot prove (nested field path, unresolved lazy
// name, non-FieldValue outer slot) declines the merge — correctness over
// operator count; both projections simply remain.
func (r *ProjectionMergeRule) OnMatch(call *ExpressionRuleCall) {
	outer := matching.Get[*expressions.LogicalProjectionExpression](call.Bindings, r.matcher)
	innerExpr := outer.GetInner().GetRangesOver().Get()
	innerProj, ok := innerExpr.(*expressions.LogicalProjectionExpression)
	if !ok {
		return
	}
	// Counted BEFORE the guards below, so "the rule never ran" and "the rule ran
	// and every slot declined" cannot print the same. That distinction is the
	// whole reason this census exists — see projection_merge_census.go.
	recordProjectionMergeFiring()
	innerVals := innerProj.GetProjectedValues()
	innerAliases := innerProj.GetAliases()
	if innerAliases != nil && len(innerAliases) != len(innerVals) {
		return // malformed alias list — decline rather than misname a slot
	}
	outerVals := outer.GetProjectedValues()
	composed := make([]values.Value, len(outerVals))
	innerSlots := make([]int, len(outerVals))
	for i, v := range outerVals {
		recordProjectionMergeSlot()
		fv, isFV := values.AsFieldValue(v)
		if !isFV {
			recordProjectionMergeNotComposable()
			return
		}
		root, isQOV := values.AsQuantifiedObjectValue(fv.ChildValue())
		if !isQOV || root.Correlation() != outer.GetInner().GetAlias() {
			recordProjectionMergeNotComposable()
			return // not provably composable — keep both projections
		}
		ordinals := fv.Path().Ordinals()
		if len(ordinals) == 0 || ordinals[0] < 0 || ordinals[0] >= len(innerVals) || innerVals[ordinals[0]] == nil {
			recordProjectionMergeNotComposable()
			return
		}
		recordProjectionMergeBaked()
		innerSlots[i] = ordinals[0]
		if len(ordinals) == 1 {
			composed[i] = innerVals[ordinals[0]]
			continue
		}
		nested, err := values.ResolveFieldOrdinals(innerVals[ordinals[0]], ordinals[1:])
		if err != nil {
			recordProjectionMergeNotComposable()
			return
		}
		composed[i] = nested
	}
	// The OUTER's EFFECTIVE output names name the merged output — the flat
	// projection replaces the outer, so its output schema must be identical.
	// The raw alias list is NOT enough: an outer output named by its VALUE's
	// own field name (a lazy `SELECT "AK"` read — alias empty) loses that
	// name when composition substitutes the inner's value (the field name
	// rides the replaced value). Pin every slot's effective name —
	// OutputColumnName(outerVal, outerAlias), the same authority the
	// runtime applies — as the merged alias, so `WITH c AS (SELECT k AS ak
	// ...) SELECT ak FROM c` keeps AK after the CTE-consumer projection
	// merges into the body (dup inner names like [K, K] must never leak as
	// the output schema; the RFC-186 winner-flip triage caught exactly that
	// as a vanished column).
	outerAliases := outer.GetAliases()
	outerOutputNames := outer.GetOutputNames()
	if len(outerOutputNames) != len(outerVals) {
		return
	}
	if outerAliases != nil && len(outerAliases) != len(outerVals) {
		return // malformed outer alias list — decline, mirroring the inner guard
	}
	outerMinted := outer.GetAliasMinted()
	outerSources := outer.GetAliasSources()
	innerSources := innerProj.GetAliasSources()
	mergedAliases := make([]string, len(outerVals))
	mergedMinted := make([]bool, len(outerVals))
	mergedSources := make([]values.ProjectionAliasSource, len(outerVals))
	for i := range outerVals {
		alias := ""
		if outerAliases != nil {
			alias = outerAliases[i]
		}
		mergedAliases[i] = outerOutputNames[i]
		// Provenance composes, it does not copy. A slot the outer had NOT
		// aliased gets its effective name written into the alias vector right
		// here — by this rule, not by the user — so it becomes machinery-named
		// even though the outer's marker said nothing. That matters because the
		// effective name of an unaliased slot is its Value's own Field, which
		// over a join is LEG-QUALIFIED ("U.NAME"): report it as a user alias and
		// the merge alone would start leaking the qualifier into `SELECT
		// u.name`'s label. A slot the outer DID alias keeps the outer's marker,
		// user alias and machinery key alike.
		mergedMinted[i] = alias == "" || (i < len(outerMinted) && outerMinted[i])
		switch {
		case alias == "":
			// This rule minted the effective output name. Its structured source
			// can only come from the exact inner slot this outer ordinal read;
			// an absent inner source stays absent rather than being recovered
			// from the newly written alias spelling.
			if innerSlot := innerSlots[i]; innerSlot >= 0 && innerSlot < len(innerSources) {
				mergedSources[i] = innerSources[innerSlot]
			}
		case i < len(outerMinted) && outerMinted[i] && i < len(outerSources):
			// An existing machinery alias keeps the outer projection's source.
			mergedSources[i] = outerSources[i]
		}
	}
	flat, err := expressions.NewLogicalProjectionExpressionWithOutputSchema(
		composed,
		mergedAliases,
		mergedMinted,
		outer.GetOutputNames(),
		innerProj.GetInner(),
	)
	if err != nil {
		call.Fail(err)
		return
	}
	flat, err = flat.WithAliasSources(mergedSources)
	if err != nil {
		call.Fail(err)
		return
	}
	call.Yield(flat.WithInheritedOutputIdentity(outer))
}

var _ ExpressionRule = (*ProjectionMergeRule)(nil)
