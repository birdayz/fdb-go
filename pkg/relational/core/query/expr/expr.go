// Package expr is the parse-tree → values.Value resolver. It
// bridges the two main Phase 3 seam packages:
//
//   - pkg/relational/core/query/semantic — identifier resolution,
//     catalog lookup, scope chain.
//   - pkg/recordlayer/query/plan/cascades — Value / Predicate
//     hierarchy.
//
// Neither semantic nor cascades depends on the other. expr sits
// above both and owns the logic that turns a parsed SQL expression
// into a typed Value tree with every identifier resolved against a
// Scope.
//
// # API
//
// Two layers:
//
//   - Resolver.Resolve* primitives — programmatic construction of
//     Value / Predicate trees from already-walked parts (caller
//     supplies each argument). Useful for tests and for synthetic
//     expressions the analyzer constructs from other inputs.
//   - Resolver.WalkExpression / WalkPredicate — parse-tree driver.
//     Dispatches ANTLR IExpressionContext variants to the right
//     Resolve* method, recursing into child contexts.
//
// Call WalkExpression for Value-returning expressions (SELECT list
// items, arithmetic operands). Call WalkPredicate for QueryPredicate-
// returning expressions (WHERE / HAVING clauses).
//
// # Handled shapes (swingshift-47 seed)
//
//   - Columns: bare (`col`) and qualified (`t.col`).
//   - Constants: integer, string, NULL. Float pending (see
//     ResolveConstant).
//   - Arithmetic: +, -, *, /.
//   - Comparisons: =, <>, !=, <, <=, >, >=, IS [NOT] DISTINCT FROM.
//   - Logical: AND / OR / NOT (with left-deep chain flattening).
//   - Unary predicates: IS NULL / IS NOT NULL.
//   - BETWEEN (desugars to AND(>=, <=)).
//   - IN with explicit literal list.
//   - LIKE (no ESCAPE).
//   - Aggregate function calls: COUNT / COUNT(*) / SUM / MIN / MAX / AVG.
//   - Parenthesised expressions (single-element RecordConstructor
//     unwrap).
//
// # Not handled (returns UnsupportedExpressionShapeError)
//
//   - Scalar function calls (UPPER, LOWER, LENGTH, …).
//   - LIKE with ESCAPE.
//   - IN with subquery / parameter / single-column.
//   - CAST to FLOAT / DOUBLE / BYTES / UUID / VECTOR (seed
//     ValueType only covers INT / STRING / BOOL — the full Type
//     hierarchy port lands in Phase 4.0).
//   - Multi-element or named-field record constructors.
//
// CAST and CONVERT to INT / BIGINT / STRING / BOOLEAN are wired
// via DataTypeFunctionCall; XOR desugars to
// (a OR b) AND NOT (a AND b); IS [NOT] TRUE / FALSE desugars via
// the 2VL `(x IS NOT NULL) AND (x = literal)` shape.
//
// Callers catching UnsupportedExpressionShapeError can fall back to
// the existing logical-builder path, which handles the full grammar
// surface at a less-structured level.
package expr

import (
	"fmt"
	"strings"
	"sync"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/semantic"
)

// SubqueryPlanner is the callback interface for building subquery
// plans from the expr walker. The embedded package provides the
// implementation that calls buildLogicalPlanForQuery and stores the
// result. The Resolver itself is agnostic to how the plan is built —
// it only needs a fresh alias and the plan reference.
//
// BuildExists receives the inner Query context from an
// ExistsExpressionAtomContext and returns:
//   - alias: a unique CorrelationIdentifier for the existential quantifier
//   - err: non-nil when the inner query cannot be planned
//
// BuildScalar receives the inner Query context from a
// SubqueryExpressionAtomContext (scalar subquery) and returns:
//   - alias: a unique CorrelationIdentifier for the scalar subquery
//   - err: non-nil when the inner query cannot be planned
//
// The planner stores the (alias → plan) mapping externally; the
// Resolver creates an ExistentialValuePredicate or ScalarSubqueryValue
// referencing the alias.
type SubqueryPlanner interface {
	BuildExists(query antlrgen.IQueryContext) (alias values.CorrelationIdentifier, err error)
	BuildScalar(query antlrgen.IQueryContext) (alias values.CorrelationIdentifier, err error)
}

