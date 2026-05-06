// Package plangen converts the embedded engine's LogicalOperator
// hierarchy into the Cascades-side RelationalExpression hierarchy.
// This is the seed of TODO Track C1 ("PlanGenerator: LogicalOperator
// → RelationalExpression adapter") — it bridges today's text-based
// logical builder to the new RelationalExpression hierarchy that the
// Cascades planner will operate on.
//
// Scope (seed): the simplest LogicalOperator types that have direct
// RelationalExpression equivalents. Operator types whose conversion
// requires re-parsing string projections / sort keys / aggregates
// from the LogicalOperator's text form into cascades.values.Value
// trees are deferred — they need the SQL parser threaded through,
// which is a bigger plumbing job (gated on the catalog-aware walker
// landing in C1's full scope).
//
// Currently supported:
//   - LogicalScan → FullUnorderedScanExpression
//   - LogicalFilter (Predicate non-nil) → LogicalFilterExpression
//   - LogicalUnion → LogicalUnionExpression (recursive); UNION
//     DISTINCT wraps with LogicalDistinctExpression
//   - LogicalDelete → DeleteExpression (keyed by target table)
//   - LogicalInsert (Source non-nil) → InsertExpression
//   - LogicalProject → LogicalProjectionExpression; each entry is
//     lowered via lowerSimpleScalarText (bare column → FieldValue,
//     int / float / bool / NULL / single-quoted string → Constant
//     or NullValue). Anything more complex falls back to
//     ErrUnsupported.
//   - LogicalSort → LogicalSortExpression; same lowering rules as
//     LogicalProject for each sort-key Expr.
//   - LogicalUpdate → UpdateExpression; same lowering rules for
//     each SET right-hand side.
//   - LogicalAggregate → GroupByExpression; parses GROUP BY keys +
//     aggregate-function text (COUNT/SUM/MIN/MAX/AVG on bare columns)
//   - LogicalLimit → LogicalLimitExpression (limit + offset)
//   - LogicalJoin (INNER/CROSS with OnPredicate or no ON) →
//     SelectExpression with two ForEach quantifiers
//
// Currently unsupported (returns ErrUnsupported):
//   - LogicalProject / LogicalSort / LogicalUpdate with arithmetic,
//     exponent-form numeric literals (`1.5E10`) or escaped string
//     literals (`'it”s'`)
//   - LogicalJoin with LEFT/RIGHT kind and text-only ON
//   - LogicalInsert without Source (VALUES literal) — needs a
//     synthetic LogicalValues source operator
//   - LogicalCTE / LogicalDDL — no equivalent
package plangen

import (
	"errors"
	"fmt"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/logical"
)

// ErrUnsupported is returned by Convert for LogicalOperator types
// the seed adapter doesn't yet handle. Callers should fall back to
// the legacy text-based logical builder.
var ErrUnsupported = errors.New("plangen: operator type not yet supported")

// Convert returns the RelationalExpression equivalent of the given
// LogicalOperator tree. Returns ErrUnsupported (wrapped with the
// concrete type name) if any node in the tree isn't yet handled.
//
// The returned RelationalExpression's Quantifiers point at fresh
// Reference instances — this is a one-way conversion; the caller
// owns the resulting tree.
func Convert(op logical.LogicalOperator) (expressions.RelationalExpression, error) {
	if op == nil {
		return nil, errors.New("plangen: nil LogicalOperator")
	}
	switch o := op.(type) {
	case *logical.LogicalScan:
		return convertScan(o), nil
	case *logical.LogicalFilter:
		return convertFilter(o)
	case *logical.LogicalUnion:
		return convertUnion(o)
	case *logical.LogicalDelete:
		return convertDelete(o)
	case *logical.LogicalInsert:
		return convertInsert(o)
	case *logical.LogicalProject:
		return convertProject(o)
	case *logical.LogicalSort:
		return convertSort(o)
	case *logical.LogicalUpdate:
		return convertUpdate(o)
	case *logical.LogicalAggregate:
		return convertAggregate(o)
	case *logical.LogicalLimit:
		return convertLimit(o)
	case *logical.LogicalJoin:
		return convertJoin(o)
	case *logical.LogicalValues:
		return convertValues(o)
	default:
		return nil, fmt.Errorf("%w: %T", ErrUnsupported, op)
	}
}

// convertScan builds a FullUnorderedScanExpression over the
// LogicalScan's table name. The Alias is dropped — RelationalExpression
// uses a Quantifier to bind aliases at the next level up.
func convertScan(s *logical.LogicalScan) expressions.RelationalExpression {
	return expressions.NewFullUnorderedScanExpression([]string{s.Table}, values.UnknownType)
}

// convertFilter builds a LogicalFilterExpression over the recursively-
// converted child. Uses structured Predicate when available; falls back
// to text-based comparison parsing for simple "col op value" predicates.
// AND-chained text predicates produce multiple entries in the filter's
// predicate list (equivalent to multiple WHERE conjuncts).
func convertFilter(f *logical.LogicalFilter) (expressions.RelationalExpression, error) {
	var preds []predicates.QueryPredicate
	if f.Predicate != nil {
		preds = []predicates.QueryPredicate{f.Predicate}
	} else if f.PredicateText != "" {
		parsed, err := parsePredicateText(f.PredicateText)
		if err != nil {
			return nil, err
		}
		preds = parsed
	} else {
		return nil, fmt.Errorf("%w: LogicalFilter without predicate", ErrUnsupported)
	}
	inner, err := Convert(f.Input)
	if err != nil {
		return nil, fmt.Errorf("filter input: %w", err)
	}
	q := expressions.ForEachQuantifier(expressions.InitialOf(inner))
	return expressions.NewLogicalFilterExpression(
		preds, q,
	), nil
}

