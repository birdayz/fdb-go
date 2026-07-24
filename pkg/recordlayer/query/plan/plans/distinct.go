package plans

import (
	"fmt"

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
	// adjacent). Two site kinds set it, and the distinction matters:
	//   - Sites that build a distinct over a NEW/DIFFERENT inner RE-DERIVE it
	//     from that inner's ordering via distinctStreamingEligible: the
	//     implement rule, and the push-distinct-below-filter / -through-fetch
	//     rules (a push preserves ordering but changes WHICH inner the distinct
	//     sits over, so the decision must be recomputed).
	//   - The physical-wrapper rebuild (WithChildren → WithInner, copy-on-write)
	//     COPIES the flag unchanged — sound only because shell completion
	//     relinks the SAME, identically-ordered inner, not a different one.
	// Default false is the safe hash-set fallback. See TODO.md C5.
	Streaming bool
}

// NewRecordQueryDistinctPlan constructs a distinct plan over the
// given inner plan.
func NewRecordQueryDistinctPlan(inner RecordQueryPlan) *RecordQueryDistinctPlan {
	return &RecordQueryDistinctPlan{innerQ: QuantifierOverPlan(inner)}
}

// NewRecordQueryDistinctPlanFromQuantifier builds a distinct whose child is a
// supplied memo quantifier instead of a snapshot over a single plan. This makes
// the plan its own cascades expression carrying its child edge directly — the
// memo holds it without a physicalDistinctWrapper (RFC-184 W2).
//
// The streaming flag is passed EXPLICITLY because it is the ordering-critical
// axis: it is sound only when the inner is ordered by the dedup key (equal rows
// adjacent), so it MUST be computed against the exact inner this quantifier
// resolves to. Unlike a plain (hash) distinct — which dedups over any inner and
// therefore carries the LIVE shared-group edge, following push-rule
// canonicalization — a STREAMING distinct freezes its ordering-critical inner
// in a DETACHED single-member final reference so planFromQuantifier resolves
// that exact member and the streaming executor never runs over an unordered
// float. See newPhysicalDistinctFor.
func NewRecordQueryDistinctPlanFromQuantifier(innerQ expressions.Quantifier, streaming bool) *RecordQueryDistinctPlan {
	return &RecordQueryDistinctPlan{innerQ: innerQ, Streaming: streaming}
}

// GetInner returns the inner plan, dereferenced through the quantifier.
func (p *RecordQueryDistinctPlan) GetInner() RecordQueryPlan {
	return planFromQuantifier(p.innerQ)
}

// GetInnerQuantifier returns the live child quantifier — the single memo edge
// the distinct ranges over. The push rules read it to reach the distinct's inner
// group; since RFC-184 W2 the memo holds the bare plan (no physicalDistinctWrapper
// whose innerQuant field they used to read), this exposes the same edge.
func (p *RecordQueryDistinctPlan) GetInnerQuantifier() expressions.Quantifier {
	return p.innerQ
}

// GetResultValue returns the flowed object value of the child quantifier — a
// distinct drops duplicate rows but reshapes nothing, so its row identity IS the
// inner's. This is the identity physicalDistinctWrapper.GetResultValue supplied
// (RFC-184 W2).
func (p *RecordQueryDistinctPlan) GetResultValue() values.Value {
	return p.innerQ.GetFlowedObjectValue()
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
func (p *RecordQueryDistinctPlan) structuralKey() *structuralKey {
	return newStructuralKey().Bool(p.Streaming)
}

func (p *RecordQueryDistinctPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryDistinctPlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

// HashCodeWithoutChildren discriminates on type and the Streaming mode.
func (p *RecordQueryDistinctPlan) HashCodeWithoutChildren() uint64 {
	return p.structuralKey().Hash("distinctplan|")
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

// WithChildren is the extraction/relink hook (plan_extraction.go's WithChildren
// interface). The distinct carries its child as a single memo edge, so the
// relink is a quantifier swap: WithQuantifiers copies the receiver (preserving
// the Streaming mode — the flag a constructor rebuild would reset and thereby
// downgrade a resume-clean streaming distinct to the memory-heavy hash-set,
// TODO C5: cross-page correctness was fixed 2026-07-20 — the hash-set is no
// longer wrong across pages; streaming is preferred for O(1) memory) and
// re-resolves GetInner through the new singleton reference. This
// replaces physicalDistinctWrapper.WithChildren (RFC-184 W2), whose separate
// snapshot plan field forced a WithInner rebuild gated on isLeafReplaceable — a
// gate that DECLINED to relink onto a non-leaf-replaceable (e.g. projection)
// pinned inner and so kept a redundant enforcer sort Java's RemoveSortRule
// elides. The unconditional swap re-resolves through GetInner, reaches the
// executable plan, and lets that elision fire — a parity gain, the same as the
// predicates-filter collapse.
func (p *RecordQueryDistinctPlan) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("RecordQueryDistinctPlan.WithChildren: expected 1 child, got %d", len(qs))
	}
	return p.WithQuantifiers(qs), nil
}

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
