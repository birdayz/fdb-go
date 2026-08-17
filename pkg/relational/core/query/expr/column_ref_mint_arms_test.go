package expr_test

import (
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/expr"
	"fdb.dev/pkg/relational/core/query/semantic"
)

// TestCorrelatedColumnRefFusesOnBothExactOwnerArms drives the shadowing-element
// and ordinary-row branches of the shared correlated reference mint. Each arm
// must resolve CO against the exact object that its quantifier actually flows:
// the element record for a shadowing source, or the source row otherwise.
func TestCorrelatedColumnRefFusesOnBothExactOwnerArms(t *testing.T) {
	t.Parallel()

	structCol := func(name string) semantic.Column {
		return semantic.Column{
			Id: semantic.NewUnquoted(name), Type: "RECORD", Nullable: true,
			StructFields: []semantic.Column{
				{Id: semantic.NewUnquoted("sk"), Type: "INT"},
				{Id: semantic.NewUnquoted("co"), Type: "INT"},
			},
		}
	}

	assertExactLeaf := func(t *testing.T, arm string, value values.Value, wantPath []int) {
		t.Helper()
		field := mustExprField(t, value)
		gotPath := field.Path().Ordinals()
		if len(gotPath) != len(wantPath) {
			t.Fatalf("%s: path = %v, want %v", arm, gotPath, wantPath)
		}
		for i := range wantPath {
			if gotPath[i] != wantPath[i] {
				t.Fatalf("%s: path = %v, want %v", arm, gotPath, wantPath)
			}
		}
		if field.DisplayName() != "CO" {
			t.Fatalf("%s: leaf display name = %q, want CO", arm, field.DisplayName())
		}
		accessor, ok := field.Path().Accessor(field.Path().Len() - 1)
		if !ok || accessor.Ordinal() != 1 {
			t.Fatalf("%s: leaf accessor = %v, want ordinal 1", arm, accessor)
		}
		owner := mustExprQOV(t, field.ChildValue())
		if owner.FlowedType().Code() != values.TypeCodeRecord {
			t.Fatalf("%s: owner flows %v, want exact record", arm, owner.FlowedType())
		}
		if field.ResultType().Code() != values.TypeCodeInt {
			t.Fatalf("%s: leaf type = %v, want INT", arm, field.ResultType())
		}

		// Moving the root one slot beyond the exact owner layout must fail;
		// this kills a regression that substitutes an unknown carrier.
		root, ok := owner.FlowedType().(*values.RecordType)
		if !ok {
			t.Fatalf("%s: exact owner thawed as %T, want *RecordType", arm, owner.FlowedType())
		}
		mutated, err := values.ResolveFieldOrdinals(owner, []int{len(root.Fields)})
		if mutated != nil {
			t.Fatalf("%s: invalid mutation returned partial value %T", arm, mutated)
		}
		var coded exprCodedResolutionError
		if !errors.As(err, &coded) || coded.Code() != values.FieldOutOfRange {
			t.Fatalf("%s: invalid mutation error = %v, want FieldOutOfRange", arm, err)
		}
	}

	t.Run("shadowing element record", func(t *testing.T) {
		t.Parallel()
		elem := &semantic.StaticTable{
			TableName:    semantic.FromSegments([]string{"E"}, false),
			TableColumns: []semantic.Column{structCol("e")},
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
		value, ok, err := expr.New(a, s).ResolveColumnShadowingQualified(
			semantic.NewUnquoted("e"), semantic.NewUnquoted("co"))
		if err != nil {
			t.Fatalf("resolve E.CO: %v", err)
		}
		if !ok {
			t.Fatal("shadowing mint declined")
		}
		assertExactLeaf(t, "shadowing", value, []int{1})
	})

	t.Run("ordinary source row", func(t *testing.T) {
		t.Parallel()
		t1 := &semantic.StaticTable{
			TableName: semantic.ParseQualifiedName("T1", false), TableColumns: []semantic.Column{structCol("n")},
		}
		t2 := &semantic.StaticTable{
			TableName: semantic.ParseQualifiedName("T2", false), TableColumns: []semantic.Column{structCol("m")},
		}
		a := semantic.NewAnalyzer(semantic.NewInMemoryCatalog(t1, t2), false)
		s := semantic.NewScope(nil)
		if err := s.AddSource(semantic.ScopeSource{Table: t1, Alias: semantic.NewUnquoted("a"), CorrelationName: "A"}); err != nil {
			t.Fatal(err)
		}
		if err := s.AddSource(semantic.ScopeSource{Table: t2, Alias: semantic.NewUnquoted("a"), CorrelationName: "A_1"}); err != nil {
			t.Fatal(err)
		}
		value, err := expr.New(a, s).ResolveQualifiedProjectionPath([]semantic.Identifier{
			semantic.NewUnquoted("a"), semantic.NewUnquoted("n"), semantic.NewUnquoted("co"),
		})
		if err != nil {
			t.Fatalf("resolve A.N.CO: %v", err)
		}
		if value == nil {
			t.Fatal("duplicate-alias projection mint declined")
		}
		assertExactLeaf(t, "ordinary", value, []int{0, 1})
	})
}