// convertUnion builds a LogicalUnionExpression over each recursively-
// converted child wrapped in a fresh Quantifier. UNION DISTINCT
// (Distinct=true) wraps the union in a LogicalDistinctExpression —
// matches Java's planner shape (Union → Distinct over Union).
func convertUnion(u *logical.LogicalUnion) (expressions.RelationalExpression, error) {
	qs := make([]expressions.Quantifier, 0, len(u.Inputs))
	for i, child := range u.Inputs {
		conv, err := Convert(child)
		if err != nil {
			return nil, fmt.Errorf("union input %d: %w", i, err)
		}
		qs = append(qs, expressions.ForEachQuantifier(expressions.InitialOf(conv)))
	}
	union := expressions.NewLogicalUnionExpression(qs)
	if !u.Distinct {
		return union, nil
	}
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(union))
	return expressions.NewLogicalDistinctExpression(innerQ), nil
}

// convertDelete builds a DeleteExpression over the recursively-
// converted child. The LogicalDelete's Target is the table name.
func convertDelete(d *logical.LogicalDelete) (expressions.RelationalExpression, error) {
	inner, err := Convert(d.Input)
	if err != nil {
		return nil, fmt.Errorf("delete input: %w", err)
	}
	q := expressions.ForEachQuantifier(expressions.InitialOf(inner))
	return expressions.NewDeleteExpression(q, d.Target), nil
}

// convertInsert builds an InsertExpression over the recursively-
// converted Source. INSERT-VALUES (Source nil) is unsupported until
// we have a LogicalValues operator to feed in. Target type is left
// Unknown — the cascades typing pass fills it from the catalog later.
func convertInsert(i *logical.LogicalInsert) (expressions.RelationalExpression, error) {
	if i.Source == nil {
		return nil, fmt.Errorf("%w: LogicalInsert without Source (VALUES literal)", ErrUnsupported)
	}
	inner, err := Convert(i.Source)
	if err != nil {
		return nil, fmt.Errorf("insert input: %w", err)
	}
	q := expressions.ForEachQuantifier(expressions.InitialOf(inner))
	return expressions.NewInsertExpression(q, i.Table, values.UnknownType), nil
}

func convertValues(v *logical.LogicalValues) (expressions.RelationalExpression, error) {
	cols := make([]values.Value, len(v.Rows))
	for i, text := range v.Rows {
		val, ok := lowerSimpleScalarText(text)
		if !ok {
			return nil, fmt.Errorf("%w: VALUES column %d: %q", ErrUnsupported, i, text)
		}
		cols[i] = val
	}
	return expressions.NewLogicalValuesExpression(cols), nil
}

func convertProject(p *logical.LogicalProject) (expressions.RelationalExpression, error) {
	projected := make([]values.Value, len(p.Projections))
	for i, pj := range p.Projections {
		v, ok := lowerSimpleScalarText(pj)
		if !ok {
			return nil, fmt.Errorf("%w: LogicalProject entry %d (%q) is not a bare column or simple literal", ErrUnsupported, i, pj)
		}
		projected[i] = v
	}
	inner, err := Convert(p.Input)
	if err != nil {
		return nil, fmt.Errorf("project input: %w", err)
	}
	q := expressions.ForEachQuantifier(expressions.InitialOf(inner))
	return expressions.NewLogicalProjectionExpression(projected, q), nil
}

// convertSort builds a LogicalSortExpression for the recursively-
// converted child. Each sort-key Expr is lowered via
// lowerSimpleScalarText (bare column / simple literal). Anything else
// (`ORDER BY a + b`, `ORDER BY UPPER(name)`, `ORDER BY t.c`) requires
// a text→Value parser we don't have yet.
//
// `LogicalSort{Keys: nil}` lowers to UnsortedLogicalSortExpression —
// matches the no-op case in Java.
func convertSort(s *logical.LogicalSort) (expressions.RelationalExpression, error) {
	keyVals := make([]values.Value, len(s.Keys))
	for i, k := range s.Keys {
		v, ok := lowerSimpleScalarText(k.Expr)
		if !ok {
			return nil, fmt.Errorf("%w: LogicalSort key %d (%q) is not a bare column or simple literal", ErrUnsupported, i, k.Expr)
		}
		keyVals[i] = v
	}
	inner, err := Convert(s.Input)
	if err != nil {
		return nil, fmt.Errorf("sort input: %w", err)
	}
	q := expressions.ForEachQuantifier(expressions.InitialOf(inner))
	if len(s.Keys) == 0 {
		return expressions.UnsortedLogicalSortExpression(q), nil
	}
	keys := make([]expressions.SortKey, len(s.Keys))
	for i, k := range s.Keys {
		keys[i] = expressions.SortKey{
			Value:   keyVals[i],
			Reverse: k.Dir == logical.SortDesc,
		}
	}
	return expressions.NewLogicalSortExpression(keys, q), nil
}

// convertUpdate builds an UpdateExpression for the recursively-
// converted child. Each SET assignment's RHS is lowered via
// lowerSimpleScalarText (bare column / simple literal). Arithmetic /
// function calls / dotted refs in the RHS all still need text→Value
// parsing.
//
// The Input is required (no SET-from-nothing).
func convertUpdate(u *logical.LogicalUpdate) (expressions.RelationalExpression, error) {
	if u.Input == nil {
		return nil, fmt.Errorf("%w: LogicalUpdate without Input", ErrUnsupported)
	}
	rhs := make([]values.Value, len(u.Sets))
	for i, a := range u.Sets {
		v, ok := lowerSimpleScalarText(a.Expr)
		if !ok {
			return nil, fmt.Errorf("%w: LogicalUpdate SET %d (%s = %q) is not a bare-column or simple-literal RHS", ErrUnsupported, i, a.Column, a.Expr)
		}
		rhs[i] = v
	}
	inner, err := Convert(u.Input)
	if err != nil {
		return nil, fmt.Errorf("update input: %w", err)
	}
	q := expressions.ForEachQuantifier(expressions.InitialOf(inner))
	transforms := make([]expressions.UpdateTransform, len(u.Sets))
	for i, a := range u.Sets {
		transforms[i] = expressions.UpdateTransform{
			FieldPath: a.Column,
			NewValue:  rhs[i],
		}
	}
	return expressions.NewUpdateExpression(q, u.Target, transforms), nil
}