// Resolver converts parsed SQL expressions into cascades Values. It
// needs a Scope (to resolve identifiers) and an Analyzer (to run
// column-reference lookup). Stateless beyond those inputs — one
// Resolver per analyzer is fine.
//
// The FunctionCatalog is built lazily on first aggregate-function
// lookup (to keep the construction path cheap when the resolver
// never sees a function call) and reused thereafter. Callers that
// want a custom catalog (scalar extensions, overridden defaults)
// can pass one via NewWithFunctionCatalog.
// Concurrency contract: Resolver is intended for a single statement
// walk and is NOT goroutine-safe — `nextOrdinal` (the positional `?`
// counter) and the lazy `funcCat` initialization both rely on
// single-goroutine access during a walk. Callers that fan parsing
// out across goroutines should construct one Resolver per goroutine.
// `funcCatOnce` makes the lazy build defensive against
// existing concurrent test patterns that share a Resolver across
// `t.Parallel()` subtests; `nextOrdinal` would still be racy in
// that pattern but no test exercises that path.
type Resolver struct {
	analyzer    *semantic.Analyzer
	scope       *semantic.Scope
	funcCat     *semantic.FunctionCatalog
	funcCatOnce sync.Once
	// nextOrdinal counts positional `?` placeholders in the order they
	// appear within a single statement walk. Statement-scoped via the
	// Resolver lifetime — one Resolver per statement, so resetting
	// per-walk falls out of construction. Matches Go's database/sql
	// NamedValue.Ordinal (1-based). Named parameters (`?foo` / `$bar`)
	// keep their declared name and consume no ordinal slot.
	nextOrdinal int
	// subqueryPlanner is the callback for building EXISTS subquery
	// plans. Set via SetSubqueryPlanner by the catalog-aware builder
	// before walking WHERE predicates. nil means EXISTS subqueries
	// decline with UnsupportedExpressionShapeError.
	subqueryPlanner SubqueryPlanner
}

// SetSubqueryPlanner installs a callback that builds logical plans for
// EXISTS subqueries. Must be called before WalkPredicate if EXISTS
// support is desired. Passing nil disables EXISTS handling (the
// walker declines with UnsupportedExpressionShapeError).
func (r *Resolver) SetSubqueryPlanner(p SubqueryPlanner) {
	r.subqueryPlanner = p
}

// New constructs a Resolver bound to a scope. Nil analyzer or nil
// scope panics — the resolver has nothing to do without either.
// Function-call resolution uses the seed defaults
// (COUNT/SUM/MIN/MAX/AVG).
func New(analyzer *semantic.Analyzer, scope *semantic.Scope) *Resolver {
	return NewWithFunctionCatalog(analyzer, scope, nil)
}

// NewWithFunctionCatalog is the New variant that lets callers
// plug in a pre-built FunctionCatalog — used when scalar functions
// or user-registered aggregates need to be resolvable.
// Passing nil uses the seed defaults on first demand.
func NewWithFunctionCatalog(analyzer *semantic.Analyzer, scope *semantic.Scope, fc *semantic.FunctionCatalog) *Resolver {
	if analyzer == nil {
		panic("expr.New: analyzer is nil")
	}
	if scope == nil {
		panic("expr.New: scope is nil")
	}
	return &Resolver{analyzer: analyzer, scope: scope, funcCat: fc}
}

// functionCatalog returns the resolver's FunctionCatalog, lazily
// building the defaults on first use when none was supplied to New.
// The sync.Once guard makes the lazy build defensive — production
// usage is single-goroutine per Resolver, but several existing test
// patterns share one Resolver across `t.Parallel()` subtests.
func (r *Resolver) functionCatalog() *semantic.FunctionCatalog {
	r.funcCatOnce.Do(func() {
		if r.funcCat == nil {
			r.funcCat = semantic.NewFunctionCatalog()
			r.funcCat.RegisterDefaults()
		}
	})
	return r.funcCat
}

