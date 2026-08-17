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
	if present && value == nil && !values.SharedFlowedType(exact).IsNullable() {
		return nil, false, layoutBindingError(values.LayoutNullabilityMismatch, "non-nullable external QOV is SQL NULL")
	}
	return value, present, nil
}

// edgeObjectBinder binds the exact object flowing over a physical quantifier
// edge. A record edge receives the complete positional row; a scalar edge
// receives the sole slot of its executor carrier. It is deliberately keyed by
// exact QOV (correlation plus exact type), not by an ambient alias lookup.
// The bindings are a LIST, scanned linearly. A binder is built once per ROW and
// holds one entry per physical edge the operator flows: one for a filter or
// projection over a single child, two where the plan also flows an alias root
// alongside its input — which is what the inline array is sized for. An operator
// that flows NONE never gets here at all; initEdgeObjectBinder returns the base
// binder untouched at its first line, so a scan builds no edge binding.
//
// So a map paid its bucket allocation and its hashing on every row to hold one or
// two entries. On a 20k-row wide scan that was 410MB, the largest single allocator
// in the whole benchmark. Keep it a list; if an operator ever flows enough edges
// for the linear scan to matter, that is the signal to reconsider, not the entry
// count of any operator that exists today.
type edgeObjectBinder struct {
	base     values.QuantifiedObjectBinder
	bindings []edgeObjectBinding
	inline   [2]edgeObjectBinding
}

type edgeObjectBinding struct {
	qov            values.QuantifiedObjectValue
	value          any
	explicitAbsent bool
}

func (b *edgeObjectBinder) binding(
	correlation values.CorrelationIdentifier,
) (edgeObjectBinding, bool) {
	for i := range b.bindings {
		if b.bindings[i].qov.Correlation() == correlation {
			return b.bindings[i], true
		}
	}
	return edgeObjectBinding{}, false
}

func newEdgeObjectBinder(
	row *PositionalRow,
	base values.QuantifiedObjectBinder,
	edges ...values.QuantifiedObjectValue,
) (values.QuantifiedObjectBinder, error) {
	return initEdgeObjectBinder(&edgeObjectBinder{}, row, base, edges...)
}

