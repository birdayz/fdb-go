package cascades

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
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
	c := func(ct ComparisonType, rhs any) Comparison { return Comparison{Type: ct, Operand: LiteralValue(rhs)} }

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
			got := tc.cmp.Eval(tc.left)
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestComparison_Eval_NullIsUnknown(t *testing.T) {
	t.Parallel()
	c := Comparison{Type: ComparisonEquals, Operand: LiteralValue(int64(5))}
	if got := c.Eval(nil); got != TriUnknown {
		t.Fatalf("left=NULL: got %v", got)
	}
	c2 := Comparison{Type: ComparisonEquals, Operand: LiteralValue(nil)}
	if got := c2.Eval(int64(5)); got != TriUnknown {
		t.Fatalf("right=NULL: got %v", got)
	}
}

func TestComparison_Eval_TypeMismatchIsUnknown(t *testing.T) {
	t.Parallel()
	c := Comparison{Type: ComparisonEquals, Operand: LiteralValue(int64(5))}
	// String vs int: types don't match, cmpAny returns (0, false),
	// Eval degrades to UNKNOWN per SQL 3VL.
	if got := c.Eval("5"); got != TriUnknown {
		t.Fatalf("type mismatch: got %v", got)
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
			got := Comparison{Type: tc.op, Operand: LiteralValue(tc.r)}.Eval(tc.l)
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
			got := Comparison{Type: tc.op, Operand: LiteralValue(tc.r)}.Eval(tc.l)
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}

	// IN-list of []byte — membership test through the same cmpAny
	// path. Verifies the bytes branch also picks up set-membership
	// semantics, not just pairwise comparators.
	hit := Comparison{
		Type:    ComparisonIn,
		Operand: LiteralValue([]any{[]byte{0x01}, []byte{0x02, 0x03}, []byte{0x04}}),
	}.Eval([]byte{0x02, 0x03})
	if hit != TriTrue {
		t.Errorf("bytes IN list hit: got %v, want TRUE", hit)
	}
	miss := Comparison{
		Type:    ComparisonIn,
		Operand: LiteralValue([]any{[]byte{0x01}, []byte{0x02, 0x03}}),
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
			got := Comparison{Type: tc.op, Operand: LiteralValue(tc.rhs)}.Eval(tc.left)
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
			got := Comparison{Type: tc.op, Operand: LiteralValue(tc.rhs)}.Eval(tc.left)
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
			got := Comparison{Type: ComparisonIn, Operand: LiteralValue(tc.list)}.Eval(tc.left)
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
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
			got := Comparison{Type: ComparisonLike, Operand: LiteralValue(tc.pattern)}.Eval(tc.s)
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// LIKE type mismatch degrades to UNKNOWN.
func TestComparison_Eval_Like_TypeMismatch(t *testing.T) {
	t.Parallel()
	if got := (Comparison{Type: ComparisonLike, Operand: LiteralValue("abc")}).Eval(int64(5)); got != TriUnknown {
		t.Fatalf("got %v", got)
	}
	if got := (Comparison{Type: ComparisonLike, Operand: LiteralValue(int64(5))}).Eval("abc"); got != TriUnknown {
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
		{ComparisonNotEquals, ComparisonEquals, true},
		{ComparisonLessThan, ComparisonGreaterThanEq, true},
		{ComparisonLessThanOrEq, ComparisonGreaterThan, true},
		{ComparisonGreaterThan, ComparisonLessThanOrEq, true},
		{ComparisonGreaterThanEq, ComparisonLessThan, true},
		{ComparisonIsNull, ComparisonIsNotNull, true},
		{ComparisonIsNotNull, ComparisonIsNull, true},
		{ComparisonIsDistinctFrom, ComparisonNotDistinctFrom, true},
		{ComparisonNotDistinctFrom, ComparisonIsDistinctFrom, true},
		// No direct negation:
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
	// Negate is an involution for the types that have a direct
	// negation: Negate(Negate(t)) == t.
	for _, tc := range cases {
		if !tc.ok {
			continue
		}
		negated, _ := tc.in.Negate()
		back, _ := negated.Negate()
		if back != tc.in {
			t.Fatalf("Negate(Negate(%s)) = %s, want %s",
				tc.in.Symbol(), back.Symbol(), tc.in.Symbol())
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
			got := Comparison{Type: ComparisonStartsWith, Operand: LiteralValue(tc.rhs)}.Eval(tc.left)
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
			got := Comparison{Type: tc.op}.Eval(tc.left)
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// Explain of a unary predicate has no RHS literal.
func TestComparisonPredicate_Explain_Unary(t *testing.T) {
	t.Parallel()
	p := NewComparisonPredicate(
		&FieldValue{Field: "middle_name", Typ: TypeString},
		Comparison{Type: ComparisonIsNull},
	)
	if got, want := p.Explain(), "middle_name IS NULL"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	p2 := NewComparisonPredicate(
		&FieldValue{Field: "email", Typ: TypeString},
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
				&FieldValue{Field: "name", Typ: TypeString},
				Comparison{Type: ComparisonEquals, Operand: LiteralValue("bob")},
			),
			want: "name = 'bob'",
		},
		{
			name: "int RHS is bare",
			pred: NewComparisonPredicate(
				&FieldValue{Field: "id", Typ: TypeInt},
				Comparison{Type: ComparisonGreaterThan, Operand: LiteralValue(int64(5))},
			),
			want: "id > 5",
		},
		{
			name: "NULL RHS (rare but possible via constant-fold)",
			pred: NewComparisonPredicate(
				&FieldValue{Field: "id", Typ: TypeInt},
				Comparison{Type: ComparisonEquals, Operand: LiteralValue(nil)},
			),
			want: "id = NULL",
		},
		{
			name: "IN list with mixed types",
			pred: NewComparisonPredicate(
				&FieldValue{Field: "role", Typ: TypeString},
				Comparison{Type: ComparisonIn, Operand: LiteralValue([]any{"admin", "owner", nil})},
			),
			want: "role IN ('admin', 'owner', NULL)",
		},
		{
			name: "IN list of ints",
			pred: NewComparisonPredicate(
				&FieldValue{Field: "id", Typ: TypeInt},
				Comparison{Type: ComparisonIn, Operand: LiteralValue([]any{int64(1), int64(2), int64(3)})},
			),
			want: "id IN (1, 2, 3)",
		},
		{
			name: "bool RHS uppercased",
			pred: NewComparisonPredicate(
				&FieldValue{Field: "active", Typ: TypeBool},
				Comparison{Type: ComparisonEquals, Operand: LiteralValue(true)},
			),
			want: "active = TRUE",
		},
		{
			name: "bool FALSE uppercased",
			pred: NewComparisonPredicate(
				&FieldValue{Field: "active", Typ: TypeBool},
				Comparison{Type: ComparisonNotEquals, Operand: LiteralValue(false)},
			),
			want: "active <> FALSE",
		},
		{
			name: "bytes as SQL hex literal",
			pred: NewComparisonPredicate(
				&FieldValue{Field: "digest", Typ: TypeUnknown},
				Comparison{Type: ComparisonEquals, Operand: LiteralValue([]byte{0x01, 0x02, 0xff})},
			),
			want: "digest = X'0102ff'",
		},
		{
			name: "empty bytes literal",
			pred: NewComparisonPredicate(
				&FieldValue{Field: "digest", Typ: TypeUnknown},
				Comparison{Type: ComparisonEquals, Operand: LiteralValue([]byte{})},
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
	c := Comparison{Type: ComparisonLessThan, Operand: LiteralValue("b")}
	if got := c.Eval("a"); got != TriTrue {
		t.Fatalf("a < b: got %v", got)
	}
	if got := c.Eval("c"); got != TriFalse {
		t.Fatalf("c < b: got %v", got)
	}
}

func TestComparisonPredicate_EndToEnd(t *testing.T) {
	t.Parallel()
	// Predicate: field `age >= 18` against a row represented as a
	// map. FieldValue.Evaluate resolves the column; Value.Evaluate
	// now drives the predicate — no more closure seam.
	operand := &FieldValue{Field: "age", Typ: TypeInt}
	cmp := Comparison{Type: ComparisonGreaterThanEq, Operand: LiteralValue(int64(18))}
	pred := NewComparisonPredicate(operand, cmp)

	row := map[string]any{"age": int64(21)}
	if got := pred.Eval(row); got != TriTrue {
		t.Fatalf("age=21 >= 18: got %v", got)
	}
	row["age"] = int64(15)
	if got := pred.Eval(row); got != TriFalse {
		t.Fatalf("age=15 >= 18: got %v", got)
	}
	row["age"] = nil
	if got := pred.Eval(row); got != TriUnknown {
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
		Comparison: Comparison{Type: ComparisonEquals, Operand: LiteralValue(int64(1))},
	}
	if got := pred.Eval(nil); got != TriUnknown {
		t.Fatalf("nil Operand: got %v", got)
	}
}

func TestComparisonPredicate_ComposesWithKleeneConnectives(t *testing.T) {
	t.Parallel()
	row := map[string]any{"age": int64(21), "rank": int64(3)}

	// (age >= 18) AND (rank < 5)
	tree := NewAnd(
		NewComparisonPredicate(&FieldValue{Field: "age", Typ: TypeInt},
			Comparison{Type: ComparisonGreaterThanEq, Operand: LiteralValue(int64(18))}),
		NewComparisonPredicate(&FieldValue{Field: "rank", Typ: TypeInt},
			Comparison{Type: ComparisonLessThan, Operand: LiteralValue(int64(5))}),
	)
	if got := tree.Eval(row); got != TriTrue {
		t.Fatalf("AND: got %v", got)
	}
	row["rank"] = int64(7)
	if got := tree.Eval(row); got != TriFalse {
		t.Fatalf("AND with rank=7: got %v", got)
	}
}

// ComparisonPredicate's operand can be an ArithmeticValue —
// exercises Value.Evaluate recursion.
func TestComparisonPredicate_ArithmeticOperand(t *testing.T) {
	t.Parallel()
	// (a + b) > 10
	sum := &ArithmeticValue{
		Op:    OpAdd,
		Left:  &FieldValue{Field: "a", Typ: TypeInt},
		Right: &FieldValue{Field: "b", Typ: TypeInt},
	}
	pred := NewComparisonPredicate(sum,
		Comparison{Type: ComparisonGreaterThan, Operand: LiteralValue(int64(10))})

	if got := pred.Eval(map[string]any{"a": int64(5), "b": int64(7)}); got != TriTrue {
		t.Fatalf("5+7=12 > 10: got %v", got)
	}
	if got := pred.Eval(map[string]any{"a": int64(3), "b": int64(4)}); got != TriFalse {
		t.Fatalf("3+4=7 > 10: got %v", got)
	}
	// NULL propagation: a=NULL -> a+b=NULL -> UNKNOWN.
	if got := pred.Eval(map[string]any{"a": nil, "b": int64(1)}); got != TriUnknown {
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
		&FieldValue{Field: "age", Typ: TypeInt},
		Comparison{Type: ComparisonEquals, Operand: &FieldValue{Field: "cutoff", Typ: TypeInt}},
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
			if got := pred.Eval(tc.row); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// `a = b + 1`: RHS is an ArithmeticValue over row columns. Proves
// arbitrary Value trees compose as RHS, not just FieldValue.
func TestComparisonPredicate_NonConstantRHS_Arithmetic(t *testing.T) {
	t.Parallel()
	pred := NewComparisonPredicate(
		&FieldValue{Field: "a", Typ: TypeInt},
		Comparison{
			Type: ComparisonEquals,
			Operand: &ArithmeticValue{
				Op:    OpAdd,
				Left:  &FieldValue{Field: "b", Typ: TypeInt},
				Right: &ConstantValue{Value: int64(1), Typ: TypeInt},
			},
		},
	)
	if got := pred.Eval(map[string]any{"a": int64(5), "b": int64(4)}); got != TriTrue {
		t.Fatalf("5 = 4+1: got %v", got)
	}
	if got := pred.Eval(map[string]any{"a": int64(5), "b": int64(5)}); got != TriFalse {
		t.Fatalf("5 = 5+1: got %v", got)
	}
}

// Plan-time constant-fold must NOT fire when the RHS is non-constant
// — `col = field` cannot be decided at plan time. The simplifier
// rule gates on IsConstantValue(RHS) per the Value migration.
func TestComparisonConstantSimplify_NonConstantRHS_NoFold(t *testing.T) {
	t.Parallel()
	rule := NewComparisonConstantSimplifyRule()
	pred := NewComparisonPredicate(
		&ConstantValue{Value: int64(5), Typ: TypeInt},
		Comparison{Type: ComparisonEquals, Operand: &FieldValue{Field: "col", Typ: TypeInt}},
	)
	if got := FireRule(rule, pred); len(got) != 0 {
		t.Fatalf("expected no yield (non-constant RHS), got %d", len(got))
	}
}

// Plan-time constant-fold fires when BOTH sides are constant. Pins
// the Value-wrapped RHS variant: `5 = 5` folds to TRUE regardless
// of whether the RHS is a raw literal or a ConstantValue.
func TestComparisonConstantSimplify_ConstantValueRHS_Folds(t *testing.T) {
	t.Parallel()
	rule := NewComparisonConstantSimplifyRule()
	pred := NewComparisonPredicate(
		&ConstantValue{Value: int64(5), Typ: TypeInt},
		Comparison{Type: ComparisonEquals, Operand: &ConstantValue{Value: int64(5), Typ: TypeInt}},
	)
	got := FireRule(rule, pred)
	if len(got) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(got))
	}
	cp, ok := got[0].(*ConstantPredicate)
	if !ok {
		t.Fatalf("expected ConstantPredicate, got %T", got[0])
	}
	if cp.Value != TriTrue {
		t.Fatalf("5=5 should be TRUE, got %v", cp.Value)
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
	if _, ok := LiteralValue(nil).(*NullValue); !ok {
		t.Fatalf("nil → %T, want *NullValue", LiteralValue(nil))
	}
	// Bool → BooleanValue (not ConstantValue{Value: bool}). The
	// IS TRUE / IS FALSE desugar + the constant-fold rule both
	// match *BooleanValue specifically.
	if _, ok := LiteralValue(true).(*BooleanValue); !ok {
		t.Fatalf("true → %T, want *BooleanValue", LiteralValue(true))
	}
	// Everything else → ConstantValue with Value preserved verbatim.
	cv, ok := LiteralValue(int64(42)).(*ConstantValue)
	if !ok {
		t.Fatalf("int64 → %T, want *ConstantValue", LiteralValue(int64(42)))
	}
	if cv.Value != int64(42) {
		t.Fatalf("int64 Value: got %v, want 42", cv.Value)
	}
	// String preserved verbatim (no quoting in the stored Value).
	cs := LiteralValue("hello").(*ConstantValue)
	if cs.Value != "hello" {
		t.Fatalf("string Value: got %v, want hello", cs.Value)
	}
	// []any stays []any — IN-list callers depend on this shape.
	list := LiteralValue([]any{int64(1), int64(2), int64(3)}).(*ConstantValue)
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
	cv, ok := c.Operand.(*ConstantValue)
	if !ok {
		t.Fatalf("Operand: %T", c.Operand)
	}
	if cv.Value != int64(18) {
		t.Fatalf("wrapped Value: %v", cv.Value)
	}
	// Unary types also accept a nil literal — Operand becomes
	// NullValue (ignored at eval time for unary).
	u := NewLiteralComparison(ComparisonIsNull, nil)
	if _, ok := u.Operand.(*NullValue); !ok {
		t.Fatalf("unary nil literal → %T, want *NullValue", u.Operand)
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
		got := likeMatch(pattern, s)
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
	var b strings.Builder
	// (?s) flag: . matches newlines too — matches SQL LIKE's
	// byte-any semantics where `%` matches any character including
	// \n / \r, not just printable chars.
	b.WriteString("(?s)^")
	for _, r := range pattern {
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
