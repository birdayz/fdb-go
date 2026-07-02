package plangen_test

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/relational/core/query/logical"
	"fdb.dev/pkg/relational/core/query/plangen"
)

// BenchmarkConvert_Scan measures the cost of converting the simplest
// LogicalScan — the leaf in every realistic plan tree.
func BenchmarkConvert_Scan(b *testing.B) {
	src := logical.NewScan("Order", "")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = plangen.Convert(src)
	}
}

// BenchmarkConvert_FilterOverScan measures a 2-deep tree (Filter w/
// QueryPredicate over Scan) — the canonical SELECT … WHERE shape.
func BenchmarkConvert_FilterOverScan(b *testing.B) {
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	src := logical.NewFilterWithPredicate(
		logical.NewScan("Order", ""),
		pT, "TRUE",
	)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = plangen.Convert(src)
	}
}

// BenchmarkConvert_NestedTree measures a 4-deep tree exercising
// Project → Sort → Filter → Scan — the typical SELECT cols FROM t
// WHERE p ORDER BY k shape.
func BenchmarkConvert_NestedTree(b *testing.B) {
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	src := logical.NewProject(
		logical.NewSort(
			logical.NewFilterWithPredicate(
				logical.NewScan("Order", ""),
				pT, "TRUE",
			),
			[]logical.SortKey{{Expr: "id", Dir: logical.SortAsc}},
		),
		[]string{"id"},
		[]string{""},
	)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = plangen.Convert(src)
	}
}

// BenchmarkConvertAndOptimise measures the Convert + Planner.Plan
// pipeline over the curated default rule set on a representative
// tree. This is the latency the SQL engine pays per query plan-time
// today (modulo the parser).
func BenchmarkConvertAndOptimise(b *testing.B) {
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	src := logical.NewProject(
		logical.NewProject(
			logical.NewFilterWithPredicate(
				logical.NewScan("Order", ""),
				pT, "TRUE",
			),
			[]string{"id", "name"},
			[]string{"", ""},
		),
		[]string{"id"},
		[]string{""},
	)
	rules := cascades.DefaultExpressionRules()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got, err := plangen.Convert(src)
		if err != nil {
			b.Fatal(err)
		}
		ref := expressions.InitialOf(got)
		p := cascades.NewPlanner(rules, nil)
		_, _, _ = p.Plan(ref)
	}
}
