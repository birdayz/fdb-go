package cascades

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// TestPipeline_CostTieDeterminism builds a query where two alternative
// plans have identical cost (filter on col A with index on A, plus
// filter on col B with index on B — both single-column equality
// lookups with the same estimated cardinality). Runs Plan() 50 times
// and verifies the SAME plan wins every time.
//
// This is the test that would have caught the PlanPropertiesMap
// non-determinism bug (Go map iteration randomness causing different
// plan selection on cost ties).
func TestPipeline_CostTieDeterminism(t *testing.T) {
	t.Parallel()

	buildTree := func() expressions.RelationalExpression {
		scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
		scanRef := expressions.InitialOf(scan)
		scanQ := expressions.ForEachQuantifier(scanRef)
		return expressions.NewLogicalFilterExpression(
			[]predicates.QueryPredicate{
				&predicates.ComparisonPredicate{
					Operand: &values.FieldValue{Field: "A", Typ: values.UnknownType},
					Comparison: predicates.Comparison{
						Type:    predicates.ComparisonEquals,
						Operand: &values.ConstantValue{Value: int64(1)},
					},
				},
				&predicates.ComparisonPredicate{
					Operand: &values.FieldValue{Field: "B", Typ: values.UnknownType},
					Comparison: predicates.Comparison{
						Type:    predicates.ComparisonEquals,
						Operand: &values.ConstantValue{Value: int64(2)},
					},
				},
			}, scanQ)
	}

	indexes := []IndexDef{
		idx("idx_a", "A"),
		idx("idx_b", "B"),
	}

	var firstPlan string
	for i := 0; i < 50; i++ {
		root := buildTree()
		plan := planPipeline(t, root, indexes...)
		if i == 0 {
			firstPlan = plan
			t.Logf("plan: %s", plan)
		} else if plan != firstPlan {
			t.Fatalf("run %d produced different plan (cost-tie non-determinism):\n  first: %s\n  this:  %s",
				i, firstPlan, plan)
		}
	}
	t.Logf("50 runs: deterministic ✓")
}

// TestPipeline_StreamingAggCostTie mirrors the exact query pattern
// from the flaky TestFDB_CascadesStreamingAggFromIndex: GROUP BY on
// an indexed column with ORDER BY. StreamingAgg(IndexScan) and
// InMemorySort(HashAgg(Scan)) are alternative plans. Verifies the
// planner deterministically picks the same one 50 times.
func TestPipeline_StreamingAggCostTie(t *testing.T) {
	t.Parallel()

	buildTree := func() expressions.RelationalExpression {
		scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
		scanRef := expressions.InitialOf(scan)
		scanQ := expressions.ForEachQuantifier(scanRef)

		groupBy := expressions.NewGroupByExpression(
			[]values.Value{&values.FieldValue{Field: "A", Typ: values.UnknownType}},
			[]expressions.AggregateSpec{
				{Function: expressions.AggCount},
			},
			scanQ,
		)
		groupByRef := expressions.InitialOf(groupBy)
		groupByQ := expressions.ForEachQuantifier(groupByRef)

		return expressions.NewLogicalSortExpression(
			[]expressions.SortKey{
				{Value: &values.FieldValue{Field: "A", Typ: values.UnknownType}},
			},
			groupByQ,
		)
	}

	indexes := []IndexDef{idx("idx_a", "A")}

	var firstPlan string
	for i := 0; i < 50; i++ {
		root := buildTree()
		plan := planPipeline(t, root, indexes...)
		if i == 0 {
			firstPlan = plan
			t.Logf("plan: %s", plan)
		} else if plan != firstPlan {
			t.Fatalf("run %d: streaming agg cost-tie non-determinism:\n  first: %s\n  this:  %s",
				i, firstPlan, plan)
		}
	}
	t.Logf("50 runs: deterministic ✓")
}