// initEdgeObjectBinder is newEdgeObjectBinder writing into storage the caller
// owns, so a per-row holder can carry the edge binder in its own allocation.
// storage must be zero and must not be reused while the binder is live.
func initEdgeObjectBinder(
	binder *edgeObjectBinder,
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
	binder.base = base
	binder.bindings = binder.inline[:0]
	for edgeIndex, edge := range edges {
		exact, ok := values.AsQuantifiedObjectValue(edge)
		if !ok || exact == nil {
			return nil, layoutBindingError(values.CorrelationForeignValue, fmt.Sprintf("edge %d QOV is not exact", edgeIndex))
		}
		if exact.Correlation() == values.CurrentCorrelation() {
			return nil, layoutBindingError(values.CorrelationKindMismatch, fmt.Sprintf("edge %d cannot use current correlation", edgeIndex))
		}
		// Read-only: flowed is compared against the runtime row shape and never
		// retained, and this runs once per ROW.
		flowed := values.SharedFlowedType(exact)
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
		if previous, exists := binder.binding(exact.Correlation()); exists {
			if !values.QuantifiedRowShapesAgree(values.SharedFlowedType(previous.qov), flowed) {
				return nil, layoutBindingError(values.CorrelationTypeConflict, fmt.Sprintf("edge %d reuses one correlation with another exact type", edgeIndex))
			}
			continue
		}
		binder.bindings = append(binder.bindings, edgeObjectBinding{
			qov: exact, value: whole, explicitAbsent: explicitAbsent,
		})
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
	if binding, exists := b.binding(exact.Correlation()); exists {
		if !values.QuantifiedRowShapesAgree(values.SharedFlowedType(binding.qov), values.SharedFlowedType(exact)) {
			return false, layoutBindingError(values.CorrelationTypeConflict,
				fmt.Sprintf("edge null-presence lookup %s: read as %s, declared %s",
					exact.Correlation().Name(), values.DescribeType(exact.FlowedType()),
					values.DescribeType(binding.qov.FlowedType())))
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
	if binding, exists := b.binding(exact.Correlation()); exists {
		if !values.QuantifiedRowShapesAgree(values.SharedFlowedType(binding.qov), values.SharedFlowedType(exact)) {
			return nil, false, layoutBindingError(values.CorrelationTypeConflict,
				fmt.Sprintf("edge lookup %s: read as %s, declared %s",
					exact.Correlation().Name(), values.DescribeType(exact.FlowedType()),
					values.DescribeType(binding.qov.FlowedType())))
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
//
// It ADOPTS base rather than copying it, and therefore takes ownership: base is
// the context this call returns, with the layout binder installed. Every caller
// mints the base immediately beforehand for exactly this call and never reads it
// again, so copying only bought a second RowEvalContext allocation per ROW — 1.1
// per row on a wide scan, for a value the caller had just finished building. A
// future caller that needs to keep its base must hand over a copy.
// The layout binder is written into storage the caller owns, for the same reason
// base is adopted: the caller has a per-row holder already and the binder is one
// more member of it.
func ordinalLayoutRowContext(
	storage *values.OrdinalBinderStorage,
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
	binder, err := values.InitOrdinalObjectBinder(storage, layout, datum, presence, baseObjects)
	if err != nil {
		return nil, err
	}
	result := base
	if result == nil {
		result = &values.RowEvalContext{}
	}
	result.Objects = binder
	if row, ok := datum.(values.OrdinalRow); ok {
		result.Positional = row
	} else {
		result.Positional = nil
	}
	return result, nil
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

// providedRecordOutputLayout resolves the record layout a plan's output boundary
// publishes on every emitted row, together with the carrier type that boundary
// checks each row against. ok=false means the boundary publishes nothing: a
// dynamic AnyRecord plan has no concrete layout yet and keeps its existing row
// carrier until a physical refinement chooses one, and a scalar carrier is bound
// by the owning evaluation phase rather than attached to a record wrapper. Every
// other unavailable layout is malformed and is reported.
func providedRecordOutputLayout(
	plan plans.RecordQueryPlan,
) (values.OrdinalLayout, values.Type, bool, error) {
	layout, err := plan.ProvidedOutputLayout()
	if err != nil {
		if unavailable, ok := err.(*plans.OrdinalLayoutUnavailableError); ok &&
			unavailable.Code == plans.OrdinalLayoutDynamicCarrier {
			return nil, nil, false, nil
		}
		return nil, nil, false, fmt.Errorf("executor: %T provided output layout: %w", plan, err)
	}
	if layout == nil {
		return nil, nil, false, layoutBindingError(values.LayoutCarrierMismatch, "plan returned nil provided layout")
	}
	if layout.CarrierKind() != values.OrdinalCarrierRecord {
		return nil, nil, false, nil
	}
	carrier := layout.Carrier()
	if carrier == nil {
		return nil, nil, false, layoutBindingError(values.LayoutCarrierMismatch, "record layout has no carrier")
	}
	// The carrier type is derived ONCE per cursor, because it is a property of
	// the LAYOUT and not of a row: this thaws a fresh Type graph out of the
	// carrier's exact handle, and the carrier is the same object for every row
	// the cursor will ever emit. Deriving it per row made a 1M-row scan thaw 1M
	// Type graphs to compare each against a row shape that had not changed.
	return layout, carrier.FlowedType(), true, nil
}

// mintedRowLayout is providedRecordOutputLayout for a plan that is about to MINT
// its own rows, and it exists to make the output boundary below a pointer check
// instead of a per-row row copy.
//
// AttachOrdinalLayout must copy a row whose layout differs, because a row
// arriving at a boundary may still be held by whoever produced it — a join
// re-emitting one outer row, a sort keeping its buffer. A row the plan is
// building right now has exactly one owner, so stamping the handle it is about
// to be held to costs nothing and the boundary then takes its identity fast
// path. Measured on a 20k-row wide scan, whose plan is a projection over a scan
// and therefore crosses two boundaries, the copies were 4.4 allocations per row
// — the largest single item in the row path.
//
// A resolution failure returns nil rather than propagating: the boundary re-runs
// the identical resolution and is the ONE place that reports it, so a producer
// that swallowed it would only hide the report behind an unstamped row.
func mintedRowLayout(plan plans.RecordQueryPlan) values.OrdinalLayout {
	layout, _, ok, err := providedRecordOutputLayout(plan)
	if err != nil || !ok {
		return nil
	}
	return layout
}

// attachProvidedOutputLayout publishes a concrete plan's selected physical
// output property on each emitted row. It is intentionally a cursor adapter:
// continuations remain the child's bytes, while every resumed value is checked
// against the same plan-authoritative layout before it reaches a parent.
//
// It stays the authority even for a producer that stamped the handle itself at
// mint time (see mintedRowLayout): the row-type-against-carrier check runs
// before the identity fast path, so a stamped row is still verified here.
func attachProvidedOutputLayout(
	plan plans.RecordQueryPlan,
	cursor recordlayer.RecordCursor[QueryResult],
) (recordlayer.RecordCursor[QueryResult], error) {
	if plan == nil || cursor == nil {
		return nil, layoutBindingError(values.LayoutCarrierMismatch, "plan and cursor must be non-nil")
	}
	layout, carrierType, ok, err := providedRecordOutputLayout(plan)
	if err != nil {
		return nil, err
	}
	if !ok {
		return cursor, nil
	}
	return recordlayer.MapErrCursor(cursor, func(result QueryResult) (QueryResult, error) {
		if result.Positional == nil {
			return QueryResult{}, layoutBindingError(values.LayoutRuntimeShape, "record plan emitted no positional row")
		}
		row, attachErr := result.Positional.AttachOrdinalLayout(layout, carrierType)
		if attachErr != nil {
			return QueryResult{}, fmt.Errorf("executor: %T emitted row type %s outside provided layout carrier %T: %w",
				plan, result.Positional.Type, layout.Carrier(), attachErr)
		}
		result.Positional = row
		return result, nil
	}), nil
}