// lowerSimpleScalarText converts a small subset of SQL scalar text
// into a cascades.values.Value. Returns (value, true) on a successful
// match; (nil, false) on anything more complex.
//
// Supported forms:
//   - bare identifier ("id", "_x", "abc") → FieldValue
//   - signed integer literal ("42", "-7") → ConstantValue(int64)
//   - signed float literal ("1.5", "-3.14") → ConstantValue(float64)
//   - boolean literal ("TRUE" / "FALSE", any case) → ConstantValue(bool)
//   - "NULL" (any case) → NullValue
//   - single-quoted string literal ("'hello'") → ConstantValue(string)
//
// String literal handling is deliberately minimal — no apostrophe-
// escape ('it”s') and no escape characters. Callers needing those
// forms must wait for the proper text→Value parser. Whitespace inside
// the input is rejected (we don't trim).
func lowerSimpleScalarText(s string) (values.Value, bool) {
	if s == "" {
		return nil, false
	}
	// Reserved-word literals MUST be checked BEFORE the bare-column
	// path — "TRUE" / "FALSE" / "NULL" are valid SQL identifiers
	// shape-wise but the SQL semantics treat them as keywords.
	if eqAsciiFold(s, "NULL") {
		return &values.NullValue{Typ: values.TypeUnknown}, true
	}
	if eqAsciiFold(s, "TRUE") {
		return &values.ConstantValue{Value: true, Typ: values.TypeUnknown}, true
	}
	if eqAsciiFold(s, "FALSE") {
		return &values.ConstantValue{Value: false, Typ: values.TypeUnknown}, true
	}
	if isBareColumn(s) {
		return &values.FieldValue{Field: s, Typ: values.UnknownType}, true
	}
	if isDottedRef(s) {
		return &values.FieldValue{Field: s, Typ: values.UnknownType}, true
	}
	// Single-quoted string literal: 'hello'
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		body := s[1 : len(s)-1]
		// Reject any apostrophe inside the body — we don't handle
		// '' escapes here.
		for _, r := range body {
			if r == '\'' {
				return nil, false
			}
		}
		return &values.ConstantValue{Value: body, Typ: values.TypeUnknown}, true
	}
	// Numeric literal: optional leading sign, digits, optional . digits
	if v, ok := tryParseInt64(s); ok {
		return &values.ConstantValue{Value: v, Typ: values.TypeUnknown}, true
	}
	if v, ok := tryParseFloat64(s); ok {
		return &values.ConstantValue{Value: v, Typ: values.TypeUnknown}, true
	}
	if v, ok := tryParseArithmetic(s); ok {
		return v, true
	}
	return nil, false
}

// tryParseArithmetic parses binary arithmetic expressions with correct
// precedence: + - (low), * / % (high). Supports parenthesized sub-
// expressions. Returns (ArithmeticValue, true) on success.
func tryParseArithmetic(s string) (values.Value, bool) {
	return parseAdditive(s)
}

func parseAdditive(s string) (values.Value, bool) {
	idx, op := findBinaryOp(s, []byte{'+', '-'})
	if idx < 0 {
		return parseMultiplicative(s)
	}
	lhs, ok := parseAdditive(s[:idx])
	if !ok {
		return nil, false
	}
	rhs, ok := parseMultiplicative(strings.TrimSpace(s[idx+1:]))
	if !ok {
		return nil, false
	}
	var aop values.ArithmeticOp
	if op == '+' {
		aop = values.OpAdd
	} else {
		aop = values.OpSub
	}
	return &values.ArithmeticValue{Op: aop, Left: lhs, Right: rhs}, true
}

func parseMultiplicative(s string) (values.Value, bool) {
	idx, op := findBinaryOp(s, []byte{'*', '/', '%'})
	if idx < 0 {
		return parseAtom(s)
	}
	lhs, ok := parseMultiplicative(s[:idx])
	if !ok {
		return nil, false
	}
	rhs, ok := parseAtom(strings.TrimSpace(s[idx+1:]))
	if !ok {
		return nil, false
	}
	var aop values.ArithmeticOp
	switch op {
	case '*':
		aop = values.OpMul
	case '/':
		aop = values.OpDiv
	case '%':
		aop = values.OpMod
	}
	return &values.ArithmeticValue{Op: aop, Left: lhs, Right: rhs}, true
}

func parseAtom(s string) (values.Value, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, false
	}
	if v, ok := tryParseFunctionCall(s); ok {
		return v, true
	}
	if s[0] == '(' && isBalancedParens(s) {
		return tryParseArithmetic(s[1 : len(s)-1])
	}
	return lowerAtomicScalar(s)
}

// tryParseFunctionCall detects IDENT(arg1, arg2, ...) and returns a
// ScalarFunctionValue. Arguments are recursively parsed as full scalar
// expressions (supporting arithmetic, nested function calls, etc.).
func tryParseFunctionCall(s string) (values.Value, bool) {
	parenIdx := strings.IndexByte(s, '(')
	if parenIdx < 1 || s[len(s)-1] != ')' {
		return nil, false
	}
	name := s[:parenIdx]
	if !isBareColumn(name) {
		return nil, false
	}
	inner := s[parenIdx+1 : len(s)-1]
	args, ok := splitFunctionArgs(inner)
	if !ok {
		return nil, false
	}
	parsedArgs := make([]values.Value, 0, len(args))
	for _, arg := range args {
		v, ok := tryParseArithmetic(strings.TrimSpace(arg))
		if !ok {
			return nil, false
		}
		parsedArgs = append(parsedArgs, v)
	}
	return values.NewScalarFunctionValue(name, values.TypeUnknown, parsedArgs...), true
}

