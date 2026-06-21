package predicates

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// GetCorrelatedToOfPredicate walks p + its descendants AND the Value
// trees those predicates carry, returning the union of every
// QuantifiedObjectValue's correlation. The result is a fresh map.
//
// Returns nil for nil input. Returns a non-nil empty map for trees
// without any correlation.
//
// Predicates that carry Values (ValuePredicate, ComparisonPredicate)
// have those Values walked too — so a `WHERE q.col = 5` with q being
// a Quantifier alias will surface q in the correlation set, even
// though the predicate-side WalkPredicate alone would only visit the
// ComparisonPredicate node.
//
// Java's equivalent walks `QueryPredicate.getCorrelatedTo()` which
// delegates per-impl. Same bridge story as values.GetCorrelatedToOfValue.
func GetCorrelatedToOfPredicate(p QueryPredicate) map[CorrelationIdentifier]struct{} {
	if p == nil {
		return nil
	}
	out := map[CorrelationIdentifier]struct{}{}
	WalkPredicate(p, func(node QueryPredicate) bool {
		switch np := node.(type) {
		case *ValuePredicate:
			for k := range values.GetCorrelatedToOfValue(np.Value) {
				out[k] = struct{}{}
			}
		case *ComparisonPredicate:
			for k := range values.GetCorrelatedToOfValue(np.Operand) {
				out[k] = struct{}{}
			}
			for k := range values.GetCorrelatedToOfValue(np.Comparison.Operand) {
				out[k] = struct{}{}
			}
		case *Placeholder:
			out[np.ParameterAlias] = struct{}{}
			for k := range values.GetCorrelatedToOfValue(np.Value) {
				out[k] = struct{}{}
			}
		case *ExistentialValuePredicate:
			for k := range values.GetCorrelatedToOfValue(np.Value) {
				out[k] = struct{}{}
			}
		}
		return true
	})
	return out
}

// AddMergeSeedAliases augments out with the source (leg) aliases of every
// source-anchored join RESULT value referenced by p's value trees (RFC-077 F2,
// partition-time RE-EXPOSURE).
//
// values.GetCorrelatedToOfValue (and hence GetCorrelatedToOfPredicate)
// deliberately HIDES an anchored join RC's leg QOVs — reporting them in the
// global correlation set inflates exploration order and tips large star joins
// past the task budget (see the anchored-RC arm in value_correlation.go).
// PartitionSelectRule's predicate classification, however, MUST see them: a
// predicate that reads a buried table's column through the merge genuinely
// depends on that table. Missing it misclassifies a predicate spanning both
// partition halves as lower-only, which pushes it below the merge to a leaf scan
// where the buried alias is unbound — the 0-row dual-correlation join. Callers
// that need partition-time (not exploration-time) correlations layer this on top.
func AddMergeSeedAliases(p QueryPredicate, out map[CorrelationIdentifier]struct{}) {
	if p == nil || out == nil {
		return
	}
	collect := func(v values.Value) {
		if v == nil {
			return
		}
		values.WalkValue(v, func(node values.Value) bool {
			// Source-anchored join RESULT value (RFC-077 F2, partition-time
			// RE-EXPOSURE). GetCorrelatedToOfValue HIDES this RC's leg QOVs (so the
			// global exploration order stays bounded). But PartitionSelectRule's
			// predicate classification MUST see the buried leg aliases: a predicate
			// reading a buried table's column through the anchored RC genuinely
			// depends on that table. Re-collect them by walking INTO the RC's fields
			// (its FieldValue(QOV(leg), col) children). Without this a spanning
			// predicate is misclassified as lower-only and pushed below the merge to
			// a leaf where the buried alias is unbound — the 0-row dual-correlation
			// join (TestFDB_JoinMerge_OuterColumn_NotDropped).
			if rc, ok := node.(*values.RecordConstructorValue); ok && rc.AnchoredJoin {
				for a := range values.GetCorrelatedToOfAnchoredJoinLegs(rc) {
					out[a] = struct{}{}
				}
			}
			return true
		})
	}
	WalkPredicate(p, func(node QueryPredicate) bool {
		switch np := node.(type) {
		case *ValuePredicate:
			collect(np.Value)
		case *ComparisonPredicate:
			collect(np.Operand)
			collect(np.Comparison.Operand)
		case *Placeholder:
			collect(np.Value)
		}
		return true
	})
}

// CorrelationIdentifier is re-exported as a type alias so package
// consumers don't need to import values just for the map key type.
type CorrelationIdentifier = values.CorrelationIdentifier
