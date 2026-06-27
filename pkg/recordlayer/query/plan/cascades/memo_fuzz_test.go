package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// FuzzMemo_MemoizeInvariant verifies the core invariant:
// structurally-equal expressions over the same child Reference(s)
// always return the same Reference from MemoizeExpression.
//
// The fuzzer generates random DAG shapes (varying record type names,
// predicate constants, and tree depth) and asserts:
//   - Two calls with equivalent expressions → same *Reference
//   - Calls with non-equivalent expressions → different *References
//   - The Memo's internal index stays consistent
func FuzzMemo_MemoizeInvariant(f *testing.F) {
	f.Add(uint8(0), uint8(1), uint8(2), false)
	f.Add(uint8(1), uint8(0), uint8(0), true)
	f.Add(uint8(2), uint8(3), uint8(1), false)
	f.Add(uint8(3), uint8(3), uint8(3), true)

	f.Fuzz(func(t *testing.T, recTypeSeed uint8, predSeed uint8, depthSeed uint8, useSort bool) {
		recTypes := []string{"A", "B", "C", "D"}
		recType := recTypes[int(recTypeSeed)%len(recTypes)]

		m := NewMemo(nil)

		// Build the leaf.
		scan := expressions.NewFullUnorderedScanExpression([]string{recType}, nil)
		scanRef := m.MemoizeExpression(scan)

		// Verify leaf idempotency.
		scan2 := expressions.NewFullUnorderedScanExpression([]string{recType}, nil)
		scanRef2 := m.MemoizeExpression(scan2)
		if scanRef != scanRef2 {
			t.Fatal("leaf memoization violated: same record type → different References")
		}

		// Different record type → different Reference.
		otherType := recTypes[(int(recTypeSeed)+1)%len(recTypes)]
		scanOther := expressions.NewFullUnorderedScanExpression([]string{otherType}, nil)
		scanRefOther := m.MemoizeExpression(scanOther)
		if recType != otherType && scanRef == scanRefOther {
			t.Fatal("distinct leaves got same Reference")
		}

		// Build a depth-1 operator over the leaf.
		var depth1Ref *expressions.Reference
		if useSort {
			keys := []expressions.SortKey{{
				Value:   values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier()),
				Reverse: false,
			}}
			sort := expressions.NewLogicalSortExpression(keys, expressions.ForEachQuantifier(scanRef))
			depth1Ref = m.MemoizeExpression(sort)

			// Rebuild same sort → same Reference.
			sort2 := expressions.NewLogicalSortExpression(keys, expressions.ForEachQuantifier(scanRef))
			depth1Ref2 := m.MemoizeExpression(sort2)
			if depth1Ref != depth1Ref2 {
				t.Fatal("non-leaf memoization violated: same sort → different References")
			}
		} else {
			predVals := []predicates.TriBool{predicates.TriTrue, predicates.TriFalse, predicates.TriUnknown, predicates.TriTrue}
			predVal := predVals[int(predSeed)%len(predVals)]
			pred := []predicates.QueryPredicate{predicates.NewConstantPredicate(predVal)}
			filter := expressions.NewLogicalFilterExpression(pred, expressions.ForEachQuantifier(scanRef))
			depth1Ref = m.MemoizeExpression(filter)

			// Rebuild same filter → same Reference.
			filter2 := expressions.NewLogicalFilterExpression(pred, expressions.ForEachQuantifier(scanRef))
			depth1Ref2 := m.MemoizeExpression(filter2)
			if depth1Ref != depth1Ref2 {
				t.Fatal("non-leaf memoization violated: same filter → different References")
			}
		}

		// Optionally build depth-2 (Distinct over the depth-1 node).
		depth := int(depthSeed) % 4
		if depth >= 2 && depth1Ref != nil {
			dist := expressions.NewLogicalDistinctExpression(expressions.ForEachQuantifier(depth1Ref))
			distRef := m.MemoizeExpression(dist)

			dist2 := expressions.NewLogicalDistinctExpression(expressions.ForEachQuantifier(depth1Ref))
			distRef2 := m.MemoizeExpression(dist2)
			if distRef != distRef2 {
				t.Fatal("depth-2 memoization violated")
			}
		}

		// Verify Memo consistency: every Reference in the Memo
		// has its members' child References also in the Memo.
		for ref := range m.References() {
			for _, member := range ref.Members() {
				for _, q := range member.GetQuantifiers() {
					child := q.GetRangesOver()
					if child == nil {
						continue
					}
					if !m.ContainsReference(child) {
						t.Fatal("Memo inconsistency: child Reference not indexed")
					}
				}
			}
		}
	})
}