// splitFunctionArgs splits a comma-separated argument list respecting
// parentheses and quoted strings. Returns the arg slices and true on
// success. An empty inner string yields a zero-length slice (zero-arg fn).
func splitFunctionArgs(s string) ([]string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, true
	}
	var args []string
	depth := 0
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return nil, false
			}
		case '\'':
			i++
			for i < len(s) && s[i] != '\'' {
				i++
			}
		case ',':
			if depth == 0 {
				args = append(args, s[start:i])
				start = i + 1
			}
		}
	}
	if depth != 0 {
		return nil, false
	}
	args = append(args, s[start:])
	return args, true
}

// lowerAtomicScalar is lowerSimpleScalarText minus arithmetic recursion.
func lowerAtomicScalar(s string) (values.Value, bool) {
	if s == "" {
		return nil, false
	}
	if eqAsciiFold(s, "NULL") {
		return &values.NullValue{Typ: values.TypeUnknown}, true
	}
	if eqAsciiFold(s, "TRUE") {
		return &values.ConstantValue{Value: true, Typ: values.TypeUnknown}, true
	}
	if eqAsciiFold(s, "FALSE") {
		return &values.ConstantValue{Value: false, Typ: values.TypeUnknown}, true
	}
	if isBareColumn(s) {
		return &values.FieldValue{Field: s, Typ: values.UnknownType}, true
	}
	if isDottedRef(s) {
		return &values.FieldValue{Field: s, Typ: values.UnknownType}, true
	}
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		body := s[1 : len(s)-1]
		for _, r := range body {
			if r == '\'' {
				return nil, false
			}
		}
		return &values.ConstantValue{Value: body, Typ: values.TypeUnknown}, true
	}
	if v, ok := tryParseInt64(s); ok {
		return &values.ConstantValue{Value: v, Typ: values.TypeUnknown}, true
	}
	if v, ok := tryParseFloat64(s); ok {
		return &values.ConstantValue{Value: v, Typ: values.TypeUnknown}, true
	}
	return nil, false
}

// findBinaryOp finds the LAST occurrence (right-most for left-associativity)
// of any of the given operator bytes at paren-depth 0. Returns the index
// and the operator byte, or (-1, 0) if not found.
func findBinaryOp(s string, ops []byte) (int, byte) {
	depth := 0
	bestIdx := -1
	var bestOp byte
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case '\'':
			i++
			for i < len(s) && s[i] != '\'' {
				i++
			}
		default:
			if depth == 0 {
				for _, op := range ops {
					if s[i] == op {
						bestIdx = i
						bestOp = op
					}
				}
			}
		}
	}
	if bestIdx <= 0 || bestIdx >= len(s)-1 {
		return -1, 0
	}
	return bestIdx, bestOp
}

