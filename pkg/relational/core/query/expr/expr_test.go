package expr_test

import (
	"errors"
	"testing"

	cascades "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
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
			{Id: semantic.NewUnquoted("admin"), Type: "BOOL", Nullable: true},
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
	fv, ok := v.(*values.FieldValue)
	if !ok {
		t.Fatalf("expected *FieldValue, got %T", v)
	}
	if fv.Field != "NAME" {
		t.Fatalf("Field: got %q, want NAME", fv.Field)
	}
	if fv.Typ != values.TypeString {
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
	fv := v.(*values.FieldValue)
	if fv.Field != "ACTIVE" {
		t.Fatalf("Field: got %q", fv.Field)
	}
	if fv.Typ != values.TypeBool {
		t.Fatalf("Typ: got %v, want TypeBool", fv.Typ)
	}
}

// sqlTypeToCascadesType is exercised via ResolveIdentifier (it's
// unexported). Pin the SQL-string → values.ValueType mapping so
// any drift (including misses like the old BYTES→TypeInt lie) surfaces
// at test time. Downstream comparators dispatch on ValueType; a bad
// mapping would silently pick the wrong path.
func TestResolver_ResolveIdentifier_TypeMapping(t *testing.T) {
	t.Parallel()
	tbl := &semantic.StaticTable{
		TableName: semantic.ParseQualifiedName("MIXED", false),
		TableColumns: []semantic.Column{
			{Id: semantic.NewUnquoted("i"), Type: "INT"},
			{Id: semantic.NewUnquoted("s"), Type: "STRING"},
			{Id: semantic.NewUnquoted("e"), Type: "ENUM"},
			{Id: semantic.NewUnquoted("b"), Type: "BOOL"},
			{Id: semantic.NewUnquoted("f"), Type: "FLOAT"},
			{Id: semantic.NewUnquoted("by"), Type: "BYTES"},
			{Id: semantic.NewUnquoted("rec"), Type: "RECORD"},
		},
	}
	cat := semantic.NewInMemoryCatalog(tbl)
	a := semantic.NewAnalyzer(cat, false)
	s := semantic.NewScope(nil)
	if err := s.AddSource(semantic.ScopeSource{Table: tbl, Alias: semantic.NewUnquoted("m")}); err != nil {
		t.Fatal(err)
	}
	r := expr.New(a, s)

	cases := map[string]values.Type{
		"i":   values.TypeInt,
		"s":   values.TypeString,
		"e":   values.TypeString,
		"b":   values.TypeBool,
		"f":   values.TypeUnknown, // no TypeFloat yet
		"by":  values.TypeUnknown, // no TypeBytes yet — prior code lied and said TypeInt
		"rec": values.TypeUnknown, // no struct/record type yet
	}
	for col, want := range cases {
		v, err := r.ResolveIdentifier(semantic.Identifier{}, semantic.NewUnquoted(col))
		if err != nil {
			t.Fatalf("resolve %q: %v", col, err)
		}
		fv := v.(*values.FieldValue)
		if fv.Typ != want {
			t.Errorf("%s: got %v, want %v", col, fv.Typ, want)
		}
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
		want values.Type
	}{
		{"int64", int64(42), values.TypeInt},
		{"int", 42, values.TypeInt},
		{"int32", int32(42), values.TypeInt},
		{"string", "hello", values.TypeString},
		{"true", true, values.TypeBool},
		{"false", false, values.TypeBool},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v, err := r.ResolveConstant(tc.lit)
			if err != nil {
				t.Fatalf("%v: %v", tc.lit, err)
			}
			// Compare by code only — BooleanValue's Type() is
			// NotNullBoolean (literals are NOT NULL); the table's
			// `want` carries the nullable singleton (TypeBool ≡
			// NullableBoolean). Same code, different nullability —
			// the .Code() check is the right migration shape.
			if v.Type().Code() != tc.want.Code() {
				t.Fatalf("Type code: got %v, want %v", v.Type().Code(), tc.want.Code())
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
	if _, ok := v.(*values.NullValue); !ok {
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

// Float literals now produce ConstantValue{Typ: TypeFloat}.
func TestResolver_ResolveConstant_Float(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)

	v, err := r.ResolveConstant(float64(3.14))
	if err != nil {
		t.Fatalf("float64: %v", err)
	}
	cv, ok := v.(*values.ConstantValue)
	if !ok {
		t.Fatalf("expected *ConstantValue, got %T", v)
	}
	if cv.Typ != values.TypeFloat {
		t.Fatalf("Typ: got %v, want TypeFloat", cv.Typ)
	}
	if cv.Value != float64(3.14) {
		t.Fatalf("Value: got %v", cv.Value)
	}

	// float32 widens to float64.
	v32, err := r.ResolveConstant(float32(2.5))
	if err != nil {
		t.Fatalf("float32: %v", err)
	}
	cv32 := v32.(*values.ConstantValue)
	if cv32.Value != float64(2.5) {
		t.Fatalf("float32 widening: got %v", cv32.Value)
	}
}

func TestResolver_ResolveArithmetic(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)

	left, _ := r.ResolveIdentifier(semantic.Identifier{}, semantic.NewUnquoted("id"))
	right, _ := r.ResolveConstant(int64(1))
	v, err := r.ResolveArithmetic(values.OpAdd, left, right)
	if err != nil {
		t.Fatalf("arith: %v", err)
	}
	av, ok := v.(*values.ArithmeticValue)
	if !ok {
		t.Fatalf("expected *ArithmeticValue, got %T", v)
	}
	if av.Op != values.OpAdd {
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
	if _, err := r.ResolveArithmetic(values.OpAdd, nil, nil); err == nil {
		t.Fatal("expected error for nil operands")
	}
}

func TestResolver_ResolveComparison(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)

	left, _ := r.ResolveIdentifier(semantic.Identifier{}, semantic.NewUnquoted("id"))
	right, _ := r.ResolveConstant(int64(5))
	pred, err := r.ResolveComparison(predicates.ComparisonEquals, left, right)
	if err != nil {
		t.Fatalf("cmp: %v", err)
	}
	cp, ok := pred.(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("expected *ComparisonPredicate, got %T", pred)
	}
	if cp.Comparison.Type != predicates.ComparisonEquals {
		t.Fatalf("Type: got %v", cp.Comparison.Type)
	}
	rhsLit, ok := values.EvaluateConstant(cp.Comparison.Operand)
	if !ok || rhsLit != int64(5) {
		t.Fatalf("Operand: got %v", cp.Comparison.Operand)
	}
}

// ResolveComparison accepts non-constant RHS — `a = b`, `a < b + 1` —
// and preserves the Value tree on the Comparison. The simplifier
// only folds when BOTH sides are known-constant; row-context
// evaluation through ComparisonPredicate.Eval reads the RHS from
// the same evalCtx as the LHS.
func TestResolver_ResolveComparison_NonConstantRHS(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)

	left, _ := r.ResolveIdentifier(semantic.Identifier{}, semantic.NewUnquoted("id"))
	rhs, _ := r.ResolveIdentifier(semantic.Identifier{}, semantic.NewUnquoted("name"))
	pred, err := r.ResolveComparison(predicates.ComparisonEquals, left, rhs)
	if err != nil {
		t.Fatalf("ResolveComparison: %v", err)
	}
	cp, ok := pred.(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("expected *ComparisonPredicate, got %T", pred)
	}
	if cp.Comparison.Operand != rhs {
		t.Fatalf("Operand: got %v, want %v", cp.Comparison.Operand, rhs)
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
	agg := v.(*values.AggregateValue)
	if agg.Op != values.AggCountStar {
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
	v, err := r.ResolveFunctionCall(fc, semantic.NewUnquoted("count"), false, []values.Value{arg})
	if err != nil {
		t.Fatalf("COUNT(id): %v", err)
	}
	agg := v.(*values.AggregateValue)
	if agg.Op != values.AggCount {
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
	v, err := r.ResolveFunctionCall(fc, semantic.NewUnquoted("SUM"), false, []values.Value{arg})
	if err != nil {
		t.Fatalf("SUM(id): %v", err)
	}
	if v.(*values.AggregateValue).Op != values.AggSum {
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

func TestResolver_ResolveCast(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)

	id, _ := r.ResolveIdentifier(semantic.Identifier{}, semantic.NewUnquoted("id"))
	v, err := r.ResolveCast(id, values.TypeString)
	if err != nil {
		t.Fatalf("CAST: %v", err)
	}
	cv := v.(*values.CastValue)
	if cv.Target != values.TypeString {
		t.Fatalf("Target: got %v, want TypeString", cv.Target)
	}

	// Unknown target rejected.
	if _, err := r.ResolveCast(id, values.TypeUnknown); err == nil {
		t.Fatal("expected error for TypeUnknown target")
	}
	// Nil child rejected.
	if _, err := r.ResolveCast(nil, values.TypeInt); err == nil {
		t.Fatal("expected error for nil child")
	}
}

func TestResolver_ResolveIsNull(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)

	id, _ := r.ResolveIdentifier(semantic.Identifier{}, semantic.NewUnquoted("name"))
	pred, err := r.ResolveIsNull(id)
	if err != nil {
		t.Fatalf("IS NULL: %v", err)
	}
	cp := pred.(*predicates.ComparisonPredicate)
	if cp.Comparison.Type != predicates.ComparisonIsNull {
		t.Fatalf("Type: got %v, want IsNull", cp.Comparison.Type)
	}

	// Evaluate.
	if mustEval(cp, map[string]any{"NAME": nil}) != predicates.TriTrue {
		t.Fatal("NULL IS NULL should be TRUE")
	}
	if mustEval(cp, map[string]any{"NAME": "foo"}) != predicates.TriFalse {
		t.Fatal("'foo' IS NULL should be FALSE")
	}
}

func TestResolver_ResolveIsNotNull(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)

	id, _ := r.ResolveIdentifier(semantic.Identifier{}, semantic.NewUnquoted("id"))
	pred, err := r.ResolveIsNotNull(id)
	if err != nil {
		t.Fatalf("IS NOT NULL: %v", err)
	}
	if pred.(*predicates.ComparisonPredicate).Comparison.Type != predicates.ComparisonIsNotNull {
		t.Fatal("Type mismatch")
	}
}

func TestResolver_ResolveLike(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)

	id, _ := r.ResolveIdentifier(semantic.Identifier{}, semantic.NewUnquoted("name"))
	pat, _ := r.ResolveConstant("hel%")
	pred, err := r.ResolveLike(id, pat)
	if err != nil {
		t.Fatalf("LIKE: %v", err)
	}
	cp := pred.(*predicates.ComparisonPredicate)
	if cp.Comparison.Type != predicates.ComparisonLike {
		t.Fatal("Type mismatch")
	}
	patLit, ok := values.EvaluateConstant(cp.Comparison.Operand)
	if !ok || patLit != "hel%" {
		t.Fatalf("pattern: got %v", cp.Comparison.Operand)
	}

	// Non-string pattern rejected.
	intPat, _ := r.ResolveConstant(int64(1))
	if _, err := r.ResolveLike(id, intPat); err == nil {
		t.Fatal("expected error for non-string pattern")
	}
}

func TestResolver_ResolveStartsWith(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)

	id, _ := r.ResolveIdentifier(semantic.Identifier{}, semantic.NewUnquoted("name"))
	pfx, _ := r.ResolveConstant("hel")
	pred, err := r.ResolveStartsWith(id, pfx)
	if err != nil {
		t.Fatalf("STARTS_WITH: %v", err)
	}
	cp := pred.(*predicates.ComparisonPredicate)
	if cp.Comparison.Type != predicates.ComparisonStartsWith {
		t.Fatal("Type mismatch")
	}
	pfxLit, ok := values.EvaluateConstant(cp.Comparison.Operand)
	if !ok || pfxLit != "hel" {
		t.Fatalf("prefix: got %v", cp.Comparison.Operand)
	}
}

func TestResolver_ResolveIn(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)

	left, _ := r.ResolveIdentifier(semantic.Identifier{}, semantic.NewUnquoted("id"))
	one, _ := r.ResolveConstant(int64(1))
	two, _ := r.ResolveConstant(int64(2))
	three, _ := r.ResolveConstant(int64(3))

	pred, err := r.ResolveIn(left, []values.Value{one, two, three})
	if err != nil {
		t.Fatalf("IN: %v", err)
	}
	cp := pred.(*predicates.ComparisonPredicate)
	if cp.Comparison.Type != predicates.ComparisonIn {
		t.Fatalf("Type: got %v, want ComparisonIn", cp.Comparison.Type)
	}
	lit, ok := values.EvaluateConstant(cp.Comparison.Operand)
	if !ok {
		t.Fatalf("Operand not constant: %v", cp.Comparison.Operand)
	}
	list, ok := lit.([]any)
	if !ok || len(list) != 3 {
		t.Fatalf("Operand: got %v", cp.Comparison.Operand)
	}
	if list[0] != int64(1) || list[1] != int64(2) || list[2] != int64(3) {
		t.Fatalf("list content: got %v", list)
	}

	// Eval against a row.
	row := map[string]any{"ID": int64(2)}
	if mustEval(cp, row) != predicates.TriTrue {
		t.Fatal("2 IN (1,2,3) should be TRUE")
	}
	row["ID"] = int64(9)
	if mustEval(cp, row) != predicates.TriFalse {
		t.Fatal("9 IN (1,2,3) should be FALSE")
	}
}

func TestResolver_ResolveIn_NonConstantRHS(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)

	left, _ := r.ResolveIdentifier(semantic.Identifier{}, semantic.NewUnquoted("id"))
	fieldRef, _ := r.ResolveIdentifier(semantic.Identifier{}, semantic.NewUnquoted("name"))
	if _, err := r.ResolveIn(left, []values.Value{fieldRef}); err == nil {
		t.Fatal("expected error for non-constant IN element")
	}
}

