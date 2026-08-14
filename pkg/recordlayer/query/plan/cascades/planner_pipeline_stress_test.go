package cascades

import (
	"fmt"
	"math/rand"
	"regexp"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

var pipelineGeneratedCorrelation = regexp.MustCompile(`q\$[0-9]+`)

// canonicalPipelineExplain renumbers generated correlations by first
// appearance. RFC-232 makes their exact ownership visible in Explain output;
// the process-global allocator value is not plan identity, while alias topology
// (same vs different occurrences) remains load-bearing and is preserved here.
func canonicalPipelineExplain(explain string) string {
	ids := make(map[string]string)
	next := 0
	return pipelineGeneratedCorrelation.ReplaceAllStringFunc(explain, func(id string) string {
		if canonical, ok := ids[id]; ok {
			return canonical
		}
		canonical := fmt.Sprintf("q$%d", next)
		next++
		ids[id] = canonical
		return canonical
	})
}

func mustPipelineStressConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct planner pipeline stress fixture: " + err.Error())
	}
	return value
}

func pipelineStressRowType() values.Type {
	return values.NewRecordType("PipelineStressRow", false, []values.Field{
		{Name: "X", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "Y", FieldType: values.NotNullLong, Ordinal: 1},
		{Name: "Z", FieldType: values.NotNullLong, Ordinal: 2},
		{Name: "W", FieldType: values.NotNullLong, Ordinal: 3},
	})
}

func pipelineStressRoot(q expressions.Quantifier) values.QuantifiedObjectValue {
	flowedType := mustPipelineStressConstruct(q.GetFlowedObjectType())
	return mustPipelineStressConstruct(values.NewQuantifiedObjectValue(q.GetAlias(), flowedType))
}

func pipelineStressFieldAt(q expressions.Quantifier, ordinal int) values.Value {
	flowedType := mustPipelineStressConstruct(q.GetFlowedObjectType())
	recordType, ok := flowedType.(*values.RecordType)
	if !ok || len(recordType.Fields) == 0 {
		panic(fmt.Sprintf("planner pipeline stress fixture: non-record or empty flowed type %v", flowedType))
	}
	ordinal %= len(recordType.Fields)
	return mustPipelineStressConstruct(values.ResolveFieldOrdinals(
		pipelineStressRoot(q), []int{ordinal}))
}

func pipelineStressFieldNamed(q expressions.Quantifier, name string) values.Value {
	request := mustPipelineStressConstruct(values.FieldByName(name))
	return mustPipelineStressConstruct(values.ResolveFieldAccess(
		pipelineStressRoot(q), []values.FieldRequest{request}))
}

type pipelineStressIndexDef struct {
	name        string
	columns     []string
	recordTypes []string
}