// ResolveIdentifier produces a cascades Value for a bare or
// qualified identifier reference. qualifier may be the zero
// Identifier for bare lookups.
//
// Currently produces a values.FieldValue for scope-resolved
// columns. Once QuantifiedObjectValue lookup lands in the logical-
// builder, this will produce a FieldValue wrapping a
// QuantifiedObjectValue to carry the source correlation.
//
// Row-key contract: FieldValue.Field is stored in the Identifier's
// case-folded form (upper-case unless the source was quoted).
// Callers feeding FieldValue.Evaluate a row map MUST key the map
// with the same case-folded names — otherwise lookup silently
// returns nil. The SQL executor's row-projection layer is
// responsible for that normalisation. Documented explicitly here
// because a subtle-lookup-fail is an easy trap when integrating.
//
// Returns the underlying semantic errors verbatim so callers can
// match via errors.As.
func (r *Resolver) ResolveIdentifier(qualifier, id semantic.Identifier) (values.Value, error) {
	col, src, err := r.analyzer.ResolveColumnRef(r.scope, qualifier, id)
	if err != nil {
		return nil, err
	}
	field := col.Id.Name()
	if src.ColumnAliasMap != nil {
		if real, ok := src.ColumnAliasMap[strings.ToUpper(field)]; ok {
			field = real
		}
	}
	needsQualification := len(r.scope.Sources()) > 1
	if !needsQualification && src.CorrelationName != "" {
		isLocal := false
		for _, localSrc := range r.scope.Sources() {
			if localSrc.CorrelationName == src.CorrelationName {
				isLocal = true
				break
			}
		}
		if !isLocal {
			needsQualification = true
		}
	}
	if src.CorrelationName != "" && needsQualification {
		corrID := values.NamedCorrelationIdentifier(src.CorrelationName)
		return values.NewFieldValue(
			values.NewQuantifiedObjectValue(corrID),
			field,
			sqlTypeToCascadesType(col.Type),
		), nil
	}
	return &values.FieldValue{
		Field: field,
		Typ:   sqlTypeToCascadesType(col.Type),
	}, nil
}

// ResolveColumnShadowingQualified resolves a column reference and, when it binds
// to a SHADOWING scope source (a lateral array unnest's AS/AT binding, RFC-142),
// returns a Value QUALIFIED to that source's correlation (FieldValue over
// QuantifiedObjectValue) — even for a BARE column where ResolveIdentifier would
// normally emit an UNqualified FieldValue.
//
// This is load-bearing for `FROM t, t.arr AS v, u` where a LATER FROM item `u`
// also has a column `v`: the unnest's element flows the merged row under BOTH the
// bare key `v` AND the qualified key `v.v`, but a subsequent join's mergeRows
// overwrites the bare `v` last-leg-wins with u.v. A bare `SELECT v` resolved to
// the unnest must therefore read the QUALIFIED `v.v` key (which mergeRows
// preserves verbatim — dotted keys are never re-prefixed), not the clobbered bare
// key. ok=false (and a nil Value) when the column does not bind to a shadowing
// source — the caller keeps its existing bare-column handling. A resolution error
// is returned verbatim (callers already validate separately, so they may ignore
// it). RFC-142.
func (r *Resolver) ResolveColumnShadowingQualified(qualifier, id semantic.Identifier) (values.Value, bool, error) {
	col, src, err := r.analyzer.ResolveColumnRef(r.scope, qualifier, id)
	if err != nil {
		return nil, false, err
	}
	if !src.Shadowing || src.CorrelationName == "" {
		return nil, false, nil
	}
	field := col.Id.Name()
	if src.ColumnAliasMap != nil {
		if real, ok := src.ColumnAliasMap[strings.ToUpper(field)]; ok {
			field = real
		}
	}
	corrID := values.NamedCorrelationIdentifier(src.CorrelationName)
	return values.NewFieldValue(
		values.NewQuantifiedObjectValue(corrID),
		field,
		sqlTypeToCascadesType(col.Type),
	), true, nil
}

// ResolveArithmetic wraps left/right Values in a cascades
// ArithmeticValue with the given operator. Used when the parser
// produces an arithmetic expression node — the analyzer resolves
// each operand recursively, then pairs them here.
//
// Operand types aren't cross-checked in the seed (both assumed
// int); real type inference replaces this when the Type hierarchy
// port lands.
func (r *Resolver) ResolveArithmetic(op values.ArithmeticOp, left, right values.Value) (values.Value, error) {
	if left == nil || right == nil {
		return nil, fmt.Errorf("expr.ResolveArithmetic: operand is nil")
	}
	return &values.ArithmeticValue{Op: op, Left: left, Right: right}, nil
}