func TestResolver_ResolveIn_NilLHS(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	if _, err := r.ResolveIn(nil, nil); err == nil {
		t.Fatal("expected error for nil LHS")
	}
}

func TestResolver_ResolveAnd(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)

	// Empty → TRUE (AND identity).
	p := r.ResolveAnd()
	cp, ok := p.(*predicates.ConstantPredicate)
	if !ok || cp.Value != predicates.TriTrue {
		t.Fatalf("empty AND: got %T %v, want TRUE", p, p)
	}

	// Single predicate returns verbatim (no And wrapping).
	inner := predicates.NewConstantPredicate(predicates.TriFalse)
	if p := r.ResolveAnd(inner); p != predicates.QueryPredicate(inner) {
		t.Fatal("single-element AND should return the predicate verbatim")
	}

	// Multi wraps.
	multi := r.ResolveAnd(
		predicates.NewConstantPredicate(predicates.TriTrue),
		predicates.NewConstantPredicate(predicates.TriFalse),
	)
	if _, ok := multi.(*predicates.AndPredicate); !ok {
		t.Fatalf("multi AND: got %T, want *AndPredicate", multi)
	}
}

func TestResolver_ResolveOr(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)

	p := r.ResolveOr()
	cp, ok := p.(*predicates.ConstantPredicate)
	if !ok || cp.Value != predicates.TriFalse {
		t.Fatalf("empty OR: got %T %v, want FALSE", p, p)
	}
}

