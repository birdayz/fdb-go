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
//
// Currently unsupported (returns ErrUnsupported):
//   - LogicalProject / LogicalSort / LogicalUpdate with arithmetic,
//     function calls, qualified column refs (`t.c`), exponent-form
//     numeric literals (`1.5E10`), or escaped string literals
//     (`'it”s'`) — all gated on text→Value parsing
//   - LogicalJoin — maps to SelectExpression with multiple
//     Quantifiers; needs predicate placement work
//   - LogicalInsert without Source (VALUES literal) — needs a
//     synthetic LogicalValues source operator
//   - LogicalValues / LogicalCTE / LogicalDDL — no equivalent
//   - LogicalFilter with PredicateText only (no QueryPredicate)
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
// converted child. Requires LogicalFilter.Predicate to be non-nil
// (the cascades QueryPredicate form); LogicalFilters built from the
// non-catalog-aware text path return ErrUnsupported.
func convertFilter(f *logical.LogicalFilter) (expressions.RelationalExpression, error) {
	if f.Predicate == nil {
		return nil, fmt.Errorf("%w: LogicalFilter without QueryPredicate (text-only path)", ErrUnsupported)
	}
	inner, err := Convert(f.Input)
	if err != nil {
		return nil, fmt.Errorf("filter input: %w", err)
	}
	q := expressions.ForEachQuantifier(expressions.InitialOf(inner))
	return expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{f.Predicate}, q,
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

// convertProject builds a LogicalProjectionExpression for the
// recursively-converted child. Each projection entry is lowered via
// lowerSimpleScalarText, which currently handles bare column names
// (→ FieldValue) and simple literal forms (integer / float / boolean
// / NULL / single-quoted string → ConstantValue / NullValue).
// Anything more (arithmetic, function call, scalar subquery, dotted
// reference) returns ErrUnsupported until a text→Value parser is
// threaded through.
//
// Aliases are dropped — the projection's column-naming is decided
// by the upstream catalog walker; the projection list captures
// only the value flow.
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
	return nil, false
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