// eqAsciiFold compares two strings case-insensitively using only
// ASCII folding. Avoids the strings.EqualFold cost for the simple
// keyword cases here — keeps lowerSimpleScalarText branch-free
// in the hot path.
func eqAsciiFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// indexFoldASCII finds the first occurrence of needle (which MUST be
// all-ASCII) in haystack, comparing case-insensitively for ASCII letters.
// Safe on arbitrary byte sequences — no ToUpper length-change hazard.
func indexFoldASCII(haystack, needle string) int {
	nLen := len(needle)
	if nLen == 0 {
		return 0
	}
	if len(haystack) < nLen {
		return -1
	}
	for i := 0; i <= len(haystack)-nLen; i++ {
		match := true
		for j := 0; j < nLen; j++ {
			ha, nb := haystack[i+j], needle[j]
			if ha >= 'A' && ha <= 'Z' {
				ha += 'a' - 'A'
			}
			if nb >= 'A' && nb <= 'Z' {
				nb += 'a' - 'A'
			}
			if ha != nb {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// hasSuffixFoldASCII checks if s ends with suffix (ASCII case-insensitive).
func hasSuffixFoldASCII(s, suffix string) bool {
	if len(s) < len(suffix) {
		return false
	}
	return eqAsciiFold(s[len(s)-len(suffix):], suffix)
}

// tryParseInt64 returns (n, true) if s is a signed integer literal
// matching the regex `^[+-]?\d+$`, else (0, false). Implemented
// without strconv to avoid pulling that import + to allow tighter
// validation (no leading zeros except for "0" itself).
func tryParseInt64(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	i := 0
	negative := false
	if s[0] == '+' || s[0] == '-' {
		negative = s[0] == '-'
		i++
		if i == len(s) {
			return 0, false // bare sign
		}
	}
	var n int64
	for ; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		// Overflow check: if multiply-by-10 would overflow, bail.
		if n > (1 << 62) {
			return 0, false
		}
		n = n*10 + int64(c-'0')
		if n < 0 { // wrap-around overflow
			return 0, false
		}
	}
	if negative {
		n = -n
	}
	return n, true
}

// tryParseFloat64 returns (f, true) if s is a simple decimal literal
// matching `^[+-]?\d+\.\d+$` (must contain a dot, must have digits on
// both sides). Avoids exponents (`1e10`) — those need full SQL
// numeric-literal handling for cross-engine alignment with
// fdb-relational's strict-uppercase-E rule.
func tryParseFloat64(s string) (float64, bool) {
	if s == "" {
		return 0, false
	}
	i := 0
	negative := false
	if s[0] == '+' || s[0] == '-' {
		negative = s[0] == '-'
		i++
	}
	dotIdx := -1
	digitsBefore := 0
	digitsAfter := 0
	for ; i < len(s); i++ {
		c := s[i]
		if c == '.' {
			if dotIdx >= 0 {
				return 0, false // two dots
			}
			dotIdx = i
			continue
		}
		if c < '0' || c > '9' {
			return 0, false
		}
		if dotIdx < 0 {
			digitsBefore++
		} else {
			digitsAfter++
		}
	}
	if dotIdx < 0 || digitsBefore == 0 || digitsAfter == 0 {
		return 0, false
	}
	// Reuse tryParseInt64 on the integer parts, then scale.
	intPart, ok := tryParseInt64(s[:dotIdx])
	if !ok {
		return 0, false
	}
	// Strip any sign before parsing the fractional part — we already
	// captured negativity above.
	fracStart := dotIdx + 1
	fracPart, ok := tryParseInt64(s[fracStart:])
	if !ok {
		return 0, false
	}
	scale := 1.0
	for j := 0; j < digitsAfter; j++ {
		scale *= 10.0
	}
	intF := float64(intPart)
	if negative && intF == 0 {
		// "-0.5" — intPart=0, sign is in `negative`.
		intF = 0
	}
	frac := float64(fracPart) / scale
	if negative && intPart == 0 {
		return -frac, true
	}
	if intF < 0 {
		return intF - frac, true
	}
	return intF + frac, true
}

// isBareColumn reports whether s is a SQL identifier with no
// punctuation — letters/digits/underscore only, starting with a
// letter or underscore. The sql parser preserves casing, so we
// don't normalise here. Empty string is rejected.
// isBalancedParens returns true if s starts with '(' and the matching
// ')' is at the very end (i.e., the outer parens wrap the entire string).
func isBalancedParens(s string) bool {
	if len(s) < 2 || s[0] != '(' || s[len(s)-1] != ')' {
		return false
	}
	depth := 0
	for i := 0; i < len(s)-1; i++ {
		if s[i] == '(' {
			depth++
		} else if s[i] == ')' {
			depth--
		}
		if depth == 0 {
			return false
		}
	}
	return depth == 1
}

func isBareColumn(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		isLetter := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_'
		isDigit := r >= '0' && r <= '9'
		if i == 0 && !isLetter {
			return false
		}
		if !isLetter && !isDigit {
			return false
		}
	}
	return true
}

// isDottedRef matches "ident.ident" or "ident.ident.ident" — qualified
// column references like table.column or schema.table.column.
func isDottedRef(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return false
	}
	for _, p := range parts {
		if !isBareColumn(p) {
			return false
		}
	}
	return true
}

// convertAggregate builds a GroupByExpression from a LogicalAggregate.
// GroupKeys are lowered via lowerSimpleScalarText (bare column names).
// Aggregates are parsed from the "FUNC(col)" text form — only the
// basic forms COUNT(*), COUNT(col), SUM(col), MIN(col), MAX(col),
// AVG(col) are supported.
func convertAggregate(a *logical.LogicalAggregate) (expressions.RelationalExpression, error) {
	inner, err := Convert(a.Input)
	if err != nil {
		return nil, fmt.Errorf("aggregate input: %w", err)
	}
	q := expressions.ForEachQuantifier(expressions.InitialOf(inner))

	groupingKeys := make([]values.Value, 0, len(a.GroupKeys))
	for _, gk := range a.GroupKeys {
		v, ok := lowerSimpleScalarText(gk)
		if !ok {
			return nil, fmt.Errorf("%w: LogicalAggregate grouping key %q cannot be lowered", ErrUnsupported, gk)
		}
		groupingKeys = append(groupingKeys, v)
	}

	aggSpecs := make([]expressions.AggregateSpec, 0, len(a.Aggregates))
	for _, aggText := range a.Aggregates {
		spec, err := parseAggregateText(aggText)
		if err != nil {
			return nil, err
		}
		aggSpecs = append(aggSpecs, spec)
	}

	return expressions.NewGroupByExpression(groupingKeys, aggSpecs, q), nil
}

// convertLimit builds a LogicalLimitExpression for the recursively-
// converted child.
func convertLimit(l *logical.LogicalLimit) (expressions.RelationalExpression, error) {
	inner, err := Convert(l.Input)
	if err != nil {
		return nil, fmt.Errorf("limit input: %w", err)
	}
	q := expressions.ForEachQuantifier(expressions.InitialOf(inner))
	return expressions.NewLogicalLimitExpression(l.Limit, l.Offset, q), nil
}

// convertJoin builds a SelectExpression from a LogicalJoin.
// Supports: CROSS JOIN (no predicate), INNER/LEFT/RIGHT JOIN with
// structured OnPredicate or simple text ON (col = literal, col = col).
// RIGHT JOIN is normalised to LEFT JOIN by swapping branches.
func convertJoin(j *logical.LogicalJoin) (expressions.RelationalExpression, error) {
	// Normalise RIGHT → LEFT by swapping branches.
	left := j.Left
	right := j.Right
	kind := j.Kind
	if kind == logical.JoinRight {
		left, right = right, left
		kind = logical.JoinLeft
	}

	if kind != logical.JoinInner && j.OnPredicate == nil && j.OnText == "" {
		return nil, fmt.Errorf("%w: LogicalJoin kind %v without predicate", ErrUnsupported, kind)
	}

	leftExpr, err := Convert(left)
	if err != nil {
		return nil, fmt.Errorf("join left: %w", err)
	}
	rightExpr, err := Convert(right)
	if err != nil {
		return nil, fmt.Errorf("join right: %w", err)
	}

	qL := expressions.ForEachQuantifier(expressions.InitialOf(leftExpr))
	qR := expressions.ForEachQuantifier(expressions.InitialOf(rightExpr))

	var preds []predicates.QueryPredicate
	if j.OnPredicate != nil {
		p, ok := j.OnPredicate.(predicates.QueryPredicate)
		if !ok {
			return nil, fmt.Errorf("%w: LogicalJoin OnPredicate is not a QueryPredicate (%T)", ErrUnsupported, j.OnPredicate)
		}
		preds = []predicates.QueryPredicate{p}
	} else if j.OnText != "" {
		parsed, err := parsePredicateText(j.OnText)
		if err != nil {
			return nil, fmt.Errorf("join ON: %w", err)
		}
		preds = parsed
	}

	var joinType expressions.JoinType
	switch kind {
	case logical.JoinLeft:
		joinType = expressions.JoinLeftOuter
	default:
		joinType = expressions.JoinInner
	}

	rv := values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
	return expressions.NewSelectExpressionWithJoinType(rv, []expressions.Quantifier{qL, qR}, preds, nil, joinType), nil
}

// parsePredicateText parses a full predicate expression respecting SQL
// precedence: OR has lower precedence than AND. Returns a slice of
// predicates — for pure AND chains the slice has one entry per conjunct
// (matching the LogicalFilterExpression's implicit AND semantics); for
// expressions containing OR, returns a single OrPredicate in the slice.
func parsePredicateText(s string) ([]predicates.QueryPredicate, error) {
	orBranches := splitOnOR(s)
	if len(orBranches) == 1 {
		return parseAndChain(orBranches[0], s)
	}
	var ors []predicates.QueryPredicate
	for _, branch := range orBranches {
		preds, err := parseAndChain(branch, s)
		if err != nil {
			return nil, err
		}
		if len(preds) == 1 {
			ors = append(ors, preds[0])
		} else {
			ors = append(ors, predicates.NewAnd(preds...))
		}
	}
	return []predicates.QueryPredicate{predicates.NewOr(ors...)}, nil
}

func parseAndChain(s, orig string) ([]predicates.QueryPredicate, error) {
	if p, ok := parseSingleComparison(s); ok {
		return []predicates.QueryPredicate{p}, nil
	}
	parts := splitOnAND(s)
	if len(parts) == 1 {
		return nil, fmt.Errorf("%w: LogicalFilter predicate %q cannot be lowered", ErrUnsupported, orig)
	}
	var preds []predicates.QueryPredicate
	for _, part := range parts {
		p, ok := parseSingleComparison(part)
		if !ok {
			return nil, fmt.Errorf("%w: LogicalFilter predicate %q cannot be lowered", ErrUnsupported, orig)
		}
		preds = append(preds, p)
	}
	return preds, nil
}

// splitOnOR splits a predicate string on " OR " (case-insensitive),
// respecting parentheses — keywords inside (...) are not split on.
func splitOnOR(s string) []string {
	return splitOnKeyword(s, " OR ", 4)
}

// splitOnKeyword splits s on the given keyword (must be case-insensitive
// ASCII), skipping occurrences inside parentheses. kwLen is len(keyword).
func splitOnKeyword(s, keyword string, kwLen int) []string {
	var parts []string
	depth := 0
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 && i+kwLen <= len(s) {
				if eqAsciiFold(s[i:i+kwLen], keyword) {
					parts = append(parts, strings.TrimSpace(s[start:i]))
					i += kwLen - 1
					start = i + 1
				}
			}
		}
	}
	parts = append(parts, strings.TrimSpace(s[start:]))
	return parts
}

