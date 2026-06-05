package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// FuzzDataAccessScan_NoPanic exercises the data-access scan path (the sole scan
// producer after RFC-076 retired ImplementIndexScanRule) with random
// predicate/column combinations to ensure no panics. It provides actual
// MatchCandidates so the candidate matching logic (column mapping, merge, prefix
// computation) is exercised through the planner.
func FuzzDataAccessScan_NoPanic(f *testing.F) {
	f.Add(byte(0), byte(0), byte(0), byte(0))
	f.Add(byte(1), byte(2), byte(3), byte(4))
	f.Add(byte(255), byte(255), byte(255), byte(255))
	f.Add(byte(0), byte(1), byte(0), byte(1))

	colPool := []string{"A", "B", "C", "D", "STATUS", "AMOUNT", "DATE"}
	cmpTypes := []predicates.ComparisonType{
		predicates.ComparisonEquals,
		predicates.ComparisonGreaterThan,
		predicates.ComparisonLessThan,
		predicates.ComparisonGreaterThanEq,
		predicates.ComparisonLessThanOrEq,
		predicates.ComparisonNotEquals,
	}

	f.Fuzz(func(t *testing.T, numPreds, numCols, predSeed, colSeed byte) {
		nPreds := int(numPreds%5) + 1
		nCols := int(numCols%4) + 1

		candCols := make([]string, nCols)
		aliases := make([]values.CorrelationIdentifier, nCols)
		for i := range candCols {
			candCols[i] = colPool[int(colSeed+byte(i))%len(colPool)]
			aliases[i] = values.UniqueCorrelationIdentifier()
		}
		cand := NewValueIndexScanMatchCandidate(
			"fuzz_index",
			[]string{"T"},
			candCols,
			aliases,
			values.UnknownType,
			numCols%2 == 0,
			nil,
		)
		ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}

		scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
		scanRef := expressions.InitialOf(scan)
		q := expressions.ForEachQuantifier(scanRef)

		preds := make([]predicates.QueryPredicate, nPreds)
		for i := range preds {
			col := colPool[int(predSeed+byte(i))%len(colPool)]
			cmpType := cmpTypes[int(predSeed+byte(i*3))%len(cmpTypes)]
			preds[i] = predicates.NewComparisonPredicate(
				&values.FieldValue{Field: col, Typ: values.TypeInt},
				predicates.NewLiteralComparison(cmpType, int64(i+1)),
			)
		}

		filter := expressions.NewLogicalFilterExpression(preds, q)
		filterRef := expressions.InitialOf(filter)

		// Drive the full planner (data-access path) for panics on random shapes —
		// exercises the same candidate matching the retired rule did, now via the
		// match infrastructure. Result/error are irrelevant; we only assert no panic.
		rules := DefaultExpressionRules()
		p := NewPlanner(rules, ctx).
			WithPlanningExpressionRules(BatchAExpressionRules()).
			WithImplementationRules(DefaultImplementationRules())
		_, _, _ = p.Plan(filterRef)
	})
}

// FuzzInExplode_NoPanic exercises the InComparisonToExplodeRule with
// random IN list sizes to ensure no panics or index-out-of-range.
func FuzzInExplode_NoPanic(f *testing.F) {
	f.Add(byte(0), byte(0))
	f.Add(byte(5), byte(1))
	f.Add(byte(255), byte(255))

	f.Fuzz(func(t *testing.T, listSize, extraPreds byte) {
		nList := int(listSize % 20)
		nExtra := int(extraPreds % 5)

		scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
		scanRef := expressions.InitialOf(scan)
		q := expressions.ForEachQuantifier(scanRef)

		var preds []predicates.QueryPredicate

		if nList > 0 {
			items := make([]any, nList)
			for i := range items {
				items[i] = int64(i)
			}
			inList := &values.ConstantValue{Value: items, Typ: values.TypeUnknown}
			preds = append(preds, predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "STATUS", Typ: values.TypeInt},
				predicates.Comparison{Type: predicates.ComparisonIn, Operand: inList},
			))
		}

		for i := 0; i < nExtra; i++ {
			preds = append(preds, predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "OTHER", Typ: values.TypeInt},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(i)),
			))
		}

		if len(preds) == 0 {
			preds = append(preds, predicates.NewConstantPredicate(predicates.TriTrue))
		}

		filter := expressions.NewLogicalFilterExpression(preds, q)
		ref := expressions.InitialOf(filter)

		rule := NewInComparisonToExplodeRule()
		FireExpressionRuleWithMemo(rule, ref, EmptyPlanContext(), nil)
	})
}

