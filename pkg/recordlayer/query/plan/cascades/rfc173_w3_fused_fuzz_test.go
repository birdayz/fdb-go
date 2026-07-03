package cascades

// RFC-173 S3-W3 — fuzz the fused-path max-match expansion (new infrastructure,
// non-negotiable-fuzz rule). The pre-existing FuzzComputeMaxMatchMap generator
// emits only FLAT single-accessor FieldValues, so it never reaches
// expandFusedFieldValue (guarded on len(Accessors) >= 2). This target builds
// FUSED paths of fuzz-driven depth and drives ComputeMaxMatchMap against every
// split-point candidate shape — asserting no panic and the full-member-set
// invariant (a fused query matches its fully-fused, every partial-split, and
// fully-chained candidate).

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func FuzzRFC173W3_FusedMaxMatch_NoPanic(f *testing.F) {
	f.Add(byte(2))
	f.Add(byte(3))
	f.Add(byte(4))
	f.Add(byte(6))
	f.Fuzz(func(t *testing.T, raw byte) {
		depth := int(raw%5) + 2 // 2..6
		// Build a depth-nested record type: top → l(depth-1) → … → leaf{C}.
		typ := values.NewRecordType("", false, []values.Field{{Name: "C", FieldType: values.NotNullLong, Ordinal: 0}})
		for i := 0; i < depth-1; i++ {
			typ = values.NewRecordType("", false, []values.Field{{Name: values.OrdinalFieldName(0), FieldType: typ, Ordinal: 0}})
		}
		q := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("m"), typ)

		// The fused query: descend all `depth` ordinals, then SimplifyValue to fuse.
		chained := func(steps int) *values.FieldValue {
			var cur values.Value = q
			var fv *values.FieldValue
			for i := 0; i < steps; i++ {
				next, err := values.NewFieldValueOfOrdinal(cur, 0)
				if err != nil {
					return nil // deeper than the type nests — skip
				}
				cur, fv = next, next
			}
			return fv
		}
		deepest := chained(depth)
		if deepest == nil {
			return
		}
		fusedV := values.SimplifyValue(deepest)
		fused, ok := fusedV.(*values.FieldValue)
		if !ok || fused.Resolved == nil || len(fused.Resolved.Accessors) != depth {
			return // compose didn't fully fuse (unexpected but not this target's bug)
		}

		// expandFusedFieldValue must yield exactly depth-1 forms, no panic.
		forms := expandFusedFieldValue(fused)
		if len(forms) != depth-1 {
			t.Fatalf("depth %d: expected %d split forms, got %d", depth, depth-1, len(forms))
		}

		// Every split-point candidate (a p-accessor fused prefix + chained
		// suffix) and the fully-fused candidate must max-match the fused query.
		if mmm := ComputeMaxMatchMap(fused, fused, nil); mmm.Size() < 1 {
			t.Fatalf("depth %d: fused-vs-fused must match", depth)
		}
		for p := 1; p <= depth-1; p++ {
			cand := candidateAtSplit(t, q, depth, p)
			if cand == nil {
				continue
			}
			if mmm := ComputeMaxMatchMap(fused, cand, nil); mmm.Size() < 1 {
				t.Fatalf("depth %d split p=%d: fused query must match this candidate shape", depth, p)
			}
		}
	})
}

// candidateAtSplit builds a candidate at split point p: a p-accessor fused
// prefix over q, then depth-p single-accessor chained nodes.
func candidateAtSplit(t *testing.T, q *values.QuantifiedObjectValue, depth, p int) *values.FieldValue {
	t.Helper()
	// Fused prefix of p accessors.
	var cur values.Value = q
	for i := 0; i < p; i++ {
		next, err := values.NewFieldValueOfOrdinal(cur, 0)
		if err != nil {
			return nil
		}
		cur = next
	}
	prefix, ok := values.SimplifyValue(cur).(*values.FieldValue)
	if !ok || len(prefix.Resolved.Accessors) != p {
		return nil
	}
	// Chain the remaining depth-p accessors as single-accessor nodes.
	cur = prefix
	var last *values.FieldValue
	for i := p; i < depth; i++ {
		next, err := values.NewFieldValueOfOrdinal(cur, 0)
		if err != nil {
			return nil
		}
		cur, last = next, next
	}
	return last
}