// tryParseSimpleComparison parses "lhs op rhs" into a ComparisonPredicate.
// Supports: = != <> < > <= >= with bare column or simple literal operands.
// For AND-chained predicates in filters, use splitOnAND + parseSingleComparison.
func tryParseSimpleComparison(s string) (predicates.QueryPredicate, bool) {
	return parseSingleComparison(s)
}

func parseSingleComparison(s string) (predicates.QueryPredicate, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, false
	}
	if s[0] == '(' && isBalancedParens(s) {
		inner := strings.TrimSpace(s[1 : len(s)-1])
		preds, err := parsePredicateText(inner)
		if err != nil {
			return nil, false
		}
		if len(preds) == 1 {
			return preds[0], true
		}
		return predicates.NewAnd(preds...), true
	}
	if len(s) > 4 && eqAsciiFold(s[:4], "NOT ") {
		rest := strings.TrimSpace(s[4:])
		if p, ok := parseSingleComparison(rest); ok {
			return predicates.NewNot(p), true
		}
	}
	if p, ok := tryParseIsNull(s); ok {
		return p, true
	}
	if p, ok := tryParseBetween(s); ok {
		return p, true
	}
	if p, ok := tryParseIn(s); ok {
		return p, true
	}
	if p, ok := tryParseLike(s); ok {
		return p, true
	}
	if p, ok := tryParseStartsWith(s); ok {
		return p, true
	}
	if p, ok := tryParseDistinctFrom(s); ok {
		return p, true
	}
	lhs, op, rhs, ok := splitComparison(s)
	if !ok {
		return nil, false
	}
	lhsVal, lhsOk := lowerSimpleScalarText(lhs)
	rhsVal, rhsOk := lowerSimpleScalarText(rhs)
	if !lhsOk || !rhsOk {
		return nil, false
	}
	compOp := textToCompOp(op)
	if compOp < 0 {
		return nil, false
	}
	return predicates.NewComparisonPredicate(
		lhsVal,
		predicates.Comparison{Type: compOp, Operand: rhsVal},
	), true
}

// splitOnAND splits a predicate string on " AND " (case-insensitive),
// respecting parentheses — keywords inside (...) are not split on.
func splitOnAND(s string) []string {
	return splitOnKeyword(s, " AND ", 5)
}

// splitComparison splits "lhs op rhs" into parts. Returns the
// trimmed LHS, the operator string, the trimmed RHS, and ok.
func splitComparison(s string) (string, string, string, bool) {
	ops := []string{"!=", "<>", "<=", ">=", "=", "<", ">"}
	for _, op := range ops {
		idx := strings.Index(s, op)
		if idx < 0 {
			continue
		}
		lhs := strings.TrimSpace(s[:idx])
		rhs := strings.TrimSpace(s[idx+len(op):])
		if lhs == "" || rhs == "" {
			continue
		}
		return lhs, op, rhs, true
	}
	return "", "", "", false
}

