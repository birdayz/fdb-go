package values

import "strings"

// MergeSeedLegsOfValue returns the SOURCE-LEG correlations a value tree depends
// on THROUGH a merged (source-anchored join) row — the partition-time
// re-exposure twin of GetCorrelatedToOfValue, at the value level rather than the
// predicate level (predicates.AddMergeSeedAliases).
//
// A multi-source lateral UNNEST reads a BURIED leg's column through the merged
// outer row: `FieldValue{Field:"A.ARR", Child:QOV(B)}` reads `QOV(B)["A.ARR"]`
// where B is the rightmost (flow) leg and A is a NON-flow leg merged into B's
// row. GetCorrelatedToOfValue reports only {B} (the QOV it references), so the
// genuine dependency on A is INVISIBLE — and PartitionSelectRule/
// PartitionBinarySelectRule, which classify bipartition validity from the
// correlation order, would let `{B, Explode}` separate from A, materializing the
// Explode against a bare B row where `A.ARR` is unbound (zero rows). This
// recovers the buried leg A from the DOTTED field prefix: a FieldValue whose
// Field is `LEG.COL` (and whose Child resolves to a QuantifiedObjectValue —
// i.e. it reads off a merged quantifier's row, not a literal anchored RC) genuinely
// depends on the source leg `LEG`. The anchored merged row always names its legs
// by their source alias as the dotted prefix (NewAnchoredJoinRecord), and those
// source aliases ARE the sibling quantifier aliases after the merge flattens — so
// the prefix maps directly to the owned quantifier.
//
// Returns a non-nil (possibly empty) map; nil input yields an empty map.
func MergeSeedLegsOfValue(v Value) map[CorrelationIdentifier]struct{} {
	out := map[CorrelationIdentifier]struct{}{}
	if v == nil {
		return out
	}
	WalkValue(v, func(node Value) bool {
		fv, ok := node.(*FieldValue)
		if !ok || fv.Child == nil {
			return true
		}
		dot := strings.IndexByte(fv.Field, '.')
		if dot <= 0 {
			return true
		}
		// The Child must bottom out in a QuantifiedObjectValue — a dotted read off
		// a merged quantifier's row. (A dotted FieldValue over a literal anchored RC
		// is the partition-time predicate case, handled by AddMergeSeedAliases.)
		if _, isQOV := leftmostQOVOfValue(fv.Child); !isQOV {
			return true
		}
		out[NamedCorrelationIdentifier(strings.ToUpper(fv.Field[:dot]))] = struct{}{}
		return true
	})
	return out
}

// leftmostQOVOfValue descends the leftmost FieldValue chain and reports the
// QuantifiedObjectValue correlation it bottoms out in (mirrors leftmostQOV in
// value_anchored_join_record.go, exposed here for MergeSeedLegsOfValue).
func leftmostQOVOfValue(v Value) (CorrelationIdentifier, bool) {
	for {
		switch x := v.(type) {
		case *QuantifiedObjectValue:
			return x.Correlation, true
		case *FieldValue:
			if x.Child == nil {
				return CorrelationIdentifier{}, false
			}
			v = x.Child
		default:
			return CorrelationIdentifier{}, false
		}
	}
}

// GetCorrelatedToOfValue walks v + its descendants and returns the
// union of every correlation-bearing leaf Value's alias. Handles
// QuantifiedObjectValue, QuantifiedRecordValue, ScalarSubqueryValue,
// ObjectValue, UnmatchedAggregateValue, and ConstantObjectValue.
// ExistsValue is a transparent composite — its child QuantifiedObjectValue
// is reached via the Children() descent.
//
// Returns nil for nil input. Returns a non-nil empty map for trees
// with no correlations.
//
// Ports Java's Value.getCorrelatedTo().
func GetCorrelatedToOfValue(v Value) map[CorrelationIdentifier]struct{} {
	if v == nil {
		return nil
	}
	out := map[CorrelationIdentifier]struct{}{}
	WalkValue(v, func(node Value) bool {
		// Source-anchored join RESULT value (RFC-077 F2, exploration-time HIDING).
		// Its leg QOVs are self-bound by the enclosing select's own join
		// quantifiers, so they are NOT external correlations — do NOT descend.
		// Reporting the buried leg aliases inflates every enclosing select's
		// correlation order and tips the ≥4-way STAR past the task budget.
		// Partition-time RE-EXPOSURE (the other half of the dual purpose) reads the
		// buried aliases structurally via predicates.AddMergeSeedAliases.
		if rc, ok := node.(*RecordConstructorValue); ok && rc.AnchoredJoin {
			return false
		}
		switch q := node.(type) {
		case *QuantifiedObjectValue:
			out[q.Correlation] = struct{}{}
		case *QuantifiedRecordValue:
			out[q.Alias] = struct{}{}
		// ExistsValue is a transparent composite (RFC-141): its child
		// QuantifiedObjectValue carries the correlation and is reached by
		// WalkValue's Children() descent, so no dedicated case is needed.
		case *ScalarSubqueryValue:
			out[q.Alias] = struct{}{}
		case *ObjectValue:
			out[q.Alias] = struct{}{}
		case *UnmatchedAggregateValue:
			out[q.UnmatchedID] = struct{}{}
		case *ConstantObjectValue:
			out[q.Alias] = struct{}{}
		}
		return true
	})
	return out
}

// GetCorrelatedToOfAnchoredJoinLegs returns the leg-quantifier correlations of a
// source-anchored join RESULT value (RFC-077 F2, partition-time RE-EXPOSURE).
// GetCorrelatedToOfValue deliberately does NOT descend into an anchored-join RC
// (exploration-time hiding keeps the search space bounded); this is the explicit
// counterpart that DOES, so PartitionSelectRule's predicate classification and
// AddMergeSeedAliases can see the buried leg aliases. It walks each field's value
// tree (FieldValue(QOV(leg), col) — possibly NESTED, when a leg is itself an
// anchored join not yet simplified away) and collects every QuantifiedObjectValue
// correlation, treating the anchored-RC children as ordinary nodes to descend.
//
// Returns nil for a nil or non-anchored input.
func GetCorrelatedToOfAnchoredJoinLegs(rc *RecordConstructorValue) map[CorrelationIdentifier]struct{} {
	if rc == nil || !rc.AnchoredJoin {
		return nil
	}
	out := map[CorrelationIdentifier]struct{}{}
	for _, f := range rc.Fields {
		WalkValue(f.Value, func(node Value) bool {
			switch q := node.(type) {
			case *QuantifiedObjectValue:
				out[q.Correlation] = struct{}{}
			case *QuantifiedRecordValue:
				out[q.Alias] = struct{}{}
			case *ObjectValue:
				out[q.Alias] = struct{}{}
			}
			return true
		})
	}
	return out
}
