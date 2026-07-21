package plans

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryAggregateIndexPlan wraps an index scan that reads from
// an aggregate index (e.g. SUM, COUNT) and reconstructs records from
// the index entries. This is a leaf plan (no children — the wrapped
// RecordQueryIndexPlan is a structural field, not a child in the plan
// tree sense). Mirrors Java's RecordQueryAggregateIndexPlan.
//
// Fields:
//   - indexPlan: the underlying index scan plan.
//   - recordTypeName: the base record type name (for metadata lookup).
//   - resultType: the rich Type of the aggregated result row.
//   - aggregateFunction: the name of the aggregate function
//     (e.g. "SUM", "COUNT", "MIN", "MAX").
type RecordQueryAggregateIndexPlan struct {
	PlanExprBase
	indexPlan         *RecordQueryIndexPlan
	recordTypeName    string
	resultType        values.Type
	aggregateFunction string
	groupCols         []string
	aggColumn         string
	// resultValue is the stable per-instance QuantifiedObjectValue standing for
	// the rows this leaf emits — minted once at construction, returned by
	// GetResultValue, EXCLUDED from Equals/Hash (its correlation id is unique per
	// instance). A bare leaf that stands as its own Cascades expression must
	// present a consistent row identity across repeated interrogations, the role
	// physicalAggregateIndexWrapper's fresh-per-call GetResultValue could not
	// (RFC-184 W2). nil for struct-literal test plans that bypass the constructor —
	// GetResultValue falls back to PlanExprBase's fresh QOV there.
	resultValue values.Value
}

// NewRecordQueryAggregateIndexPlan constructs an aggregate index plan.
func NewRecordQueryAggregateIndexPlan(
	indexPlan *RecordQueryIndexPlan,
	recordTypeName string,
	resultType values.Type,
	aggregateFunction string,
) *RecordQueryAggregateIndexPlan {
	if resultType == nil {
		resultType = values.UnknownType
	}
	return &RecordQueryAggregateIndexPlan{
		indexPlan:         indexPlan,
		recordTypeName:    recordTypeName,
		resultType:        resultType,
		aggregateFunction: aggregateFunction,
		resultValue:       values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier()),
	}
}

// GetResultValue returns the aggregate-index plan's STABLE per-instance result
// value — the single correlation identity a bare aggregate-index plan carries as
// its own memo expression (RFC-184 W2). Falls back to PlanExprBase (a fresh QOV
// per call) for struct-literal test plans that bypass the constructor
// (resultValue is nil).
func (p *RecordQueryAggregateIndexPlan) GetResultValue() values.Value {
	if p.resultValue == nil {
		return p.PlanExprBase.GetResultValue()
	}
	return p.resultValue
}

// WithGroupColumns sets the grouping and aggregate column names for
// the executor to map index entries to result rows.
func (p *RecordQueryAggregateIndexPlan) WithGroupColumns(groupCols []string, aggColumn string) *RecordQueryAggregateIndexPlan {
	p.groupCols = groupCols
	p.aggColumn = aggColumn
	return p
}

// GetGroupCols returns the grouping column names.
func (p *RecordQueryAggregateIndexPlan) GetGroupCols() []string { return p.groupCols }

// GetAggColumn returns the aggregate column name.
func (p *RecordQueryAggregateIndexPlan) GetAggColumn() string { return p.aggColumn }

// CanonicalAggColumnName returns the canonical column name the executor's
// aggregateIndexCursor writes the aggregate value under: "FUNC(*)" for an
// empty aggColumn (e.g. COUNT(*)), else "FUNC(col)". Single source of that
// name so the cursor and planColumnNamesWithMD stay byte-identical.
func (p *RecordQueryAggregateIndexPlan) CanonicalAggColumnName() string {
	if p.aggColumn == "" {
		return p.aggregateFunction + "(*)"
	}
	return p.aggregateFunction + "(" + p.aggColumn + ")"
}