// ResolveComparison wraps left/right Values in a cascades
// ComparisonPredicate. Mirrors the analyzer's job of lifting
// `a > b` from a parse-tree comparison node to a predicate node.
//
// Both LHS and RHS are carried as Values — non-constant RHS
// (`a = b`, `a < b + 1`, `a = CAST(col AS INT)`) composes uniformly
// with constant RHS. Plan-time folding (`5 = 5` → TRUE) happens in
// ComparisonConstantSimplifyRule when both sides are constant;
// row-context evaluation (FieldValue RHS) runs through
// ComparisonPredicate.Eval.
//
// Does NOT pre-fold even when both operands are constant. `5 = 5`
// produces a real ComparisonPredicate; the fixpoint simplifier
// folds it to TRUE via ComparisonConstantSimplifyRule. Eager
// folding here would hide foldable shapes from rule matchers that
// expect to see them.
func (r *Resolver) ResolveComparison(op predicates.ComparisonType, left, right values.Value) (predicates.QueryPredicate, error) {
	if left == nil || right == nil {
		return nil, fmt.Errorf("expr.ResolveComparison: operand is nil")
	}
	return predicates.NewComparisonPredicate(left, predicates.Comparison{
		Type: op, Operand: right,
	}), nil
}

// ResolveCast wraps v in a CastValue with the target type. Rejects
// nil child (programmer error) and Unknown target (use the direct
// Value if the target is genuinely unknown).
func (r *Resolver) ResolveCast(v values.Value, target values.Type) (values.Value, error) {
	if v == nil {
		return nil, fmt.Errorf("expr.ResolveCast: child is nil")
	}
	if target == nil || target.Code() == values.TypeCodeUnknown {
		return nil, fmt.Errorf("expr.ResolveCast: target UnknownType")
	}
	return values.NewCastValue(v, target), nil
}

// ResolveIsNull builds `v IS NULL`. Unary — Comparison.Operand is
// nil (Eval ignores it for unary types).
func (r *Resolver) ResolveIsNull(v values.Value) (predicates.QueryPredicate, error) {
	if v == nil {
		return nil, fmt.Errorf("expr.ResolveIsNull: operand is nil")
	}
	return predicates.NewComparisonPredicate(v, predicates.Comparison{Type: predicates.ComparisonIsNull}), nil
}

// ResolveIsNotNull builds `v IS NOT NULL`. Unary.
func (r *Resolver) ResolveIsNotNull(v values.Value) (predicates.QueryPredicate, error) {
	if v == nil {
		return nil, fmt.Errorf("expr.ResolveIsNotNull: operand is nil")
	}
	return predicates.NewComparisonPredicate(v, predicates.Comparison{Type: predicates.ComparisonIsNotNull}), nil
}

// ResolveLike builds `lhs LIKE pattern`. Pattern must be a plan-time
// constant string (parameter-bound patterns land with the
// parameter-Comparison design).
func (r *Resolver) ResolveLike(lhs values.Value, pattern values.Value) (predicates.QueryPredicate, error) {
	return r.ResolveLikeWithEscape(lhs, pattern, 0)
}

// ResolveLikeWithEscape is the LIKE … ESCAPE form. escape == 0 is
// equivalent to ResolveLike. Pattern must be a plan-time constant
// string. The escape rune is carried verbatim on the resulting
// Comparison.
func (r *Resolver) ResolveLikeWithEscape(lhs values.Value, pattern values.Value, escape rune) (predicates.QueryPredicate, error) {
	if lhs == nil || pattern == nil {
		return nil, fmt.Errorf("expr.ResolveLike: operand is nil")
	}
	lit, ok := values.EvaluateConstant(pattern)
	if !ok {
		return nil, fmt.Errorf("expr.ResolveLike: pattern must be a constant in the seed; got %T", pattern)
	}
	s, ok := lit.(string)
	if !ok {
		return nil, fmt.Errorf("expr.ResolveLike: pattern must be a string; got %T", lit)
	}
	return predicates.NewComparisonPredicate(lhs, predicates.Comparison{
		Type:    predicates.ComparisonLike,
		Operand: values.LiteralValue(s),
		Escape:  escape,
	}), nil
}

// ResolveStartsWith builds `lhs STARTS_WITH prefix`. Prefix must be
// a constant string.
func (r *Resolver) ResolveStartsWith(lhs values.Value, prefix values.Value) (predicates.QueryPredicate, error) {
	if lhs == nil || prefix == nil {
		return nil, fmt.Errorf("expr.ResolveStartsWith: operand is nil")
	}
	lit, ok := values.EvaluateConstant(prefix)
	if !ok {
		return nil, fmt.Errorf("expr.ResolveStartsWith: prefix must be a constant in the seed; got %T", prefix)
	}
	s, ok := lit.(string)
	if !ok {
		return nil, fmt.Errorf("expr.ResolveStartsWith: prefix must be a string; got %T", lit)
	}
	return predicates.NewComparisonPredicate(lhs, predicates.Comparison{
		Type: predicates.ComparisonStartsWith, Operand: values.LiteralValue(s),
	}), nil
}

