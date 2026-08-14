package cascades

// Fuzzes canonical fused-path max matching. RFC-232 admits one representation
// for a resolved nested read: a QOV-rooted FieldValue carrying the complete
// ordinal vector. Building the same descent one step at a time must therefore
// converge on the direct full-path value, and max-match must recognize every
// construction sequence without expanding back into chained FieldValues.

import (
	"slices"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func FuzzFusedMaxMatch_NoPanic(f *testing.F) {
	f.Add(byte(2))
	f.Add(byte(3))
	f.Add(byte(4))
	f.Add(byte(6))
	f.Fuzz(func(t *testing.T, raw byte) {
		depth := int(raw%5) + 2 // 2..6
		// Build a depth-nested record type: top -> _0 -> ... -> leaf{C}.
		typ := values.NewRecordType("Leaf", false, []values.Field{
			{Name: "C", FieldType: values.NotNullLong, Ordinal: 0},
		})
		for i := 0; i < depth-1; i++ {
			typ = values.NewRecordType("", false, []values.Field{
				{Name: values.OrdinalFieldName(0), FieldType: typ, Ordinal: 0},
			})
		}
		qov, err := values.NewQuantifiedObjectValue(
			values.NamedCorrelationIdentifier("m"), typ)
		qov = mustConstruct(t, qov, err)

		ordinals := make([]int, depth)
		direct, err := values.ResolveFieldOrdinals(qov, ordinals)
		direct = mustConstruct(t, direct, err)
		directField, ok := values.AsFieldValue(direct)
		if !ok || directField.Path() == nil {
			t.Fatalf("depth %d: direct path = %T, want exact FieldValue", depth, direct)
		}
		if got := directField.Path().Ordinals(); !slices.Equal(got, ordinals) {
			t.Fatalf("depth %d: direct path = %v, want %v", depth, got, ordinals)
		}

		// The inverse fused->chain expansion was deliberately retired: exact
		// scalar paths have no alternative representation to publish.
		if forms := expandValueForMatching(direct); len(forms) != 0 {
			t.Fatalf("depth %d: canonical scalar path produced %d alternate forms", depth, len(forms))
		}
		if match := ComputeMaxMatchMap(direct, direct, nil); match.Size() < 1 {
			t.Fatalf("depth %d: canonical path must max-match itself", depth)
		}

		// Resolve a prefix atomically, then append the suffix one ordinal at a
		// time. Every split must canonicalize to the same QOV-rooted full path.
		for prefixLen := 1; prefixLen < depth; prefixLen++ {
			candidate := candidateAtSplit(t, qov, depth, prefixLen)
			if !values.ValuesStructurallyEqual(direct, candidate) {
				t.Fatalf("depth %d split %d: candidate did not canonicalize to direct path",
					depth, prefixLen)
			}
			candidateField, isField := values.AsFieldValue(candidate)
			if !isField || candidateField.Path() == nil ||
				!slices.Equal(candidateField.Path().Ordinals(), ordinals) {
				t.Fatalf("depth %d split %d: candidate = %T path %v, want %v",
					depth, prefixLen, candidate, fieldOrdinals(candidate), ordinals)
			}
			if match := ComputeMaxMatchMap(direct, candidate, nil); match.Size() < 1 {
				t.Fatalf("depth %d split %d: canonical equivalents must max-match", depth, prefixLen)
			}
		}
	})
}

// candidateAtSplit resolves a prefix of length prefixLen, then resolves each
// remaining step separately. ResolveFieldOrdinals fuses each new step onto the
// admitted prefix's original exact QOV root.
func candidateAtSplit(
	t testing.TB,
	qov values.QuantifiedObjectValue,
	depth, prefixLen int,
) values.Value {
	t.Helper()
	prefix := make([]int, prefixLen)
	current, err := values.ResolveFieldOrdinals(qov, prefix)
	current = mustConstruct(t, current, err)
	for i := prefixLen; i < depth; i++ {
		next, resolveErr := values.ResolveFieldOrdinals(current, []int{0})
		current = mustConstruct(t, next, resolveErr)
	}
	return current
}

func fieldOrdinals(value values.Value) []int {
	field, ok := values.AsFieldValue(value)
	if !ok || field.Path() == nil {
		return nil
	}
	return field.Path().Ordinals()
}
