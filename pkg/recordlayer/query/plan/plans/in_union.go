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
	bindingAliases []values.CorrelationIdentifier
	comparisonKeys []values.Value
	reverse        bool
	// maxSize is DELIBERATELY EXCLUDED from structuralKey, and the reason is not
	// self-evident — it changes whether the rule produces this plan at all, which is
	// exactly the shape of thing identity normally has to carry (compare
	// liveGroupsOnly on the aggregate-index plan, which IS in its key for precisely
	// that reason).
	//
	// It is safe to exclude only because it is a per-RUN constant: every value comes
	// from GetPlannerConfiguration().AttemptFailedInJoinAsUnionMaxSize
	// (rule_implement_in_union.go), so no two InUnion plans within one planner run can
	// differ in it, and the memo therefore never has two candidates to confuse.
	//
	// THAT ARGUMENT EXPIRES the moment maxSize becomes per-plan — a per-call override,
	// a hint, a rule that derives it from the IN-list — at which point two plans
	// differing only in maxSize would intern as one and whichever arrived first would
	// be served. Add it to structuralKey in the same change that makes it vary.
	maxSize   int
	inSources [][]any
}

func NewRecordQueryInUnionPlan(
	inner RecordQueryPlan,
	bindingNames []string,
	comparisonKeys []values.Value,
	reverse bool,
) (*RecordQueryInUnionPlan, error) {
	bindingAliases := make([]values.CorrelationIdentifier, len(bindingNames))
	for i, name := range bindingNames {
		bindingAliases[i] = values.NamedCorrelationIdentifier(name)
	}
	return NewRecordQueryInUnionPlanWithBindingAliases(
		inner, bindingAliases, comparisonKeys, reverse)
}

// NewRecordQueryInUnionPlanWithBindingAliases preserves the exact correlation
// kind of every IN binding. Planner-minted aliases are Unique identifiers; a
// string round-trip remints them as Named identifiers with the same spelling,
// which exact QOV lookup correctly treats as a different binding.
func NewRecordQueryInUnionPlanWithBindingAliases(
	inner RecordQueryPlan,
	bindingAliases []values.CorrelationIdentifier,
	comparisonKeys []values.Value,
	reverse bool,
) (*RecordQueryInUnionPlan, error) {
	return NewRecordQueryInUnionPlanFromQuantifierWithBindingAliases(
		QuantifierOverPlan(inner), bindingAliases, comparisonKeys, reverse, 0)
}

func newRecordQueryInUnionPlanFromQuantifier(
	innerQ expressions.Quantifier,
	bindingAliases []values.CorrelationIdentifier,
	comparisonKeys []values.Value,
	reverse bool,
	maxSize int,
) (*RecordQueryInUnionPlan, error) {
	base, err := newPlanExprBaseForQuantifier("RecordQueryInUnionPlan", innerQ)
	if err != nil {
		return nil, err
	}
	ck := make([]values.Value, len(comparisonKeys))
	copy(ck, comparisonKeys)
	return &RecordQueryInUnionPlan{
		PlanExprBase:   base,
		innerQ:         innerQ,
		bindingAliases: append([]values.CorrelationIdentifier(nil), bindingAliases...),
		comparisonKeys: ck,
		reverse:        reverse,
		maxSize:        maxSize,
	}, nil
}

func NewRecordQueryInUnionPlanWithBindingAliasesAndMaxSize(
	inner RecordQueryPlan,
	bindingAliases []values.CorrelationIdentifier,
	comparisonKeys []values.Value,
	reverse bool,
	maxSize int,
) (*RecordQueryInUnionPlan, error) {
	return NewRecordQueryInUnionPlanFromQuantifierWithBindingAliases(
		QuantifierOverPlan(inner), bindingAliases, comparisonKeys, reverse, maxSize)
}

// NewRecordQueryInUnionPlanFromQuantifier builds the InUnion over the LIVE inner
// quantifier the implement rule memoized, rather than over a plan snapshot. The
// inner may be a SHARED multi-member group (the unordered path) whose per-ordering
// winner resolves at extraction via ref.Winner() (planFromQuantifier) — the
// deferred-winner case. The plan carries the inner edge once, with no wrapper
// snapshot (RFC-184 W2). Callers still replay WithInSources afterward.
func NewRecordQueryInUnionPlanFromQuantifier(
	innerQ expressions.Quantifier,
	bindingNames []string,
	comparisonKeys []values.Value,
	reverse bool,
	maxSize int,
) (*RecordQueryInUnionPlan, error) {
	bindingAliases := make([]values.CorrelationIdentifier, len(bindingNames))
	for i, name := range bindingNames {
		bindingAliases[i] = values.NamedCorrelationIdentifier(name)
	}
	return NewRecordQueryInUnionPlanFromQuantifierWithBindingAliases(
		innerQ, bindingAliases, comparisonKeys, reverse, maxSize)
}

