package plans

import (
	"fmt"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryCoveringIndexValuePlan is Java's RecordQueryCoveringIndexValuePlan:
// an index scan that builds the queried record from each entry with an entry
// reader Value instead of copiers. The planner never emits it, as Java's does
// not; it is built by hand around a planned covering scan. Like the covering
// plan, the index plan is a field, not a child.
type RecordQueryCoveringIndexValuePlan struct {
	PlanExprBase
	indexPlan      *RecordQueryIndexPlan
	recordTypeName string
	reader         *values.RecordConstructorValue
	resultValue    values.Value
}

// NewRecordQueryCoveringIndexValuePlan reads indexPlan's entries into
// recordTypeName's record with reader.
func NewRecordQueryCoveringIndexValuePlan(
	indexPlan *RecordQueryIndexPlan, recordTypeName string, reader *values.RecordConstructorValue,
) (*RecordQueryCoveringIndexValuePlan, error) {
	if indexPlan == nil || reader == nil || recordTypeName == "" {
		return nil, fmt.Errorf("RecordQueryCoveringIndexValuePlan: needs an index plan, a record type and a reader")
	}
	base, err := newPlanExprBaseForType("RecordQueryCoveringIndexValuePlan", indexPlan.GetResultType())
	if err != nil {
		return nil, err
	}
	return &RecordQueryCoveringIndexValuePlan{
		PlanExprBase:   base,
		indexPlan:      indexPlan,
		recordTypeName: recordTypeName,
		reader:         reader,
		resultValue:    base.resultValue,
	}, nil
}

func (p *RecordQueryCoveringIndexValuePlan) inner() *RecordQueryIndexPlan {
	if p == nil {
		return nil
	}
	return p.indexPlan
}

// GetIndexPlan returns the scanned index plan (a field, not a child).
func (p *RecordQueryCoveringIndexValuePlan) GetIndexPlan() *RecordQueryIndexPlan { return p.inner() }

// WithIndexPlan returns a copy over a rewritten index plan; the reader stays.
func (p *RecordQueryCoveringIndexValuePlan) WithIndexPlan(inner *RecordQueryIndexPlan) *RecordQueryCoveringIndexValuePlan {
	cp := *p
	cp.indexPlan = inner
	return &cp
}

// GetRecordTypeName is the queried record type the reader builds.
func (p *RecordQueryCoveringIndexValuePlan) GetRecordTypeName() string { return p.recordTypeName }

// GetIndexEntryToRecordValue is the entry reader.
func (p *RecordQueryCoveringIndexValuePlan) GetIndexEntryToRecordValue() *values.RecordConstructorValue {
	return p.reader
}

// GetIndexName delegates to the index plan.
func (p *RecordQueryCoveringIndexValuePlan) GetIndexName() string { return p.indexPlan.GetIndexName() }

// IsReverse delegates to the index plan.
func (p *RecordQueryCoveringIndexValuePlan) IsReverse() bool { return p.indexPlan.IsReverse() }

// IsStrictlySorted delegates to the index plan.
func (p *RecordQueryCoveringIndexValuePlan) IsStrictlySorted() bool {
	return p.indexPlan.IsStrictlySorted()
}

// ProducesDistinctRecords delegates to the index plan.
func (p *RecordQueryCoveringIndexValuePlan) ProducesDistinctRecords() bool {
	return p.indexPlan.ProducesDistinctRecords()
}

// GetResultValue returns the constructor-minted result value.
func (p *RecordQueryCoveringIndexValuePlan) GetResultValue() values.Value { return p.resultValue }

// GetResultType is the index plan's flowed record type.
func (p *RecordQueryCoveringIndexValuePlan) GetResultType() values.Type {
	return p.indexPlan.GetResultType()
}

// GetChildren returns nil: the index plan is a field.
func (p *RecordQueryCoveringIndexValuePlan) GetChildren() []RecordQueryPlan { return nil }

// structuralKey folds the index plan, the record type and the reader, as
// Java's structuralEquals does.
func (p *RecordQueryCoveringIndexValuePlan) structuralKey() *structuralKey {
	return newStructuralKey().
		Sub(p.indexPlan.structuralKey()).
		Str(p.recordTypeName).
		Value(p.reader)
}

func (p *RecordQueryCoveringIndexValuePlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryCoveringIndexValuePlan)
	return ok && p.keyFor(p).Equal(o.keyFor(o))
}

