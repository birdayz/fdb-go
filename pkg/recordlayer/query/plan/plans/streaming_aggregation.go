package plans

import (
	"fmt"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryStreamingAggregationPlan groups input rows by grouping
// keys and computes aggregates over each group in a streaming fashion.
// The plan requires that the inner plan produces rows already sorted
// by the grouping keys — no materialisation needed.
//
// Mirrors Java's RecordQueryStreamingAggregationPlan: the streaming
// operator reads sorted input and emits one output row per change in
// the grouping-key combination. When the inner is NOT ordered by
// grouping keys, ImplementStreamingAggregationRule does not fire —
// a sort is needed first, or the hash-aggregate path (future) is
// used instead.
type RecordQueryStreamingAggregationPlan struct {
	PlanExprBase
	innerQ       expressions.Quantifier
	groupingKeys []values.Value
	aggregates   []expressions.AggregateSpec
	// outputNames is the aggregate's private native row schema, frozen before
	// its key/operand evaluation programs are reanchored onto physical child
	// edges. In particular, a computed key's SQL name must not acquire a memo
	// alias merely because a relink changed the QOV that evaluates it.
	outputNames []string
	resultValue values.QuantifiedObjectValue
}

func NewRecordQueryStreamingAggregationPlan(
	inner RecordQueryPlan,
	groupingKeys []values.Value,
	aggregates []expressions.AggregateSpec,
) (*RecordQueryStreamingAggregationPlan, error) {
	return newRecordQueryStreamingAggregationPlan(
		QuantifierOverPlan(inner), groupingKeys, aggregates, nil)
}

// NewRecordQueryStreamingAggregationPlanFromQuantifier builds a streaming
// aggregation whose child is a supplied memo quantifier instead of a snapshot
// over a single plan. This makes the plan its own cascades expression carrying
// its child edge directly — the memo holds it without a physicalStreamingAggWrapper
// (RFC-184 W2).
//
// Streaming aggregation is a PRODUCER, not an ordering-delegator: it reshapes
// rows (one output row per grouping-key change) and provides its OWN output
// ordering. But it has a CORRECTNESS PRECONDITION — the inner must be ordered by
// the grouping keys — so the emitter chooses the child edge per arm: a plain
// count-only aggregation (no grouping keys) or a self-contained ordered producer
// (an InMemorySort it builds, a covering index scan) carries the LIVE
// shared-group edge, while a DELEGATING ordered inner (an existing Fetch/Filter
// spine) is frozen deep by pinOrderedSpine + FinalOf so it cannot float to an
// unordered sibling and split groups. The grouping keys and aggregate specs are
// preserved so OutputRecordType / GetResultValue stay stable.
func NewRecordQueryStreamingAggregationPlanFromQuantifier(
	innerQ expressions.Quantifier,
	groupingKeys []values.Value,
	aggregates []expressions.AggregateSpec,
) (*RecordQueryStreamingAggregationPlan, error) {
	return newRecordQueryStreamingAggregationPlan(innerQ, groupingKeys, aggregates, nil)
}

func newRecordQueryStreamingAggregationPlan(
	innerQ expressions.Quantifier,
	groupingKeys []values.Value,
	aggregates []expressions.AggregateSpec,
	outputNames []string,
) (*RecordQueryStreamingAggregationPlan, error) {
	if innerQ.GetRangesOver() == nil {
		return nil, fmt.Errorf("RecordQueryStreamingAggregationPlan requires an inner plan")
	}
	reanchoredKeys := make([]values.Value, len(groupingKeys))
	for i, key := range groupingKeys {
		var err error
		reanchoredKeys[i], err = reanchorCurrentValueForInput(key, innerQ)
		if err != nil {
			return nil, fmt.Errorf("RecordQueryStreamingAggregationPlan grouping key %d input carrier: %w", i, err)
		}
		reanchoredKeys[i], err = pinStreamingAggregationInputFrontier(reanchoredKeys[i], innerQ)
		if err != nil {
			return nil, fmt.Errorf("RecordQueryStreamingAggregationPlan grouping key %d input frontier: %w", i, err)
		}
	}
	reanchoredAggregates := append([]expressions.AggregateSpec(nil), aggregates...)
	for i := range reanchoredAggregates {
		if reanchoredAggregates[i].Operand == nil {
			continue
		}
		var err error
		reanchoredAggregates[i].Operand, err = reanchorCurrentValueForInput(
			reanchoredAggregates[i].Operand, innerQ)
		if err != nil {
			return nil, fmt.Errorf("RecordQueryStreamingAggregationPlan aggregate %d input carrier: %w", i, err)
		}
		reanchoredAggregates[i].Operand, err = pinStreamingAggregationInputFrontier(
			reanchoredAggregates[i].Operand, innerQ)
		if err != nil {
			return nil, fmt.Errorf("RecordQueryStreamingAggregationPlan aggregate %d input frontier: %w", i, err)
		}
	}
	if outputNames == nil {
		outputNames = expressions.GroupByOutputColumnNames(groupingKeys, aggregates)
	}
	resultType, err := streamingAggregationOutputRecordType(reanchoredKeys, reanchoredAggregates, outputNames)
	if err != nil {
		return nil, err
	}
	base, err := newPlanExprBaseForType("RecordQueryStreamingAggregationPlan", resultType)
	if err != nil {
		return nil, err
	}
	resultValue, ok := values.AsQuantifiedObjectValue(base.resultValue)
	if !ok {
		return nil, fmt.Errorf("RecordQueryStreamingAggregationPlan result Value: base did not produce an exact QOV")
	}
	return &RecordQueryStreamingAggregationPlan{
		PlanExprBase: base,
		innerQ:       innerQ,
		groupingKeys: reanchoredKeys,
		aggregates:   reanchoredAggregates,
		outputNames:  append([]string(nil), outputNames...),
		resultValue:  resultValue,
	}, nil
}

// pinStreamingAggregationInputFrontier seals fields which were just moved onto
// a selected child's exact output carrier as direct reads of that flat row. An
// unpinned field is source/leg-relative; routing it through the aggregate's
// join-window evaluator after materialization asks for a current QOV which no
// retained source window declares. Exploratory edges and external correlations
// remain unchanged until extraction selects an exact layout.
func pinStreamingAggregationInputFrontier(
	value values.Value,
	inputQ expressions.Quantifier,
) (values.Value, error) {
	layout, selected, err := selectedInputOrdinalLayout(inputQ)
	if err != nil {
		return nil, err
	}
	if !selected {
		return value, nil
	}
	return values.PinValueToExactFrontier(value, layout.Carrier())
}

func (p *RecordQueryStreamingAggregationPlan) GetInner() RecordQueryPlan {
	return planFromQuantifier(p.innerQ)
}

// GetInnerQuantifier returns the live child quantifier — the single memo edge the
// aggregation ranges over. Since RFC-184 W2 the memo holds the bare plan (no
// physicalStreamingAggWrapper whose innerQuant field was read), this exposes the
// same edge for derivations and extraction.
func (p *RecordQueryStreamingAggregationPlan) GetInnerQuantifier() expressions.Quantifier {
	return p.innerQ
}

// GetResultValue flows a TYPED QOV whose RecordType is the aggregate's output
// schema ([groupKeys, aggregates], the plan's single naming authority), so the
// resolver BAKES downstream references to ordinals at plan time (Java's
// getFieldNameToOrdinalMap). A downstream ref then reads the aggregateCursor's
// PositionalRow by Get(ordinal) — order, not spelling — robust to redundant
// spellings of the same column. A streaming aggregation is a PRODUCER: it does
// NOT flow its inner's rows through, so this must NOT delegate to the child's
// flowed value (unlike the filter/distinct passthroughs). This is the identity
// physicalStreamingAggWrapper.GetResultValue supplied (RFC-184 W2).
func (p *RecordQueryStreamingAggregationPlan) GetResultValue() values.Value {
	return p.resultValue
}

// GetQuantifiers reports the real child quantifier, overriding
// PlanExprBase's none.
func (p *RecordQueryStreamingAggregationPlan) GetQuantifiers() []expressions.Quantifier {
	if p.innerQ.GetRangesOver() == nil {
		return nil
	}
	return []expressions.Quantifier{p.innerQ}
}

func (p *RecordQueryStreamingAggregationPlan) GetGroupingKeys() []values.Value { return p.groupingKeys }
func (p *RecordQueryStreamingAggregationPlan) GetAggregates() []expressions.AggregateSpec {
	return p.aggregates
}

func (p *RecordQueryStreamingAggregationPlan) GetResultType() values.Type {
	return p.OutputRecordType()
}

// OutputColumnNames is the SINGLE naming authority for this plan's output row:
// grouping keys (in GROUP BY order) then aggregates (in aggregate order), each
// alias-preferring. The ordinal model bakes downstream references against
// this order, and the executor's aggregateCursor emits its PositionalRow with these
// exact names — so a reference over the aggregate resolves by Get(ordinal) (Java's
// getFieldValueForFieldOrdinals) instead of a spelling-sensitive name lookup.
func (p *RecordQueryStreamingAggregationPlan) OutputColumnNames() []string {
	return append([]string(nil), p.outputNames...)
}

// OutputRecordType is OutputColumnNames as a RAW RecordType (ordinal == slice
// position; dup-name-safe). Flowed as the aggregate's result-value QOV type so the
// resolver BAKES downstream references to ordinals at plan time.
func (p *RecordQueryStreamingAggregationPlan) OutputRecordType() *values.RecordType {
	if p.resultValue == nil {
		return nil
	}
	resultType, _ := p.resultValue.FlowedType().(*values.RecordType)
	return resultType
}

func streamingAggregationOutputRecordType(
	groupingKeys []values.Value,
	aggregates []expressions.AggregateSpec,
	names []string,
) (*values.RecordType, error) {
	if len(names) != len(groupingKeys)+len(aggregates) {
		return nil, fmt.Errorf(
			"RecordQueryStreamingAggregationPlan output schema width %d does not match %d grouping keys + %d aggregates",
			len(names), len(groupingKeys), len(aggregates))
	}
	fields := make([]values.Field, 0, len(names))
	for i, key := range groupingKeys {
		if key == nil {
			return nil, fmt.Errorf("RecordQueryStreamingAggregationPlan grouping key %d is nil", i)
		}
		if _, err := values.ExactTypeForValue(key); err != nil {
			return nil, fmt.Errorf("RecordQueryStreamingAggregationPlan grouping key %d: %w", i, err)
		}
		fields = append(fields, values.Field{Name: names[i], FieldType: key.Type(), Ordinal: i})
	}
	for i, aggregate := range aggregates {
		var resultType values.Type
		switch aggregate.Function {
		case expressions.AggCount:
			resultType = values.NullableLong
		case expressions.AggAvg:
			if aggregate.Operand == nil {
				return nil, fmt.Errorf("RecordQueryStreamingAggregationPlan aggregate %d AVG requires an operand", i)
			}
			resultType = values.NullableDouble
		case expressions.AggSum, expressions.AggMin, expressions.AggMax:
			if aggregate.Operand == nil {
				return nil, fmt.Errorf("RecordQueryStreamingAggregationPlan aggregate %d %s requires an operand", i, aggregate.Function)
			}
			resultType = values.WithNullability(aggregate.Operand.Type(), true)
		default:
			return nil, fmt.Errorf("RecordQueryStreamingAggregationPlan aggregate %d has unsupported function %d", i, aggregate.Function)
		}
		if _, err := values.SnapshotExactType(resultType); err != nil {
			return nil, fmt.Errorf("RecordQueryStreamingAggregationPlan aggregate %d result type: %w", i, err)
		}
		ordinal := len(groupingKeys) + i
		fields = append(fields, values.Field{Name: names[ordinal], FieldType: resultType, Ordinal: ordinal})
	}
	result := &values.RecordType{Fields: fields}
	if _, err := values.SnapshotExactType(result); err != nil {
		return nil, fmt.Errorf("RecordQueryStreamingAggregationPlan output record: %w", err)
	}
	return result, nil
}

func (p *RecordQueryStreamingAggregationPlan) GetChildren() []RecordQueryPlan {
	inner := p.GetInner()
	if inner == nil {
		return nil
	}
	return []RecordQueryPlan{inner}
}

// structuralKey folds the grouping keys (by value) then per aggregate the
// Function discriminator + Operand (by value), pairing equality and hashing so
// they can never disagree on which fields matter.
func (p *RecordQueryStreamingAggregationPlan) structuralKey() *structuralKey {
	k := newStructuralKey().Values(p.groupingKeys)
	for _, a := range p.aggregates {
		k.Int(int(a.Function)).Value(a.Operand)
	}
	return k
}

func (p *RecordQueryStreamingAggregationPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryStreamingAggregationPlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

func (p *RecordQueryStreamingAggregationPlan) HashCodeWithoutChildren() uint64 {
	return p.structuralKey().Hash("streamagg|")
}

func (p *RecordQueryStreamingAggregationPlan) Explain() string {
	keys := values.ExplainPlanValues(p.groupingKeys)
	innerLabel := "<nil>"
	if inner := p.GetInner(); inner != nil {
		innerLabel = inner.Explain()
	}
	return fmt.Sprintf("StreamingAgg(keys=[%s], %s)", strings.Join(keys, ", "), innerLabel)
}

var (
	_ RecordQueryPlan                  = (*RecordQueryStreamingAggregationPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryStreamingAggregationPlan)(nil)
)

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryStreamingAggregationPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// WithQuantifiers rebuilds the aggregation over the given child quantifier.
// Grouping keys and aggregate operands are evaluation programs over the input
// edge, so replacing that edge must checked-rebase every exact QOV root before
// publishing the replacement plan. Reconstructing through the constructor also
// rebuilds PlanExprBase and the aggregate's output QOV from the rebased values;
// a shallow copy would retain a stale admitted result contract.
func (p *RecordQueryStreamingAggregationPlan) WithQuantifiers(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if err := validateQuantifierArity("RecordQueryStreamingAggregationPlan", len(qs), 1); err != nil {
		return nil, err
	}
	oldInput, err := p.innerQ.RequireFlowedObjectValue()
	if err != nil {
		return nil, fmt.Errorf("RecordQueryStreamingAggregationPlan.WithQuantifiers old input: %w", err)
	}
	newInput, err := qs[0].RequireFlowedObjectValue()
	if err != nil {
		return nil, fmt.Errorf("RecordQueryStreamingAggregationPlan.WithQuantifiers new input: %w", err)
	}
	if !oldInput.FlowedType().Equals(newInput.FlowedType()) {
		return nil, fmt.Errorf(
			"RecordQueryStreamingAggregationPlan.WithQuantifiers input type changed from %s to %s",
			oldInput.FlowedType(), newInput.FlowedType())
	}

	rebasedKeys := make([]values.Value, len(p.groupingKeys))
	for i, key := range p.groupingKeys {
		rebasedKeys[i], err = rebaseStreamingAggregationInputValue(
			key, oldInput, newInput, p.innerQ)
		if err != nil {
			return nil, fmt.Errorf("RecordQueryStreamingAggregationPlan.WithQuantifiers grouping key %d: %w", i, err)
		}
	}

	rebasedAggregates := append([]expressions.AggregateSpec(nil), p.aggregates...)
	for i := range rebasedAggregates {
		if rebasedAggregates[i].Operand == nil {
			continue // COUNT(*) has no input Value to rebase.
		}
		rebasedAggregates[i].Operand, err = rebaseStreamingAggregationInputValue(
			rebasedAggregates[i].Operand, oldInput, newInput, p.innerQ)
		if err != nil {
			return nil, fmt.Errorf("RecordQueryStreamingAggregationPlan.WithQuantifiers aggregate %d operand: %w", i, err)
		}
	}

	rebuilt, err := newRecordQueryStreamingAggregationPlan(qs[0], rebasedKeys, rebasedAggregates, p.outputNames)
	if err != nil {
		return nil, err
	}
	return rebuilt, nil
}

func validateStreamingAggregationOldInputRoots(
	value values.Value,
	input values.QuantifiedObjectValue,
	inputQ expressions.Quantifier,
) error {
	layout, selected, err := selectedInputOrdinalLayout(inputQ)
	if err != nil {
		return fmt.Errorf("selected input layout: %w", err)
	}
	var rootErr error
	values.WalkValue(value, func(node values.Value) bool {
		root, ok := values.AsQuantifiedObjectValue(node)
		if !ok {
			return true
		}
		if root.Correlation() == input.Correlation() {
			if root.FlowedType().Equals(input.FlowedType()) {
				return true
			}
			if !selected {
				// A live edge may eventually select a merged-row layout whose
				// rightmost source window conventionally shares the edge alias.
				// Exact validation is deferred until that concrete layout exists.
				return true
			}
			// A source window may deliberately share the quantifier's alias (a
			// merged join box conventionally uses its rightmost leg alias). Exact
			// type, not alias alone, distinguishes it from the whole edge.
			if selected {
				provided, providesErr := values.LayoutProvides(layout, root)
				if providesErr == nil && provided {
					return true
				}
			}
			rootErr = fmt.Errorf(
				"QOV root type %s disagrees with input edge type %s",
				root.FlowedType(), input.FlowedType())
			return false
		}
		if root.Correlation() == values.CurrentCorrelation() {
			if selected && root != layout.Carrier() {
				rootErr = fmt.Errorf("current QOV root is not the selected input layout carrier")
				return false
			}
			if !selected && !root.FlowedType().Equals(input.FlowedType()) {
				rootErr = fmt.Errorf(
					"current QOV root type %s disagrees with input edge type %s",
					root.FlowedType(), input.FlowedType())
				return false
			}
			return true
		}
		if !selected {
			return true
		}
		provided, providesErr := values.LayoutProvides(layout, root)
		if providesErr == nil && provided {
			return true
		}
		rootErr = fmt.Errorf(
			"QOV root correlation %s is foreign to input edge %s",
			root.Correlation().Name(), input.Correlation().Name())
		return false
	})
	return rootErr
}

// rebaseStreamingAggregationInputValue validates both sides of the edge
// rewrite. A streaming aggregate's key/operand program is owned by its sole
// input edge, but that edge can share its correlation with a narrower source
// window of a merged row. The rewrite therefore moves only the exact declared
// edge root; accepting a foreign root or alias-rebasing a differently typed
// window would create an incoherent runtime binding.
func rebaseStreamingAggregationInputValue(
	value values.Value,
	oldInput values.QuantifiedObjectValue,
	newInput values.QuantifiedObjectValue,
	oldInputQ expressions.Quantifier,
) (values.Value, error) {
	if value == nil {
		return nil, fmt.Errorf("value is nil")
	}
	// Resolve source windows/current handles against the selected old child
	// before validating ownership. This is essential for a materializer: its
	// output layout is current-only, while a retained program may have been
	// authored against source windows of the materializer's input. The child
	// lineage maps that program to the materialized carrier without republishing
	// those windows at runtime.
	normalized, err := reanchorCurrentValueForInput(value, oldInputQ)
	if err != nil {
		return nil, err
	}
	if err := validateStreamingAggregationOldInputRoots(normalized, oldInput, oldInputQ); err != nil {
		return nil, err
	}
	// An exploratory physical edge may conventionally share its correlation
	// with one source window of a merged join row. Those two objects have
	// different exact types: the edge flows the complete merged row, while the
	// window flows only its source leg. An alias-only rebase rewrites both and
	// destroys the source identity before a selected materializer can map the
	// leg-local ordinal to the merged output slot (duplicate field names then
	// silently select the first leg). Move only the declared whole-edge root;
	// same-correlation/different-type windows remain available to the selected
	// child's exact lineage authority.
	rebased, err := values.TranslateDeclaredEdgeRoot(normalized, oldInput, newInput)
	if err != nil {
		return nil, err
	}
	if rebased == nil {
		return nil, fmt.Errorf("checked rebase returned nil")
	}
	return rebased, nil
}

// WithChildren is the extraction/relink hook (plan_extraction.go's WithChildren
// interface). The aggregation carries its child as a single memo edge, so the
// relink is an atomic child/program rebuild: WithQuantifiers checked-rebases the
// grouping keys and aggregate operands before re-resolving GetInner through the
// new singleton reference. This replaces physicalStreamingAggWrapper.WithChildren
// (RFC-184 W2), whose separate snapshot plan field forced a constructor rebuild
// gated on isLeafReplaceable. Streaming aggregation is a PRODUCER, not on the
// ordering-delegation spine, so the emitter has already frozen (or kept live)
// the ordering-correct inner per arm — extraction recurses through that edge
// faithfully.
func (p *RecordQueryStreamingAggregationPlan) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("RecordQueryStreamingAggregationPlan.WithChildren: expected 1 child, got %d", len(qs))
	}
	return p.WithQuantifiers(qs)
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryStreamingAggregationPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