func NewRecordQueryInUnionPlanFromQuantifierWithBindingAliases(
	innerQ expressions.Quantifier,
	bindingAliases []values.CorrelationIdentifier,
	comparisonKeys []values.Value,
	reverse bool,
	maxSize int,
) (*RecordQueryInUnionPlan, error) {
	return newRecordQueryInUnionPlanFromQuantifier(
		innerQ, bindingAliases, comparisonKeys, reverse, maxSize)
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
	return p.PlanExprBase.GetResultValue()
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
func (p *RecordQueryInUnionPlan) WithInner(inner RecordQueryPlan) (*RecordQueryInUnionPlan, error) {
	cp := *p
	cp.innerQ = QuantifierOverPlan(inner)
	base, err := newPlanExprBaseForQuantifier("RecordQueryInUnionPlan", cp.innerQ)
	if err != nil {
		return nil, err
	}
	cp.PlanExprBase = base
	return &cp, nil
}

func (p *RecordQueryInUnionPlan) GetBindingNames() []string {
	result := make([]string, len(p.bindingAliases))
	for i, alias := range p.bindingAliases {
		result[i] = alias.Name()
	}
	return result
}

func (p *RecordQueryInUnionPlan) GetBindingAliases() []values.CorrelationIdentifier {
	return append([]values.CorrelationIdentifier(nil), p.bindingAliases...)
}
func (p *RecordQueryInUnionPlan) GetComparisonKeys() []values.Value { return p.comparisonKeys }
func (p *RecordQueryInUnionPlan) IsReverse() bool                   { return p.reverse }
func (p *RecordQueryInUnionPlan) GetMaxSize() int                   { return p.maxSize }

// GetInSources returns the LIVE slice; callers must not write through it. See
// WithInSources for why sharing is broken at the write end rather than here: cost.go
// reads these per costing call, so a defensive copy would sit in the planner's hot
// loop.
func (p *RecordQueryInUnionPlan) GetInSources() [][]any { return p.inSources }

// WithInSources returns a COPY carrying the materialized IN sources, because a plan
// method must never write through its receiver.
//
// inSources is in this plan's structuralKey, so writing it in place rewrites the
// identity of a plan that may already be in the memo — and since the structural-hash
// memo landed, under an UNCHANGED owner, which is the one staleness the memo's owner
// check cannot see: it compares identity, not content. Every caller happened to set
// before yielding, so nothing was ever wrong; that is exactly the "guarded by
// accident" shape, with no rule keeping it true.
//
// Scope of that claim, because an unscoped count is the thing this repo keeps
// getting wrong: across WithInValues, WithSourceKind and WithInSources together,
// 8 invocations in NON-TEST sources, spread over 4 rule files and 4 enclosing
// functions. An earlier draft said "five call sites", which is not the count under
// any definition.
//
// Deliberately no test-inclusive total here. A first correction added one, and it
// was false on arrival: the very commit that wrote "40 invocations including tests"
// went on to add five more test arms, so the number was stale before it was pushed.
// A figure that moves whenever anyone writes a test cannot stay true in a comment.
// The non-test count is the one that means something — it is the set that has to be
// audited when this rule changes — and it is stable.
func (p *RecordQueryInUnionPlan) WithInSources(sources [][]any) *RecordQueryInUnionPlan {
	cp := *p
	// Deep enough to break sharing at both levels: the outer slice AND each inner
	// list. The push-set-operation-through-fetch rule passes one plan's
	// GetInSources() directly into another plan's builder, so without this two
	// memoized plans own one array and an element write rewrites both structural
	// keys under unchanged pointers — invisible to the memo's owner check, which
	// compares identity rather than content.
	//
	// NIL-NESS IS LOAD-BEARING AND MUST SURVIVE THE COPY. The cost model reads a nil
	// source list as "in-list size unknown at plan time" and an EMPTY one as "known
	// to be empty" — different costs, different plans. `make([][]any, 0)` is non-nil,
	// so copying unconditionally silently converts the first into the second; that
	// broke three TestInUnionHintCost_UsesValueCombinationCount arms. The inner
	// `append([]any(nil), …)` already preserves nil for a nil element.
	if sources != nil {
		cp.inSources = make([][]any, len(sources))
		for i, src := range sources {
			cp.inSources[i] = copyPreservingNil(src)
		}
	} else {
		cp.inSources = nil
	}
	return &cp
}

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
	if len(p.bindingAliases) == 0 || len(p.inSources) != len(p.bindingAliases) {
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

func (p *RecordQueryInUnionPlan) GetResultType() values.Type { return p.GetResultValue().Type() }

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
		Int(len(p.bindingAliases)).
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
	if hash, ok := p.cachedStructuralHash(p); ok {
		return hash
	}
	hash := p.structuralKey().Hash("inunionplan|")
	p.storeStructuralHash(p, hash)
	return hash
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
	return fmt.Sprintf("InUnion(%s, bindings=%d, %s)", innerLabel, len(p.bindingAliases), dir)
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
func (p *RecordQueryInUnionPlan) WithQuantifiers(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if err := validateQuantifierArity("RecordQueryInUnionPlan", len(qs), 1); err != nil {
		return nil, err
	}
	cp := *p
	base, err := newPlanExprBaseForQuantifier("RecordQueryInUnionPlan", qs[0])
	if err != nil {
		return nil, err
	}
	cp.PlanExprBase = base
	cp.innerQ = qs[0]
	return &cp, nil
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
	return p.WithQuantifiers(qs)
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryInUnionPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
