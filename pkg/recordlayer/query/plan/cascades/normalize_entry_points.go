package cascades

import (
	"math"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

// The three normal-form entry points, and the saturating arithmetic their size
// guard needs. All three route through ONE normalizeInternal over the
// mode-parameterized machinery in normal_form.go — which is what Java has
// (BooleanPredicateNormalizer.normalize and normalizeAndSimplify differ only in
// whether applyAbsorptionLaw runs, :209-229) and what Go had two disagreeing
// copies of before RFC-240.

// NormalizerDefaultSizeLimit is BooleanPredicateNormalizer.DEFAULT_SIZE_LIMIT
// (:117) — the limit the default normalizer instances carry.
const NormalizerDefaultSizeLimit = 1_000_000

// cnfSizeLimit is the same constant under the name the CNF rule reads it by.
// Java's NormalizePredicatesRule takes it from the planner configuration's
// complexity threshold, whose default IS DEFAULT_SIZE_LIMIT.
const cnfSizeLimit = 1_000_000

// normalFormSizeSaturated is the value a normal-form size computation reports
// when it overflows int64. Java uses Math.addExact/Math.multiplyExact
// (:342-360) and shouldNormalize catches the ArithmeticException (:296-301)
// with "our computation caused an integer overflow so the normal form would
// _definitely_ be too big" — overflow means REJECT, never admit. Go has no
// checked arithmetic, so the computation saturates here instead; MaxInt64
// exceeds every limit a caller can pass, so every `size > limit` guard rejects.
//
// Testing the accumulator for a negative result AFTER the multiply does not
// work: an AND of 32 four-way ORs has a true DNF size of 4^32 == 2^64, which
// wraps to exactly ZERO — smaller than every limit — so the guard would admit
// the predicate and the distribution would then materialize the full cross
// product.
const normalFormSizeSaturated = math.MaxInt64

// saturatingAddSize adds two non-negative normal-form sizes, clamping at
// normalFormSizeSaturated instead of wrapping.
func saturatingAddSize(a, b int64) int64 {
	if a > normalFormSizeSaturated-b {
		return normalFormSizeSaturated
	}
	return a + b
}

// saturatingMulSize multiplies two non-negative normal-form sizes, clamping at
// normalFormSizeSaturated instead of wrapping.
func saturatingMulSize(a, b int64) int64 {
	if a == 0 || b == 0 {
		return 0
	}
	if a > normalFormSizeSaturated/b {
		return normalFormSizeSaturated
	}
	return a * b
}

// normalizeInternal is Java's normalizeInternal (:241-253): decline when the
// predicate is already in normal form, decline when the normal form would
// exceed the size limit, otherwise return the major-of-minor list-of-lists.
//
// ok=false means "no transformation", which the callers turn into Java's
// Optional.empty() — the (pred, false) return their own callers resolve by
// keeping the original.
func normalizeInternal(
	pred predicates.QueryPredicate, sizeLimit int, mode normalFormMode,
) ([][]predicates.QueryPredicate, bool) {
	if pred == nil {
		return nil, false
	}
	if isInNormalForm(pred, mode) {
		return nil, false
	}
	if normalFormSize(pred, false, mode) > int64(sizeLimit) {
		return nil, false
	}
	return toNormalized(pred, false, mode), true
}

// rebuildNormalForm assembles the list-of-lists back into a predicate:
// major(minor(clause) for each clause). Java's
// `mode.majorWithChildren(majorOfMinor.stream().map(mode::minorWithChildren))`
// (:212-214, :229).
func rebuildNormalForm(majorOfMinor [][]predicates.QueryPredicate, mode normalFormMode) predicates.QueryPredicate {
	children := make([]predicates.QueryPredicate, 0, len(majorOfMinor))
	for _, clause := range majorOfMinor {
		children = append(children, mode.minorWithChildren(clause))
	}
	return mode.majorWithChildren(children)
}

// normalizeCNF converts a predicate to conjunctive normal form — Java's
// normalizeAndSimplify in Mode.CNF (:209-215), which is what
// NormalizePredicatesRule calls.
//
// Returns (result, true) when a transformation was applied, or (original,
// false) when the predicate is already in CNF or the normal form would exceed
// sizeLimit.
func normalizeCNF(pred predicates.QueryPredicate, sizeLimit int) (predicates.QueryPredicate, bool) {
	majorOfMinor, ok := normalizeInternal(pred, sizeLimit, normalFormCNF)
	if !ok {
		return pred, false
	}
	return rebuildNormalForm(applyAbsorption(majorOfMinor), normalFormCNF), true
}

// NormalizeDNF converts a predicate to disjunctive normal form — Java's
// normalizeAndSimplify in Mode.DNF.
func NormalizeDNF(pred predicates.QueryPredicate, sizeLimit int) (predicates.QueryPredicate, bool) {
	majorOfMinor, ok := normalizeInternal(pred, sizeLimit, normalFormDNF)
	if !ok {
		return pred, false
	}
	return rebuildNormalForm(applyAbsorption(majorOfMinor), normalFormDNF), true
}

// NormalizeDNFWithoutSimplification is Java's `normalize` in Mode.DNF
// (:227-229): pure distribution with NOT pushed down, WITHOUT absorption and
// WITHOUT dedup.
//
// The distinction is WIRE-VISIBLE and that is why the variant exists. Java's
// index-predicate producer calls normalize, not normalizeAndSimplify
// (MaterializedViewIndexGenerator.java:675), and Go's caller is
// pkg/relational/core/query/ddl/generator_predicate.go:55 — an absorbed clause
// would change the stored RecordMetaDataProto.Predicate bytes relative to Java.
//
// TestNormalFormWritePath_IsUnchanged holds this path's output against a golden
// captured before RFC-240 rewrote the machinery underneath it.
func NormalizeDNFWithoutSimplification(pred predicates.QueryPredicate, sizeLimit int) (predicates.QueryPredicate, bool) {
	majorOfMinor, ok := normalizeInternal(pred, sizeLimit, normalFormDNF)
	if !ok {
		return pred, false
	}
	return rebuildNormalForm(majorOfMinor, normalFormDNF), true
}
