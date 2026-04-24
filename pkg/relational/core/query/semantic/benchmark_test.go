package semantic

import "testing"

// Microbenchmarks for the semantic package hot paths. Seeded at
// swingshift-47; no perf gate yet — exists so regressions show up
// once the analyzer wires into the logical-builder.

func BenchmarkNormalizeString_Unquoted(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = NormalizeString("some_table_name", false)
	}
}

func BenchmarkNormalizeString_Quoted(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = NormalizeString(`"Some_Table_Name"`, false)
	}
}

func BenchmarkNew_Identifier(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = NewUnquoted("age")
	}
}

func BenchmarkParseQualifiedName_TwoSegments(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = ParseQualifiedName("schema.table", false)
	}
}

func BenchmarkInMemoryCatalog_LookupTable(b *testing.B) {
	cat := buildTestCatalog()
	name := ParseQualifiedName("users", false)
	for i := 0; i < b.N; i++ {
		_, _ = cat.LookupTable(name)
	}
}

func BenchmarkScope_ResolveColumn(b *testing.B) {
	cat := buildTestCatalog()
	users, _ := cat.LookupTable(ParseQualifiedName("users", false))
	scope := NewScope(nil)
	if err := scope.AddSource(ScopeSource{Table: users, Alias: NewUnquoted("u")}); err != nil {
		b.Fatalf("AddSource: %v", err)
	}
	target := NewUnquoted("name")
	for i := 0; i < b.N; i++ {
		_, _, _ = scope.ResolveColumn(target)
	}
}

func BenchmarkScope_ResolveQualifiedColumn(b *testing.B) {
	cat := buildTestCatalog()
	users, _ := cat.LookupTable(ParseQualifiedName("users", false))
	scope := NewScope(nil)
	if err := scope.AddSource(ScopeSource{Table: users, Alias: NewUnquoted("u")}); err != nil {
		b.Fatalf("AddSource: %v", err)
	}
	qualifier := NewUnquoted("u")
	col := NewUnquoted("name")
	for i := 0; i < b.N; i++ {
		_, _, _ = scope.ResolveQualifiedColumn(qualifier, col)
	}
}
