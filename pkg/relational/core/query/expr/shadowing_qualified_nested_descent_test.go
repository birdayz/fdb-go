package expr_test

import (
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/expr"
	"fdb.dev/pkg/relational/core/query/semantic"
)

func TestShadowingQualifiedResolvesTheLeafAgainstExactElement(t *testing.T) {
	t.Parallel()

	elem := &semantic.StaticTable{
		TableName: semantic.FromSegments([]string{"E"}, false),
		TableColumns: []semantic.Column{{
			Id: semantic.NewUnquoted("e"), Type: "RECORD", Nullable: true,
			StructFields: []semantic.Column{
				{Id: semantic.NewUnquoted("sk"), Type: "INT"},
				{Id: semantic.NewUnquoted("co"), Type: "INT"},
			},
		}},
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
	if err := s.AddSource(semantic.ScopeSource{Table: elem, Alias: semantic.NewUnquoted("e"), CorrelationName: "E", Shadowing: true}); err != nil {
		t.Fatal(err)
	}
	r := expr.New(a, s)
	descends, err := r.DescendsIntoStruct(semantic.NewUnquoted("e"), semantic.NewUnquoted("co"))
	if err != nil {
		t.Fatalf("DescendsIntoStruct(E.CO): %v", err)
	}
	if !descends {
		t.Fatal("fixture no longer resolves E.CO as a struct descent")
	}

	value, ok, err := r.ResolveColumnShadowingQualified(
		semantic.NewUnquoted("e"), semantic.NewUnquoted("co"))
	if err != nil {
		t.Fatalf("ResolveColumnShadowingQualified(E.CO): %v", err)
	}
	if !ok {
		t.Fatal("shadowing-qualified mint declined")
	}
	field := mustExprField(t, value)
	if got := field.Path().Ordinals(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("element-relative path = %v, want [1] for CO", got)
	}
	if field.DisplayName() != "CO" || exprAccessorName(t, field.Path(), 0) != "CO" {
		t.Fatalf("resolved leaf = %q / %q, want CO", field.DisplayName(), exprAccessorName(t, field.Path(), 0))
	}
	if field.ResultType().Code() != values.TypeCodeInt {
		t.Fatalf("leaf type = %v, want INT", field.ResultType())
	}
	owner := mustExprQOV(t, field.ChildValue())
	if owner.Correlation().Name() != "E" || owner.FlowedType().Code() != values.TypeCodeRecord {
		t.Fatalf("owner = correlation %q type %v, want E / exact record", owner.Correlation().Name(), owner.FlowedType())
	}

	mutated, resolveErr := values.ResolveFieldOrdinals(owner, []int{2})
	if mutated != nil {
		t.Fatalf("invalid leaf mutation returned partial value %T", mutated)
	}
	var coded exprCodedResolutionError
	if !errors.As(resolveErr, &coded) || coded.Code() != values.FieldOutOfRange {
		t.Fatalf("invalid leaf mutation error = %v, want FieldOutOfRange", resolveErr)
	}
}

func TestShadowingQualifiedDirectScalarReturnsExactElementQOV(t *testing.T) {
	t.Parallel()
	elem := &semantic.StaticTable{
		TableName:    semantic.FromSegments([]string{"X"}, false),
		TableColumns: []semantic.Column{{Id: semantic.NewUnquoted("x"), Type: "INT", Nullable: true}},
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
	if err := s.AddSource(semantic.ScopeSource{Table: elem, Alias: semantic.NewUnquoted("x"), CorrelationName: "X", Shadowing: true}); err != nil {
		t.Fatal(err)
	}
	value, ok, err := expr.New(a, s).ResolveColumnShadowingQualified(semantic.Identifier{}, semantic.NewUnquoted("x"))
	if err != nil {
		t.Fatalf("ResolveColumnShadowingQualified(X): %v", err)
	}
	if !ok {
		t.Fatal("shadowing scalar mint declined")
	}
	qov := mustExprQOV(t, value)
	requireExprType(t, qov.FlowedType(), values.NullableInt)
}
