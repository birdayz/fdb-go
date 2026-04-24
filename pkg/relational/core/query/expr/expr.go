// Package expr is the parse-tree → cascades.Value resolver. It
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
//   - XOR.
//   - Scalar function calls.
//   - LIKE with ESCAPE.
//   - IN with subquery / parameter / single-column.
//   - CAST.
//   - IS TRUE / IS FALSE.
//   - Multi-element or named-field record constructors.
//
// Callers catching UnsupportedExpressionShapeError can fall back to
// the existing logical-builder path, which handles the full grammar
// surface at a less-structured level.
package expr

import (
	"fmt"

	cascades "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/semantic"
)

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
type Resolver struct {
	analyzer *semantic.Analyzer
	scope    *semantic.Scope
	funcCat  *semantic.FunctionCatalog
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
func (r *Resolver) functionCatalog() *semantic.FunctionCatalog {
	if r.funcCat == nil {
		r.funcCat = semantic.NewFunctionCatalog()
		r.funcCat.RegisterDefaults()
	}
	return r.funcCat
}

// ResolveIdentifier produces a cascades Value for a bare or
// qualified identifier reference. qualifier may be the zero
// Identifier for bare lookups.
//
// Currently produces a cascades.FieldValue for scope-resolved
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
func (r *Resolver) ResolveIdentifier(qualifier, id semantic.Identifier) (cascades.Value, error) {
	col, _, err := r.analyzer.ResolveColumnRef(r.scope, qualifier, id)
	if err != nil {
		return nil, err
	}
	return &cascades.FieldValue{
		Field: col.Id.Name(),
		Typ:   sqlTypeToCascadesValueType(col.Type),
	}, nil
}

// ResolveArithmetic wraps left/right Values in a cascades
// ArithmeticValue with the given operator. Used when the parser
// produces an arithmetic expression node — the analyzer resolves
// each operand recursively, then pairs them here.
//
// Operand types aren't cross-checked in the seed (both assumed
// int); real type inference replaces this when the Type hierarchy
// port lands.
func (r *Resolver) ResolveArithmetic(op cascades.ArithmeticOp, left, right cascades.Value) (cascades.Value, error) {
	if left == nil || right == nil {
		return nil, fmt.Errorf("expr.ResolveArithmetic: operand is nil")
	}
	return &cascades.ArithmeticValue{Op: op, Left: left, Right: right}, nil
}

// ResolveComparison wraps left/right Values in a cascades
// ComparisonPredicate. Mirrors the analyzer's job of lifting
// `a > b` from a parse-tree comparison node to a predicate node.
//
// The Comparison's Operand is set from right.Evaluate(nil) when
// right is constant (per cascades.IsConstantValue); for
// non-constant RHS the current seed doesn't build a predicate
// (returns an error) — the real Comparison type will take a Value
// on the RHS in a later commit.
//
// Does NOT pre-fold even when both operands are constant. `5 = 5`
// produces a real ComparisonPredicate; the fixpoint simplifier
// folds it to TRUE via ComparisonConstantSimplifyRule. Eager
// folding here would hide foldable shapes from rule matchers that
// expect to see them.
func (r *Resolver) ResolveComparison(op cascades.ComparisonType, left, right cascades.Value) (cascades.QueryPredicate, error) {
	if left == nil || right == nil {
		return nil, fmt.Errorf("expr.ResolveComparison: operand is nil")
	}
	rhs, ok := cascades.EvaluateConstant(right)
	if !ok {
		return nil, fmt.Errorf("expr.ResolveComparison: RHS must be a constant in the seed; got %T", right)
	}
	return cascades.NewComparisonPredicate(left, cascades.Comparison{
		Type: op, Operand: rhs,
	}), nil
}

// ResolveCast wraps v in a CastValue with the target type. Rejects
// nil child (programmer error) and TypeUnknown target (use the
// direct Value if the target is genuinely unknown).
func (r *Resolver) ResolveCast(v cascades.Value, target cascades.ValueType) (cascades.Value, error) {
	if v == nil {
		return nil, fmt.Errorf("expr.ResolveCast: child is nil")
	}
	if target == cascades.TypeUnknown {
		return nil, fmt.Errorf("expr.ResolveCast: target TypeUnknown")
	}
	return cascades.NewCastValue(v, target), nil
}

// ResolveIsNull builds `v IS NULL`. Unary — Comparison.Operand is
// nil (Eval ignores it for unary types).
func (r *Resolver) ResolveIsNull(v cascades.Value) (cascades.QueryPredicate, error) {
	if v == nil {
		return nil, fmt.Errorf("expr.ResolveIsNull: operand is nil")
	}
	return cascades.NewComparisonPredicate(v, cascades.Comparison{Type: cascades.ComparisonIsNull}), nil
}

// ResolveIsNotNull builds `v IS NOT NULL`. Unary.
func (r *Resolver) ResolveIsNotNull(v cascades.Value) (cascades.QueryPredicate, error) {
	if v == nil {
		return nil, fmt.Errorf("expr.ResolveIsNotNull: operand is nil")
	}
	return cascades.NewComparisonPredicate(v, cascades.Comparison{Type: cascades.ComparisonIsNotNull}), nil
}

// ResolveLike builds `lhs LIKE pattern`. Pattern must be a plan-time
// constant string (parameter-bound patterns land with the
// parameter-Comparison design).
func (r *Resolver) ResolveLike(lhs cascades.Value, pattern cascades.Value) (cascades.QueryPredicate, error) {
	if lhs == nil || pattern == nil {
		return nil, fmt.Errorf("expr.ResolveLike: operand is nil")
	}
	lit, ok := cascades.EvaluateConstant(pattern)
	if !ok {
		return nil, fmt.Errorf("expr.ResolveLike: pattern must be a constant in the seed; got %T", pattern)
	}
	s, ok := lit.(string)
	if !ok {
		return nil, fmt.Errorf("expr.ResolveLike: pattern must be a string; got %T", lit)
	}
	return cascades.NewComparisonPredicate(lhs, cascades.Comparison{Type: cascades.ComparisonLike, Operand: s}), nil
}

// ResolveStartsWith builds `lhs STARTS_WITH prefix`. Prefix must be
// a constant string.
func (r *Resolver) ResolveStartsWith(lhs cascades.Value, prefix cascades.Value) (cascades.QueryPredicate, error) {
	if lhs == nil || prefix == nil {
		return nil, fmt.Errorf("expr.ResolveStartsWith: operand is nil")
	}
	lit, ok := cascades.EvaluateConstant(prefix)
	if !ok {
		return nil, fmt.Errorf("expr.ResolveStartsWith: prefix must be a constant in the seed; got %T", prefix)
	}
	s, ok := lit.(string)
	if !ok {
		return nil, fmt.Errorf("expr.ResolveStartsWith: prefix must be a string; got %T", lit)
	}
	return cascades.NewComparisonPredicate(lhs, cascades.Comparison{Type: cascades.ComparisonStartsWith, Operand: s}), nil
}

// ResolveIn builds a ComparisonPredicate{ComparisonIn} from a left
// Value and a list of constant RHS values. Every RHS must be a
// plan-time constant (per cascades.EvaluateConstant); non-constant
// elements return an error so callers lift the expression-based
// IN-list to the value-space form explicitly.
//
// The RHS Operand is a []any of evaluated literals.
func (r *Resolver) ResolveIn(left cascades.Value, rhs []cascades.Value) (cascades.QueryPredicate, error) {
	if left == nil {
		return nil, fmt.Errorf("expr.ResolveIn: LHS is nil")
	}
	list := make([]any, 0, len(rhs))
	for i, v := range rhs {
		lit, ok := cascades.EvaluateConstant(v)
		if !ok {
			return nil, fmt.Errorf("expr.ResolveIn: element %d is not constant (%T)", i, v)
		}
		list = append(list, lit)
	}
	return cascades.NewComparisonPredicate(left, cascades.Comparison{
		Type: cascades.ComparisonIn, Operand: list,
	}), nil
}

// ResolveAnd combines N predicates via Kleene AND. A single
// predicate returns verbatim (no wrapping); empty list returns
// ConstantPredicate(TRUE) — the AND identity.
func (r *Resolver) ResolveAnd(preds ...cascades.QueryPredicate) cascades.QueryPredicate {
	switch len(preds) {
	case 0:
		return cascades.NewConstantPredicate(cascades.TriTrue)
	case 1:
		return preds[0]
	}
	return cascades.NewAnd(preds...)
}

// ResolveOr combines N predicates via Kleene OR. Empty list returns
// ConstantPredicate(FALSE) — the OR identity. Single predicate
// returns verbatim.
func (r *Resolver) ResolveOr(preds ...cascades.QueryPredicate) cascades.QueryPredicate {
	switch len(preds) {
	case 0:
		return cascades.NewConstantPredicate(cascades.TriFalse)
	case 1:
		return preds[0]
	}
	return cascades.NewOr(preds...)
}

// ResolveNot wraps a predicate in a Kleene NOT. Nil child returns
// ConstantPredicate(UNKNOWN) — the only sensible interpretation.
func (r *Resolver) ResolveNot(pred cascades.QueryPredicate) cascades.QueryPredicate {
	if pred == nil {
		return cascades.NewConstantPredicate(cascades.TriUnknown)
	}
	return cascades.NewNot(pred)
}

// ResolveFunctionCall dispatches a function call against the given
// catalogue. For known aggregates (COUNT/SUM/MIN/MAX/AVG) it returns
// the corresponding cascades.AggregateValue. Scalar function support
// comes once the scalar-function catalogue is wired in.
//
// isStar=true signals the argument was `*` (COUNT(*)) — args must be
// empty in that case.
func (r *Resolver) ResolveFunctionCall(
	funcCatalog *semantic.FunctionCatalog,
	name semantic.Identifier,
	isStar bool,
	args []cascades.Value,
) (cascades.Value, error) {
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
	if op == cascades.AggCountStar {
		return cascades.NewAggregateValue(op, nil), nil
	}
	return cascades.NewAggregateValue(op, args[0]), nil
}

// aggregateOpForName maps a normalized aggregate function name +
// star flag to the corresponding cascades.AggregateOp. Not exported
// — called via ResolveFunctionCall.
func aggregateOpForName(name string, isStar bool) (cascades.AggregateOp, bool) {
	switch name {
	case "COUNT":
		if isStar {
			return cascades.AggCountStar, true
		}
		return cascades.AggCount, true
	case "SUM":
		return cascades.AggSum, true
	case "MIN":
		return cascades.AggMin, true
	case "MAX":
		return cascades.AggMax, true
	case "AVG":
		return cascades.AggAvg, true
	}
	return cascades.AggInvalid, false
}

// sqlTypeToCascadesValueType maps the seed's string-valued SQL type
// (from semantic.Column.Type) to cascades.ValueType. Coarse — the
// seed ValueType enum has only Int / String / Bool; everything else
// falls through to TypeUnknown. Real type inference lands with the
// Type hierarchy port.
func sqlTypeToCascadesValueType(sqlType string) cascades.ValueType {
	switch sqlType {
	case "INT":
		return cascades.TypeInt
	case "STRING", "ENUM":
		return cascades.TypeString
	case "BOOL":
		return cascades.TypeBool
	case "FLOAT", "BYTES", "RECORD":
		// Seed enum lacks dedicated Float/Bytes/Record types. Fall
		// through to Unknown rather than silently lie about INT / STRING
		// representation — a mistyped column at the resolver boundary
		// would cascade into wrong comparator picks downstream.
		return cascades.TypeUnknown
	}
	return cascades.TypeUnknown
}

// ResolveConstant wraps a Go-native literal in a cascades
// ConstantValue with the appropriate type tag. Useful for inlining
// literal arguments when building a Value tree from a parsed
// expression.
//
// Returns an error when the literal's runtime type doesn't map to
// any seed ValueType — nil, int, int32, int64, string, bool are
// supported. float32/float64 are explicitly flagged pending the
// Type hierarchy port (cascades.TypeFloat doesn't exist yet; don't
// silently pretend they're ints).
func (r *Resolver) ResolveConstant(lit any) (cascades.Value, error) {
	switch v := lit.(type) {
	case nil:
		return cascades.NewNullValue(cascades.TypeUnknown), nil
	case bool:
		return cascades.NewBooleanValue(v), nil
	case int:
		return &cascades.ConstantValue{Value: int64(v), Typ: cascades.TypeInt}, nil
	case int32:
		return &cascades.ConstantValue{Value: int64(v), Typ: cascades.TypeInt}, nil
	case int64:
		return &cascades.ConstantValue{Value: v, Typ: cascades.TypeInt}, nil
	case string:
		return &cascades.ConstantValue{Value: v, Typ: cascades.TypeString}, nil
	case float32, float64:
		// Pending cascades.TypeFloat in the Type hierarchy port.
		// Specific error message so future maintainers know this is
		// the companion site for FLOAT support.
		return nil, fmt.Errorf(
			"expr.ResolveConstant: float literals unsupported until cascades.TypeFloat lands (Phase 4.0 Type hierarchy); got %v", v)
	}
	return nil, fmt.Errorf("expr.ResolveConstant: unsupported literal type %T", lit)
}
