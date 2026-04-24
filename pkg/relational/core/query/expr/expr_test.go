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

func TestResolver_ResolveFunctionCall_CountStar(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	fc := semantic.NewFunctionCatalog()
	fc.RegisterDefaults()

	v, err := r.ResolveFunctionCall(fc, semantic.NewUnquoted("COUNT"), true, nil)
	if err != nil {
		t.Fatalf("COUNT(*): %v", err)
	}
	agg := v.(*cascades.AggregateValue)
	if agg.Op != cascades.AggCountStar {
		t.Fatalf("Op: got %v, want AggCountStar", agg.Op)
	}
	if agg.Operand != nil {
		t.Fatal("COUNT(*) should have nil operand")
	}
}

func TestResolver_ResolveFunctionCall_CountCol(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	fc := semantic.NewFunctionCatalog()
	fc.RegisterDefaults()

	arg, _ := r.ResolveIdentifier(semantic.Identifier{}, semantic.NewUnquoted("id"))
	v, err := r.ResolveFunctionCall(fc, semantic.NewUnquoted("count"), false, []cascades.Value{arg})
	if err != nil {
		t.Fatalf("COUNT(id): %v", err)
	}
	agg := v.(*cascades.AggregateValue)
	if agg.Op != cascades.AggCount {
		t.Fatalf("Op: got %v, want AggCount", agg.Op)
	}
}

func TestResolver_ResolveFunctionCall_Sum(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	fc := semantic.NewFunctionCatalog()
	fc.RegisterDefaults()

	arg, _ := r.ResolveIdentifier(semantic.Identifier{}, semantic.NewUnquoted("id"))
	v, err := r.ResolveFunctionCall(fc, semantic.NewUnquoted("SUM"), false, []cascades.Value{arg})
	if err != nil {
		t.Fatalf("SUM(id): %v", err)
	}
	if v.(*cascades.AggregateValue).Op != cascades.AggSum {
		t.Fatalf("Op mismatch")
	}
}

func TestResolver_ResolveFunctionCall_StarOnNonStarFunc(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	fc := semantic.NewFunctionCatalog()
	fc.RegisterDefaults()

	_, err := r.ResolveFunctionCall(fc, semantic.NewUnquoted("SUM"), true, nil)
	if err == nil {
		t.Fatal("SUM(*) should error; only COUNT accepts star")
	}
}

func TestResolver_ResolveFunctionCall_ArityMismatch(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	fc := semantic.NewFunctionCatalog()
	fc.RegisterDefaults()

	// SUM with 0 args.
	_, err := r.ResolveFunctionCall(fc, semantic.NewUnquoted("SUM"), false, nil)
	if err == nil {
		t.Fatal("expected arity error")
	}
	var ae *semantic.FunctionArityError
	if !errors.As(err, &ae) {
		t.Fatalf("expected FunctionArityError, got %T", err)
	}
}

func TestResolver_ResolveFunctionCall_UnknownFunc(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	fc := semantic.NewFunctionCatalog()
	fc.RegisterDefaults()

	_, err := r.ResolveFunctionCall(fc, semantic.NewUnquoted("UNKNOWN_FN"), false, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	var nf *semantic.FunctionNotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("expected FunctionNotFoundError, got %T", err)
	}
}

func TestResolver_ResolveAnd(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)

	// Empty → TRUE (AND identity).
	p := r.ResolveAnd()
	cp, ok := p.(*cascades.ConstantPredicate)
	if !ok || cp.Value != cascades.TriTrue {
		t.Fatalf("empty AND: got %T %v, want TRUE", p, p)
	}

	// Single predicate returns verbatim (no And wrapping).
	inner := cascades.NewConstantPredicate(cascades.TriFalse)
	if p := r.ResolveAnd(inner); p != cascades.QueryPredicate(inner) {
		t.Fatal("single-element AND should return the predicate verbatim")
	}

	// Multi wraps.
	multi := r.ResolveAnd(
		cascades.NewConstantPredicate(cascades.TriTrue),
		cascades.NewConstantPredicate(cascades.TriFalse),
	)
	if _, ok := multi.(*cascades.AndPredicate); !ok {
		t.Fatalf("multi AND: got %T, want *AndPredicate", multi)
	}
}

func TestResolver_ResolveOr(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)

	p := r.ResolveOr()
	cp, ok := p.(*cascades.ConstantPredicate)
	if !ok || cp.Value != cascades.TriFalse {
		t.Fatalf("empty OR: got %T %v, want FALSE", p, p)
	}
}

func TestResolver_ResolveNot(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)

	// Nil → UNKNOWN.
	p := r.ResolveNot(nil)
	cp, ok := p.(*cascades.ConstantPredicate)
	if !ok || cp.Value != cascades.TriUnknown {
		t.Fatalf("nil NOT: got %T %v, want UNKNOWN", p, p)
	}

	// Wrapping.
	inner := cascades.NewConstantPredicate(cascades.TriTrue)
	wrapped := r.ResolveNot(inner)
	if _, ok := wrapped.(*cascades.NotPredicate); !ok {
		t.Fatalf("expected NotPredicate, got %T", wrapped)
	}
}

// Integration: build a bigger expression from primitives and verify
// it evaluates correctly. Exercises the full resolver stack — column
// ref → field value, constants → literal values, arithmetic → op,
// comparison → predicate — and checks the resulting tree evaluates
// to the expected TriBool against a sample row.
func TestResolver_Integration_AgeGreaterEighteen(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)

	// Expression: id + 1 > 5
	idRef, err := r.ResolveIdentifier(semantic.Identifier{}, semantic.NewUnquoted("id"))
	if err != nil {
		t.Fatal(err)
	}
	one, _ := r.ResolveConstant(int64(1))
	sum, err := r.ResolveArithmetic(cascades.OpAdd, idRef, one)
	if err != nil {
		t.Fatal(err)
	}
	five, _ := r.ResolveConstant(int64(5))
	pred, err := r.ResolveComparison(cascades.ComparisonGreaterThan, sum, five)
	if err != nil {
		t.Fatal(err)
	}

	row := map[string]any{"ID": int64(7)} // id+1 = 8 > 5 → TRUE
	got := pred.Eval(row)
	if got != cascades.TriTrue {
		t.Fatalf("8 > 5: expected TRUE, got %v", got)
	}
	row["ID"] = int64(2) // 2+1 = 3 > 5 → FALSE
	got = pred.Eval(row)
	if got != cascades.TriFalse {
		t.Fatalf("3 > 5: expected FALSE, got %v", got)
	}

	// Explain output should read cleanly.
	if got, want := pred.Explain(), "(ID + 1) > 5"; got != want {
		t.Fatalf("Explain: got %q, want %q", got, want)
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
