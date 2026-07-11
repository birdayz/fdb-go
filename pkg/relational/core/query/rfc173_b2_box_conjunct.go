package query

// RFC-173 Outcome-B B2 sub-slice A — the FILTERED box unnest ordinalizes.
//
// A WHERE conjunct referencing a box leg (`FROM (LA LEFT JOIN LB ON …),
// LA.ARR AS X WHERE LA.K = 100`) used to decline the gathered ordinal path
// unconditionally (the blanket gather decline), falling to the name-model
// binary unnest lowering — the P4-enclosed + P5-un-enclosed producer pair the
// census pinned. But the gathered seed carries a BURIED window for EVERY box
// leaf (addBuriedBakeWindows: bakeCorr = the $BOX quantifier, per-leaf
// [leafOffset, width)), so a box-leg conjunct CAN bake positionally —
// ofOrdinal(QOV($BOX, boxConcat), leafOffset+idx) — evaluated against the
// box's OUTPUT row, which is WHERE-above-LEFT semantics for BOTH legs (a
// preserved-leg conjunct filters real values; a null-supplied-leg conjunct
// sees NULL on padded rows and drops them; predicate pushdown below the box is
// the optimizer's job, never hand-placed at translation — Java's placement).
//
// The verdict is computed PRE-translation (metadata-only): a post-merge
// decline would be poisoned by translation side state (the enclosedGatherCache
// lesson), and a post-merge loud error would regress shapes the name model
// answers correctly today. Anything not POSITIVELY bakeable — a subquery or
// EXISTS value in the conjunct, a foreign correlation, an unresolvable
// box-leg reference — is Unbakeable and keeps today's name-model plan
// (fail-open, correct rows).

import (
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// The unnestBoxLegConjunct verdict states.
const (
	boxConjNone       = 0 // no box-leg conjunct
	boxConjBakeable   = 1 // every box-leg ref resolves in the seed's buried windows — the gather ADMITS
	boxConjUnbakeable = 2 // EXISTS-path / subquery-carrying / unresolvable — the gather DECLINES (name model)
)

// classifyBoxLegConjunct decides Bakeable vs Unbakeable for a NON-EXISTS WHERE
// over `box, box-leg.arr AS x` — called only after nonExistsConjunctRefsOuterLeg
// reported a box-leg reference. PURE (metadata-only, no translation):
//   - the box-as-one-leg seed map must derive (ordinalJoinSeedFields);
//   - no conjunct may carry a subquery/EXISTS value or a correlation outside
//     the box legs ∪ the unnest's own AS/AT aliases (a scalar-subquery alias is
//     a foreign correlation — its conjunct stays on the name-model path, the
//     booked Slice-2b posture, without a new loud error);
//   - every box-leg reference (legRef shape) must FieldIndex-resolve in its
//     buried window's leafTyp — an unresolvable ref would strand as a lazy name
//     read over the positional row (the wrong-slot class).
func (t *cascadesTranslator) classifyBoxLegConjunct(box *logical.LogicalJoin, u *logical.LogicalUnnest, pred predicates.QueryPredicate) int {
	_, legTypes := t.ordinalJoinSeedFields([]clusterLeg{clusterLegOf(box, false)})
	if legTypes == nil {
		return boxConjUnbakeable
	}
	allowed := map[string]struct{}{}
	for a := range outerBoundAliases(box) {
		allowed[strings.ToUpper(a)] = struct{}{}
	}
	if u.Alias != "" {
		allowed[strings.ToUpper(u.Alias)] = struct{}{}
	}
	if u.AtAlias != "" {
		allowed[strings.ToUpper(u.AtAlias)] = struct{}{}
	}
	verdict := boxConjBakeable
	for _, conj := range splitNonExistsPredicates(pred) {
		predicates.ReplaceValues(conj, func(v values.Value) values.Value {
			switch nv := v.(type) {
			case *values.ScalarSubqueryValue, *values.ExistsValue:
				verdict = boxConjUnbakeable
			case *values.QuantifiedObjectValue:
				if _, ok := allowed[strings.ToUpper(nv.Correlation.Name())]; !ok {
					verdict = boxConjUnbakeable // a foreign correlation (e.g. a scalar-subquery alias)
				}
			case *values.FieldValue:
				if key, isRef := legRef(v); isRef {
					if w, isLeg := legTypes[key]; isLeg {
						if w.leafTyp == nil {
							verdict = boxConjUnbakeable
						} else if _, found := w.leafTyp.FieldIndex(nv.Field); !found {
							verdict = boxConjUnbakeable
						}
					}
				} else if nv.Child == nil && strings.Contains(nv.Field, ".") {
					// A dotted frontier read — cannot be positively attributed to
					// a window here; keep the name-model path (fail-open).
					verdict = boxConjUnbakeable
				}
			}
			return v
		})
	}
	return verdict
}