func TestResolver_ResolveNot(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)

	// Nil → UNKNOWN.
	p := r.ResolveNot(nil)
	cp, ok := p.(*predicates.ConstantPredicate)
	if !ok || cp.Value != predicates.TriUnknown {
		t.Fatalf("nil NOT: got %T %v, want UNKNOWN", p, p)
	}

	// Wrapping.
	inner := predicates.NewConstantPredicate(predicates.TriTrue)
	wrapped := r.ResolveNot(inner)
	if _, ok := wrapped.(*predicates.NotPredicate); !ok {
		t.Fatalf("expected NotPredicate, got %T", wrapped)
	}
}

// End-to-end: expr-built predicates run cleanly through the cascades
// Simplify driver. Builds `(5 = 5) AND (id > 0)` and confirms Simplify
// folds the tautology to just `id > 0`.
func TestResolver_FeedsCascadesSimplify(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)

	// (5 = 5)
	five1, _ := r.ResolveConstant(int64(5))
	five2, _ := r.ResolveConstant(int64(5))
	tautology, _ := r.ResolveComparison(predicates.ComparisonEquals, five1, five2)

	// (id > 0)
	id, _ := r.ResolveIdentifier(semantic.Identifier{}, semantic.NewUnquoted("id"))
	zero, _ := r.ResolveConstant(int64(0))
	nonFold, _ := r.ResolveComparison(predicates.ComparisonGreaterThan, id, zero)

	// Combined AND.
	combined := r.ResolveAnd(tautology, nonFold)

	// Run through the simplifier.
	simplified := cascades.Simplify(combined, cascades.DefaultSimplifyRules())

	// Tautology should fold; `id > 0` survives alone.
	if got, want := simplified.Explain(), "ID > 0"; got != want {
		t.Fatalf("Simplify: got %q, want %q", got, want)
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
	sum, err := r.ResolveArithmetic(values.OpAdd, idRef, one)
	if err != nil {
		t.Fatal(err)
	}
	five, _ := r.ResolveConstant(int64(5))
	pred, err := r.ResolveComparison(predicates.ComparisonGreaterThan, sum, five)
	if err != nil {
		t.Fatal(err)
	}

	row := map[string]any{"ID": int64(7)} // id+1 = 8 > 5 → TRUE
	got := mustEval(pred, row)
	if got != predicates.TriTrue {
		t.Fatalf("8 > 5: expected TRUE, got %v", got)
	}
	row["ID"] = int64(2) // 2+1 = 3 > 5 → FALSE
	got = mustEval(pred, row)
	if got != predicates.TriFalse {
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