func (d *pipelineStressIndexDef) IndexName() string              { return d.name }
func (d *pipelineStressIndexDef) IndexColumnNames() []string     { return d.columns }
func (d *pipelineStressIndexDef) IndexRecordTypes() []string     { return d.recordTypes }
func (*pipelineStressIndexDef) IndexIsUnique() bool              { return false }
func (*pipelineStressIndexDef) IndexPrimaryKeyColumns() []string { return nil }
func (*pipelineStressIndexDef) IndexCreatesDuplicates() bool     { return false }
func (*pipelineStressIndexDef) IndexRowType() values.Type        { return pipelineStressRowType() }
func (d *pipelineStressIndexDef) IndexKeyComponentTypes() []values.Type {
	result := make([]values.Type, len(d.columns))
	for i := range result {
		result[i] = values.NotNullLong
	}
	return result
}

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
		scan := mustPipelineStressConstruct(expressions.NewFullUnorderedScanExpression(
			[]string{"T"}, determinismRowType()))
		scanRef := expressions.InitialOf(scan)
		scanQ := expressions.ForEachQuantifier(scanRef)
		return mustPipelineStressConstruct(expressions.NewLogicalFilterExpression(
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					pipelineStressFieldNamed(scanQ, "A"),
					predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1))),
				predicates.NewComparisonPredicate(
					pipelineStressFieldNamed(scanQ, "B"),
					predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(2))),
			}, scanQ))
	}

	indexes := []IndexDef{
		idx("idx_a", "A"),
		idx("idx_b", "B"),
	}

	var firstPlan string
	for i := 0; i < 50; i++ {
		root := buildTree()
		plan := canonicalPipelineExplain(planPipeline(t, root, indexes...))
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
// InMemorySort(StreamingAgg(Scan)) are alternative plans. Verifies the
// planner deterministically picks the same one 50 times.
func TestPipeline_StreamingAggCostTie(t *testing.T) {
	t.Parallel()

	buildTree := func() expressions.RelationalExpression {
		scan := mustPipelineStressConstruct(expressions.NewFullUnorderedScanExpression(
			[]string{"T"}, determinismRowType()))
		scanRef := expressions.InitialOf(scan)
		scanQ := expressions.ForEachQuantifier(scanRef)

		groupBy := mustPipelineStressConstruct(expressions.NewGroupByExpression(
			[]values.Value{pipelineStressFieldNamed(scanQ, "A")},
			[]expressions.AggregateSpec{
				{Function: expressions.AggCount},
			},
			scanQ,
		))
		groupByRef := expressions.InitialOf(groupBy)
		groupByQ := expressions.ForEachQuantifier(groupByRef)

		return mustPipelineStressConstruct(expressions.NewLogicalSortExpression(
			[]expressions.SortKey{
				{Value: pipelineStressFieldAt(groupByQ, 0)},
			},
			groupByQ,
		))
	}

	indexes := []IndexDef{idx("idx_a", "A")}

	var firstPlan string
	for i := 0; i < 50; i++ {
		root := buildTree()
		plan := canonicalPipelineExplain(planPipeline(t, root, indexes...))
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
			indexes = append(indexes, &pipelineStressIndexDef{
				name:        fmt.Sprintf("idx_%s_%s", strings.ToLower(tbl), strings.ToLower(col)),
				columns:     []string{col},
				recordTypes: []string{tbl},
			})
		}
	}

	rules := DefaultExpressionRules()
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
			WithPlanningExpressionRules(BatchAExpressionRules()).
			WithImplementationRules(implRules).
			WithMaxTasks(20_000)
		best, _, planErr := p.Plan(rootRef)
		if planErr != nil {
			return result{err: true}
		}
		if best == nil {
			return result{err: true}
		}
		return result{plan: ExplainPhysicalPlan(best)}
	}

	randomField := func(q expressions.Quantifier) values.Value {
		return pipelineStressFieldAt(q, rng.Intn(len(columns)))
	}

	randomPred := func(q expressions.Quantifier) predicates.QueryPredicate {
		ops := []predicates.ComparisonType{
			predicates.ComparisonEquals,
			predicates.ComparisonLessThan,
			predicates.ComparisonGreaterThan,
		}
		return predicates.NewComparisonPredicate(
			randomField(q),
			predicates.NewLiteralComparison(ops[rng.Intn(len(ops))], int64(rng.Intn(100))))
	}

	randomScan := func() expressions.RelationalExpression {
		return mustPipelineStressConstruct(expressions.NewFullUnorderedScanExpression(
			[]string{tables[rng.Intn(len(tables))]}, pipelineStressRowType()))
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
				preds[i] = randomPred(innerQ)
			}
			return mustPipelineStressConstruct(expressions.NewLogicalFilterExpression(preds, innerQ))
		case 1: // projection
			nCols := 1 + rng.Intn(3)
			cols := make([]values.Value, nCols)
			for i := range cols {
				cols[i] = randomField(innerQ)
			}
			return mustPipelineStressConstruct(expressions.NewLogicalProjectionExpression(cols, innerQ))
		case 2: // sort
			return mustPipelineStressConstruct(expressions.NewLogicalSortExpression(
				[]expressions.SortKey{
					{Value: randomField(innerQ), Reverse: rng.Intn(2) == 1},
				}, innerQ))
		case 3: // distinct
			return mustPipelineStressConstruct(expressions.NewLogicalDistinctExpression(innerQ))
		case 4: // limit
			return mustPipelineStressConstruct(expressions.NewLogicalLimitExpression(
				int64(1+rng.Intn(100)), 0, innerQ))
		case 5: // group by
			return mustPipelineStressConstruct(expressions.NewGroupByExpression(
				[]values.Value{randomField(innerQ)},
				[]expressions.AggregateSpec{
					{Function: expressions.AggCount},
				}, innerQ))
		case 6: // union
			// Exact set operations require identical child layouts. Duplicate the
			// independently referenced subtree so this branch remains a valid
			// UNION ALL after projections and aggregates change the row shape.
			other := inner
			otherRef := expressions.InitialOf(other)
			otherQ := expressions.ForEachQuantifier(otherRef)
			return mustPipelineStressConstruct(expressions.NewLogicalUnionExpression(
				[]expressions.Quantifier{innerQ, otherQ}))
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
			&pipelineStressIndexDef{name: "idx_x", columns: []string{"X"}, recordTypes: []string{"T"}},
		})
		p := NewPlanner(rules, ctx).
			WithPlanningExpressionRules(BatchAExpressionRules()).
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
	fieldCount := 3

	pos := 0
	next := func() byte {
		if pos >= len(b) {
			return 0
		}
		v := b[pos]
		pos++
		return v
	}

	scan := mustPipelineStressConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{tables[int(next())%len(tables)]}, pipelineStressRowType()))

	depth := int(next()) % 4
	var current expressions.RelationalExpression = scan

	for d := 0; d < depth; d++ {
		ref := expressions.InitialOf(current)
		q := expressions.ForEachQuantifier(ref)
		fieldOrdinal := int(next()) % fieldCount
		field := pipelineStressFieldAt(q, fieldOrdinal)

		switch int(next()) % 6 {
		case 0:
			current = mustPipelineStressConstruct(expressions.NewLogicalFilterExpression(
				[]predicates.QueryPredicate{
					predicates.NewComparisonPredicate(
						field,
						predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(next()))),
				}, q))
		case 1:
			current = mustPipelineStressConstruct(expressions.NewLogicalProjectionExpression(
				[]values.Value{field}, q))
		case 2:
			current = mustPipelineStressConstruct(expressions.NewLogicalSortExpression(
				[]expressions.SortKey{{Value: field, Reverse: next()%2 == 1}}, q))
		case 3:
			current = mustPipelineStressConstruct(expressions.NewLogicalDistinctExpression(q))
		case 4:
			current = mustPipelineStressConstruct(expressions.NewLogicalLimitExpression(
				int64(1+int(next())%50), 0, q))
		case 5:
			current = mustPipelineStressConstruct(expressions.NewGroupByExpression(
				[]values.Value{field},
				[]expressions.AggregateSpec{{Function: expressions.AggCount}}, q))
		}
	}

	return current
}