// tryParseBetween handles "col BETWEEN low AND high" → col >= low AND col <= high.
func tryParseBetween(s string) (predicates.QueryPredicate, bool) {
	betIdx := indexFoldASCII(s, " BETWEEN ")
	if betIdx < 0 {
		return nil, false
	}
	col := strings.TrimSpace(s[:betIdx])
	rest := s[betIdx+len(" BETWEEN "):]
	andIdx := indexFoldASCII(rest, " AND ")
	if andIdx < 0 {
		return nil, false
	}
	low := strings.TrimSpace(rest[:andIdx])
	high := strings.TrimSpace(rest[andIdx+5:])

	colVal, colOk := lowerSimpleScalarText(col)
	lowVal, lowOk := lowerSimpleScalarText(low)
	highVal, highOk := lowerSimpleScalarText(high)
	if !colOk || !lowOk || !highOk {
		return nil, false
	}

	geq := predicates.NewComparisonPredicate(colVal, predicates.Comparison{Type: predicates.ComparisonGreaterThanEq, Operand: lowVal})
	leq := predicates.NewComparisonPredicate(colVal, predicates.Comparison{Type: predicates.ComparisonLessThanOrEq, Operand: highVal})
	return predicates.NewAnd(geq, leq), true
}

// tryParseIsNull handles "col IS NULL" and "col IS NOT NULL" patterns.
func tryParseIsNull(s string) (predicates.QueryPredicate, bool) {
	if hasSuffixFoldASCII(s, " IS NOT NULL") {
		col := strings.TrimSpace(s[:len(s)-len(" IS NOT NULL")])
		v, ok := lowerSimpleScalarText(col)
		if !ok {
			return nil, false
		}
		return predicates.NewComparisonPredicate(
			v, predicates.Comparison{Type: predicates.ComparisonIsNotNull},
		), true
	}
	if hasSuffixFoldASCII(s, " IS NULL") {
		col := strings.TrimSpace(s[:len(s)-len(" IS NULL")])
		v, ok := lowerSimpleScalarText(col)
		if !ok {
			return nil, false
		}
		return predicates.NewComparisonPredicate(
			v, predicates.Comparison{Type: predicates.ComparisonIsNull},
		), true
	}
	return nil, false
}

// tryParseIn handles "col IN (v1, v2, ...)" and "col NOT IN (v1, v2, ...)".
func tryParseIn(s string) (predicates.QueryPredicate, bool) {
	var col, listStr string
	var negate bool

	if idx := indexFoldASCII(s, " NOT IN "); idx >= 0 {
		col = strings.TrimSpace(s[:idx])
		listStr = strings.TrimSpace(s[idx+len(" NOT IN "):])
		negate = true
	} else if idx := indexFoldASCII(s, " IN "); idx >= 0 {
		col = strings.TrimSpace(s[:idx])
		listStr = strings.TrimSpace(s[idx+len(" IN "):])
	} else {
		return nil, false
	}

	if len(listStr) < 2 || listStr[0] != '(' || listStr[len(listStr)-1] != ')' {
		return nil, false
	}
	inner := listStr[1 : len(listStr)-1]

	colVal, ok := lowerSimpleScalarText(col)
	if !ok {
		return nil, false
	}

	elements := splitInList(inner)
	if len(elements) == 0 {
		return nil, false
	}

	list := make([]any, 0, len(elements))
	for _, elem := range elements {
		v, ok := lowerSimpleScalarText(elem)
		if !ok {
			return nil, false
		}
		lit, ok := values.EvaluateConstant(v)
		if !ok {
			return nil, false
		}
		list = append(list, lit)
	}

	pred := predicates.NewComparisonPredicate(colVal, predicates.Comparison{
		Type:    predicates.ComparisonIn,
		Operand: &values.ConstantValue{Value: list, Typ: values.TypeUnknown},
	})
	if negate {
		return predicates.NewNot(pred), true
	}
	return pred, true
}

// splitInList splits a comma-separated list respecting single-quoted strings.
func splitInList(s string) []string {
	var parts []string
	var buf strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch == '\'' {
			inQuote = !inQuote
			buf.WriteByte(ch)
		} else if ch == ',' && !inQuote {
			part := strings.TrimSpace(buf.String())
			if part != "" {
				parts = append(parts, part)
			}
			buf.Reset()
		} else {
			buf.WriteByte(ch)
		}
	}
	if rest := strings.TrimSpace(buf.String()); rest != "" {
		parts = append(parts, rest)
	}
	return parts
}

// tryParseLike handles "col LIKE 'pattern'" and "col NOT LIKE 'pattern'"
// with optional ESCAPE clause.
func tryParseLike(s string) (predicates.QueryPredicate, bool) {
	var col, rest string
	var negate bool

	if idx := indexFoldASCII(s, " NOT LIKE "); idx >= 0 {
		col = strings.TrimSpace(s[:idx])
		rest = strings.TrimSpace(s[idx+len(" NOT LIKE "):])
		negate = true
	} else if idx := indexFoldASCII(s, " LIKE "); idx >= 0 {
		col = strings.TrimSpace(s[:idx])
		rest = strings.TrimSpace(s[idx+len(" LIKE "):])
	} else {
		return nil, false
	}

	colVal, ok := lowerSimpleScalarText(col)
	if !ok {
		return nil, false
	}

	var pattern string
	var escape rune
	if escIdx := indexFoldASCII(rest, " ESCAPE "); escIdx >= 0 {
		patternPart := strings.TrimSpace(rest[:escIdx])
		escapePart := strings.TrimSpace(rest[escIdx+len(" ESCAPE "):])
		if len(patternPart) < 2 || patternPart[0] != '\'' || patternPart[len(patternPart)-1] != '\'' {
			return nil, false
		}
		pattern = patternPart[1 : len(patternPart)-1]
		if len(escapePart) < 2 || escapePart[0] != '\'' || escapePart[len(escapePart)-1] != '\'' {
			return nil, false
		}
		escBody := escapePart[1 : len(escapePart)-1]
		runes := []rune(escBody)
		if len(runes) != 1 {
			return nil, false
		}
		escape = runes[0]
	} else {
		if len(rest) < 2 || rest[0] != '\'' || rest[len(rest)-1] != '\'' {
			return nil, false
		}
		pattern = rest[1 : len(rest)-1]
	}

	patternVal := &values.ConstantValue{Value: pattern, Typ: values.TypeUnknown}
	pred := predicates.NewComparisonPredicate(colVal, predicates.Comparison{
		Type:    predicates.ComparisonLike,
		Operand: patternVal,
		Escape:  escape,
	})
	if negate {
		return predicates.NewNot(pred), true
	}
	return pred, true
}

