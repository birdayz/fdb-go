package plans

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// FetchIndexRecords governs how to interpret the primary key of an
// index entry when fetching the base record. Mirrors Java's
// `RecordQueryFetchFromPartialRecordPlan.FetchIndexRecords` enum.
type FetchIndexRecords int

const (
	// FetchIndexRecordsPrimaryKey fetches the base record by its
	// primary key (the standard path).
	FetchIndexRecordsPrimaryKey FetchIndexRecords = iota
	// FetchIndexRecordsSyntheticConstituents fetches synthetic record
	// constituents (for synthetic/joined record types).
	FetchIndexRecordsSyntheticConstituents
)

// RecordQueryFetchFromPartialRecordPlan transforms a stream of partial
// records (index entries from a covering index scan) into full records
// by fetching via primary key. Mirrors Java's
// `RecordQueryFetchFromPartialRecordPlan`.
//
// The plan has:
//   - An inner plan that produces index entries (partial records).
//   - A TranslateValueFunction that maps values from the full-record
//     domain to the partial-record (index) domain — used by push-through
//     rules to determine which predicates/values can be evaluated before
//     the fetch.
//   - A result type (the full record type post-fetch).
//   - A FetchIndexRecords mode.
type RecordQueryFetchFromPartialRecordPlan struct {
	PlanExprBase
	innerQ                 expressions.Quantifier
	translateValueFunction TranslateValueFunction
	resultType             values.Type
	fetchIndexRecords      FetchIndexRecords
}

// NewRecordQueryFetchFromPartialRecordPlan constructs the plan.
func NewRecordQueryFetchFromPartialRecordPlan(
	inner RecordQueryPlan,
	translateValueFunction TranslateValueFunction,
	resultType values.Type,
	fetchIndexRecords FetchIndexRecords,
) *RecordQueryFetchFromPartialRecordPlan {
	if resultType == nil {
		resultType = values.UnknownType
	}
	if translateValueFunction == nil {
		translateValueFunction = UnableToTranslate
	}
	return &RecordQueryFetchFromPartialRecordPlan{
		innerQ:                 QuantifierOverPlan(inner),
		translateValueFunction: translateValueFunction,
		resultType:             resultType,
		fetchIndexRecords:      fetchIndexRecords,
	}
}

// NewRecordQueryFetchFromPartialRecordPlanFromQuantifier builds a fetch whose
// child is a LIVE memo quantifier (a push/data-access rule passes a
// ForEachQuantifier over the freshly-memoized covering-scan singleton) instead
// of a snapshot over a single plan. This makes the fetch its own cascades
// expression carrying its child edge directly: the memo holds it without a
// physical wrapper, and GetInner / GetQuantifiers / OrderingSourceRef all
// resolve through the one live edge (RFC-184 W2). Mirrors the field defaulting
// of NewRecordQueryFetchFromPartialRecordPlan.
func NewRecordQueryFetchFromPartialRecordPlanFromQuantifier(
	innerQ expressions.Quantifier,
	translateValueFunction TranslateValueFunction,
	resultType values.Type,
	fetchIndexRecords FetchIndexRecords,
) *RecordQueryFetchFromPartialRecordPlan {
	if resultType == nil {
		resultType = values.UnknownType
	}
	if translateValueFunction == nil {
		translateValueFunction = UnableToTranslate
	}
	return &RecordQueryFetchFromPartialRecordPlan{
		innerQ:                 innerQ,
		translateValueFunction: translateValueFunction,
		resultType:             resultType,
		fetchIndexRecords:      fetchIndexRecords,
	}
}

// GetInner returns the inner plan (typically a covering index scan),
// dereferenced through the quantifier.
func (p *RecordQueryFetchFromPartialRecordPlan) GetInner() RecordQueryPlan {
	return planFromQuantifier(p.innerQ)
}

// GetInnerQuantifier returns the live child quantifier — the single memo edge
// the fetch ranges over. Push/data-access rules that match a physical fetch in
// the memo need its inner GROUP (GetRangesOver) and alias to re-plan around it;
// since RFC-184 W2 the memo holds the bare plan (no physicalFetchFromPartialRecordWrapper
// whose innerQuant field they used to read), this exposes the same edge.
func (p *RecordQueryFetchFromPartialRecordPlan) GetInnerQuantifier() expressions.Quantifier {
	return p.innerQ
}

// GetQuantifiers reports the real child quantifier, overriding
// PlanExprBase's none.
func (p *RecordQueryFetchFromPartialRecordPlan) GetQuantifiers() []expressions.Quantifier {
	if p.innerQ.GetRangesOver() == nil {
		return nil
	}
	return []expressions.Quantifier{p.innerQ}
}

