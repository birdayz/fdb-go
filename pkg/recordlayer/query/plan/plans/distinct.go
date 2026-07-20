package plans

import (
	"fmt"
	"hash/fnv"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryDistinctPlan removes duplicate rows from an inner
// plan's row stream. Mirrors Java's `RecordQueryUnorderedPrimaryKeyDistinctPlan`
// (the simpler unordered-distinct shape — Java has multiple
// distinct-plan flavors: ordered / unordered / by-key / by-row).
// The seed picks unordered-by-row.
//
// Result type matches inner — distinct doesn't reshape rows.
// The child is stored ONCE, as a Quantifier over a Reference — Java's shape
// (`private final Quantifier.Physical inner`, dereferenced by getChild()).
// The raw `inner RecordQueryPlan` pointer it replaces was the second storage
// location for the same edge; a nil-inner "shell" was precisely the state
// where that pointer and the wrapper's quantifier disagreed. With one
// location there is nothing to disagree with. RFC-183 P5 step 2.
type RecordQueryDistinctPlan struct {
	PlanExprBase
	innerQ expressions.Quantifier
	// Streaming selects the resume-clean adjacent-dedup executor
	// (distinctStreamCursor) instead of the fresh-per-page hash-set. It is
	// sound ONLY when the inner is ordered by the dedup key (equal rows
	// adjacent); the planner sets it exclusively when it guarantees that
	// ordering. Default false preserves the hash-set path for every other
	// RecordQueryDistinctPlan creator (physical rebuild, push-through rules),
	// which do not establish that ordering. See TODO.md C5.
	Streaming bool
}

// NewRecordQueryDistinctPlan constructs a distinct plan over the
// given inner plan.
func NewRecordQueryDistinctPlan(inner RecordQueryPlan) *RecordQueryDistinctPlan {
	return &RecordQueryDistinctPlan{innerQ: QuantifierOverPlan(inner)}
}

// GetInner returns the inner plan, dereferenced through the quantifier.
func (p *RecordQueryDistinctPlan) GetInner() RecordQueryPlan {
	return planFromQuantifier(p.innerQ)
}

// GetResultType returns the inner's result type.
func (p *RecordQueryDistinctPlan) GetResultType() values.Type {
	inner := p.GetInner()
	if inner == nil {
		return values.UnknownType
	}
	return inner.GetResultType()
}

// GetChildren returns the inner plan as the only child.
func (p *RecordQueryDistinctPlan) GetChildren() []RecordQueryPlan {
	inner := p.GetInner()
	if inner == nil {
		return nil
	}
	return []RecordQueryPlan{inner}
}

// GetQuantifiers reports the real child quantifier, overriding
// PlanExprBase's none.
func (p *RecordQueryDistinctPlan) GetQuantifiers() []expressions.Quantifier {
	if p.innerQ.GetRangesOver() == nil {
		return nil
	}
	return []expressions.Quantifier{p.innerQ}
}

// EqualsWithoutChildren — distinct plans carry only the Streaming mode as
// operator-specific data; a streaming and a hash-set distinct dedup to the same
// rows but are NOT execution-interchangeable (streaming assumes ordered input),
// so they must not be conflated in the memo.
func (p *RecordQueryDistinctPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryDistinctPlan)
	return ok && o.Streaming == p.Streaming
}

// HashCodeWithoutChildren discriminates on type and the Streaming mode.
func (p *RecordQueryDistinctPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	if p.Streaming {
		h.Write([]byte("distinctplan|streaming"))
	} else {
		h.Write([]byte("distinctplan"))
	}
	return h.Sum64()
}

// Explain renders Distinct(inner). The Streaming mode is an execution-only
// detail (resume-clean adjacent-dedup vs fresh-per-page hash-set) that produces
// identical rows, so it is deliberately NOT surfaced here — keeping plan-shape
// assertions stable. The fix it enables is proved by row-level cross-page tests,
// not EXPLAIN.
func (p *RecordQueryDistinctPlan) Explain() string {
	innerLabel := "<nil>"
	if inner := p.GetInner(); inner != nil {
		innerLabel = inner.Explain()
	}
	return fmt.Sprintf("Distinct(%s)", innerLabel)
}

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryDistinctPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// WithQuantifiers returns a copy ranging over the given child quantifier —
// Java's copy-on-write withChild(Reference). The receiver is never mutated,
// which is what keeps a memoized plan safe to share.
func (p *RecordQueryDistinctPlan) WithQuantifiers(qs []expressions.Quantifier) expressions.RelationalExpression {
	if len(qs) != 1 {
		return p
	}
	cp := *p
	cp.innerQ = qs[0]
	return &cp
}

var (
	_ RecordQueryPlan                  = (*RecordQueryDistinctPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryDistinctPlan)(nil)
)

// WithInner returns a copy with the inner replaced and every other field
// preserved — the extraction-relink rebuild path (see findPhysicalPlan's
// shell completion). A constructor rebuild would drop fields the setters
// carry, so identity-preserving copy is the only safe form.
func (p *RecordQueryDistinctPlan) WithInner(inner RecordQueryPlan) *RecordQueryDistinctPlan {
	cp := *p
	cp.innerQ = QuantifierOverPlan(inner)
	return &cp
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryDistinctPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