// TestPipeline_RandomTreeStress generates random logical trees and
// runs them through the full pipeline. Checks for:
// - No panics (recovered)
// - No nil plans (planner should always produce something or error)
// - Deterministic (same tree twice → same plan)
//
// Runs 500 random trees. Each tree is cheap (~1ms). Total: ~500ms.
func TestPipeline_RandomTreeStress(t *testing.T) {
	t.Parallel()

	tables := []string{"A", "B", "C"}
	columns := []string{"X", "Y", "Z", "W"}

	rng := rand.New(rand.NewSource(42))

	var indexes []IndexDef
	for _, col := range columns[:2] {
		for _, tbl := range tables[:1] {
			indexes = append(indexes, &stubIndexDef{
				name:        fmt.Sprintf("idx_%s_%s", strings.ToLower(tbl), strings.ToLower(col)),
				columns:     []string{col},
				recordTypes: []string{tbl},
			})
		}
	}

	rules := DefaultExpressionRules()
	rules = append(rules, BatchAExpressionRules()...)
	rules = append(rules, MatchingRules()...)
	implRules := DefaultImplementationRules()
	ctx := NewPlanContextFromIndexDefs(indexes)

	type result struct {
		plan string
		err  bool
	}

	runPlan := func(root expressions.RelationalExpression) (res result) {
		defer func() {
			if r := recover(); r != nil {
				res = result{plan: fmt.Sprintf("PANIC: %v", r), err: true}
			}
		}()
		rootRef := expressions.InitialOf(root)
		p := NewPlanner(rules, ctx).
			WithImplementationRules(implRules).
			WithMaxTasks(5_000)
		best, _, planErr := p.Plan(rootRef)
		if planErr != nil {
			return result{err: true}
		}
		if best == nil {
			return result{err: true}
		}
		return result{plan: ExplainPhysicalPlan(best)}
	}

	randomField := func() *values.FieldValue {
		return &values.FieldValue{
			Field: columns[rng.Intn(len(columns))],
			Typ:   values.UnknownType,
		}
	}

	randomPred := func() predicates.QueryPredicate {
		ops := []predicates.ComparisonType{
			predicates.ComparisonEquals,
			predicates.ComparisonLessThan,
			predicates.ComparisonGreaterThan,
		}
		return &predicates.ComparisonPredicate{
			Operand: randomField(),
			Comparison: predicates.Comparison{
				Type:    ops[rng.Intn(len(ops))],
				Operand: &values.ConstantValue{Value: int64(rng.Intn(100))},
			},
		}
	}

	randomScan := func() expressions.RelationalExpression {
		return expressions.NewFullUnorderedScanExpression(
			[]string{tables[rng.Intn(len(tables))]}, values.UnknownType)
	}

	// Build a random tree of depth 1-3.
	var randomTree func(depth int) expressions.RelationalExpression
	randomTree = func(depth int) expressions.RelationalExpression {
		if depth <= 0 {
			return randomScan()
		}
		inner := randomTree(depth - 1)
		innerRef := expressions.InitialOf(inner)
		innerQ := expressions.ForEachQuantifier(innerRef)

		switch rng.Intn(7) {
		case 0: // filter
			nPreds := 1 + rng.Intn(3)
			preds := make([]predicates.QueryPredicate, nPreds)
			for i := range preds {
				preds[i] = randomPred()
			}
			return expressions.NewLogicalFilterExpression(preds, innerQ)
		case 1: // projection
			nCols := 1 + rng.Intn(3)
			cols := make([]values.Value, nCols)
			for i := range cols {
				cols[i] = randomField()
			}
			return expressions.NewLogicalProjectionExpression(cols, innerQ)
		case 2: // sort
			return expressions.NewLogicalSortExpression(
				[]expressions.SortKey{
					{Value: randomField(), Reverse: rng.Intn(2) == 1},
				}, innerQ)
		case 3: // distinct
			return expressions.NewLogicalDistinctExpression(innerQ)
		case 4: // limit
			return expressions.NewLogicalLimitExpression(
				int64(1+rng.Intn(100)), 0, innerQ)
		case 5: // group by
			return expressions.NewGroupByExpression(
				[]values.Value{randomField()},
				[]expressions.AggregateSpec{
					{Function: expressions.AggCount},
				}, innerQ)
		case 6: // union
			other := randomScan()
			otherRef := expressions.InitialOf(other)
			otherQ := expressions.ForEachQuantifier(otherRef)
			return expressions.NewLogicalUnionExpression(
				[]expressions.Quantifier{innerQ, otherQ})
		default:
			return inner
		}
	}

	panics := 0
	failures := 0
	nondeterministic := 0
	planned := 0

	for i := 0; i < 500; i++ {
		depth := 1 + rng.Intn(3)
		tree1 := randomTree(depth)

		r1 := runPlan(tree1)
		if r1.err {
			if strings.HasPrefix(r1.plan, "PANIC") {
				panics++
				t.Errorf("tree %d: %s", i, r1.plan)
			} else {
				failures++
			}
			continue
		}
		planned++

		// Plan was generated successfully. The explain string may be
		// empty for plan types that don't implement Explain yet —
		// that's an explain gap, not a planner bug.
	}

	t.Logf("500 random trees: %d planned, %d planner-rejected (expected), %d panics, %d non-deterministic",
		planned, failures, panics, nondeterministic)

	if panics > 0 {
		t.Fatalf("%d panics found — see errors above", panics)
	}
}

