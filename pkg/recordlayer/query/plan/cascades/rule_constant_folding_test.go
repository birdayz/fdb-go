package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func constantFoldingYield(t *testing.T, rule CascadesRule, input predicates.QueryPredicate) predicates.QueryPredicate {
	t.Helper()
	got, err := FireRule(rule, input)
	if err != nil {
		t.Fatalf("FireRule: %v", err)
	}
	switch len(got) {
	case 0:
		return nil
	case 1:
		return got[0].(predicates.QueryPredicate)
	}
	t.Fatalf("expected at most one yield, got %d", len(got))
	return nil
}

func wantConstant(t *testing.T, name string, got predicates.QueryPredicate, want predicates.TriBool) {
	t.Helper()
	cp, ok := got.(*predicates.ConstantPredicate)
	if !ok || cp.Value != want {
		t.Fatalf("%s: got %v, want constant %v", name, got, want)
	}
}

// TestConstantFoldingValuePredicateRule pins Java's
// ConstantFoldingValuePredicateRule over Go's ComparisonPredicate: it folds
// only over EFFECTIVE constants (ConstantPredicateFoldingUtil.foldComparisonMaybe).
func TestConstantFoldingValuePredicateRule(t *testing.T) {
	t.Parallel()
	rule := NewConstantFoldingValuePredicateRule()
	nullLong := values.NewNullValue(values.NullableLong)
	notNullBool := valueFoldField(0) // NOT NULL BOOLEAN column
	nullableLong := valueFoldField(1)
	cmp := func(operand values.Value, typ predicates.ComparisonType, rhs values.Value) predicates.QueryPredicate {
		return &predicates.ComparisonPredicate{Operand: operand, Comparison: predicates.Comparison{Type: typ, Operand: rhs}}
	}
	for _, c := range []struct {
		name string
		in   predicates.QueryPredicate
		want *predicates.TriBool // nil: no fold
	}{
		{"NULL IS NULL", cmp(nullLong, predicates.ComparisonIsNull, nil), ptrTri(predicates.TriTrue)},
		{"NOT NULL operand IS NULL", cmp(notNullBool, predicates.ComparisonIsNull, nil), ptrTri(predicates.TriFalse)},
		{"NOT NULL operand IS NOT NULL", cmp(notNullBool, predicates.ComparisonIsNotNull, nil), ptrTri(predicates.TriTrue)},
		{"nullable operand IS NULL", cmp(nullableLong, predicates.ComparisonIsNull, nil), nil},
		{"NULL = column", cmp(nullLong, predicates.ComparisonEquals, nullableLong), ptrTri(predicates.TriUnknown)},
		{"column < NULL", cmp(nullableLong, predicates.ComparisonLessThan, nullLong), ptrTri(predicates.TriUnknown)},
		{"TRUE = TRUE", cmp(values.NewBooleanValue(true), predicates.ComparisonEquals, values.NewBooleanValue(true)), ptrTri(predicates.TriTrue)},
		{"TRUE <> FALSE", cmp(values.NewBooleanValue(true), predicates.ComparisonNotEquals, values.NewBooleanValue(false)), ptrTri(predicates.TriTrue)},
		// Two non-boolean literals are not effective constants: Java keeps
		// `@c17 EQUALS @c9` and raises a sibling division.
		{"1 = 2", cmp(values.LiteralValue(int64(1)), predicates.ComparisonEquals, values.LiteralValue(int64(2))), nil},
		{"TRUE < FALSE", cmp(values.NewBooleanValue(true), predicates.ComparisonLessThan, values.NewBooleanValue(false)), nil},
	} {
		got := constantFoldingYield(t, rule, c.in)
		if c.want == nil {
			if got != nil {
				t.Errorf("%s: folded to %v, want no fold", c.name, got)
			}
			continue
		}
		wantConstant(t, c.name, got, *c.want)
	}
}

func ptrTri(v predicates.TriBool) *predicates.TriBool { return &v }

