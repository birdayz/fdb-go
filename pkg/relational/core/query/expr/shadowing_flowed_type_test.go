package expr_test

import (
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/expr"
	"fdb.dev/pkg/relational/core/query/semantic"
)

// TestShadowingSourceFlowsItsExactElementType proves that an UNNEST binding's
// virtual one-column lookup table never leaks into value identity. Each mint
// returns the exact scalar element QOV, while a normal source reference carries
// its exact row owner.
func TestShadowingSourceFlowsItsExactElementType(t *testing.T) {
	t.Parallel()

	users := &semantic.StaticTable{
		TableName: semantic.ParseQualifiedName("USERS", false),
		TableColumns: []semantic.Column{
			{Id: semantic.NewUnquoted("id"), Type: "INT"},
			{Id: semantic.NewUnquoted("name"), Type: "STRING", Nullable: true},
		},
	}
	elem := &semantic.StaticTable{
		TableName:    semantic.FromSegments([]string{"X"}, false),
		TableColumns: []semantic.Column{{Id: semantic.NewUnquoted("x"), Type: "INT", Nullable: true}},
	}
	a := semantic.NewAnalyzer(semantic.NewInMemoryCatalog(users, elem), false)
	s := semantic.NewScope(nil)
	if err := s.AddSource(semantic.ScopeSource{Table: users, Alias: semantic.NewUnquoted("u"), CorrelationName: "U"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddSource(semantic.ScopeSource{Table: elem, Alias: semantic.NewUnquoted("x"), CorrelationName: "X", Shadowing: true}); err != nil {
		t.Fatal(err)
	}
	r := expr.New(a, s)

	normal, err := r.ResolveIdentifier(semantic.NewUnquoted("u"), semantic.NewUnquoted("name"))
	if err != nil {
		t.Fatalf("resolve U.NAME: %v", err)
	}
	normalOwner := mustExprQOV(t, mustExprField(t, normal).ChildValue())
	if normalOwner.FlowedType().Code() != values.TypeCodeRecord {
		t.Fatalf("normal source owner flows %v, want exact row", normalOwner.FlowedType())
	}

	assertElement := func(t *testing.T, mint string, value values.Value) {
		t.Helper()
		qov := mustExprQOV(t, value)
		requireExprType(t, qov.FlowedType(), values.NullableInt)
		if qov.Correlation().Name() != "X" {
			t.Fatalf("%s correlation = %q, want X", mint, qov.Correlation().Name())
		}
		// A scalar exact owner cannot be reinterpreted as the retired virtual
		// row. Attempting a field mutation is a typed failure with no value.
		mutated, resolveErr := values.ResolveFieldOrdinals(qov, []int{0})
		if mutated != nil {
			t.Fatalf("%s scalar mutation returned partial value %T", mint, mutated)
		}
		var coded exprCodedResolutionError
		if !errors.As(resolveErr, &coded) || coded.Code() != values.FieldNonRecord {
			t.Fatalf("%s scalar mutation error = %v, want FieldNonRecord", mint, resolveErr)
		}
	}

	bare, err := r.ResolveIdentifier(semantic.Identifier{}, semantic.NewUnquoted("x"))
	if err != nil {
		t.Fatalf("resolve bare X: %v", err)
	}
	assertElement(t, "ResolveIdentifier", bare)

	shadowed, ok, err := r.ResolveColumnShadowingQualified(semantic.Identifier{}, semantic.NewUnquoted("x"))
	if err != nil {
		t.Fatalf("ResolveColumnShadowingQualified: %v", err)
	}
	if !ok {
		t.Fatal("ResolveColumnShadowingQualified declined shadowing source")
	}
	assertElement(t, "ResolveColumnShadowingQualified", shadowed)

	if value, declineErr := r.ResolveQualifiedProjection(semantic.NewUnquoted("x"), semantic.NewUnquoted("x")); declineErr != nil || value != nil {
		t.Fatalf("unquoted unique shadowing alias should remain owned by ordinary emission: value=%v err=%v", value, declineErr)
	}
	assertQualifiedProjectionExactShadowingElement(t, assertElement)
}

func assertQualifiedProjectionExactShadowingElement(
	t *testing.T,
	assertElement func(*testing.T, string, values.Value),
) {
	t.Helper()
	quoted := semantic.FromNormalized("x")
	elem := &semantic.StaticTable{
		TableName:    semantic.FromSegments([]string{"x"}, false),
		TableColumns: []semantic.Column{{Id: quoted, Type: "INT", Nullable: true}},
	}
	other := &semantic.StaticTable{
		TableName:    semantic.ParseQualifiedName("OTHER", false),
		TableColumns: []semantic.Column{{Id: semantic.NewUnquoted("id"), Type: "INT"}},
	}
	a := semantic.NewAnalyzer(semantic.NewInMemoryCatalog(elem, other), false)
	s := semantic.NewScope(nil)
	if err := s.AddSource(semantic.ScopeSource{Table: other, Alias: semantic.NewUnquoted("o"), CorrelationName: "O"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddSource(semantic.ScopeSource{Table: elem, Alias: quoted, CorrelationName: quoted.Name(), Shadowing: true}); err != nil {
		t.Fatal(err)
	}
	value, err := expr.New(a, s).ResolveQualifiedProjection(quoted, quoted)
	if err != nil {
		t.Fatalf("ResolveQualifiedProjection quoted shadowing alias: %v", err)
	}
	if value == nil {
		t.Fatal("quoted shadowing alias mint declined")
	}
	assertElement(t, "ResolveQualifiedProjection", value)
}