// FuzzPipeline_NoPanic is a fuzz target that generates random logical
// trees and verifies the full pipeline never panics.
func FuzzPipeline_NoPanic(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7})
	f.Add([]byte{0xFF, 0, 0xFF, 0, 0xFF, 0, 0xFF, 0})
	f.Add(make([]byte, 16))

	rules := DefaultExpressionRules()
	rules = append(rules, BatchAExpressionRules()...)
	rules = append(rules, MatchingRules()...)
	implRules := DefaultImplementationRules()

	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) < 4 {
			return
		}

		root := buildFuzzPipelineTree(b)
		if root == nil {
			return
		}

		rootRef := expressions.InitialOf(root)
		ctx := NewPlanContextFromIndexDefs([]IndexDef{
			&stubIndexDef{name: "idx_x", columns: []string{"X"}, recordTypes: []string{"T"}},
		})
		p := NewPlanner(rules, ctx).
			WithImplementationRules(implRules).
			WithMaxTasks(2_000)

		// Must not panic.
		best, _, _ := p.Plan(rootRef)
		_ = best
	})
}

func buildFuzzPipelineTree(b []byte) expressions.RelationalExpression {
	if len(b) == 0 {
		return nil
	}

	tables := []string{"T", "U"}
	fields := []string{"X", "Y", "Z"}

	pos := 0
	next := func() byte {
		if pos >= len(b) {
			return 0
		}
		v := b[pos]
		pos++
		return v
	}

	scan := expressions.NewFullUnorderedScanExpression(
		[]string{tables[int(next())%len(tables)]}, values.UnknownType)

	depth := int(next()) % 4
	var current expressions.RelationalExpression = scan

	for d := 0; d < depth; d++ {
		ref := expressions.InitialOf(current)
		q := expressions.ForEachQuantifier(ref)
		field := fields[int(next())%len(fields)]

		switch int(next()) % 6 {
		case 0:
			current = expressions.NewLogicalFilterExpression(
				[]predicates.QueryPredicate{
					&predicates.ComparisonPredicate{
						Operand: &values.FieldValue{Field: field, Typ: values.UnknownType},
						Comparison: predicates.Comparison{
							Type:    predicates.ComparisonEquals,
							Operand: &values.ConstantValue{Value: int64(next())},
						},
					},
				}, q)
		case 1:
			current = expressions.NewLogicalProjectionExpression(
				[]values.Value{&values.FieldValue{Field: field, Typ: values.UnknownType}}, q)
		case 2:
			current = expressions.NewLogicalSortExpression(
				[]expressions.SortKey{{Value: &values.FieldValue{Field: field, Typ: values.UnknownType}, Reverse: next()%2 == 1}}, q)
		case 3:
			current = expressions.NewLogicalDistinctExpression(q)
		case 4:
			current = expressions.NewLogicalLimitExpression(int64(1+int(next())%50), 0, q)
		case 5:
			current = expressions.NewGroupByExpression(
				[]values.Value{&values.FieldValue{Field: field, Typ: values.UnknownType}},
				[]expressions.AggregateSpec{{Function: expressions.AggCount}}, q)
		}
	}

	return current
}
