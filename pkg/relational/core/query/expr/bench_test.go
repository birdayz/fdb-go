package expr_test

import (
	"testing"

	cascades "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/expr"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/semantic"
)

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

func BenchmarkResolveComparison(b *testing.B) {
	a, s := buildScopeForBench(b)
	r := expr.New(a, s)
	lhs, _ := r.ResolveIdentifier(semantic.Identifier{}, semantic.NewUnquoted("id"))
	rhs, _ := r.ResolveConstant(int64(5))
	for i := 0; i < b.N; i++ {
		_, _ = r.ResolveComparison(cascades.ComparisonEquals, lhs, rhs)
	}
}