// GetResultType returns the full record type post-fetch.
func (p *RecordQueryFetchFromPartialRecordPlan) GetResultType() values.Type { return p.resultType }

// GetFetchIndexRecords returns the fetch mode.
func (p *RecordQueryFetchFromPartialRecordPlan) GetFetchIndexRecords() FetchIndexRecords {
	return p.fetchIndexRecords
}

// GetTranslateValueFunction returns the push-value function.
func (p *RecordQueryFetchFromPartialRecordPlan) GetTranslateValueFunction() TranslateValueFunction {
	return p.translateValueFunction
}

// PushValue attempts to translate a value from the full-record domain
// (correlated to sourceAlias) to the partial-record domain (correlated
// to targetAlias). Returns the translated value and true on success,
// or nil and false if translation is not possible.
//
// Mirrors Java's `RecordQueryFetchFromPartialRecordPlan.pushValue`.
func (p *RecordQueryFetchFromPartialRecordPlan) PushValue(
	value values.Value,
	sourceAlias values.CorrelationIdentifier,
	targetAlias values.CorrelationIdentifier,
) (values.Value, bool) {
	return p.translateValueFunction(value, sourceAlias, targetAlias)
}

// IsReverse delegates to the inner plan's reverse flag. Mirrors Java's
// RecordQueryFetchFromPartialRecordPlan.isReverse() which returns
// getChild().isReverse().
func (p *RecordQueryFetchFromPartialRecordPlan) IsReverse() bool {
	inner := p.GetInner()
	if inner == nil {
		return false
	}
	if rev, ok := inner.(interface{ IsReverse() bool }); ok {
		return rev.IsReverse()
	}
	return false
}

// GetChildren returns the inner plan.
func (p *RecordQueryFetchFromPartialRecordPlan) GetChildren() []RecordQueryPlan {
	inner := p.GetInner()
	if inner == nil {
		return nil
	}
	return []RecordQueryPlan{inner}
}

// structuralKey lists the field that distinguishes this fetch in the memo: the
// fetch mode. The inner is the caller's responsibility via children, so it is
// excluded. The same key drives both EqualsPlanWithoutChildren and
// HashCodeWithoutChildren.
func (p *RecordQueryFetchFromPartialRecordPlan) structuralKey() *structuralKey {
	return newStructuralKey().Int(int(p.fetchIndexRecords))
}

func (p *RecordQueryFetchFromPartialRecordPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryFetchFromPartialRecordPlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

func (p *RecordQueryFetchFromPartialRecordPlan) HashCodeWithoutChildren() uint64 {
	return p.structuralKey().Hash("fetchfrompartialrecordplan|")
}

// Explain renders Fetch(inner).
func (p *RecordQueryFetchFromPartialRecordPlan) Explain() string {
	innerLabel := "<nil>"
	if inner := p.GetInner(); inner != nil {
		innerLabel = inner.Explain()
	}
	return fmt.Sprintf("Fetch(%s)", innerLabel)
}

var (
	_ RecordQueryPlan                  = (*RecordQueryFetchFromPartialRecordPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryFetchFromPartialRecordPlan)(nil)
)

// WithInner returns a copy with the inner replaced and every other field
// preserved — the extraction-relink rebuild path (see findPhysicalPlan's
// shell completion). A constructor rebuild would drop fields the setters
// carry, so identity-preserving copy is the only safe form.
func (p *RecordQueryFetchFromPartialRecordPlan) WithInner(inner RecordQueryPlan) *RecordQueryFetchFromPartialRecordPlan {
	cp := *p
	cp.innerQ = QuantifierOverPlan(inner)
	return &cp
}

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryFetchFromPartialRecordPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// WithQuantifiers returns a copy ranging over the given child quantifier —
// Java's copy-on-write withChild(Reference).
func (p *RecordQueryFetchFromPartialRecordPlan) WithQuantifiers(qs []expressions.Quantifier) expressions.RelationalExpression {
	if len(qs) != 1 {
		return p
	}
	cp := *p
	cp.innerQ = qs[0]
	return &cp
}

// WithChildren is the extraction/relink hook (plan_extraction.go's WithChildren
// interface). Because the fetch carries its child as a single LIVE memo edge, the
// relink is exactly a quantifier swap: WithQuantifiers preserves the translate
// function, result type and fetch mode, and GetInner re-resolves through the new
// singleton reference. This replaces physicalFetchFromPartialRecordWrapper.WithChildren
// (RFC-184 W2), whose separate snapshot plan field forced a constructor rebuild —
// a single live child edge needs none.
func (p *RecordQueryFetchFromPartialRecordPlan) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("RecordQueryFetchFromPartialRecordPlan.WithChildren: expected 1 child, got %d", len(qs))
	}
	return p.WithQuantifiers(qs), nil
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryFetchFromPartialRecordPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
