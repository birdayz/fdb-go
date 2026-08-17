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

	// The R3 residual dedup. Set together, by WithNarrowedDedup only, and read
	// by the executor and by EXPLAIN. Both are in structuralKey; see the
	// wrong-rows argument there.
	//
	// narrowedProofIndexName is the secondary UNIQUE index whose uniqueness
	// licenses retaining ONLY the exempt rows. Empty means the ordinary full
	// dedup, which is every distinct plan not built over a qualifying index.
	//
	// narrowedExemptSlots are the dedup-key slot positions holding that index's
	// key components; nil means "test every slot" — the conservative form, which
	// over-approximates the exempt set and so stays sound.
	narrowedProofIndexName string
	narrowedExemptSlots    []int
}

// NewRecordQueryDistinctPlan constructs a distinct plan over the
// given inner plan.
func NewRecordQueryDistinctPlan(inner RecordQueryPlan) (*RecordQueryDistinctPlan, error) {
	return NewRecordQueryDistinctPlanFromQuantifier(QuantifierOverPlan(inner), false)
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
func NewRecordQueryDistinctPlanFromQuantifier(innerQ expressions.Quantifier, streaming bool) (*RecordQueryDistinctPlan, error) {
	base, err := newPlanExprBaseForQuantifier("RecordQueryDistinctPlan", innerQ)
	if err != nil {
		return nil, err
	}
	return &RecordQueryDistinctPlan{PlanExprBase: base, innerQ: innerQ, Streaming: streaming}, nil
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
	return p.PlanExprBase.GetResultValue()
}

// GetResultType returns the inner's result type.
func (p *RecordQueryDistinctPlan) GetResultType() values.Type { return p.GetResultValue().Type() }

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

// EqualsWithoutChildren — distinct plans carry the Streaming mode and the R3
// narrowing as operator-specific data. A streaming and a hash-set distinct dedup
// to the same rows but are NOT execution-interchangeable (streaming assumes
// ordered input), so they must not be conflated in the memo.
//
// The NARROWING is in identity for a stronger reason than the streaming mode,
// and it is a wrong-rows condition rather than a plan-quality one. A narrowed
// operator's seen-set holds ONLY the exempt keys; a full one holds every key. A
// re-plan that flipped narrowed↔full while resuming a DistinctHashContinuation
// would rebuild a set from a serialization written under the other discipline —
// a full operator resuming from a narrowed continuation believes it has already
// seen every key the narrowing omitted, and drops rows it must emit. Collapsing
// the two in the memo is what would let that happen silently.
func (p *RecordQueryDistinctPlan) structuralKey() *structuralKey {
	k := newStructuralKey().Bool(p.Streaming).
		Str(p.narrowedProofIndexName).
		Int(len(p.narrowedExemptSlots))
	for _, slot := range p.narrowedExemptSlots {
		k = k.Int(slot)
	}
	return k
}

// WithNarrowedDedup returns a copy whose dedup is RESIDUAL: only rows carrying
// an exempt key component — NULL or NaN — enter the seen-set, and every other
// row passes through in O(1) with nothing retained.
//
// This is R3, the route taken when neither the metadata proof nor a
// NULL-rejecting predicate establishes that the index's exempt set is empty. The
// soundness argument is one line: the index's uniqueness already guarantees at
// most one row per non-exempt key value, so a non-exempt row can duplicate
// neither another non-exempt row (uniqueness) nor an exempt one (exemptness is a
// property of the key value itself, so their keys differ by construction).
//
// It STRICTLY DOMINATES the full operator, which is why it needs no cost-model
// trade-off and no cost formula moves: the narrowed seen-set is a SUBSET of the
// full one on every input — identical in the worst case where every row is
// exempt, and EMPTY on the ordinary case of a nullable column holding no NULLs,
// where the operator degenerates to a pass-through retaining nothing.
//
// exemptSlots are positions in the DEDUP KEY's slot order (what distinctKey
// packs) holding the proving index's key components. A nil slice means "test
// every slot", which is the conservative form: it over-approximates the exempt
// set, so it is still a subset of the full seen-set and still sound, just less
// narrow. That is the fail-safe direction — the alternative, guessing a
// position, would under-approximate and drop rows.
//
// indexName is the proving index, and it is recorded for the SAME reason a full
// elision records it. R3 reads as unconditional because it removes no operator,
// and that reading is wrong: R3 rests on the index's uniqueness exactly as R1
// and R2 do. Withdraw the uniqueness guarantee and a non-exempt row can
// duplicate another non-exempt row — which is precisely what the narrowed
// seen-set no longer catches. So the plan carries the stamp, the dependency walk
// collects it through DistinctProofStamped, and the execution-time revalidation
// 40001s a statement whose proving index left READABLE.
// The STREAMING executor is REFUSED the narrowing, and the refusal lives here
// rather than at the call site because a plan that carries the flag and an
// executor that ignores it is the worst of the three possible states: EXPLAIN
// renders `narrowed-by`, an acceptance criterion reads it as fired, and the
// executor took the streaming branch and dedupped every row exactly as before.
//
// Refusing costs nothing. The streaming executor retains ONE key — the previous
// row's — so there is no seen-set for a narrowing to shrink; the whole benefit
// is on the hash path, whose set the narrowing empties. And no dependency is
// lost by not stamping: streaming is only chosen when the inner is ordered by
// the dedup key, which on these shapes is a scan of the proving index itself, so
// the plan already names the index and the dependency walk already finds it.
// That is the same SOLE-LICENSE reasoning the elision arm applies — a stamp
// records a dependency the plan's correctness rests on and nothing else.
func (p *RecordQueryDistinctPlan) WithNarrowedDedup(indexName string, exemptSlots []int) *RecordQueryDistinctPlan {
	if indexName == "" || p.Streaming {
		return p
	}
	cp := *p
	cp.narrowedProofIndexName = indexName
	cp.narrowedExemptSlots = append([]int(nil), exemptSlots...)
	return &cp
}

// GetDistinctProofIndexName implements DistinctProofStamped. On a distinct plan
// the stamp does not mean "an operator was elided above this node" — it means
// "this operator's dedup was NARROWED on the strength of that index". Both are
// dependencies of the same kind and for the same reason, so the dependency walk
// asks one capability and gets both.
func (p *RecordQueryDistinctPlan) GetDistinctProofIndexName() string {
	return p.narrowedProofIndexName
}

// GetNarrowedExemptSlots returns the dedup-key slot positions the residual dedup
// tests, or nil for "every slot".
func (p *RecordQueryDistinctPlan) GetNarrowedExemptSlots() []int {
	return p.narrowedExemptSlots
}

// IsNarrowedDedup reports whether this distinct dedups only the exempt subset.
func (p *RecordQueryDistinctPlan) IsNarrowedDedup() bool {
	return p.narrowedProofIndexName != ""
}

func (p *RecordQueryDistinctPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryDistinctPlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

// HashCodeWithoutChildren discriminates on type and the Streaming mode.
func (p *RecordQueryDistinctPlan) HashCodeWithoutChildren() uint64 {
	if hash, ok := p.cachedStructuralHash(p); ok {
		return hash
	}
	hash := p.structuralKey().Hash("distinctplan|")
	p.storeStructuralHash(p, hash)
	return hash
}

// Explain renders Distinct(inner), plus the R3 narrowing when present.
//
// The Streaming mode is an execution-only detail (resume-clean adjacent-dedup vs
// fresh-per-page hash-set) that produces identical rows, so it is deliberately
// NOT surfaced here — keeping plan-shape assertions stable. The fix it enables
// is proved by row-level cross-page tests, not EXPLAIN.
//
// The NARROWING is rendered, and the difference from Streaming is the reason.
// Streaming picks between two executors that retain the same keys; narrowing
// changes WHICH ROWS THE OPERATOR RETAINS. That is the optimization itself, and
// an acceptance criterion has to be able to assert it fired — a narrowed
// distinct that silently degraded to a full one is otherwise indistinguishable
// from one that never narrowed. Naming the licensing index makes it a positive
// assertion rather than an absence, for the same reason the elision's own stamp
// is rendered.
func (p *RecordQueryDistinctPlan) Explain() string {
	innerLabel := "<nil>"
	if inner := p.GetInner(); inner != nil {
		innerLabel = inner.Explain()
	}
	if p.narrowedProofIndexName != "" {
		return fmt.Sprintf("Distinct(%s) narrowed-by:%s", innerLabel, p.narrowedProofIndexName)
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
func (p *RecordQueryDistinctPlan) WithQuantifiers(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if err := validateQuantifierArity("RecordQueryDistinctPlan", len(qs), 1); err != nil {
		return nil, err
	}
	cp := *p
	base, err := newPlanExprBaseForQuantifier("RecordQueryDistinctPlan", qs[0])
	if err != nil {
		return nil, err
	}
	cp.PlanExprBase = base
	cp.innerQ = qs[0]
	return &cp, nil
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
	return p.WithQuantifiers(qs)
}

// WithInner returns a copy with the inner replaced and every other field
// preserved — the extraction-relink rebuild path (see findPhysicalPlan's
// shell completion). A constructor rebuild would drop fields the setters
// carry, so identity-preserving copy is the only safe form.
func (p *RecordQueryDistinctPlan) WithInner(inner RecordQueryPlan) (*RecordQueryDistinctPlan, error) {
	cp := *p
	cp.innerQ = QuantifierOverPlan(inner)
	base, err := newPlanExprBaseForQuantifier("RecordQueryDistinctPlan", cp.innerQ)
	if err != nil {
		return nil, err
	}
	cp.PlanExprBase = base
	return &cp, nil
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryDistinctPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
