package plans

import (
	"encoding/binary"
	"fmt"
	"math"
	"reflect"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryInUnionPlan is the IN-union variant: the inner plan is
// executed once per Cartesian-product combination of IN-source bindings,
// and results are merge-sorted by comparison keys. Mirrors Java's
// RecordQueryInUnionOnValuesPlan.
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

// NewRecordQueryInUnionPlanFromQuantifier builds the InUnion over the LIVE inner
// quantifier the implement rule memoized, rather than over a plan snapshot. The
// inner may be a SHARED multi-member group (the unordered path) whose per-ordering
// winner resolves at extraction via ref.Winner() (planFromQuantifier) — the
// deferred-winner case. The plan carries the inner edge once, with no wrapper
// snapshot (RFC-184 W2). Callers still replay SetInSources afterward.
func NewRecordQueryInUnionPlanFromQuantifier(
	innerQ expressions.Quantifier,
	bindingNames []string,
	comparisonKeys []values.Value,
	reverse bool,
	maxSize int,
) *RecordQueryInUnionPlan {
	bn := make([]string, len(bindingNames))
	copy(bn, bindingNames)
	ck := make([]values.Value, len(comparisonKeys))
	copy(ck, comparisonKeys)
	return &RecordQueryInUnionPlan{
		innerQ:         innerQ,
		bindingNames:   bn,
		comparisonKeys: ck,
		reverse:        reverse,
		maxSize:        maxSize,
	}
}

func (p *RecordQueryInUnionPlan) GetInner() RecordQueryPlan { return planFromQuantifier(p.innerQ) }

// GetInnerQuantifier returns the live child quantifier — the single memo edge the
// InUnion ranges over. derivationsForInUnion reads its alias to decorrelate the
// inner against the IN-source bindings; since RFC-184 W2 the memo holds the bare
// plan (no physicalInUnionWrapper whose innerQuant field it used to read), this
// exposes the same edge.
func (p *RecordQueryInUnionPlan) GetInnerQuantifier() expressions.Quantifier {
	return p.innerQ
}

// GetResultValue flows the live child quantifier's object value — the InUnion
// emits its inner's rows once per IN-source binding, so its row identity IS the
// inner's. This is the identity physicalInUnionWrapper.GetResultValue supplied
// (RFC-184 W2).
func (p *RecordQueryInUnionPlan) GetResultValue() values.Value {
	return p.innerQ.GetFlowedObjectValue()
}

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

// LiteralFanout returns the exact number of child executions represented by
// the plan-time IN sources. Each source is one binding dimension, so the
// fanout is their Cartesian-product size. A nil inner source in a present
// dimension means its runtime value was unavailable during planning and
// therefore returns known=false; a non-nil empty source is an exact zero.
// An absent outer source slice is a pass-through, even when binding names
// exist; executeInUnion has always used that constructor shape to mean "no
// materialized sources." Once sources are present, their dimension count must
// equal the binding count.
//
// Overflow also returns unknown instead of wrapping to a negative cardinality.
func (p *RecordQueryInUnionPlan) LiteralFanout() (fanout int64, known bool) {
	if p == nil {
		return 0, false
	}
	if len(p.inSources) == 0 {
		return 1, true
	}
	if len(p.bindingNames) == 0 || len(p.inSources) != len(p.bindingNames) {
		return 0, false
	}
	// A single known-empty dimension makes the whole Cartesian product empty,
	// even when another dimension is unknown. Check all supplied sources before
	// rejecting nil dimensions so the result is order-invariant.
	for _, source := range p.inSources {
		if source != nil && len(source) == 0 {
			return 0, true
		}
	}
	fanout = 1
	for _, source := range p.inSources {
		if source == nil {
			return 0, false
		}
		size := int64(len(source))
		if fanout > math.MaxInt64/size {
			return 0, false
		}
		fanout *= size
	}
	return fanout, true
}

func (p *RecordQueryInUnionPlan) GetResultType() values.Type {
	if p.innerQ.GetRangesOver() == nil {
		return values.UnknownType
	}
	// TYPE resolution, not identity: the inner is a deferred-winner group still
	// multi-member during planning; every alternative shares the row shape, so
	// any member answers without tripping the singleton guard (RFC-184 W2).
	return planTypeFromQuantifier(p.innerQ).GetResultType()
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

// WithChildren is the extraction/relink hook (plan_extraction.go's WithChildren
// interface). Because the InUnion carries its child as a single LIVE memo edge,
// the relink is exactly a quantifier swap: WithQuantifiers preserves every other
// field (bindingNames, comparisonKeys, reverse, maxSize, inSources) and GetInner
// re-resolves through the new singleton reference. This replaces
// physicalInUnionWrapper.WithChildren (RFC-184 W2), whose separate snapshot plan
// field forced a WithInner rebuild gated on isLeafReplaceable — a single live
// child edge relinks to ref.Winner() unconditionally.
func (p *RecordQueryInUnionPlan) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("RecordQueryInUnionPlan.WithChildren: expected 1 child, got %d", len(qs))
	}
	return p.WithQuantifiers(qs), nil
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryInUnionPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