func (p *RecordQueryCoveringIndexValuePlan) HashCodeWithoutChildren() uint64 {
	if hash, ok := p.cachedStructuralHash(p); ok {
		return hash
	}
	hash := p.keyFor(p).Hash("coveringindexvalueplan|")
	p.storeStructuralHash(p, hash)
	return hash
}

// Explain renders the covering scan with the reader after the arrow, the only
// place Java's two covering plans explain differently:
// IndexScan(IDX, [=] COVERING -> {ID: KEY:[1], A: KEY:[0]}).
func (p *RecordQueryCoveringIndexValuePlan) Explain() string {
	label := p.indexPlan.explainWithCovering()
	i := strings.Index(label, " COVERING")
	if i < 0 {
		return label
	}
	i += len(" COVERING")
	var b strings.Builder
	b.WriteString(label[:i])
	b.WriteString(" -> {")
	for j, field := range p.reader.Fields {
		if j > 0 {
			b.WriteString(", ")
		}
		b.WriteString(field.Name)
		b.WriteString(": ")
		b.WriteString(values.ExplainValue(field.Value))
	}
	b.WriteString("}")
	b.WriteString(label[i:])
	return b.String()
}

var (
	_ RecordQueryPlan                  = (*RecordQueryCoveringIndexValuePlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryCoveringIndexValuePlan)(nil)
)

func (p *RecordQueryCoveringIndexValuePlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// WithQuantifiers returns the plan unchanged; it has none.
func (p *RecordQueryCoveringIndexValuePlan) WithQuantifiers(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if err := validateQuantifierArity("RecordQueryCoveringIndexValuePlan", len(qs), 0); err != nil {
		return nil, err
	}
	return p, nil
}

// GetCorrelatedToWithoutChildren is the index plan's correlations.
func (p *RecordQueryCoveringIndexValuePlan) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return p.indexPlan.GetCorrelatedToWithoutChildren()
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryCoveringIndexValuePlan) GetRecordQueryPlan() RecordQueryPlan { return p }

// GetScanComparisons delegates to the index plan.
func (p *RecordQueryCoveringIndexValuePlan) GetScanComparisons() []*predicates.ComparisonRange {
	return p.indexPlan.GetScanComparisons()
}

// GetKeyComponentTypes delegates to the index plan.
func (p *RecordQueryCoveringIndexValuePlan) GetKeyComponentTypes() []values.Type {
	return p.indexPlan.GetKeyComponentTypes()
}

// GetRecordTypes delegates to the index plan.
func (p *RecordQueryCoveringIndexValuePlan) GetRecordTypes() []string {
	return p.indexPlan.GetRecordTypes()
}

// GetFlowedType delegates to the index plan.
func (p *RecordQueryCoveringIndexValuePlan) GetFlowedType() values.Type {
	return p.indexPlan.GetFlowedType()
}

// GetPKColumnNames delegates to the index plan.
func (p *RecordQueryCoveringIndexValuePlan) GetPKColumnNames() []string {
	return p.indexPlan.GetPKColumnNames()
}

// GetCommonPrimaryKeyValues delegates to the index plan.
func (p *RecordQueryCoveringIndexValuePlan) GetCommonPrimaryKeyValues() []values.Value {
	return p.indexPlan.GetCommonPrimaryKeyValues()
}

// HintCost delegates to the index plan.
func (p *RecordQueryCoveringIndexValuePlan) HintCost(children []properties.Cost, stats properties.StatisticsProvider) properties.Cost {
	return p.inner().HintCost(children, stats)
}

// ProvenCardinalities delegates to the index plan
// (CardinalitiesVisitor.visitRecordQueryCoveringIndexValuePlan).
func (p *RecordQueryCoveringIndexValuePlan) ProvenCardinalities(children []properties.Cardinalities) properties.Cardinalities {
	return p.inner().ProvenCardinalities(children)
}

// HintOrdering delegates to the index plan.
func (p *RecordQueryCoveringIndexValuePlan) HintOrdering() properties.Ordering {
	return p.inner().HintOrdering()
}

// HintRichOrdering delegates to the index plan.
func (p *RecordQueryCoveringIndexValuePlan) HintRichOrdering() *properties.RichOrdering {
	return p.inner().HintRichOrdering()
}
