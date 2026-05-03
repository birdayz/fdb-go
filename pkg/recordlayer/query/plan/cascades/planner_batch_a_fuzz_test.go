package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// FuzzPlanner_WithBatchA_NoPanic pins that the Planner with the
// FULL rule set — DefaultExpressionRules + BatchAExpressionRules —
// terminates and doesn't panic on random expression trees. The
// existing FuzzPlanner_PlanFullPipeline drives a smaller logical-
// only set; this one specifically stresses the implement rules
// (PrimaryScan / ImplementFilter / ImplementSort /
// ImplementDistinct / ImplementTypeFilter / ImplementUnion /
// ImplementIntersection) interacting with the logical-rewrite
// chain.
//
// Why a separate target: the implement rules introduce physical
// wrappers into References which the logical rewrites must
// tolerate. The 5-wrapper-symmetry asymmetries that bit us mid-
// shift would surface here as planner non-termination or panics
// — not as missed-fire bugs in unit tests.
//
// MaxTasks set generously since the combined rule set produces
// more alternatives than the logical-only set; the seed expression
// shapes from buildFuzzExpression typically converge in <2k tasks.
func FuzzPlanner_WithBatchA_NoPanic(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7})
	f.Add([]byte{0xFF, 0xFF, 0xFF, 0xFF})
	f.Add(make([]byte, 16))
	// Specific seeds exercising shapes the 7-wrapper symmetry fix
	// enabled — the buildFuzzExpression case selectors map to the
	// shapes documented in fixpoint_fuzz_test.go.
	//   0 → Filter
	//   5 → Union(2 children)
	//   7 → Intersection(2 children)
	// This corpus pre-loads Filter-over-Union, Filter-over-Intersection,
	// Union-over-Intersection, etc., so the first iteration exercises
	// the wrapper-symmetry paths.
	f.Add([]byte{0, 5, 0, 0, 0, 0, 0, 0}) // Filter over Union
	f.Add([]byte{0, 7, 0, 0, 0, 0, 0, 0}) // Filter over Intersection
	f.Add([]byte{5, 7, 0, 0, 0, 0, 0, 0}) // Union over Intersection
	f.Add([]byte{7, 5, 0, 0, 0, 0, 0, 0}) // Intersection over Union

	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) < 4 {
			return
		}
		expr := buildFuzzExpression(b, 0, 0)
		ref := expressions.InitialOf(expr)

		// Combine logical-rewrite + Batch A read-side implement rules.
		rules := append(DefaultExpressionRules(), BatchAExpressionRules()...)

		p := NewPlanner(rules, nil)
		p.MaxTasks = 50_000

		plan, _, err := p.Plan(ref)
		if err != nil && err != ErrPlannerCapHit {
			t.Fatalf("Plan: unexpected err %v", err)
		}
		// Plan succeeded → root must have a BestMember stamp.
		if err == nil && !p.HasBestMember(ref) {
			t.Fatal("Plan succeeded but root Reference has no BestMember stamp — invariant break")
		}
		// Plan succeeded → returned plan must be non-nil (cost
		// extraction always picks SOME member from a non-empty
		// Reference).
		if err == nil && plan == nil {
			t.Fatal("Plan succeeded but plan is nil — invariant break")
		}
	})
}

// FuzzPlanner_WithIndexCandidates_NoPanic exercises the full planner
// (Default + BatchA rules) with actual MatchCandidates so that
// ImplementIndexScanRule, IndexIntersectionRule, OrderedIndexScanRule,
// and SortOverOrderedElimRule all fire during the same planning run.
// Catches panics in the index-rule interaction paths that nil-PlanContext
// targets can't reach.
func FuzzPlanner_WithIndexCandidates_NoPanic(f *testing.F) {
	f.Add(byte(2), byte(3), byte(1), byte(0))
	f.Add(byte(0), byte(0), byte(0), byte(0))
	f.Add(byte(5), byte(5), byte(3), byte(255))
	f.Add(byte(2), byte(1), byte(0), byte(128)) // GroupBy path (seed >= 128)

	colPool := []string{"A", "B", "C", "D", "STATUS", "AMOUNT", "DATE"}

	f.Fuzz(func(t *testing.T, numPreds, numCands, numSort, seed byte) {
		nPreds := int(numPreds%4) + 1
		nCands := int(numCands%3) + 1
		nSort := int(numSort % 3)

		var candidates []MatchCandidate
		for c := 0; c < nCands; c++ {
			nCols := int(seed+byte(c*7))%3 + 1
			cols := make([]string, nCols)
			aliases := make([]values.CorrelationIdentifier, nCols)
			for i := range cols {
				cols[i] = colPool[(int(seed)+c*3+i)%len(colPool)]
				aliases[i] = values.UniqueCorrelationIdentifier()
			}
			candidates = append(candidates, NewValueIndexScanMatchCandidate(
				"idx_"+cols[0],
				[]string{"T"},
				cols,
				aliases,
				values.UnknownType,
				seed%5 == 0,
			))
		}
		ctx := &indexTestPlanContext{candidates: candidates}

		scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
		scanRef := expressions.InitialOf(scan)
		q := expressions.ForEachQuantifier(scanRef)

		preds := make([]predicates.QueryPredicate, nPreds)
		for i := range preds {
			col := colPool[(int(seed)+i)%len(colPool)]
			preds[i] = predicates.NewComparisonPredicate(
				&values.FieldValue{Field: col, Typ: values.TypeInt},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(i+1)),
			)
		}

		filter := expressions.NewLogicalFilterExpression(preds, q)
		var topRef *expressions.Reference

		if nSort > 0 {
			filterRef := expressions.InitialOf(filter)
			filterQ := expressions.ForEachQuantifier(filterRef)
			sortKeys := make([]expressions.SortKey, nSort)
			for i := range sortKeys {
				col := colPool[(int(seed)+i*2)%len(colPool)]
				sortKeys[i] = expressions.SortKey{
					Value: &values.FieldValue{Field: col, Typ: values.UnknownType},
				}
			}
			sort := expressions.NewLogicalSortExpression(sortKeys, filterQ)
			topRef = expressions.InitialOf(sort)
		} else {
			topRef = expressions.InitialOf(filter)
		}

		// Optionally wrap with GroupBy (seed >= 128 triggers agg path).
		if seed >= 128 {
			topQ := expressions.ForEachQuantifier(topRef)
			nGroupKeys := int(seed%3) + 1
			groupKeys := make([]values.Value, nGroupKeys)
			for i := range groupKeys {
				groupKeys[i] = &values.FieldValue{
					Field: colPool[(int(seed)+i*5)%len(colPool)],
					Typ:   values.UnknownType,
				}
			}
			gb := expressions.NewGroupByExpression(
				groupKeys,
				[]expressions.AggregateSpec{
					{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
				},
				topQ,
			)
			topRef = expressions.InitialOf(gb)
		}

		rules := append(DefaultExpressionRules(), BatchAExpressionRules()...)
		p := NewPlanner(rules, ctx)
		p.MaxTasks = 50_000

		plan, _, err := p.Plan(topRef)
		if err != nil && err != ErrPlannerCapHit {
			t.Fatalf("Plan: unexpected err %v", err)
		}
		if err == nil && plan == nil {
			t.Fatal("Plan succeeded but plan is nil")
		}
	})
}