// FuzzOrderedIndexScan_NoPanic exercises the OrderedIndexScanRule with
// random sort key / index column combinations to ensure no panics.
func FuzzOrderedIndexScan_NoPanic(f *testing.F) {
	f.Add(byte(1), byte(2), byte(0))
	f.Add(byte(3), byte(1), byte(42))
	f.Add(byte(0), byte(5), byte(255))

	colPool := []string{"A", "B", "C", "D", "STATUS", "DATE", "AMOUNT"}

	f.Fuzz(func(t *testing.T, numSortKeys, numCols, seed byte) {
		nSort := int(numSortKeys%4) + 1
		nCols := int(numCols%5) + 1

		candCols := make([]string, nCols)
		aliases := make([]values.CorrelationIdentifier, nCols)
		for i := range candCols {
			candCols[i] = colPool[(int(seed)+i)%len(colPool)]
			aliases[i] = values.UniqueCorrelationIdentifier()
		}
		cand := NewValueIndexScanMatchCandidate(
			"fuzz_ordered_idx",
			[]string{"T"},
			candCols,
			aliases,
			values.UnknownType,
			false,
			nil,
		)
		ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}

		scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
		scanRef := expressions.InitialOf(scan)
		q := expressions.ForEachQuantifier(scanRef)

		sortKeys := make([]expressions.SortKey, nSort)
		for i := range sortKeys {
			col := colPool[(int(seed)+i*2)%len(colPool)]
			sortKeys[i] = expressions.SortKey{
				Value: &values.FieldValue{Field: col, Typ: values.UnknownType},
			}
		}

		sort := expressions.NewLogicalSortExpression(sortKeys, q)
		sortRef := expressions.InitialOf(sort)

		rule := NewOrderedIndexScanRule()
		FireExpressionRuleWithMemo(rule, sortRef, ctx, nil)
	})
}

// FuzzComparisonRange_MergeChain exercises the ComparisonRange merge
// logic with random comparison sequences to ensure no panics and that
// merge failure (Ok=false) never leaves the range in an inconsistent state.
func FuzzComparisonRange_MergeChain(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5})
	f.Add([]byte{0, 0, 0, 0})
	f.Add([]byte{255, 255, 255, 255, 255, 255, 255, 255})

	cmpTypes := []predicates.ComparisonType{
		predicates.ComparisonEquals,
		predicates.ComparisonGreaterThan,
		predicates.ComparisonLessThan,
		predicates.ComparisonGreaterThanEq,
		predicates.ComparisonLessThanOrEq,
		predicates.ComparisonNotEquals,
	}

	f.Fuzz(func(t *testing.T, ops []byte) {
		if len(ops) < 2 {
			return
		}
		r := predicates.EmptyComparisonRange()
		for i := 0; i+1 < len(ops); i += 2 {
			cmpType := cmpTypes[int(ops[i])%len(cmpTypes)]
			val := int64(ops[i+1])
			c := predicates.NewLiteralComparison(cmpType, val)
			res := r.Merge(&c)
			if res.Ok {
				r = res.Range
			}
			if r.IsEquality() && r.GetEqualityComparison() == nil {
				t.Fatal("equality range has nil comparison")
			}
			if r.IsInequality() && len(r.GetInequalityComparisons()) == 0 {
				t.Fatal("inequality range has empty comparisons")
			}
		}
	})
}
