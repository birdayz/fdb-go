package executor

import (
	"fmt"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// evaluationObjectBinder is the migration adapter for genuine external/outer
// correlations already carried by EvaluationContext. Local/current roots are
// always intercepted by the selected OrdinalLayout before this base is asked.
type evaluationObjectBinder struct {
	base values.CorrelationBinder
}

func (b *evaluationObjectBinder) IsExplicitNullQuantifiedBinding(qov values.QuantifiedObjectValue) (bool, error) {
	if b == nil || b.base == nil {
		return false, nil
	}
	if proof, ok := b.base.(values.ExplicitNullQuantifiedObjectBinder); ok {
		return proof.IsExplicitNullQuantifiedBinding(qov)
	}
	return false, nil
}

func (b *evaluationObjectBinder) GetQuantifiedBinding(qov values.QuantifiedObjectValue) (any, bool, error) {
	exact, ok := values.AsQuantifiedObjectValue(qov)
	if !ok || exact == nil {
		return nil, false, layoutBindingError(values.CorrelationForeignValue, "external binding QOV is not exact")
	}
	if b == nil || b.base == nil {
		return nil, false, nil
	}
	if exactBase, ok := b.base.(values.QuantifiedObjectBinder); ok {
		value, present, err := exactBase.GetQuantifiedBinding(qov)
		if err != nil || present {
			return value, present, err
		}
	}
	value, present := b.base.GetCorrelationBinding(exact.Correlation())
	if present && value == nil && !exact.FlowedType().IsNullable() {
		return nil, false, layoutBindingError(values.LayoutNullabilityMismatch, "non-nullable external QOV is SQL NULL")
	}
	return value, present, nil
}

// edgeObjectBinder binds the exact object flowing over a physical quantifier
// edge. A record edge receives the complete positional row; a scalar edge
// receives the sole slot of its executor carrier. It is deliberately keyed by
// exact QOV (correlation plus exact type), not by an ambient alias lookup.
type edgeObjectBinder struct {
	base     values.QuantifiedObjectBinder
	bindings map[values.CorrelationIdentifier]edgeObjectBinding
}

type edgeObjectBinding struct {
	qov            values.QuantifiedObjectValue
	value          any
	explicitAbsent bool
}

func newEdgeObjectBinder(
	row *PositionalRow,
	base values.QuantifiedObjectBinder,
	edges ...values.QuantifiedObjectValue,
) (values.QuantifiedObjectBinder, error) {
	if len(edges) == 0 {
		return base, nil
	}
	if row == nil || row.Type == nil {
		return nil, layoutBindingError(values.LayoutRuntimeShape, "edge binding requires a typed positional row")
	}
	wholeRow, explicitAbsent, err := row.wholeObjectBinding()
	if err != nil {
		return nil, err
	}
	binder := &edgeObjectBinder{
		base:     base,
		bindings: make(map[values.CorrelationIdentifier]edgeObjectBinding, len(edges)),
	}
	for edgeIndex, edge := range edges {
		exact, ok := values.AsQuantifiedObjectValue(edge)
		if !ok || exact == nil {
			return nil, layoutBindingError(values.CorrelationForeignValue, fmt.Sprintf("edge %d QOV is not exact", edgeIndex))
		}
		if exact.Correlation() == values.CurrentCorrelation() {
			return nil, layoutBindingError(values.CorrelationKindMismatch, fmt.Sprintf("edge %d cannot use current correlation", edgeIndex))
		}
		flowed := exact.FlowedType()
		var whole any
		switch flowed.Code() {
		case values.TypeCodeRecord:
			if !row.Type.Equals(flowed) {
				return nil, layoutBindingError(values.LayoutTypeMismatch, fmt.Sprintf("edge %d record type disagrees with runtime row", edgeIndex))
			}
			whole = wholeRow
		default:
			if len(row.Slots) != 1 || len(row.Type.Fields) != 1 || !row.Type.Fields[0].FieldType.Equals(flowed) {
				return nil, layoutBindingError(values.LayoutTypeMismatch, fmt.Sprintf("edge %d scalar type disagrees with runtime row", edgeIndex))
			}
			if explicitAbsent {
				whole = nil
			} else {
				whole = row.Slots[0]
			}
			if whole == nil && !explicitAbsent && !flowed.IsNullable() {
				return nil, layoutBindingError(values.LayoutNullabilityMismatch, fmt.Sprintf("edge %d non-nullable scalar is SQL NULL", edgeIndex))
			}
		}
		if previous, exists := binder.bindings[exact.Correlation()]; exists {
			if !previous.qov.FlowedType().Equals(flowed) {
				return nil, layoutBindingError(values.CorrelationTypeConflict, fmt.Sprintf("edge %d reuses one correlation with another exact type", edgeIndex))
			}
			continue
		}
		binder.bindings[exact.Correlation()] = edgeObjectBinding{
			qov: exact, value: whole, explicitAbsent: explicitAbsent,
		}
	}
	return binder, nil
}

// IsExplicitNullQuantifiedBinding proves the one exceptional non-nullable nil
// edge: an exact FirstOrDefault record shell marked absent as a whole object.
// A plain nil from any other binder remains a nullability mismatch in values'
// ordinal binder. The lookup repeats exact type validation so a same-spelled or
// differently-typed QOV cannot borrow another edge's absence proof.
func (b *edgeObjectBinder) IsExplicitNullQuantifiedBinding(view values.QuantifiedObjectValue) (bool, error) {
	exact, ok := values.AsQuantifiedObjectValue(view)
	if !ok || exact == nil {
		return false, layoutBindingError(values.CorrelationForeignValue, "edge null-presence lookup QOV is not exact")
	}
	if binding, exists := b.bindings[exact.Correlation()]; exists {
		if !binding.qov.FlowedType().Equals(exact.FlowedType()) {
			return false, layoutBindingError(values.CorrelationTypeConflict, "edge null-presence lookup type disagrees with declared binding")
		}
		return binding.explicitAbsent, nil
	}
	if base, ok := b.base.(values.ExplicitNullQuantifiedObjectBinder); ok {
		return base.IsExplicitNullQuantifiedBinding(view)
	}
	return false, nil
}

func (b *edgeObjectBinder) GetQuantifiedBinding(view values.QuantifiedObjectValue) (any, bool, error) {
	exact, ok := values.AsQuantifiedObjectValue(view)
	if !ok || exact == nil {
		return nil, false, layoutBindingError(values.CorrelationForeignValue, "edge lookup QOV is not exact")
	}
	if binding, exists := b.bindings[exact.Correlation()]; exists {
		if !binding.qov.FlowedType().Equals(exact.FlowedType()) {
			return nil, false, layoutBindingError(values.CorrelationTypeConflict, "edge lookup type disagrees with declared binding")
		}
		return binding.value, true, nil
	}
	if b.base != nil {
		return b.base.GetQuantifiedBinding(view)
	}
	return nil, false, nil
}

// ordinalLayoutRowContext exact-recognizes executor-owned carriers before
// asking values to materialize their private layout windows. The returned
// context preserves all base evaluation capabilities and installs the exact
// QOV binder; QOV evaluation therefore cannot use the ambient positional-row
// fallback.
func ordinalLayoutRowContext(
	layout values.OrdinalLayout,
	carrier any,
	presence values.WindowMatchPresence,
	base *values.RowEvalContext,
) (*values.RowEvalContext, error) {
	var datum any
	switch typed := carrier.(type) {
	case *PositionalRow:
		if typed == nil || typed.Layout == nil || typed.Layout != layout {
			return nil, layoutBindingError(values.LayoutCarrierMismatch, "positional carrier does not retain the selected exact layout")
		}
		datum = typed
	default:
		return nil, layoutBindingError(values.LayoutForeignValue, "runtime carrier is not executor-owned")
	}

	var baseObjects values.QuantifiedObjectBinder
	if base != nil {
		baseObjects = base.Objects
	}
	binder, err := values.NewOrdinalObjectBinder(layout, datum, presence, baseObjects)
	if err != nil {
		return nil, err
	}
	var result values.RowEvalContext
	if base != nil {
		result = *base
	}
	result.Objects = binder
	if row, ok := datum.(values.OrdinalRow); ok {
		result.Positional = row
	} else {
		result.Positional = nil
	}
	return &result, nil
}

// scalarLayoutRowContext binds the executor's one-slot wrapper for a scalar
// physical result to the plan-owned scalar layout. Scalar plans intentionally
// do not attach that layout to the wrapper (the wrapper is a record-shaped
// transport, not the scalar carrier); the owning evaluation phase unwraps the
// sole slot and supplies it to the exact current-carrier binder instead.
func scalarLayoutRowContext(
	layout values.OrdinalLayout,
	row *PositionalRow,
	base *EvaluationContext,
	edges ...values.QuantifiedObjectValue,
) (*values.RowEvalContext, error) {
	if layout == nil || layout.CarrierKind() != values.OrdinalCarrierScalar {
		return nil, layoutBindingError(values.LayoutTypeMismatch, "scalar evaluation requires a scalar provided layout")
	}
	if row == nil || row.Type == nil || len(row.Slots) != 1 || len(row.Type.Fields) != 1 {
		return nil, layoutBindingError(values.LayoutRuntimeShape, "scalar evaluation requires one typed transport slot")
	}
	result := rowEvalContextForPositional(row, base)
	edgeBinder, err := newEdgeObjectBinder(row, result.Objects, edges...)
	if err != nil {
		return nil, err
	}
	binder, err := values.NewOrdinalObjectBinder(layout, row.Slots[0], row.LayoutPresence, edgeBinder)
	if err != nil {
		return nil, err
	}
	result.Objects = binder
	return result, nil
}

func layoutBindingError(code values.ResolutionErrorCode, detail string) error {
	return &values.ResolutionError{ErrorCode: code, Path: "executor.layout", Detail: detail}
}

func requireSoleInputQOV(plan plans.RecordQueryPlan) (values.QuantifiedObjectValue, error) {
	if plan == nil {
		return nil, layoutBindingError(values.LayoutCarrierMismatch, "input owner is nil")
	}
	quantifiers := plan.GetQuantifiers()
	if len(quantifiers) != 1 {
		return nil, layoutBindingError(values.LayoutTypeMismatch, fmt.Sprintf("%T has %d input quantifiers, want one", plan, len(quantifiers)))
	}
	qov, err := quantifiers[0].RequireFlowedObjectValue()
	if err != nil {
		return nil, fmt.Errorf("%T input QOV: %w", plan, err)
	}
	return qov, nil
}

// attachProvidedOutputLayout publishes a concrete plan's selected physical
// output property on each emitted row. It is intentionally a cursor adapter:
// continuations remain the child's bytes, while every resumed value is checked
// against the same plan-authoritative layout before it reaches a parent.
//
// A dynamic AnyRecord plan has no concrete layout yet and keeps its existing
// row carrier until a physical refinement chooses one. Every other unavailable
// layout is malformed and fails before a row is exposed.
func attachProvidedOutputLayout(
	plan plans.RecordQueryPlan,
	cursor recordlayer.RecordCursor[QueryResult],
) (recordlayer.RecordCursor[QueryResult], error) {
	if plan == nil || cursor == nil {
		return nil, layoutBindingError(values.LayoutCarrierMismatch, "plan and cursor must be non-nil")
	}
	layout, err := plan.ProvidedOutputLayout()
	if err != nil {
		if unavailable, ok := err.(*plans.OrdinalLayoutUnavailableError); ok &&
			unavailable.Code == plans.OrdinalLayoutDynamicCarrier {
			return cursor, nil
		}
		return nil, fmt.Errorf("executor: %T provided output layout: %w", plan, err)
	}
	if layout == nil {
		return nil, layoutBindingError(values.LayoutCarrierMismatch, "plan returned nil provided layout")
	}
	if layout.CarrierKind() != values.OrdinalCarrierRecord {
		// Scalar plans currently materialize their result as a one-column
		// QueryResult at the executor boundary. Their scalar carrier is bound in
		// the owning evaluation phase rather than attached to a record wrapper.
		return cursor, nil
	}
	return recordlayer.MapErrCursor(cursor, func(result QueryResult) (QueryResult, error) {
		if result.Positional == nil {
			return QueryResult{}, layoutBindingError(values.LayoutRuntimeShape, "record plan emitted no positional row")
		}
		row, attachErr := result.Positional.AttachOrdinalLayout(layout)
		if attachErr != nil {
			return QueryResult{}, fmt.Errorf("executor: %T emitted row type %s outside provided layout carrier %T: %w",
				plan, result.Positional.Type, layout.Carrier(), attachErr)
		}
		result.Positional = row
		return result, nil
	}), nil
}