// OutputColumnNames returns the column names this plan's rows are keyed by —
// the grouping columns (verbatim, as the cursor writes them) followed by the
// canonical aggregate name. A bare aggregate-index plan is always UNALIASED
// (an aliased SELECT tops with a Project that owns the rename), so there is no
// alias to carry; these are exactly the keys aggregateIndexCursor writes. Used
// by planColumnNamesWithMD so a UNION position-remap can normalize a grouped
// aggregate-index branch (RFC-081).
func (p *RecordQueryAggregateIndexPlan) OutputColumnNames() []string {
	names := make([]string, 0, len(p.groupCols)+1)
	names = append(names, p.groupCols...)
	return append(names, p.CanonicalAggColumnName())
}

// GetIndexPlan returns the underlying index plan.
func (p *RecordQueryAggregateIndexPlan) GetIndexPlan() *RecordQueryIndexPlan { return p.indexPlan }

// GetRecordTypeName returns the base record type name.
func (p *RecordQueryAggregateIndexPlan) GetRecordTypeName() string { return p.recordTypeName }

// GetAggregateFunction returns the aggregate function name.
func (p *RecordQueryAggregateIndexPlan) GetAggregateFunction() string { return p.aggregateFunction }

// GetIndexName returns the index name from the underlying plan.
func (p *RecordQueryAggregateIndexPlan) GetIndexName() string {
	return p.indexPlan.GetIndexName()
}

// IsReverse delegates to the underlying index plan.
func (p *RecordQueryAggregateIndexPlan) IsReverse() bool {
	return p.indexPlan.IsReverse()
}

// GetResultType returns the aggregate result type.
func (p *RecordQueryAggregateIndexPlan) GetResultType() values.Type { return p.resultType }

// GetChildren returns nil — this is a leaf plan. The wrapped index
// plan is a structural field, not a child (mirrors Java where
// RecordQueryAggregateIndexPlan implements
// RecordQueryPlanWithNoChildren).
func (p *RecordQueryAggregateIndexPlan) GetChildren() []RecordQueryPlan { return nil }

// structuralKey folds the aggregate-index identity: the base record-type name,
// the aggregate-function name, and the embedded RecordQueryIndexPlan compared
// STRUCTURALLY via Sub (its own structuralKey — exactly what
// indexPlan.EqualsPlanWithoutChildren does). The stable per-instance resultValue
// is excluded (RFC-184 W2). Drives both Equals and Hash — the hash now folds the
// full nested index key (the hand-rolled hash folded only the index NAME),
// strengthening it while preserving equal⟹same-hash.
func (p *RecordQueryAggregateIndexPlan) structuralKey() *structuralKey {
	return newStructuralKey().
		Str(p.recordTypeName).
		Str(p.aggregateFunction).
		Sub(p.indexPlan.structuralKey())
}

// EqualsWithoutChildren compares index plan, record type name, and
// result type.
func (p *RecordQueryAggregateIndexPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryAggregateIndexPlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

// HashCodeWithoutChildren mixes index plan hash, record type, and
// aggregate function.
func (p *RecordQueryAggregateIndexPlan) HashCodeWithoutChildren() uint64 {
	return p.structuralKey().Hash("aggregateindexplan|")
}

// Explain renders AggregateIndex(function, indexName, [groupCols], recordType).
func (p *RecordQueryAggregateIndexPlan) Explain() string {
	if len(p.groupCols) > 0 {
		return fmt.Sprintf("AggregateIndex(%s, %s, %v, %s)",
			p.aggregateFunction, p.indexPlan.GetIndexName(), p.groupCols, p.recordTypeName)
	}
	return fmt.Sprintf("AggregateIndex(%s, %s, %s)",
		p.aggregateFunction, p.indexPlan.GetIndexName(), p.recordTypeName)
}

var (
	_ RecordQueryPlan                  = (*RecordQueryAggregateIndexPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryAggregateIndexPlan)(nil)
)

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryAggregateIndexPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// WithQuantifiers returns this plan unchanged — it has no quantifiers to
// replace while children are raw pointers (RFC-183 P5 step 1).
func (p *RecordQueryAggregateIndexPlan) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return p
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryAggregateIndexPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
