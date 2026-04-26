package expr_test

import (
	"testing"

	cascades "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/expr"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/semantic"
)

// parseWhereForBench is the benchmark-side analogue of
// parseFirstWhereExpr — takes *testing.B and fails via b.Fatal.
func parseWhereForBench(b *testing.B, sql string) antlrgen.IExpressionContext {
	b.Helper()
	root, err := parser.Parse(sql)
	if err != nil {
		b.Fatal(err)
	}
	sel := root.Statements().AllStatement()[0].SelectStatement()
	body := sel.Query().QueryExpressionBody().(*antlrgen.QueryTermDefaultContext)
	simple := body.QueryTerm().(*antlrgen.SimpleTableContext)
	where := simple.FromClause().WhereExpr()
	return where.Expression()
}

func buildScopeForBench(b *testing.B) (*semantic.Analyzer, *semantic.Scope) {
	b.Helper()
	users := &semantic.StaticTable{
		TableName: semantic.ParseQualifiedName("USERS", false),
		TableColumns: []semantic.Column{
			{Id: semantic.NewUnquoted("id"), Type: "INT"},
			{Id: semantic.NewUnquoted("name"), Type: "STRING", Nullable: true},
		},
	}
	cat := semantic.NewInMemoryCatalog(users)
	a := semantic.NewAnalyzer(cat, false)
	s := semantic.NewScope(nil)
	if err := s.AddSource(semantic.ScopeSource{Table: users, Alias: semantic.NewUnquoted("u")}); err != nil {
		b.Fatal(err)
	}
	return a, s
}

func BenchmarkResolveIdentifier(b *testing.B) {
	a, s := buildScopeForBench(b)
	r := expr.New(a, s)
	name := semantic.NewUnquoted("name")
	for i := 0; i < b.N; i++ {
		_, _ = r.ResolveIdentifier(semantic.Identifier{}, name)
	}
}

func BenchmarkResolveConstant_Int64(b *testing.B) {
	a, s := buildScopeForBench(b)
	r := expr.New(a, s)
	for i := 0; i < b.N; i++ {
		_, _ = r.ResolveConstant(int64(42))
	}
}

// BenchmarkWalkPredicate_Comparison measures the full parse-tree →
// predicates.ComparisonPredicate path on a representative WHERE clause.
func BenchmarkWalkPredicate_Comparison(b *testing.B) {
	a, s := buildScopeForBench(b)
	r := expr.New(a, s)
	ctx := parseWhereForBench(b, "SELECT * FROM users WHERE id = 1")
	for i := 0; i < b.N; i++ {
		_, _ = r.WalkPredicate(ctx)
	}
}

// BenchmarkWalkPredicate_AndChain measures an AND with 3 children.
func BenchmarkWalkPredicate_AndChain(b *testing.B) {
	a, s := buildScopeForBench(b)
	r := expr.New(a, s)
	ctx := parseWhereForBench(b, "SELECT * FROM users WHERE id = 1 AND name = 'bob' AND id = 1")
	for i := 0; i < b.N; i++ {
		_, _ = r.WalkPredicate(ctx)
	}
}

// BenchmarkWalkExpression_Aggregate measures COUNT(*) resolution
// through the walker. FunctionCatalog construction inside the
// walker is the hot path we may want to amortise later by
// threading a catalog through New().
func BenchmarkWalkExpression_Aggregate(b *testing.B) {
	a, s := buildScopeForBench(b)
	r := expr.New(a, s)
	ctx := parseWhereForBench(b, "SELECT * FROM users WHERE COUNT(*)")
	for i := 0; i < b.N; i++ {
		_, _ = r.WalkExpression(ctx)
	}
}

// BenchmarkWalkPredicate_Between measures BETWEEN → AND(>=, <=)
// desugar cost. Non-trivial because it builds two predicates +
// an And.
func BenchmarkWalkPredicate_Between(b *testing.B) {
	a, s := buildScopeForBench(b)
	r := expr.New(a, s)
	ctx := parseWhereForBench(b, "SELECT * FROM users WHERE id BETWEEN 1 AND 10")
	for i := 0; i < b.N; i++ {
		_, _ = r.WalkPredicate(ctx)
	}
}

// BenchmarkFullStack measures the full parse → walk → simplify path
// per WHERE clause. This is the cost-per-SQL-statement overhead
// the future logical-builder integration will incur.
func BenchmarkFullStack(b *testing.B) {
	a, s := buildScopeForBench(b)
	r := expr.New(a, s)
	// Parse once (parsing is amortised across query planning in
	// practice); just measure walk + simplify.
	ctx := parseWhereForBench(b, "SELECT * FROM users WHERE id = 1 AND name = 'bob'")
	rules := cascades.DefaultSimplifyRules()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		pred, err := r.WalkPredicate(ctx)
		if err != nil {
			b.Fatal(err)
		}
		_ = cascades.Simplify(pred, rules)
	}
}

func BenchmarkResolveComparison(b *testing.B) {
	a, s := buildScopeForBench(b)
	r := expr.New(a, s)
	lhs, _ := r.ResolveIdentifier(semantic.Identifier{}, semantic.NewUnquoted("id"))
	rhs, _ := r.ResolveConstant(int64(5))
	for i := 0; i < b.N; i++ {
		_, _ = r.ResolveComparison(predicates.ComparisonEquals, lhs, rhs)
	}
}