// ResolveIn builds a ComparisonPredicate{ComparisonIn} from a left
// Value and a list of constant RHS values. Every RHS must be a
// plan-time constant (per values.EvaluateConstant); non-constant
// elements return an error so callers lift the expression-based
// IN-list to the value-space form explicitly.
//
// The RHS Operand is a []any of evaluated literals.
func (r *Resolver) ResolveIn(left values.Value, rhs []values.Value) (predicates.QueryPredicate, error) {
	if left == nil {
		return nil, fmt.Errorf("expr.ResolveIn: LHS is nil")
	}
	list := make([]any, 0, len(rhs))
	for i, v := range rhs {
		lit, ok := values.EvaluateConstant(v)
		if !ok {
			return nil, fmt.Errorf("expr.ResolveIn: element %d is not constant (%T)", i, v)
		}
		list = append(list, lit)
	}
	return predicates.NewComparisonPredicate(left, predicates.Comparison{
		Type:    predicates.ComparisonIn,
		Operand: &values.ConstantValue{Value: list, Typ: values.TypeUnknown},
	}), nil
}

// ResolveAnd combines N predicates via Kleene AND. A single
// predicate returns verbatim (no wrapping); empty list returns
// ConstantPredicate(TRUE) — the AND identity.
func (r *Resolver) ResolveAnd(preds ...predicates.QueryPredicate) predicates.QueryPredicate {
	switch len(preds) {
	case 0:
		return predicates.NewConstantPredicate(predicates.TriTrue)
	case 1:
		return preds[0]
	}
	return predicates.NewAnd(preds...)
}

// ResolveOr combines N predicates via Kleene OR. Empty list returns
// ConstantPredicate(FALSE) — the OR identity. Single predicate
// returns verbatim.
func (r *Resolver) ResolveOr(preds ...predicates.QueryPredicate) predicates.QueryPredicate {
	switch len(preds) {
	case 0:
		return predicates.NewConstantPredicate(predicates.TriFalse)
	case 1:
		return preds[0]
	}
	return predicates.NewOr(preds...)
}

// ResolveNot wraps a predicate in a Kleene NOT. Nil child returns
// ConstantPredicate(UNKNOWN) — the only sensible interpretation.
func (r *Resolver) ResolveNot(pred predicates.QueryPredicate) predicates.QueryPredicate {
	if pred == nil {
		return predicates.NewConstantPredicate(predicates.TriUnknown)
	}
	return predicates.NewNot(pred)
}

// ResolveFunctionCall dispatches a function call against the given
// catalogue. For known aggregates (COUNT/SUM/MIN/MAX/AVG) it returns
// the corresponding values.AggregateValue. Scalar function support
// comes once the scalar-function catalogue is wired in.
//
// isStar=true signals the argument was `*` (COUNT(*)) — args must be
// empty in that case.
func (r *Resolver) ResolveFunctionCall(
	funcCatalog *semantic.FunctionCatalog,
	name semantic.Identifier,
	isStar bool,
	args []values.Value,
) (values.Value, error) {
	if funcCatalog == nil {
		return nil, fmt.Errorf("expr.ResolveFunctionCall: function catalog is nil")
	}
	spec, ok := funcCatalog.Lookup(name)
	if !ok {
		return nil, &semantic.FunctionNotFoundError{Name: name}
	}
	if isStar {
		if !spec.AllowsStar {
			return nil, fmt.Errorf("expr.ResolveFunctionCall: %s does not accept *", name)
		}
		if len(args) > 0 {
			return nil, fmt.Errorf("expr.ResolveFunctionCall: star form takes no args; got %d", len(args))
		}
	} else {
		if err := spec.ValidateArity(len(args)); err != nil {
			return nil, err
		}
	}
	if spec.Kind != semantic.FunctionAggregate {
		return nil, fmt.Errorf("expr.ResolveFunctionCall: scalar function %s not supported in seed", name)
	}
	// Aggregate dispatch — seed knows the five SQL standards.
	op, ok := aggregateOpForName(spec.Name, isStar)
	if !ok {
		return nil, fmt.Errorf("expr.ResolveFunctionCall: unknown aggregate %s", spec.Name)
	}
	if op == values.AggCountStar {
		return values.NewAggregateValue(op, nil), nil
	}
	return values.NewAggregateValue(op, args[0]), nil
}