// tryParseStartsWith handles "STARTS_WITH(col, 'prefix')" function-call syntax.
func tryParseStartsWith(s string) (predicates.QueryPredicate, bool) {
	if len(s) < len("STARTS_WITH(") {
		return nil, false
	}
	if !eqAsciiFold(s[:len("STARTS_WITH(")], "STARTS_WITH(") {
		return nil, false
	}
	if s[len(s)-1] != ')' {
		return nil, false
	}
	inner := s[len("STARTS_WITH(") : len(s)-1]
	parts := splitInList(inner)
	if len(parts) != 2 {
		return nil, false
	}
	colVal, ok := lowerSimpleScalarText(parts[0])
	if !ok {
		return nil, false
	}
	prefixVal, ok := lowerSimpleScalarText(parts[1])
	if !ok {
		return nil, false
	}
	return predicates.NewComparisonPredicate(colVal, predicates.Comparison{
		Type:    predicates.ComparisonStartsWith,
		Operand: prefixVal,
	}), true
}

// tryParseDistinctFrom handles "col IS DISTINCT FROM val" and
// "col IS NOT DISTINCT FROM val" (null-safe inequality/equality).
func tryParseDistinctFrom(s string) (predicates.QueryPredicate, bool) {
	if idx := indexFoldASCII(s, " IS NOT DISTINCT FROM "); idx >= 0 {
		col := strings.TrimSpace(s[:idx])
		val := strings.TrimSpace(s[idx+len(" IS NOT DISTINCT FROM "):])
		colVal, ok := lowerSimpleScalarText(col)
		if !ok {
			return nil, false
		}
		rhsVal, ok := lowerSimpleScalarText(val)
		if !ok {
			return nil, false
		}
		return predicates.NewComparisonPredicate(colVal, predicates.Comparison{
			Type:    predicates.ComparisonNotDistinctFrom,
			Operand: rhsVal,
		}), true
	}
	if idx := indexFoldASCII(s, " IS DISTINCT FROM "); idx >= 0 {
		col := strings.TrimSpace(s[:idx])
		val := strings.TrimSpace(s[idx+len(" IS DISTINCT FROM "):])
		colVal, ok := lowerSimpleScalarText(col)
		if !ok {
			return nil, false
		}
		rhsVal, ok := lowerSimpleScalarText(val)
		if !ok {
			return nil, false
		}
		return predicates.NewComparisonPredicate(colVal, predicates.Comparison{
			Type:    predicates.ComparisonIsDistinctFrom,
			Operand: rhsVal,
		}), true
	}
	return nil, false
}

func textToCompOp(op string) predicates.ComparisonType {
	switch op {
	case "=":
		return predicates.ComparisonEquals
	case "!=", "<>":
		return predicates.ComparisonNotEquals
	case "<":
		return predicates.ComparisonLessThan
	case "<=":
		return predicates.ComparisonLessThanOrEq
	case ">":
		return predicates.ComparisonGreaterThan
	case ">=":
		return predicates.ComparisonGreaterThanEq
	default:
		return -1
	}
}

// parseAggregateText parses "FUNC(operand)" aggregate text into an
// AggregateSpec. Supported forms: COUNT(*), COUNT(col), SUM(col),
// MIN(col), MAX(col), AVG(col).
func parseAggregateText(s string) (expressions.AggregateSpec, error) {
	lparen := strings.IndexByte(s, '(')
	rparen := strings.LastIndexByte(s, ')')
	if lparen < 1 || rparen <= lparen {
		return expressions.AggregateSpec{}, fmt.Errorf("%w: cannot parse aggregate %q", ErrUnsupported, s)
	}
	funcName := strings.TrimSpace(s[:lparen])
	operandText := strings.TrimSpace(s[lparen+1 : rparen])

	fn, ok := lookupAggFunc(funcName)
	if !ok {
		return expressions.AggregateSpec{}, fmt.Errorf("%w: unknown aggregate function %q", ErrUnsupported, funcName)
	}

	var operand values.Value
	if operandText == "*" {
		operand = &values.FieldValue{Field: "*", Typ: values.UnknownType}
	} else {
		v, ok := lowerSimpleScalarText(operandText)
		if !ok {
			return expressions.AggregateSpec{}, fmt.Errorf("%w: cannot lower aggregate operand %q", ErrUnsupported, operandText)
		}
		operand = v
	}

	return expressions.AggregateSpec{Function: fn, Operand: operand}, nil
}

func lookupAggFunc(name string) (expressions.AggregateFunction, bool) {
	switch strings.ToUpper(name) {
	case "COUNT":
		return expressions.AggCount, true
	case "SUM":
		return expressions.AggSum, true
	case "MIN":
		return expressions.AggMin, true
	case "MAX":
		return expressions.AggMax, true
	case "AVG":
		return expressions.AggAvg, true
	default:
		return 0, false
	}
}