// TestConstantFoldingBooleanValuePredicateRule pins the fold of Go's boolean
// ValuePredicate as Java's ValuePredicate(value, EQUALS TRUE).
func TestConstantFoldingBooleanValuePredicateRule(t *testing.T) {
	t.Parallel()
	rule := NewConstantFoldingBooleanValuePredicateRule()
	wantConstant(t, "TRUE", constantFoldingYield(t, rule, predicates.NewValuePredicate(values.NewBooleanValue(true))), predicates.TriTrue)
	wantConstant(t, "FALSE", constantFoldingYield(t, rule, predicates.NewValuePredicate(values.NewBooleanValue(false))), predicates.TriFalse)
	wantConstant(t, "NULL", constantFoldingYield(t, rule, predicates.NewValuePredicate(values.NewNullValue(values.NullableBoolean))), predicates.TriUnknown)
	if got := constantFoldingYield(t, rule, predicates.NewValuePredicate(valueFoldField(0))); got != nil {
		t.Fatalf("a column folded to %v", got)
	}
}

// TestValuePredicateSimplificationRule pins that the leaf rule simplifies a
// comparison's values, yields nothing for an unchanged leaf, and ignores a
// connective.
func TestValuePredicateSimplificationRule(t *testing.T) {
	t.Parallel()
	rule := NewValuePredicateSimplificationRule()
	field := valueFoldField(1)
	// COALESCE(NULL, TRUE) is folded by the PREDICATE value set's COALESCE
	// rule (a NULL head skipped, a BOOLEAN literal head returned).
	coalesce := values.NewScalarFunctionValue("COALESCE", values.NullableBoolean,
		values.NewNullValue(values.NullableBoolean), values.NewBooleanValue(true))
	in := &predicates.ComparisonPredicate{Operand: coalesce, Comparison: predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.NewBooleanValue(true)}}
	got, ok := constantFoldingYield(t, rule, in).(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("no simplified comparison")
	}
	if _, isBool := got.Operand.(*values.BooleanValue); !isBool {
		t.Fatalf("operand %s, want the BOOLEAN head", values.ExplainValue(got.Operand))
	}
	unchanged := &predicates.ComparisonPredicate{Operand: field, Comparison: predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(5))}}
	if got := constantFoldingYield(t, rule, unchanged); got != nil {
		t.Fatalf("an unchanged leaf yielded %v", got)
	}
	if got := constantFoldingYield(t, rule, predicates.NewAnd(unchanged, unchanged)); got != nil {
		t.Fatalf("a connective yielded %v", got)
	}
}

// TestConstantFoldingRules_Conjunction pins the set over a whole conjunction:
// a TRUE conjunct is the AND identity and drops, a FALSE one annuls it, and a
// NOT over a constant predicate is not folded (Java has no such rule).
func TestConstantFoldingRules_Conjunction(t *testing.T) {
	t.Parallel()
	field := valueFoldField(1)
	keep := &predicates.ComparisonPredicate{Operand: field, Comparison: predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: values.LiteralValue(int64(0))}}
	isTrue := &predicates.ComparisonPredicate{Operand: valueFoldField(0), Comparison: predicates.Comparison{Type: predicates.ComparisonIsNotNull}}
	isFalse := &predicates.ComparisonPredicate{Operand: valueFoldField(0), Comparison: predicates.Comparison{Type: predicates.ComparisonIsNull}}

	got, err := Simplify(predicates.NewAnd(keep, isTrue), constantFoldingRules())
	if err != nil {
		t.Fatal(err)
	}
	if !predicates.PredicateEquals(got, keep) {
		t.Fatalf("[n > 0, TRUE] simplified to %s, want n > 0", got.Explain())
	}
	got, err = Simplify(predicates.NewAnd(keep, isFalse), constantFoldingRules())
	if err != nil {
		t.Fatal(err)
	}
	wantConstant(t, "[n > 0, FALSE]", got, predicates.TriFalse)
	notNull := predicates.NewNot(predicates.NewConstantPredicate(predicates.TriUnknown))
	got, err = Simplify(notNull, constantFoldingRules())
	if err != nil {
		t.Fatal(err)
	}
	if _, folded := got.(*predicates.ConstantPredicate); folded {
		t.Fatalf("NOT over a constant folded to %s", got.Explain())
	}
}
