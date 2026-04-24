package expr_test

import (
	"errors"
	"testing"

	cascades "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/expr"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/semantic"
)

func buildScope(t *testing.T) (*semantic.Analyzer, *semantic.Scope) {
	t.Helper()
	users := &semantic.StaticTable{
		TableName: semantic.ParseQualifiedName("USERS", false),
		TableColumns: []semantic.Column{
			{Id: semantic.NewUnquoted("id"), Type: "INT"},
			{Id: semantic.NewUnquoted("name"), Type: "STRING", Nullable: true},
			{Id: semantic.NewUnquoted("active"), Type: "BOOL"},
		},
	}
	cat := semantic.NewInMemoryCatalog(users)
	a := semantic.NewAnalyzer(cat, false)
	s := semantic.NewScope(nil)
	if err := s.AddSource(semantic.ScopeSource{
		Table: users, Alias: semantic.NewUnquoted("u"),
	}); err != nil {
		t.Fatal(err)
	}
	return a, s
}

func TestResolver_ResolveIdentifier_Bare(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)

	v, err := r.ResolveIdentifier(semantic.Identifier{}, semantic.NewUnquoted("name"))
	if err != nil {
		t.Fatalf("name: %v", err)
	}
	fv, ok := v.(*cascades.FieldValue)
	if !ok {
		t.Fatalf("expected *FieldValue, got %T", v)
	}
	if fv.Field != "NAME" {
		t.Fatalf("Field: got %q, want NAME", fv.Field)
	}
	if fv.Typ != cascades.TypeString {
		t.Fatalf("Typ: got %v, want TypeString", fv.Typ)
	}
}

func TestResolver_ResolveIdentifier_Qualified(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)

	v, err := r.ResolveIdentifier(semantic.NewUnquoted("u"), semantic.NewUnquoted("active"))
	if err != nil {
		t.Fatalf("u.active: %v", err)
	}
	fv := v.(*cascades.FieldValue)
	if fv.Field != "ACTIVE" {
		t.Fatalf("Field: got %q", fv.Field)
	}
	if fv.Typ != cascades.TypeBool {
		t.Fatalf("Typ: got %v, want TypeBool", fv.Typ)
	}
}

func TestResolver_ResolveIdentifier_Missing(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)

	_, err := r.ResolveIdentifier(semantic.Identifier{}, semantic.NewUnquoted("nonexistent"))
	if err == nil {
		t.Fatal("expected error")
	}
	var cnf *semantic.ColumnNotFoundError
	if !errors.As(err, &cnf) {
		t.Fatalf("expected ColumnNotFoundError, got %T", err)
	}
}

func TestResolver_ResolveConstant(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)

	cases := []struct {
		name string
		lit  any
		want cascades.ValueType
	}{
		{"int64", int64(42), cascades.TypeInt},
		{"int", 42, cascades.TypeInt},
		{"int32", int32(42), cascades.TypeInt},
		{"string", "hello", cascades.TypeString},
		{"true", true, cascades.TypeBool},
		{"false", false, cascades.TypeBool},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v, err := r.ResolveConstant(tc.lit)
			if err != nil {
				t.Fatalf("%v: %v", tc.lit, err)
			}
			if v.Type() != tc.want {
				t.Fatalf("Type: got %v, want %v", v.Type(), tc.want)
			}
		})
	}
}

func TestResolver_ResolveConstant_Nil(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)

	v, err := r.ResolveConstant(nil)
	if err != nil {
		t.Fatalf("nil: %v", err)
	}
	if _, ok := v.(*cascades.NullValue); !ok {
		t.Fatalf("nil should produce *NullValue, got %T", v)
	}
}

func TestResolver_ResolveConstant_Unsupported(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)

	// A type the seed doesn't support.
	if _, err := r.ResolveConstant([]int{1, 2}); err == nil {
		t.Fatal("expected error for []int literal")
	}
}

func TestResolver_ResolveArithmetic(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)

	left, _ := r.ResolveIdentifier(semantic.Identifier{}, semantic.NewUnquoted("id"))
	right, _ := r.ResolveConstant(int64(1))
	v, err := r.ResolveArithmetic(cascades.OpAdd, left, right)
	if err != nil {
		t.Fatalf("arith: %v", err)
	}
	av, ok := v.(*cascades.ArithmeticValue)
	if !ok {
		t.Fatalf("expected *ArithmeticValue, got %T", v)
	}
	if av.Op != cascades.OpAdd {
		t.Fatalf("Op: got %v, want OpAdd", av.Op)
	}
	if av.Left == nil || av.Right == nil {
		t.Fatal("operands should be non-nil")
	}
}

func TestResolver_ResolveArithmetic_NilOperand(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	if _, err := r.ResolveArithmetic(cascades.OpAdd, nil, nil); err == nil {
		t.Fatal("expected error for nil operands")
	}
}

func TestResolver_ResolveComparison(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)

	left, _ := r.ResolveIdentifier(semantic.Identifier{}, semantic.NewUnquoted("id"))
	right, _ := r.ResolveConstant(int64(5))
	pred, err := r.ResolveComparison(cascades.ComparisonEquals, left, right)
	if err != nil {
		t.Fatalf("cmp: %v", err)
	}
	cp, ok := pred.(*cascades.ComparisonPredicate)
	if !ok {
		t.Fatalf("expected *ComparisonPredicate, got %T", pred)
	}
	if cp.Comparison.Type != cascades.ComparisonEquals {
		t.Fatalf("Type: got %v", cp.Comparison.Type)
	}
	if cp.Comparison.Operand != int64(5) {
		t.Fatalf("Operand: got %v", cp.Comparison.Operand)
	}
}

func TestResolver_ResolveComparison_NonConstantRHS(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)

	left, _ := r.ResolveIdentifier(semantic.Identifier{}, semantic.NewUnquoted("id"))
	rhs, _ := r.ResolveIdentifier(semantic.Identifier{}, semantic.NewUnquoted("name"))
	if _, err := r.ResolveComparison(cascades.ComparisonEquals, left, rhs); err == nil {
		t.Fatal("expected error for non-constant RHS (seed limitation)")
	}
}

func TestResolver_Nil_InputPanics(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic for nil analyzer")
			}
		}()
		_ = expr.New(nil, s)
	}()

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic for nil scope")
			}
		}()
		_ = expr.New(a, nil)
	}()
}