// aggregateOpForName maps a normalized aggregate function name +
// star flag to the corresponding values.AggregateOp. Not exported
// — called via ResolveFunctionCall.
func aggregateOpForName(name string, isStar bool) (values.AggregateOp, bool) {
	switch name {
	case "COUNT":
		if isStar {
			return values.AggCountStar, true
		}
		return values.AggCount, true
	case "SUM":
		return values.AggSum, true
	case "MIN":
		return values.AggMin, true
	case "MAX":
		return values.AggMax, true
	case "AVG":
		return values.AggAvg, true
	}
	return values.AggInvalid, false
}

// sqlTypeToCascadesType maps the seed's string-valued SQL type
// (from semantic.Column.Type) to a cascades values.Type. Coarse —
// the seed maps INT/STRING/BOOL/ENUM to the matching primitive
// singletons; everything else falls through to UnknownType. Real
// type inference (proper nullability + structured-type recursion)
// is future work.
//
// INTEGER is a recognized SYNONYM for INT (the standard SQL spelling —
// the metadata-derivation paths in cascades_generator / system_rows
// already emit "INTEGER", so it must not silently fall to UNKNOWN
// here). Both bridge to the legacy nullable-LONG default (matching
// Java Record Layer's int64 representation), consistent with the
// existing INT→LONG width contract.
//
// "INT NOT NULL" / "INTEGER NOT NULL" map to the NON-NULL INT singleton
// (NotNullInt) — Java's Type.primitiveType(INT, false). This is the
// planner-internal spelling for a column known to be a non-null integer
// at construction time, notably the array-unnest WITH ORDINALITY ordinal
// (RFC-142): a 1-based, never-NULL INT whose result-set metadata must
// report INT, not UNKNOWN, and which matches the translator's ordinal
// FieldValue type (values.NotNullInt). Real catalog columns never carry
// this spelling, so the legacy INT→LONG bridge above is undisturbed.
func sqlTypeToCascadesType(sqlType string) values.Type {
	switch sqlType {
	case "INT", "INTEGER":
		return values.TypeInt
	case "INT NOT NULL", "INTEGER NOT NULL":
		return values.NotNullInt
	case "STRING", "ENUM":
		return values.TypeString
	case "BOOL":
		return values.TypeBool
	case "FLOAT", "BYTES", "RECORD":
		// Fall through to Unknown rather than silently lie about
		// INT / STRING representation — a mistyped column at the
		// resolver boundary would cascade into wrong comparator
		// picks downstream.
		return values.TypeUnknown
	}
	return values.TypeUnknown
}

// ResolveConstant wraps a Go-native literal in a cascades
// ConstantValue with the appropriate type tag. Useful for inlining
// literal arguments when building a Value tree from a parsed
// expression.
//
// Returns an error when the literal's runtime type doesn't map to
// any seed ValueType — nil, int, int32, int64, float32, float64,
// string, bool are supported. Float literals carry TypeFloat;
// arithmetic over floats still goes through ArithmeticValue's
// int-only Eval (mixed-type arith returns nil per the seed
// contract — a real arithmetic-over-float requires the Type
// hierarchy port to set up coercion).
func (r *Resolver) ResolveConstant(lit any) (values.Value, error) {
	switch v := lit.(type) {
	case nil:
		return values.NewNullValue(values.TypeUnknown), nil
	case bool:
		return values.NewBooleanValue(v), nil
	case int:
		return &values.ConstantValue{Value: int64(v), Typ: values.TypeInt}, nil
	case int32:
		return &values.ConstantValue{Value: int64(v), Typ: values.TypeInt}, nil
	case int64:
		return &values.ConstantValue{Value: v, Typ: values.TypeInt}, nil
	case string:
		return &values.ConstantValue{Value: v, Typ: values.TypeString}, nil
	case float32:
		return &values.ConstantValue{Value: float64(v), Typ: values.TypeFloat}, nil
	case float64:
		return &values.ConstantValue{Value: v, Typ: values.TypeFloat}, nil
	case []byte:
		return &values.ConstantValue{Value: v, Typ: values.NullableBytes}, nil
	}
	return nil, fmt.Errorf("expr.ResolveConstant: unsupported literal type %T", lit)
}
