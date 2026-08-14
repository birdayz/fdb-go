// Go extension — no Java equivalent.
//
// Java's Cascades has no physical sort operator; RemoveSortRule
// eliminates the sort via index ordering or fails the query.
// This plan materializes the inner result and sorts in memory.
package plans

import (
	"fmt"
	"slices"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// SortKey is a sort key + direction for in-memory sorting. ValueExpr is
// REQUIRED: it carries the key's plan-time-baked Value, which the
// executor evaluates POSITIONALLY per row. The field-only form (a Field lookup
// with a nil ValueExpr) is no longer supported — the runtime name fallback was
// deleted, so the executor rejects a nil ValueExpr as a malformed plan (loud,
// never a name read; pinned by TestSortCursor_UnbakedKeyIsLoud). Field is
// DISPLAY-ONLY (Explain + ordering-hint name match). Every planner path that
// builds a SortKey sets ValueExpr unconditionally (rule_implement_in_memory_sort,
// rule_implement_streaming_agg).
type SortKey struct {
	Field      string
	Desc       bool
	NullsFirst bool
	ValueExpr  values.Value // REQUIRED: the plan-time-baked key Value, evaluated per row
}

// RecordQueryInMemorySortPlan materializes the inner plan's output and
// sorts it in memory.
//
// Go extension — Java's Cascades has no physical sort operator.
//
// Cascades still optimizes the inner plan (index scans, predicate
// pushdown, join ordering). Only the final sort is post-processed.
// The cost model ensures index-based sort elimination is preferred
// when an index exists.
type RecordQueryInMemorySortPlan struct {
	PlanExprBase
	innerQ   expressions.Quantifier
	sortKeys []SortKey
}

func NewRecordQueryInMemorySortPlan(inner RecordQueryPlan, sortKeys []SortKey) (*RecordQueryInMemorySortPlan, error) {
	return NewRecordQueryInMemorySortPlanFromQuantifier(QuantifierOverPlan(inner), sortKeys)
}

// NewRecordQueryInMemorySortPlanFromQuantifier builds an in-memory sort whose
// child is a supplied memo quantifier instead of a snapshot over a single plan.
// This makes the plan its own cascades expression carrying its child edge
// directly — the memo holds it without a physicalInMemorySortWrapper (RFC-184 W2).
//
// The sort RE-SORTS its input, so it does not care what order the child provides:
// unlike the ordering-DELEGATOR wrappers (which pin an ordered spine), it only
// needs the cheapest VALID child member for ANY ordering. When the emitter hands
// it the LIVE shared-group edge (ForEachQuantifier(innerRef)), GetInner resolves
// through planFromQuantifier → innerRef.Winner() — the group's OPTIMIZE-chosen
// cheapest member (unified_tasks.OptimizeGroupTask stamps the overall cost winner,
// ordering-agnostic). Cost (concretePlanCounts walks GetChildren → GetInner) and
// extraction (rebuild recurses the same edge) therefore resolve the SAME member,
// closing the cost-over-first / extract-over-best gap the wrapper carried
// (plan_expression.go's planFromQuantifier note). The sort keys are copied so the
// provided ordering (HintOrdering) stays stable across relinks.
func NewRecordQueryInMemorySortPlanFromQuantifier(innerQ expressions.Quantifier, sortKeys []SortKey) (*RecordQueryInMemorySortPlan, error) {
	keys, err := reanchorSortKeysForInput(sortKeys, innerQ)
	if err != nil {
		return nil, err
	}
	base, err := newInMemorySortPlanExprBase(innerQ)
	if err != nil {
		return nil, err
	}
	return &RecordQueryInMemorySortPlan{PlanExprBase: base, innerQ: innerQ, sortKeys: keys}, nil
}

func reanchorSortKeysForInput(
	sortKeys []SortKey,
	innerQ expressions.Quantifier,
) ([]SortKey, error) {
	keys := slices.Clone(sortKeys)
	for i := range keys {
		if keys[i].ValueExpr == nil {
			return nil, fmt.Errorf("RecordQueryInMemorySortPlan key %d has nil ValueExpr", i)
		}
		var err error
		keys[i].ValueExpr, err = reanchorCurrentValueForInput(keys[i].ValueExpr, innerQ)
		if err != nil {
			return nil, fmt.Errorf("RecordQueryInMemorySortPlan key %d input carrier: %w", i, err)
		}
	}
	return keys, nil
}

// reanchorInputValueToOutput carries an input-bound Value across the sort's
// runtime output. reanchorCurrentValueForInput performs the complete checked
// source/producer/layout normalization onto the selected child's exact carrier;
// this method then moves that current-rooted program to the sort's fresh output
// carrier (same exact row type, distinct owner handle).
func (p *RecordQueryInMemorySortPlan) reanchorInputValueToOutput(value values.Value) (values.Value, error) {
	outputLayout, err := p.ProvidedOutputLayout()
	if err != nil {
		return nil, fmt.Errorf("RecordQueryInMemorySortPlan output layout: %w", err)
	}
	// A current-rooted program has already crossed the child's producer
	// boundary. This includes both the selected child's exact carrier and this
	// sort's own carrier on a second extraction/relink pass; a sort preserves
	// column ordinals across those two same-shaped phases. Re-running source
	// normalization would reinterpret current ordinal 4 through the FlatMap RC
	// by the leaf name NAME and collapse it onto the first NAME at ordinal 1.
	// ReanchorValueForLayout still checks the exact row shape and moves the
	// immutable handle onto this output phase, so a foreign current type fails.
	if _, currentRooted := values.GetCorrelatedToOfValue(value)[values.CurrentCorrelation()]; currentRooted {
		reanchored, reanchorErr := values.ReanchorValueForLayout(
			value, outputLayout.Carrier(), outputLayout)
		if reanchorErr != nil {
			return nil, fmt.Errorf("RecordQueryInMemorySortPlan output program: %w", reanchorErr)
		}
		return reanchored, nil
	}
	// A sort can sit directly above a materializing NLJ. The parent projection
	// still names a retained logical source (T1.N.SK), while the sort input edge
	// names the complete joined row. Crossing the child's materializer is the
	// only checked authority that maps that source-relative nested path onto the
	// joined carrier; generic edge normalization cannot, because T1 is not the
	// whole sort-input edge.
	normalized := value
	if child := selectedPlanFromQuantifier(p.innerQ); child != nil {
		if materializer, ok := childValueMaterializer(child); ok {
			normalized, err = materializer.reanchorInputValueToOutput(normalized)
			if err != nil {
				return nil, fmt.Errorf("RecordQueryInMemorySortPlan input materializer: %w", err)
			}
		}
	}
	if _, nowCurrent := values.GetCorrelatedToOfValue(normalized)[values.CurrentCorrelation()]; !nowCurrent {
		normalized, err = reanchorCurrentValueForInput(normalized, p.innerQ)
	}
	if err != nil {
		return nil, fmt.Errorf("RecordQueryInMemorySortPlan input program: %w", err)
	}
	// Do not run producer matching a second time here. At this point a buried
	// S.NAME has already become input-current ordinal 4. Matching that resolved
	// field against the FlatMap RC by accessor names again sees R.NAME as the
	// sole one-step NAME and collapses it to ordinal 1. The exact input carrier
	// is the proof that producer normalization is complete.
	reanchored, err := values.ReanchorValueForLayout(
		normalized, outputLayout.Carrier(), outputLayout)
	if err != nil {
		return nil, fmt.Errorf("RecordQueryInMemorySortPlan output program: %w", err)
	}
	return reanchored, nil
}

// newInMemorySortPlanExprBase states the materialization boundary explicitly.
// The sort consumes its selected child in that child's exact physical layout,
// but its buffer emits a newly materialized row and therefore publishes a fresh
// current-only identity layout for the same exact logical type. Forwarding a
// join's source windows through the buffer would claim that the new carrier
// still owns the child's address space and makes continuation restoration
// dependent on stale window identities.
//
// A live exploratory edge has no selected child yet, so it can publish only the
// fresh output layout. Extraction relinks the sort over the selected singleton
// and this helper then installs the exact input requirement atomically.
func newInMemorySortPlanExprBase(innerQ expressions.Quantifier) (PlanExprBase, error) {
	const owner = "RecordQueryInMemorySortPlan"
	input, err := innerQ.RequireFlowedObjectValue()
	if err != nil {
		return PlanExprBase{}, fmt.Errorf("%s input: %w", owner, err)
	}
	provided, err := newIdentityOutputLayout(input.FlowedType())
	if err != nil {
		return PlanExprBase{}, fmt.Errorf("%s provided output layout: %w", owner, err)
	}
	var required []OrdinalLayoutRequirement
	if child := selectedPlanFromQuantifier(innerQ); child != nil {
		childLayout, layoutErr := child.ProvidedOutputLayout()
		if layoutErr != nil {
			return PlanExprBase{}, fmt.Errorf("%s required input layout: %w", owner, layoutErr)
		}
		if !childLayout.Carrier().FlowedType().Equals(input.FlowedType()) {
			return PlanExprBase{}, fmt.Errorf(
				"%s required input layout: child carrier type %s disagrees with edge type %s",
				owner, childLayout.Carrier().FlowedType(), input.FlowedType())
		}
		requirement, requirementErr := RequireExactLayout(childLayout)
		if requirementErr != nil {
			return PlanExprBase{}, fmt.Errorf("%s required input layout: %w", owner, requirementErr)
		}
		required = []OrdinalLayoutRequirement{requirement}
	}
	return newPlanExprBaseWithProperties(owner, provided.Carrier(), required, provided)
}

func (p *RecordQueryInMemorySortPlan) GetInner() RecordQueryPlan {
	return planFromQuantifier(p.innerQ)
}

// GetInnerQuantifier returns the live child quantifier — the single memo edge the
// sort ranges over. Since RFC-184 W2 the memo holds the bare plan (no
// physicalInMemorySortWrapper whose innerQuant field was read), this exposes the
// same edge for derivations and extraction.
func (p *RecordQueryInMemorySortPlan) GetInnerQuantifier() expressions.Quantifier {
	return p.innerQ
}

func (p *RecordQueryInMemorySortPlan) GetSortKeys() []SortKey { return p.sortKeys }

// GetQuantifiers reports the real child quantifier, overriding
// PlanExprBase's none.
func (p *RecordQueryInMemorySortPlan) GetQuantifiers() []expressions.Quantifier {
	if p.innerQ.GetRangesOver() == nil {
		return nil
	}
	return []expressions.Quantifier{p.innerQ}
}

// GetResultType returns the inner plan's result type: an in-memory sort
// reorders rows but preserves the inner's row shape, so it flows the inner's
// type through (matching pass-through plans like Filter / Fetch). Nil inner
// degrades to UnknownType.
func (p *RecordQueryInMemorySortPlan) GetResultType() values.Type { return p.GetResultValue().Type() }

func (p *RecordQueryInMemorySortPlan) GetChildren() []RecordQueryPlan {
	inner := p.GetInner()
	if inner == nil {
		return nil
	}
	return []RecordQueryPlan{inner}
}

// EqualsWithoutChildren compares the sort keys by SEMANTIC identity (RFC-176 P2
// / F21): each key's plan-time-baked ValueExpr is compared via the plans-package
// semanticValueEquals (semantic Value equality under the empty alias map), not
// by pointer. Two independently-built sort keys over the same Value are the same
// plan — pointer identity would spuriously split them into distinct memo members
// (the incomplete-F21 case). Desc / NullsFirst are the direction.
//
// SortKey.Field is DISPLAY-ONLY and is NOT folded (RFC-197 item 3). It used to be,
// "so an explain-name difference still separates identities" — but a display-name
// difference IS NOT an identity difference, and folding it splits one plan into
// two memo members whenever two producers render the same baked key under
// different names. The ordinal-addressed ValueExpr is the identity; the string
// beside it is what Explain prints.
// structuralKey folds the InMemorySort identity: the ordered sort-key list via
// sortKeyEqual (Desc / NullsFirst + semantic ValueExpr). Drives both Equals and
// Hash.
func (p *RecordQueryInMemorySortPlan) structuralKey() *structuralKey {
	return newStructuralKey().SortKeys(p.sortKeys)
}

func (p *RecordQueryInMemorySortPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryInMemorySortPlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

// sortKeyEqual reports semantic equality of two sort keys: the direction
// (Desc / NullsFirst) and the semantic ValueExpr — the plan-time-baked key, whose
// ordinals are the column identity. The display Field is excluded (RFC-197 item
// 3), exactly as Java excludes the per-accessor name from ResolvedAccessor
// equality (FieldValue.java:676-685). Pairs with HashCodeWithoutChildren, which
// folds the identical set — preserving the equal⟹same-hash memo invariant.
func sortKeyEqual(a, b SortKey) bool {
	return a.Desc == b.Desc &&
		a.NullsFirst == b.NullsFirst &&
		semanticValueEquals(a.ValueExpr, b.ValueExpr)
}

func (p *RecordQueryInMemorySortPlan) HashCodeWithoutChildren() uint64 {
	return p.structuralKey().Hash("inmemsort|")
}

func (p *RecordQueryInMemorySortPlan) Explain() string {
	keys := make([]string, len(p.sortKeys))
	for i, k := range p.sortKeys {
		dir := "ASC"
		if k.Desc {
			dir = "DESC"
		}
		keys[i] = k.Field + " " + dir
	}
	innerLabel := "<nil>"
	if inner := p.GetInner(); inner != nil {
		innerLabel = inner.Explain()
	}
	return fmt.Sprintf("InMemorySort([%s], %s)", strings.Join(keys, ", "), innerLabel)
}

var (
	_ RecordQueryPlan                  = (*RecordQueryInMemorySortPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryInMemorySortPlan)(nil)
)

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryInMemorySortPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// WithQuantifiers returns a copy ranging over the given child quantifier —
// Java's copy-on-write withChild(Reference).
func (p *RecordQueryInMemorySortPlan) WithQuantifiers(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if err := validateQuantifierArity("RecordQueryInMemorySortPlan", len(qs), 1); err != nil {
		return nil, err
	}
	oldInput, err := p.innerQ.RequireFlowedObjectValue()
	if err != nil {
		return nil, fmt.Errorf("RecordQueryInMemorySortPlan.WithQuantifiers old input: %w", err)
	}
	newInput, err := qs[0].RequireFlowedObjectValue()
	if err != nil {
		return nil, fmt.Errorf("RecordQueryInMemorySortPlan.WithQuantifiers new input: %w", err)
	}
	if !oldInput.FlowedType().Equals(newInput.FlowedType()) {
		return nil, fmt.Errorf(
			"RecordQueryInMemorySortPlan.WithQuantifiers input type changed from %s to %s",
			oldInput.FlowedType(), newInput.FlowedType())
	}
	oldLayout, oldSelected, err := selectedInputOrdinalLayout(p.innerQ)
	if err != nil {
		return nil, fmt.Errorf("RecordQueryInMemorySortPlan.WithQuantifiers old layout: %w", err)
	}
	newLayout, newSelected, err := selectedInputOrdinalLayout(qs[0])
	if err != nil {
		return nil, fmt.Errorf("RecordQueryInMemorySortPlan.WithQuantifiers new layout: %w", err)
	}
	exactSelectedRelink := oldSelected && newSelected
	keys := slices.Clone(p.sortKeys)
	oldAlias := oldInput.Correlation()
	newAlias := newInput.Correlation()
	var aliasMap values.AliasMap
	if oldAlias != newAlias {
		aliasMap, err = values.NewAliasMap([]values.AliasPair{{Source: oldAlias, Target: newAlias}})
		if err != nil {
			return nil, fmt.Errorf("RecordQueryInMemorySortPlan.WithQuantifiers alias map: %w", err)
		}
	}
	for i := range keys {
		if keys[i].ValueExpr == nil {
			return nil, fmt.Errorf("RecordQueryInMemorySortPlan.WithQuantifiers key %d has nil ValueExpr", i)
		}
		key := keys[i].ValueExpr
		exactOutput := false
		if exactSelectedRelink {
			key, exactOutput, err = translateSortKeyAcrossSelectedOutput(
				key, oldInput, oldLayout, newLayout)
			if err != nil {
				return nil, fmt.Errorf(
					"RecordQueryInMemorySortPlan.WithQuantifiers key %d selected output: %w", i, err)
			}
		}
		if exactOutput {
			keys[i].ValueExpr = key
			continue
		}
		if aliasMap != nil {
			key, err = values.RebaseValueChecked(key, aliasMap)
			if err != nil {
				return nil, fmt.Errorf("RecordQueryInMemorySortPlan.WithQuantifiers key %d: %w", i, err)
			}
		}
		key, err = reanchorCurrentValueForInput(key, qs[0])
		if err != nil {
			return nil, fmt.Errorf(
				"RecordQueryInMemorySortPlan.WithQuantifiers key %d input carrier: %w", i, err)
		}
		keys[i].ValueExpr = key
	}
	base, err := newInMemorySortPlanExprBase(qs[0])
	if err != nil {
		return nil, err
	}
	cp := *p
	cp.PlanExprBase = base
	cp.innerQ = qs[0]
	cp.sortKeys = keys
	return &cp, nil
}

// translateSortKeyAcrossSelectedOutput carries a key that is already expressed
// at one selected child's OUTPUT boundary to the replacement selected child's
// exact output carrier. A sort key can retain either the old quantifier edge or
// the old selected layout carrier, so both checked representations are
// translated. Once every QOV leaf is the replacement carrier, its ordinals are
// final and must not be fed through the child's input-to-output materializer a
// second time. Doing so can reinterpret output ID#0 through a FlatMap result
// program and rematch it to a different same-named source at X#1.
//
// Pointer identity is load-bearing for the phase-carrier arm. An independently
// minted same-shaped current row, a foreign source, or a mixed program does not
// take this bypass and remains on the ordinary checked producer path.
func translateSortKeyAcrossSelectedOutput(
	key values.Value,
	oldInput values.QuantifiedObjectValue,
	oldLayout values.OrdinalLayout,
	newLayout values.OrdinalLayout,
) (values.Value, bool, error) {
	if key == nil || oldInput == nil || oldLayout == nil || newLayout == nil ||
		oldLayout.Carrier() == nil || newLayout.Carrier() == nil {
		return key, false, fmt.Errorf("selected sort-key relink requires exact key, edge, and layouts")
	}
	translated, err := values.TranslateDeclaredEdgeRoot(key, oldInput, newLayout.Carrier())
	if err != nil {
		return nil, false, err
	}
	translated, err = values.TranslatePhaseRoot(
		translated, oldLayout.Carrier(), newLayout.Carrier())
	if err != nil {
		return nil, false, err
	}
	if !valueReferencesOnlyExactQOV(translated, newLayout.Carrier()) {
		return translated, false, nil
	}
	translated, err = values.ReanchorValueForLayout(
		translated, newLayout.Carrier(), newLayout)
	if err != nil {
		return nil, false, err
	}
	return translated, true, nil
}

// WithChildren is the extraction/relink hook (plan_extraction.go's WithChildren
// interface). The sort carries its child as a single memo edge, so the relink is
// a quantifier swap: WithQuantifiers copies the receiver (preserving the sort
// keys) and re-resolves GetInner through the new singleton reference. This
// replaces physicalInMemorySortWrapper.WithChildren (RFC-184 W2), whose separate
// snapshot plan field forced a findBestPhysicalPlan constructor rebuild. Because
// extraction hands qs[0] the child group's already-resolved winner (a singleton
// holding the cheapest member), a pure quantifier swap resolves the exact plan
// the cost model costed — no separate best-member re-pick is needed.
func (p *RecordQueryInMemorySortPlan) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("RecordQueryInMemorySortPlan.WithChildren: expected 1 child, got %d", len(qs))
	}
	return p.WithQuantifiers(qs)
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryInMemorySortPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
