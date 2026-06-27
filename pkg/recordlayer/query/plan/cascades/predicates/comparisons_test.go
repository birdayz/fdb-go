package predicates

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

var _ QueryPredicate = (*ComparisonPredicate)(nil)

func TestComparisonType_Symbol(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   ComparisonType
		want string
	}{
		{ComparisonEquals, "="},
		{ComparisonNotEquals, "<>"},
		{ComparisonLessThan, "<"},
		{ComparisonLessThanOrEq, "<="},
		{ComparisonGreaterThan, ">"},
		{ComparisonGreaterThanEq, ">="},
		{ComparisonIsNull, "IS NULL"},
		{ComparisonIsNotNull, "IS NOT NULL"},
		{ComparisonStartsWith, "STARTS_WITH"},
		{ComparisonIn, "IN"},
		{ComparisonIsDistinctFrom, "IS DISTINCT FROM"},
		{ComparisonNotDistinctFrom, "IS NOT DISTINCT FROM"},
		{ComparisonLike, "LIKE"},
		{ComparisonType(999), "?"},
	}
	for _, tc := range cases {
		if got := tc.in.Symbol(); got != tc.want {
			t.Fatalf("%d: got %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestComparison_Eval_Integers(t *testing.T) {
	t.Parallel()
	c := func(ct ComparisonType, rhs any) Comparison {
		return Comparison{Type: ct, Operand: values.LiteralValue(rhs)}
	}

	cases := []struct {
		name string
		cmp  Comparison
		left any
		want TriBool
	}{
		// Equality
		{"1 = 1", c(ComparisonEquals, int64(1)), int64(1), TriTrue},
		{"1 = 2", c(ComparisonEquals, int64(2)), int64(1), TriFalse},
		// Inequality
		{"1 <> 2", c(ComparisonNotEquals, int64(2)), int64(1), TriTrue},
		{"1 <> 1", c(ComparisonNotEquals, int64(1)), int64(1), TriFalse},
		// Strict lt/gt
		{"1 < 2", c(ComparisonLessThan, int64(2)), int64(1), TriTrue},
		{"2 < 1", c(ComparisonLessThan, int64(1)), int64(2), TriFalse},
		{"1 < 1", c(ComparisonLessThan, int64(1)), int64(1), TriFalse},
		{"2 > 1", c(ComparisonGreaterThan, int64(1)), int64(2), TriTrue},
		{"1 > 2", c(ComparisonGreaterThan, int64(2)), int64(1), TriFalse},
		// Inclusive lt/gt
		{"1 <= 1", c(ComparisonLessThanOrEq, int64(1)), int64(1), TriTrue},
		{"1 <= 2", c(ComparisonLessThanOrEq, int64(2)), int64(1), TriTrue},
		{"2 <= 1", c(ComparisonLessThanOrEq, int64(1)), int64(2), TriFalse},
		{"1 >= 1", c(ComparisonGreaterThanEq, int64(1)), int64(1), TriTrue},
		{"2 >= 1", c(ComparisonGreaterThanEq, int64(1)), int64(2), TriTrue},
		{"1 >= 2", c(ComparisonGreaterThanEq, int64(2)), int64(1), TriFalse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, _ := tc.cmp.Eval(tc.left)
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestComparison_Eval_NullIsUnknown(t *testing.T) {
	t.Parallel()
	c := Comparison{Type: ComparisonEquals, Operand: values.LiteralValue(int64(5))}
	if got, _ := c.Eval(nil); got != TriUnknown {
		t.Fatalf("left=NULL: got %v", got)
	}
	c2 := Comparison{Type: ComparisonEquals, Operand: values.LiteralValue(nil)}
	if got, _ := c2.Eval(int64(5)); got != TriUnknown {
		t.Fatalf("right=NULL: got %v", got)
	}
}

func TestComparison_Eval_TypeMismatchErrors(t *testing.T) {
	t.Parallel()
	c := Comparison{Type: ComparisonEquals, Operand: values.LiteralValue(int64(5))}
	_, err := c.Eval("5")
	if err == nil {
		t.Fatal("expected error on type mismatch")
	}
	var tmErr *TypeMismatchError
	if !errors.As(err, &tmErr) {
		t.Fatalf("expected *TypeMismatchError, got %T", err)
	}
}

// Numeric promotion: mixed integer widths compare by int64-promoted
// values, mixed int/float by float64-promoted values. Mirrors Java's
// `functions.CompareValues` behavior so cross-width WHERE predicates
// (e.g. `int32_col > 18` with a literal int64) don't degrade to
// UNKNOWN.
// Bool equality + ordering: used by the expression resolver's
// `x IS TRUE` / `x IS FALSE` desugar. SQL orders FALSE < TRUE.
func TestComparison_Eval_BoolEquality(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		op   ComparisonType
		l, r any
		want TriBool
	}{
		{"TRUE = TRUE", ComparisonEquals, true, true, TriTrue},
		{"FALSE = FALSE", ComparisonEquals, false, false, TriTrue},
		{"TRUE = FALSE", ComparisonEquals, true, false, TriFalse},
		{"TRUE <> FALSE", ComparisonNotEquals, true, false, TriTrue},
		{"FALSE < TRUE", ComparisonLessThan, false, true, TriTrue},
		{"TRUE > FALSE", ComparisonGreaterThan, true, false, TriTrue},
		{"TRUE = 1: type mismatch", ComparisonEquals, true, int64(1), TriUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, _ := Comparison{Type: tc.op, Operand: values.LiteralValue(tc.r)}.Eval(tc.l)
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// Bytes are lexicographic (matches SQL BINARY and proto bytes).
// Mixed bytes/string is a type mismatch → UNKNOWN, not a coerce.
func TestComparison_Eval_BytesComparison(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		op   ComparisonType
		l, r any
		want TriBool
	}{
		{"equal bytes", ComparisonEquals, []byte{0x01, 0x02}, []byte{0x01, 0x02}, TriTrue},
		{"unequal bytes", ComparisonEquals, []byte{0x01, 0x02}, []byte{0x01, 0x03}, TriFalse},
		{"lt shorter prefix", ComparisonLessThan, []byte{0x01, 0x02}, []byte{0x01, 0x02, 0x00}, TriTrue},
		{"gt higher byte", ComparisonGreaterThan, []byte{0x02}, []byte{0x01, 0xff}, TriTrue},
		{"empty vs non-empty", ComparisonLessThan, []byte{}, []byte{0x00}, TriTrue},
		{"bytes vs string: UNKNOWN", ComparisonEquals, []byte("abc"), "abc", TriUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, _ := Comparison{Type: tc.op, Operand: values.LiteralValue(tc.r)}.Eval(tc.l)
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}

	// IN-list of []byte — membership test through the same cmpAny
	// path. Verifies the bytes branch also picks up set-membership
	// semantics, not just pairwise comparators.
	hit, _ := Comparison{
		Type:    ComparisonIn,
		Operand: values.LiteralValue([]any{[]byte{0x01}, []byte{0x02, 0x03}, []byte{0x04}}),
	}.Eval([]byte{0x02, 0x03})
	if hit != TriTrue {
		t.Errorf("bytes IN list hit: got %v, want TRUE", hit)
	}
	miss, _ := Comparison{
		Type:    ComparisonIn,
		Operand: values.LiteralValue([]any{[]byte{0x01}, []byte{0x02, 0x03}}),
	}.Eval([]byte{0x99})
	if miss != TriFalse {
		t.Errorf("bytes IN list miss: got %v, want FALSE", miss)
	}
}

func TestComparison_Eval_NumericPromotion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		op   ComparisonType
		rhs  any
		left any
		want TriBool
	}{
		{"int32 vs int64 eq", ComparisonEquals, int64(18), int32(18), TriTrue},
		{"int vs int64 lt", ComparisonLessThan, int64(10), int(5), TriTrue},
		{"int8 vs int64 gt", ComparisonGreaterThan, int64(0), int8(7), TriTrue},
		{"int16 vs int32 eq", ComparisonEquals, int32(42), int16(42), TriTrue},
		{"float64 vs int64 gt", ComparisonGreaterThan, int64(10), float64(10.5), TriTrue},
		{"int32 vs float64 lt", ComparisonLessThan, float64(10.5), int32(10), TriTrue},
		{"float32 vs float64 eq", ComparisonEquals, float64(1.5), float32(1.5), TriTrue},
		{"int64 vs float64 eq for whole", ComparisonEquals, float64(18), int64(18), TriTrue},
		// Genuine mismatch still degrades.
		{"int64 vs bool: UNKNOWN", ComparisonEquals, true, int64(1), TriUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, _ := Comparison{Type: tc.op, Operand: values.LiteralValue(tc.rhs)}.Eval(tc.left)
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// IS [NOT] DISTINCT FROM: SQL null-safe (in)equality — always
// resolves to TRUE/FALSE, never UNKNOWN. Two NULLs are NOT DISTINCT.
func TestComparison_Eval_IsDistinctFrom(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		op   ComparisonType
		left any
		rhs  any
		want TriBool
	}{
		{"NULL IS DISTINCT FROM NULL", ComparisonIsDistinctFrom, nil, nil, TriFalse},
		{"NULL IS NOT DISTINCT FROM NULL", ComparisonNotDistinctFrom, nil, nil, TriTrue},
		{"NULL IS DISTINCT FROM 1", ComparisonIsDistinctFrom, nil, int64(1), TriTrue},
		{"1 IS DISTINCT FROM NULL", ComparisonIsDistinctFrom, int64(1), nil, TriTrue},
		{"1 IS NOT DISTINCT FROM NULL", ComparisonNotDistinctFrom, int64(1), nil, TriFalse},
		{"1 IS DISTINCT FROM 1", ComparisonIsDistinctFrom, int64(1), int64(1), TriFalse},
		{"1 IS NOT DISTINCT FROM 1", ComparisonNotDistinctFrom, int64(1), int64(1), TriTrue},
		{"1 IS DISTINCT FROM 2", ComparisonIsDistinctFrom, int64(1), int64(2), TriTrue},
		{"1 IS NOT DISTINCT FROM 2", ComparisonNotDistinctFrom, int64(1), int64(2), TriFalse},
		// Numeric promotion works through the distinct-from path.
		{"int32(5) NOT DISTINCT int64(5)", ComparisonNotDistinctFrom, int32(5), int64(5), TriTrue},
		// Type mismatch without NULL → treated as distinct (not equal).
		{"int NOT DISTINCT string", ComparisonNotDistinctFrom, int64(5), "5", TriFalse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, _ := Comparison{Type: tc.op, Operand: values.LiteralValue(tc.rhs)}.Eval(tc.left)
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// IN: membership test against a []any list. SQL semantics: empty
// list → FALSE; match → TRUE; no match with no NULL element → FALSE;
// no match but list contains NULL → UNKNOWN; NULL LHS → UNKNOWN.
func TestComparison_Eval_In(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		left any
		list []any
		want TriBool
	}{
		{"5 IN (1,5,9)", int64(5), []any{int64(1), int64(5), int64(9)}, TriTrue},
		{"5 IN (1,2,3)", int64(5), []any{int64(1), int64(2), int64(3)}, TriFalse},
		{"5 IN ()", int64(5), []any{}, TriFalse},
		{"NULL IN (1)", nil, []any{int64(1)}, TriUnknown},
		{"5 IN (1,NULL) no match", int64(5), []any{int64(1), nil}, TriUnknown},
		{"5 IN (5,NULL) match wins over NULL", int64(5), []any{int64(5), nil}, TriTrue},
		{"mixed width: int32 vs int64", int32(5), []any{int64(1), int64(5)}, TriTrue},
		{"'a' IN ('a','b')", "a", []any{"a", "b"}, TriTrue},
		{"'z' IN ('a','b')", "z", []any{"a", "b"}, TriFalse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, _ := Comparison{Type: ComparisonIn, Operand: values.LiteralValue(tc.list)}.Eval(tc.left)
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestComparison_Eval_In_OnlyNullList pins SQL 3VL: a list containing
// only NULLs surfaces UNKNOWN (no concrete match found, but a NULL
// element prevented a definitive miss). Matches the existing 5
// IN (1,NULL) case but with no non-NULL element at all.
func TestComparison_Eval_In_OnlyNullList(t *testing.T) {
	t.Parallel()
	got, _ := Comparison{Type: ComparisonIn, Operand: values.LiteralValue([]any{nil, nil})}.Eval(int64(5))
	if got != TriUnknown {
		t.Fatalf("5 IN (NULL,NULL): got %v, want TriUnknown", got)
	}
}

// TestComparison_Eval_In_NonSliceRHS pins the boundary: when the RHS
// isn't a []any (Cascades author bug, malformed plan), IN degrades to
// UNKNOWN rather than panicking. Matches the LIKE/STARTS_WITH type-
// mismatch convention.
func TestComparison_Eval_In_NonSliceRHS(t *testing.T) {
	t.Parallel()
	got, _ := Comparison{Type: ComparisonIn, Operand: values.LiteralValue(int64(5))}.Eval(int64(5))
	if got != TriUnknown {
		t.Fatalf("5 IN <non-slice>: got %v, want TriUnknown", got)
	}
}

// TestComparison_Eval_In_CrossTypeLHS verifies that a string LHS
// against a list of int64 returns a TypeMismatchError — matching
// Java's CANNOT_CONVERT_TYPE for incompatible IN-list types.
func TestComparison_Eval_In_CrossTypeLHS(t *testing.T) {
	t.Parallel()
	_, err := Comparison{Type: ComparisonIn, Operand: values.LiteralValue([]any{int64(1), int64(2), int64(3)})}.Eval("hello")
	if err == nil {
		t.Fatal("expected TypeMismatchError")
	}
	var tmErr *TypeMismatchError
	if !errors.As(err, &tmErr) {
		t.Fatalf("expected *TypeMismatchError, got %T", err)
	}
}

// LIKE: SQL pattern matching with `%` / `_`. Anchored both ends.
// No ESCAPE support yet.
func TestComparison_Eval_Like(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		pattern string
		s       string
		want    TriBool
	}{
		{"exact match", "hello", "hello", TriTrue},
		{"wildcard prefix", "hel%", "hello", TriTrue},
		{"wildcard suffix", "%llo", "hello", TriTrue},
		{"wildcard middle", "h%o", "hello", TriTrue},
		{"only %", "%", "anything", TriTrue},
		{"empty pattern, empty string", "", "", TriTrue},
		{"underscore one char", "h_llo", "hello", TriTrue},
		{"underscore wrong length", "h_llo", "hllo", TriFalse},
		{"no match", "hel", "hello", TriFalse},
		{"no match anchored suffix", "llo", "hello", TriFalse},
		{"multi-wildcard backtrack", "a%b%c", "axbycxyc", TriTrue},
		{"unmatched literal", "abc", "abd", TriFalse},
		{"trailing % matches all remaining", "a%", "a", TriTrue},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, _ := Comparison{Type: ComparisonLike, Operand: values.LiteralValue(tc.pattern)}.Eval(tc.s)
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// LIKE type mismatch degrades to UNKNOWN.
func TestComparison_Eval_Like_TypeMismatch(t *testing.T) {
	t.Parallel()
	if got, _ := (Comparison{Type: ComparisonLike, Operand: values.LiteralValue("abc")}).Eval(int64(5)); got != TriUnknown {
		t.Fatalf("got %v", got)
	}
	if got, _ := (Comparison{Type: ComparisonLike, Operand: values.LiteralValue(int64(5))}).Eval("abc"); got != TriUnknown {
		t.Fatalf("got %v", got)
	}
}

// STARTS_WITH: string-prefix comparison. Degrades to UNKNOWN on
// non-string operands (matches numeric type-mismatch behavior).
func TestComparisonType_IsEquality(t *testing.T) {
	t.Parallel()
	cases := []struct {
		t    ComparisonType
		want bool
	}{
		{ComparisonEquals, true},
		{ComparisonIn, true},
		{ComparisonIsNull, true},
		{ComparisonNotDistinctFrom, true},
		{ComparisonNotEquals, false},
		{ComparisonLessThan, false},
		{ComparisonGreaterThanEq, false},
		{ComparisonStartsWith, false},
		{ComparisonIsDistinctFrom, false},
	}
	for _, tc := range cases {
		if got := tc.t.IsEquality(); got != tc.want {
			t.Fatalf("%s: got %v, want %v", tc.t.Symbol(), got, tc.want)
		}
	}
}

func TestComparisonType_Negate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   ComparisonType
		want ComparisonType
		ok   bool
	}{
		{ComparisonEquals, ComparisonNotEquals, true},
		{ComparisonLessThan, ComparisonGreaterThanEq, true},
		{ComparisonLessThanOrEq, ComparisonGreaterThan, true},
		{ComparisonGreaterThan, ComparisonLessThanOrEq, true},
		{ComparisonGreaterThanEq, ComparisonLessThan, true},
		// Java's invertComparisonType rejects these:
		{ComparisonNotEquals, ComparisonNotEquals, false},
		{ComparisonIsNull, ComparisonIsNull, false},
		{ComparisonIsNotNull, ComparisonIsNotNull, false},
		{ComparisonIsDistinctFrom, ComparisonIsDistinctFrom, false},
		{ComparisonNotDistinctFrom, ComparisonNotDistinctFrom, false},
		{ComparisonIn, ComparisonIn, false},
		{ComparisonStartsWith, ComparisonStartsWith, false},
		{ComparisonLike, ComparisonLike, false},
	}
	for _, tc := range cases {
		got, ok := tc.in.Negate()
		if got != tc.want || ok != tc.ok {
			t.Fatalf("%s: got (%s, %v), want (%s, %v)",
				tc.in.Symbol(), got.Symbol(), ok, tc.want.Symbol(), tc.ok)
		}
	}
	// Inequality pairs are involutions: Negate(Negate(t)) == t.
	// EQUALS→NOT_EQUALS is one-way (Java's invertComparisonType
	// doesn't invert NOT_EQUALS back to EQUALS).
	involutionPairs := []ComparisonType{
		ComparisonLessThan, ComparisonLessThanOrEq,
		ComparisonGreaterThan, ComparisonGreaterThanEq,
	}
	for _, ct := range involutionPairs {
		negated, _ := ct.Negate()
		back, _ := negated.Negate()
		if back != ct {
			t.Fatalf("Negate(Negate(%s)) = %s, want %s",
				ct.Symbol(), back.Symbol(), ct.Symbol())
		}
	}
}

func TestComparison_Eval_StartsWith(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		left any
		rhs  any
		want TriBool
	}{
		{"hello STARTS_WITH hel", "hello", "hel", TriTrue},
		{"hello STARTS_WITH hello", "hello", "hello", TriTrue},
		{"hello STARTS_WITH helloworld", "hello", "helloworld", TriFalse},
		{"empty STARTS_WITH empty", "", "", TriTrue},
		{"abc STARTS_WITH empty", "abc", "", TriTrue},
		{"empty STARTS_WITH abc", "", "abc", TriFalse},
		{"int LHS degrades", int64(5), "5", TriUnknown},
		{"int RHS degrades", "5", int64(5), TriUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, _ := Comparison{Type: ComparisonStartsWith, Operand: values.LiteralValue(tc.rhs)}.Eval(tc.left)
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// IS NULL / IS NOT NULL: unary SQL 2VL comparisons that resolve
// definitively even on NULL input (unlike ordinary comparisons which
// degrade to UNKNOWN).
func TestComparison_Eval_IsNullIsNotNull(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		op   ComparisonType
		left any
		want TriBool
	}{
		{"NULL IS NULL", ComparisonIsNull, nil, TriTrue},
		{"5 IS NULL", ComparisonIsNull, int64(5), TriFalse},
		{"'' IS NULL", ComparisonIsNull, "", TriFalse},
		{"NULL IS NOT NULL", ComparisonIsNotNull, nil, TriFalse},
		{"5 IS NOT NULL", ComparisonIsNotNull, int64(5), TriTrue},
		{"0 IS NOT NULL", ComparisonIsNotNull, int64(0), TriTrue},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Operand deliberately nil — unary comparisons must
			// ignore it (no UNKNOWN degradation).
			got, _ := Comparison{Type: tc.op}.Eval(tc.left)
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// IS NULL / IS NOT NULL with FieldValue LHS — exercise the
// ComparisonPredicate.Eval path with a row context. Unary
// comparisons short-circuit the RHS evaluation (Operand stays nil
// and is never read), so the only signal is the LHS-from-row
// resolution. Pin the truth table for present-and-non-NULL,
// present-and-NULL, and missing-from-row.
func TestComparisonPredicate_IsNull_NonConstantLHS(t *testing.T) {
	t.Parallel()
	isNull := NewComparisonPredicate(
		&values.FieldValue{Field: "name", Typ: values.TypeString},
		Comparison{Type: ComparisonIsNull},
	)
	isNotNull := NewComparisonPredicate(
		&values.FieldValue{Field: "name", Typ: values.TypeString},
		Comparison{Type: ComparisonIsNotNull},
	)
	cases := []struct {
		name        string
		row         map[string]any
		wantNull    TriBool
		wantNotNull TriBool
	}{
		{"present non-NULL", map[string]any{"name": "bob"}, TriFalse, TriTrue},
		{"present NULL", map[string]any{"name": nil}, TriTrue, TriFalse},
		{"missing from row", map[string]any{}, TriTrue, TriFalse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got, _ := isNull.Eval(tc.row); got != tc.wantNull {
				t.Errorf("IS NULL: got %v, want %v", got, tc.wantNull)
			}
			if got, _ := isNotNull.Eval(tc.row); got != tc.wantNotNull {
				t.Errorf("IS NOT NULL: got %v, want %v", got, tc.wantNotNull)
			}
		})
	}
}

// Explain of a unary predicate has no RHS literal.
func TestComparisonPredicate_Explain_Unary(t *testing.T) {
	t.Parallel()
	p := NewComparisonPredicate(
		&values.FieldValue{Field: "middle_name", Typ: values.TypeString},
		Comparison{Type: ComparisonIsNull},
	)
	if got, want := p.Explain(), "middle_name IS NULL"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	p2 := NewComparisonPredicate(
		&values.FieldValue{Field: "email", Typ: values.TypeString},
		Comparison{Type: ComparisonIsNotNull},
	)
	if got, want := p2.Explain(), "email IS NOT NULL"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Binary-comparison Explain renders the RHS operand consistently
// with ExplainValue: strings quoted, NULL rendered as NULL, IN-list
// rendered as a paren list. Catches silent drift — the previous
// `%v` fallback rendered `NAME = bob` without quotes, visually
// indistinguishable from a column reference.
func TestComparisonPredicate_Explain_Binary(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		pred *ComparisonPredicate
		want string
	}{
		{
			name: "string RHS is quoted",
			pred: NewComparisonPredicate(
				&values.FieldValue{Field: "name", Typ: values.TypeString},
				Comparison{Type: ComparisonEquals, Operand: values.LiteralValue("bob")},
			),
			want: "name = 'bob'",
		},
		{
			name: "int RHS is bare",
			pred: NewComparisonPredicate(
				&values.FieldValue{Field: "id", Typ: values.TypeInt},
				Comparison{Type: ComparisonGreaterThan, Operand: values.LiteralValue(int64(5))},
			),
			want: "id > 5",
		},
		{
			name: "NULL RHS (rare but possible via constant-fold)",
			pred: NewComparisonPredicate(
				&values.FieldValue{Field: "id", Typ: values.TypeInt},
				Comparison{Type: ComparisonEquals, Operand: values.LiteralValue(nil)},
			),
			want: "id = NULL",
		},
		{
			name: "IN list with mixed types",
			pred: NewComparisonPredicate(
				&values.FieldValue{Field: "role", Typ: values.TypeString},
				Comparison{Type: ComparisonIn, Operand: values.LiteralValue([]any{"admin", "owner", nil})},
			),
			want: "role IN ('admin', 'owner', NULL)",
		},
		{
			name: "IN list of ints",
			pred: NewComparisonPredicate(
				&values.FieldValue{Field: "id", Typ: values.TypeInt},
				Comparison{Type: ComparisonIn, Operand: values.LiteralValue([]any{int64(1), int64(2), int64(3)})},
			),
			want: "id IN (1, 2, 3)",
		},
		{
			name: "bool RHS uppercased",
			pred: NewComparisonPredicate(
				&values.FieldValue{Field: "active", Typ: values.TypeBool},
				Comparison{Type: ComparisonEquals, Operand: values.LiteralValue(true)},
			),
			want: "active = TRUE",
		},
		{
			name: "bool FALSE uppercased",
			pred: NewComparisonPredicate(
				&values.FieldValue{Field: "active", Typ: values.TypeBool},
				Comparison{Type: ComparisonNotEquals, Operand: values.LiteralValue(false)},
			),
			want: "active <> FALSE",
		},
		{
			name: "bytes as SQL hex literal",
			pred: NewComparisonPredicate(
				&values.FieldValue{Field: "digest", Typ: values.TypeUnknown},
				Comparison{Type: ComparisonEquals, Operand: values.LiteralValue([]byte{0x01, 0x02, 0xff})},
			),
			want: "digest = X'0102ff'",
		},
		{
			name: "empty bytes literal",
			pred: NewComparisonPredicate(
				&values.FieldValue{Field: "digest", Typ: values.TypeUnknown},
				Comparison{Type: ComparisonEquals, Operand: values.LiteralValue([]byte{})},
			),
			want: "digest = X''",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.pred.Explain(); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// IsUnary flag is correct for each ComparisonType.
func TestComparisonType_IsUnary(t *testing.T) {
	t.Parallel()
	cases := []struct {
		t    ComparisonType
		want bool
	}{
		{ComparisonEquals, false},
		{ComparisonNotEquals, false},
		{ComparisonLessThan, false},
		{ComparisonLessThanOrEq, false},
		{ComparisonGreaterThan, false},
		{ComparisonGreaterThanEq, false},
		{ComparisonIsNull, true},
		{ComparisonIsNotNull, true},
	}
	for _, tc := range cases {
		if got := tc.t.IsUnary(); got != tc.want {
			t.Fatalf("%v: got %v, want %v", tc.t.Symbol(), got, tc.want)
		}
	}
}

func TestComparison_Eval_Strings(t *testing.T) {
	t.Parallel()
	c := Comparison{Type: ComparisonLessThan, Operand: values.LiteralValue("b")}
	if got, _ := c.Eval("a"); got != TriTrue {
		t.Fatalf("a < b: got %v", got)
	}
	if got, _ := c.Eval("c"); got != TriFalse {
		t.Fatalf("c < b: got %v", got)
	}
}

func TestComparisonPredicate_EndToEnd(t *testing.T) {
	t.Parallel()
	// Predicate: field `age >= 18` against a row represented as a
	// map. FieldValue.Evaluate resolves the column; Value.Evaluate
	// now drives the predicate — no more closure seam.
	operand := &values.FieldValue{Field: "age", Typ: values.TypeInt}
	cmp := Comparison{Type: ComparisonGreaterThanEq, Operand: values.LiteralValue(int64(18))}
	pred := NewComparisonPredicate(operand, cmp)

	row := map[string]any{"age": int64(21)}
	if got, _ := pred.Eval(row); got != TriTrue {
		t.Fatalf("age=21 >= 18: got %v", got)
	}
	row["age"] = int64(15)
	if got, _ := pred.Eval(row); got != TriFalse {
		t.Fatalf("age=15 >= 18: got %v", got)
	}
	row["age"] = nil
	if got, _ := pred.Eval(row); got != TriUnknown {
		t.Fatalf("age=NULL >= 18: got %v", got)
	}

	if got := pred.Explain(); got != "age >= 18" {
		t.Fatalf("Explain: got %q", got)
	}
}

func TestComparisonPredicate_NilOperand(t *testing.T) {
	t.Parallel()
	pred := &ComparisonPredicate{
		// No Operand set — Eval degrades to UNKNOWN.
		Comparison: Comparison{Type: ComparisonEquals, Operand: values.LiteralValue(int64(1))},
	}
	if got, _ := pred.Eval(nil); got != TriUnknown {
		t.Fatalf("nil Operand: got %v", got)
	}
}

func TestComparisonPredicate_ComposesWithKleeneConnectives(t *testing.T) {
	t.Parallel()
	row := map[string]any{"age": int64(21), "rank": int64(3)}

	// (age >= 18) AND (rank < 5)
	tree := NewAnd(
		NewComparisonPredicate(&values.FieldValue{Field: "age", Typ: values.TypeInt},
			Comparison{Type: ComparisonGreaterThanEq, Operand: values.LiteralValue(int64(18))}),
		NewComparisonPredicate(&values.FieldValue{Field: "rank", Typ: values.TypeInt},
			Comparison{Type: ComparisonLessThan, Operand: values.LiteralValue(int64(5))}),
	)
	if got, _ := tree.Eval(row); got != TriTrue {
		t.Fatalf("AND: got %v", got)
	}
	row["rank"] = int64(7)
	if got, _ := tree.Eval(row); got != TriFalse {
		t.Fatalf("AND with rank=7: got %v", got)
	}
}

// ComparisonPredicate's operand can be an ArithmeticValue —
// exercises Value.Evaluate recursion.
func TestComparisonPredicate_ArithmeticOperand(t *testing.T) {
	t.Parallel()
	// (a + b) > 10
	sum := &values.ArithmeticValue{
		Op:    values.OpAdd,
		Left:  &values.FieldValue{Field: "a", Typ: values.TypeInt},
		Right: &values.FieldValue{Field: "b", Typ: values.TypeInt},
	}
	pred := NewComparisonPredicate(sum,
		Comparison{Type: ComparisonGreaterThan, Operand: values.LiteralValue(int64(10))})

	if got, _ := pred.Eval(map[string]any{"a": int64(5), "b": int64(7)}); got != TriTrue {
		t.Fatalf("5+7=12 > 10: got %v", got)
	}
	if got, _ := pred.Eval(map[string]any{"a": int64(3), "b": int64(4)}); got != TriFalse {
		t.Fatalf("3+4=7 > 10: got %v", got)
	}
	// NULL propagation: a=NULL -> a+b=NULL -> UNKNOWN.
	if got, _ := pred.Eval(map[string]any{"a": nil, "b": int64(1)}); got != TriUnknown {
		t.Fatalf("a=NULL: got %v", got)
	}
}

// Non-constant RHS: `a = b` evaluates both sides against the row
// context and dispatches through EvalAgainst. Verifies the
// symmetric-eval path that the Comparison.Operand → Value change
// unblocks. Before that change the RHS was a plan-time literal —
// `a = b` couldn't compose at all.
func TestComparisonPredicate_NonConstantRHS(t *testing.T) {
	t.Parallel()
	// age = rank_cutoff (both FieldValues)
	pred := NewComparisonPredicate(
		&values.FieldValue{Field: "age", Typ: values.TypeInt},
		Comparison{Type: ComparisonEquals, Operand: &values.FieldValue{Field: "cutoff", Typ: values.TypeInt}},
	)

	cases := []struct {
		name string
		row  map[string]any
		want TriBool
	}{
		{"equal", map[string]any{"age": int64(18), "cutoff": int64(18)}, TriTrue},
		{"unequal", map[string]any{"age": int64(21), "cutoff": int64(18)}, TriFalse},
		{"lhs NULL", map[string]any{"age": nil, "cutoff": int64(18)}, TriUnknown},
		{"rhs NULL", map[string]any{"age": int64(18), "cutoff": nil}, TriUnknown},
		{"both NULL", map[string]any{"age": nil, "cutoff": nil}, TriUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got, _ := pred.Eval(tc.row); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// `a IS [NOT] DISTINCT FROM b` with both sides as FieldValues —
// pins the null-safe binary path through ComparisonPredicate.Eval.
// EvalAgainst's IsDistinctFrom branch treats both sides as
// already-resolved any-typed values, so a nil from FieldValue
// (missing-field row) behaves identically to a literal nil.
func TestComparisonPredicate_IsDistinctFrom_NonConstantRHS(t *testing.T) {
	t.Parallel()
	dist := NewComparisonPredicate(
		&values.FieldValue{Field: "a", Typ: values.TypeInt},
		Comparison{Type: ComparisonIsDistinctFrom, Operand: &values.FieldValue{Field: "b", Typ: values.TypeInt}},
	)
	notDist := NewComparisonPredicate(
		&values.FieldValue{Field: "a", Typ: values.TypeInt},
		Comparison{Type: ComparisonNotDistinctFrom, Operand: &values.FieldValue{Field: "b", Typ: values.TypeInt}},
	)

	cases := []struct {
		name        string
		row         map[string]any
		wantDist    TriBool
		wantNotDist TriBool
	}{
		{"both NULL", map[string]any{"a": nil, "b": nil}, TriFalse, TriTrue},
		{"a NULL b 5", map[string]any{"a": nil, "b": int64(5)}, TriTrue, TriFalse},
		{"a 5 b NULL", map[string]any{"a": int64(5), "b": nil}, TriTrue, TriFalse},
		{"both 5", map[string]any{"a": int64(5), "b": int64(5)}, TriFalse, TriTrue},
		{"both 5 vs 6", map[string]any{"a": int64(5), "b": int64(6)}, TriTrue, TriFalse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got, _ := dist.Eval(tc.row); got != tc.wantDist {
				t.Errorf("IS DISTINCT FROM: got %v, want %v", got, tc.wantDist)
			}
			if got, _ := notDist.Eval(tc.row); got != tc.wantNotDist {
				t.Errorf("IS NOT DISTINCT FROM: got %v, want %v", got, tc.wantNotDist)
			}
		})
	}
}

// `a = b + 1`: RHS is an ArithmeticValue over row columns. Proves
// arbitrary Value trees compose as RHS, not just FieldValue.
func TestComparisonPredicate_NonConstantRHS_Arithmetic(t *testing.T) {
	t.Parallel()
	pred := NewComparisonPredicate(
		&values.FieldValue{Field: "a", Typ: values.TypeInt},
		Comparison{
			Type: ComparisonEquals,
			Operand: &values.ArithmeticValue{
				Op:    values.OpAdd,
				Left:  &values.FieldValue{Field: "b", Typ: values.TypeInt},
				Right: &values.ConstantValue{Value: int64(1), Typ: values.TypeInt},
			},
		},
	)
	if got, _ := pred.Eval(map[string]any{"a": int64(5), "b": int64(4)}); got != TriTrue {
		t.Fatalf("5 = 4+1: got %v", got)
	}
	if got, _ := pred.Eval(map[string]any{"a": int64(5), "b": int64(5)}); got != TriFalse {
		t.Fatalf("5 = 5+1: got %v", got)
	}
}

// Non-constant RHS Explain — formatComparisonRHS routes to
// ExplainValue when IsConstantValue(Operand) is false. Pins the
// rendering for FieldValue / ArithmeticValue / CastValue RHS shapes
// the Operand → Value migration unblocked.
func TestComparisonPredicate_Explain_NonConstantRHS(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		pred *ComparisonPredicate
		want string
	}{
		{
			name: "FieldValue RHS",
			pred: NewComparisonPredicate(
				&values.FieldValue{Field: "age", Typ: values.TypeInt},
				Comparison{Type: ComparisonEquals, Operand: &values.FieldValue{Field: "cutoff", Typ: values.TypeInt}},
			),
			want: "age = cutoff",
		},
		{
			name: "Arithmetic RHS over fields",
			pred: NewComparisonPredicate(
				&values.FieldValue{Field: "a", Typ: values.TypeInt},
				Comparison{Type: ComparisonLessThan, Operand: &values.ArithmeticValue{
					Op:    values.OpAdd,
					Left:  &values.FieldValue{Field: "b", Typ: values.TypeInt},
					Right: &values.ConstantValue{Value: int64(1), Typ: values.TypeInt},
				}},
			),
			want: "a < (b + 1)",
		},
		{
			name: "CastValue RHS over field",
			pred: NewComparisonPredicate(
				&values.FieldValue{Field: "id", Typ: values.TypeInt},
				Comparison{Type: ComparisonEquals, Operand: values.NewCastValue(&values.FieldValue{Field: "raw", Typ: values.TypeString}, values.TypeInt)},
			),
			want: "id = CAST(raw AS INT)",
		},
		{
			// Composite constant RHS (CAST over literal) — Explain
			// preserves the user-written shape rather than collapsing
			// to the folded literal. The simplifier handles the fold;
			// rendering doesn't.
			name: "CastValue RHS over constant preserves shape",
			pred: NewComparisonPredicate(
				&values.FieldValue{Field: "x", Typ: values.TypeInt},
				Comparison{Type: ComparisonEquals, Operand: values.NewCastValue(&values.ConstantValue{Value: int64(5), Typ: values.TypeInt}, values.TypeInt)},
			),
			want: "x = CAST(5 AS INT)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.pred.Explain(); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// LiteralValue wraps Go-native literals in the matching Value
// subtype. Pins: nil → NullValue, bool → BooleanValue (with the
// bool-pointer contract), everything else → ConstantValue.
func TestLiteralValue(t *testing.T) {
	t.Parallel()
	// Nil literal becomes NullValue, NOT ConstantValue{Value: nil}.
	// The simplifier distinguishes these — NullValue matches the
	// constant-fold whitelist cleanly, ConstantValue with Value=nil
	// would too but conflates with typed missing values.
	if _, ok := values.LiteralValue(nil).(*values.NullValue); !ok {
		t.Fatalf("nil → %T, want *NullValue", values.LiteralValue(nil))
	}
	// Bool → BooleanValue (not ConstantValue{Value: bool}). The
	// IS TRUE / IS FALSE desugar + the constant-fold rule both
	// match *BooleanValue specifically.
	if _, ok := values.LiteralValue(true).(*values.BooleanValue); !ok {
		t.Fatalf("true → %T, want *BooleanValue", values.LiteralValue(true))
	}
	// Everything else → ConstantValue with Value preserved verbatim.
	cv, ok := values.LiteralValue(int64(42)).(*values.ConstantValue)
	if !ok {
		t.Fatalf("int64 → %T, want *ConstantValue", values.LiteralValue(int64(42)))
	}
	if cv.Value != int64(42) {
		t.Fatalf("int64 Value: got %v, want 42", cv.Value)
	}
	// String preserved verbatim (no quoting in the stored Value).
	cs := values.LiteralValue("hello").(*values.ConstantValue)
	if cs.Value != "hello" {
		t.Fatalf("string Value: got %v, want hello", cs.Value)
	}
	// []any stays []any — IN-list callers depend on this shape.
	list := values.LiteralValue([]any{int64(1), int64(2), int64(3)}).(*values.ConstantValue)
	arr, ok := list.Value.([]any)
	if !ok || len(arr) != 3 {
		t.Fatalf("[]any Value: got %v", list.Value)
	}
}

// NewLiteralComparison is the common-case Comparison constructor.
// It uses LiteralValue internally; the test pins that the wrapper
// composes correctly (Type preserved, RHS wrapped).
func TestNewLiteralComparison(t *testing.T) {
	t.Parallel()
	c := NewLiteralComparison(ComparisonGreaterThan, int64(18))
	if c.Type != ComparisonGreaterThan {
		t.Fatalf("Type: got %v", c.Type)
	}
	cv, ok := c.Operand.(*values.ConstantValue)
	if !ok {
		t.Fatalf("Operand: %T", c.Operand)
	}
	if cv.Value != int64(18) {
		t.Fatalf("wrapped Value: %v", cv.Value)
	}
	// Unary types also accept a nil literal — Operand becomes
	// NullValue (ignored at eval time for unary).
	u := NewLiteralComparison(ComparisonIsNull, nil)
	if _, ok := u.Operand.(*values.NullValue); !ok {
		t.Fatalf("unary nil literal → %T, want *NullValue", u.Operand)
	}
}

// LIKE with ESCAPE — pin the matcher's escape-handling truth
// table. escape == 0 disables (regression check: original
// semantics preserved). Non-zero escape makes the next char
// literal. Trailing escape rune (no following char) is treated
// as no-match — required by SQL semantics.
func TestLikeMatch_Escape(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		pattern string
		s       string
		escape  rune
		want    bool
	}{
		// escape=0 → no escape handling, original wildcard semantics.
		{"no-escape: %% matches anything", "%", "a%b", 0, true},
		// `\%` with escape `\` matches literal `%`.
		{"escape blocks wildcard", `a\%b`, "a%b", '\\', true},
		{"escape blocks wildcard, x rejected", `a\%b`, "axb", '\\', false},
		// `\_` with escape `\` matches literal `_`.
		{"escape blocks underscore wildcard", `a\_b`, "a_b", '\\', true},
		{"escape blocks underscore, x rejected", `a\_b`, "axb", '\\', false},
		// Custom escape character (`!` instead of `\`).
		{"custom escape !", `a!%b`, "a%b", '!', true},
		// Mixed wildcard + escaped: `a\%%c` = literal a, literal %, then anything-c.
		{"escaped + wildcard", `a\%%c`, "a%xyzc", '\\', true},
		{"escaped + wildcard, no leading %", `a\%%c`, "axyzc", '\\', false},
		// Trailing escape with nothing after — pattern can't match
		// (the escape-of-EOF is a malformed pattern; our impl
		// rejects it rather than silently treating escape as literal).
		{"trailing escape, all input consumed", `a\`, "a", '\\', false},
		// Lone trailing escape with input still to match — also
		// rejected. Same malformed-pattern contract: an escape with
		// no following char is never a valid match, regardless of
		// whether input remains. (Fixed by the fuzz-found bug; this
		// row pins the symmetric behavior.)
		{"trailing escape, input still remaining", `a\`, `a\`, '\\', false},
		// Escape preceding a non-meta character: implementation-
		// defined per SQL standard. Our matcher consumes the escape
		// and treats the next character as a literal regardless of
		// meta-ness — so `a\b` matches `ab`, NOT `a\b`. Documents
		// the chosen behavior.
		{"escape over non-meta yields literal next char", `a\b`, "ab", '\\', true},
		{"escape over non-meta does NOT match escape+next", `a\b`, `a\b`, '\\', false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := likeMatch(tc.pattern, tc.s, tc.escape); got != tc.want {
				t.Fatalf("got %v, want %v (pattern=%q s=%q escape=%q)",
					got, tc.want, tc.pattern, tc.s, tc.escape)
			}
		})
	}
}

// LIKE+ESCAPE Explain renders the ESCAPE clause inline so the
// output round-trips back to recognisable SQL.
func TestComparisonPredicate_Explain_LikeEscape(t *testing.T) {
	t.Parallel()
	pred := NewComparisonPredicate(
		&values.FieldValue{Field: "name", Typ: values.TypeString},
		Comparison{
			Type:    ComparisonLike,
			Operand: values.LiteralValue(`a\%b`),
			Escape:  '\\',
		},
	)
	want := `name LIKE 'a\%b' ESCAPE '\'`
	if got := pred.Explain(); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// LIKE+ESCAPE with single-quote as the escape character produces
// valid SQL — the quote inside the literal is doubled per SQL
// string-literal escaping. Without doubling, the output `ESCAPE ”'`
// would be unbalanced and unparseable.
func TestComparisonPredicate_Explain_LikeEscape_SingleQuoteEscape(t *testing.T) {
	t.Parallel()
	pred := NewComparisonPredicate(
		&values.FieldValue{Field: "name", Typ: values.TypeString},
		Comparison{
			Type:    ComparisonLike,
			Operand: values.LiteralValue("a%b"),
			Escape:  '\'',
		},
	)
	want := `name LIKE 'a%b' ESCAPE ''''`
	if got := pred.Explain(); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// LIKE without ESCAPE doesn't emit a stray "ESCAPE ”" clause.
func TestComparisonPredicate_Explain_LikeNoEscape(t *testing.T) {
	t.Parallel()
	pred := NewComparisonPredicate(
		&values.FieldValue{Field: "name", Typ: values.TypeString},
		Comparison{Type: ComparisonLike, Operand: values.LiteralValue("hel%")},
	)
	want := `name LIKE 'hel%'`
	if got := pred.Explain(); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Comparison.Eval with non-zero Escape evaluates LIKE through the
// escape-aware matcher. Pin the round-trip from Comparison{Escape}
// down to likeMatch.
func TestComparison_Eval_LikeWithEscape(t *testing.T) {
	t.Parallel()
	c := Comparison{
		Type:    ComparisonLike,
		Operand: values.LiteralValue(`a\%b`),
		Escape:  '\\',
	}
	if got, _ := c.Eval("a%b"); got != TriTrue {
		t.Errorf("a%%b: got %v, want TRUE", got)
	}
	if got, _ := c.Eval("axb"); got != TriFalse {
		t.Errorf("axb: got %v, want FALSE (escape blocked wildcard)", got)
	}
}

// Float comparisons through ComparisonPredicate.Eval — both operands
// float, mixed int/float (cmpAny promotion), NULL propagation.
// Pinned because the float comparison path reaches further than
// the arithmetic path (ArithmeticValue.Evaluate stays int-only;
// see handover follow-up "Arithmetic over floats").
func TestComparisonPredicate_FloatComparisons(t *testing.T) {
	t.Parallel()
	pred := NewComparisonPredicate(
		&values.FieldValue{Field: "price", Typ: values.TypeFloat},
		Comparison{Type: ComparisonGreaterThan, Operand: values.LiteralValue(float64(3.14))},
	)
	cases := []struct {
		name string
		row  map[string]any
		want TriBool
	}{
		{"4.5 > 3.14", map[string]any{"price": float64(4.5)}, TriTrue},
		{"2.0 > 3.14", map[string]any{"price": float64(2.0)}, TriFalse},
		{"int 5 > 3.14 (cross-type promotion)", map[string]any{"price": int64(5)}, TriTrue},
		{"int 2 > 3.14 (cross-type promotion)", map[string]any{"price": int64(2)}, TriFalse},
		{"NULL > 3.14", map[string]any{"price": nil}, TriUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got, _ := pred.Eval(tc.row); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// LIKE with FieldValue RHS — the Operand→Value migration unlocks
// dynamic patterns sourced from the row. Each row's `pattern`
// column drives the LIKE match against that same row's `name`.
// Non-string operands degrade to UNKNOWN per SQL 3VL.
func TestComparisonPredicate_Like_FieldValueRHS(t *testing.T) {
	t.Parallel()
	pred := NewComparisonPredicate(
		&values.FieldValue{Field: "name", Typ: values.TypeString},
		Comparison{
			Type:    ComparisonLike,
			Operand: &values.FieldValue{Field: "pattern", Typ: values.TypeString},
		},
	)
	cases := []struct {
		row  map[string]any
		want TriBool
	}{
		{map[string]any{"name": "hello", "pattern": "hel%"}, TriTrue},
		{map[string]any{"name": "hello", "pattern": "world"}, TriFalse},
		{map[string]any{"name": "hello", "pattern": "%"}, TriTrue},
		{map[string]any{"name": "abc", "pattern": "_b_"}, TriTrue},
		// NULL pattern → UNKNOWN per SQL 3VL.
		{map[string]any{"name": "hello", "pattern": nil}, TriUnknown},
		// NULL name → UNKNOWN.
		{map[string]any{"name": nil, "pattern": "hel%"}, TriUnknown},
		// Non-string pattern (numeric mismatch) → UNKNOWN.
		{map[string]any{"name": "hello", "pattern": int64(5)}, TriUnknown},
	}
	for _, tc := range cases {
		if got, _ := pred.Eval(tc.row); got != tc.want {
			t.Errorf("row=%v: got %v, want %v", tc.row, got, tc.want)
		}
	}
}

// FuzzLikeMatch cross-checks likeMatch against a regex-based oracle.
// `%` → `.*`, `_` → `.`, all other chars are regex-escaped. Both
// anchored with `^...$`. Mismatch = likeMatch bug.
func FuzzLikeMatch(f *testing.F) {
	// Seed corpus: known-good patterns + strings.
	f.Add("hello", "hello")
	f.Add("h%o", "hello")
	f.Add("h_llo", "hello")
	f.Add("%", "")
	f.Add("", "")
	f.Add("a%b%c", "axbycxyc")
	f.Add("__", "ab")
	f.Fuzz(func(t *testing.T, pattern, s string) {
		// Cap runaway inputs — the matcher is O(n*m); want fuzz to
		// fail fast on pathological seeds rather than burn CPU.
		if len(pattern) > 128 || len(s) > 128 {
			t.Skip()
		}
		// Oracle iterates runes; byte-level LIKE-matcher diverges
		// on invalid UTF-8 (replacement-char substitution). SQL
		// strings should be valid UTF-8, so constrain the corpus.
		if !utf8.ValidString(pattern) || !utf8.ValidString(s) {
			t.Skip()
		}
		got := likeMatch(pattern, s, 0)
		want := likeMatchRegexOracle(pattern, s)
		if got != want {
			t.Fatalf("mismatch: pattern=%q s=%q got=%v want=%v",
				pattern, s, got, want)
		}
	})
}

// likeMatchRegexOracle is a known-good reference impl that translates
// the LIKE pattern to a Go regex and runs it anchored.
func likeMatchRegexOracle(pattern, s string) bool {
	return likeMatchRegexOracleWithEscape(pattern, s, 0)
}

// likeMatchRegexOracleWithEscape is the escape-aware oracle. When
// escape != 0, the rune in the pattern equal to escape consumes the
// next character and emits it as a regex-quoted literal — exactly
// the contract likeMatch documents. A trailing escape (no following
// char) is malformed: the oracle returns false uniformly, mirroring
// likeMatch's malformed-pattern handling.
func likeMatchRegexOracleWithEscape(pattern, s string, escape rune) bool {
	runes := []rune(pattern)
	var b strings.Builder
	b.WriteString("(?s)^")
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if escape != 0 && r == escape {
			if i+1 >= len(runes) {
				return false // trailing escape — malformed
			}
			b.WriteString(regexp.QuoteMeta(string(runes[i+1])))
			i++
			continue
		}
		switch r {
		case '%':
			b.WriteString(".*")
		case '_':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	re := regexp.MustCompile(b.String())
	return re.MatchString(s)
}

// FuzzLikeMatchEscape — same regex-oracle cross-check as FuzzLikeMatch
// but feeds a non-zero escape rune through both implementations.
// The escape rune itself is fuzzed via an int8 parameter (mapped to
// printable ASCII) so adversarial cases like escape=='%' or '_'
// (escape character collides with a wildcard) get coverage too.
//
// Catches divergence between the matcher's escape handling and the
// oracle on adversarial pattern/escape combinations.
func FuzzLikeMatchEscape(f *testing.F) {
	// Seeds covering the documented escape behaviors. The third
	// parameter is the escape-char index — we map it to a printable
	// ASCII rune below so the fuzzer explores `\\`, `!`, `%`, `_`,
	// etc. without invalid-UTF-8 noise.
	f.Add(`a\%b`, "a%b", int8(0)) // escape='\\'
	f.Add(`a\_b`, "a_b", int8(0)) // escape='\\'
	f.Add(`%\%`, "x%", int8(0))   // escape='\\'
	f.Add(`\%`, "%", int8(0))     // escape='\\'
	f.Add(`\`, "", int8(0))       // escape='\\'
	f.Add(`a!%b`, "a%b", int8(1)) // escape='!'
	f.Add(`%%%`, "abc", int8(2))  // escape='%' — collides with wildcard
	f.Fuzz(func(t *testing.T, pattern, s string, escIdx int8) {
		if len(pattern) > 128 || len(s) > 128 {
			t.Skip()
		}
		if !utf8.ValidString(pattern) || !utf8.ValidString(s) {
			t.Skip()
		}
		// Map the int8 index to a printable rune. Using a small set
		// keeps the fuzz space tractable while still hitting the
		// adversarial escape-equals-wildcard cases.
		escapeChars := []rune{'\\', '!', '%', '_', '#', '@'}
		escape := escapeChars[(int(escIdx)%len(escapeChars)+len(escapeChars))%len(escapeChars)]
		got := likeMatch(pattern, s, escape)
		want := likeMatchRegexOracleWithEscape(pattern, s, escape)
		if got != want {
			t.Fatalf("mismatch: pattern=%q s=%q escape=%q got=%v want=%v",
				pattern, s, escape, got, want)
		}
	})
}
