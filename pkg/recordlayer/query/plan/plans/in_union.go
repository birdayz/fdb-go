package plans

import (
	"encoding/binary"
	"fmt"
	"reflect"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryInUnionPlan is the IN-union variant: the inner plan is
// executed once per IN-source value, and results are merge-sorted by
// comparison keys. Mirrors Java's RecordQueryInUnionOnValuesPlan.
type RecordQueryInUnionPlan struct {
	PlanExprBase
	innerQ         expressions.Quantifier
	bindingNames   []string
	comparisonKeys []values.Value
	reverse        bool
	maxSize        int
	inSources      [][]any
}

func NewRecordQueryInUnionPlan(
	inner RecordQueryPlan,
	bindingNames []string,
	comparisonKeys []values.Value,
	reverse bool,
) *RecordQueryInUnionPlan {
	bn := make([]string, len(bindingNames))
	copy(bn, bindingNames)
	ck := make([]values.Value, len(comparisonKeys))
	copy(ck, comparisonKeys)
	return &RecordQueryInUnionPlan{
		innerQ:         QuantifierOverPlan(inner),
		bindingNames:   bn,
		comparisonKeys: ck,
		reverse:        reverse,
	}
}

func NewRecordQueryInUnionPlanWithMaxSize(
	inner RecordQueryPlan,
	bindingNames []string,
	comparisonKeys []values.Value,
	reverse bool,
	maxSize int,
) *RecordQueryInUnionPlan {
	p := NewRecordQueryInUnionPlan(inner, bindingNames, comparisonKeys, reverse)
	p.maxSize = maxSize
	return p
}

func (p *RecordQueryInUnionPlan) GetInner() RecordQueryPlan { return planFromQuantifier(p.innerQ) }

// GetQuantifiers reports the real child quantifier, overriding
// PlanExprBase's none.
func (p *RecordQueryInUnionPlan) GetQuantifiers() []expressions.Quantifier {
	if p.innerQ.GetRangesOver() == nil {
		return nil
	}
	return []expressions.Quantifier{p.innerQ}
}

// WithInner returns a copy with the inner replaced and EVERY other field
// preserved (bindingNames, comparisonKeys, reverse, maxSize, inSources) —
// the extraction-relink rebuild path.
func (p *RecordQueryInUnionPlan) WithInner(inner RecordQueryPlan) *RecordQueryInUnionPlan {
	cp := *p
	cp.innerQ = QuantifierOverPlan(inner)
	return &cp
}
func (p *RecordQueryInUnionPlan) GetBindingNames() []string         { return p.bindingNames }
func (p *RecordQueryInUnionPlan) GetComparisonKeys() []values.Value { return p.comparisonKeys }
func (p *RecordQueryInUnionPlan) IsReverse() bool                   { return p.reverse }
func (p *RecordQueryInUnionPlan) GetMaxSize() int                   { return p.maxSize }
func (p *RecordQueryInUnionPlan) GetInSources() [][]any             { return p.inSources }
func (p *RecordQueryInUnionPlan) SetInSources(sources [][]any)      { p.inSources = sources }

func (p *RecordQueryInUnionPlan) GetResultType() values.Type {
	if inner := p.GetInner(); inner != nil {
		return inner.GetResultType()
	}
	return values.UnknownType
}

func (p *RecordQueryInUnionPlan) GetChildren() []RecordQueryPlan {
	inner := p.GetInner()
	if inner == nil {
		return nil
	}
	return []RecordQueryPlan{inner}
}

// structuralKey folds the InUnion identity. reverse is direct. bindingNames
// contribute only their COUNT (Int): they are internal correlation aliases
// minted by UniqueCorrelationIdentifier (a process-global counter) — comparing
// the arbitrary names made every replanned IN-union non-equal → plan-cache churn
// + nondeterministic Explain (RFC-164 WS-4). comparisonKeys and inSources join
// identity per Java RecordQueryInUnionPlan.equalsWithoutChildren (both set before
// the plan is memoized, so sibling alternatives differing in merge keys or
// IN-literals must NOT collapse): comparisonKeys via semantic Value equality
// (Values), inSources via reflect.DeepEqual (Equatable). Drives both Equals/Hash.
//
// The inSources hash folds only DIMENSIONS (len + per-dim len), never the literal
// payloads: hashing arbitrary `any` comparands bit-exactly would break
// equal⟹same-hash the other way (DeepEqual treats +0.0 == -0.0 for floats, whose
// bits differ). Same-shape different-literal collisions are resolved by the eq.
func (p *RecordQueryInUnionPlan) structuralKey() *structuralKey {
	var dims []byte
	dims = binary.BigEndian.AppendUint64(dims, uint64(len(p.inSources)))
	for _, d := range p.inSources {
		dims = binary.BigEndian.AppendUint64(dims, uint64(len(d)))
	}
	return newStructuralKey().
		Bool(p.reverse).
		Int(len(p.bindingNames)).
		Values(p.comparisonKeys).
		Equatable(p.inSources, func(other any) bool {
			o, ok := other.([][]any)
			return ok && reflect.DeepEqual(p.inSources, o)
		}, dims)
}

func (p *RecordQueryInUnionPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryInUnionPlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

func (p *RecordQueryInUnionPlan) HashCodeWithoutChildren() uint64 {
	return p.structuralKey().Hash("inunionplan|")
}

func (p *RecordQueryInUnionPlan) Explain() string {
	innerLabel := "<nil>"
	if inner := p.GetInner(); inner != nil {
		innerLabel = inner.Explain()
	}
	dir := "ASC"
	if p.reverse {
		dir = "DESC"
	}
	// The binding correlation aliases (process-global counters) are not rendered —
	// only the COUNT of IN bindings is structural (RFC-164 WS-4; see in_join.go).
	return fmt.Sprintf("InUnion(%s, bindings=%d, %s)", innerLabel, len(p.bindingNames), dir)
}

var (
	_ RecordQueryPlan                  = (*RecordQueryInUnionPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryInUnionPlan)(nil)
)

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryInUnionPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// WithQuantifiers returns a copy ranging over the given child quantifier —
// Java's copy-on-write withChild(Reference).
func (p *RecordQueryInUnionPlan) WithQuantifiers(qs []expressions.Quantifier) expressions.RelationalExpression {
	if len(qs) != 1 {
		return p
	}
	cp := *p
	cp.innerQ = qs[0]
	return &cp
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryInUnionPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
