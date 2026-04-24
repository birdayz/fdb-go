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
// Swingshift-47 seed scope: bare column references + constant
// literals. Operators (arithmetic, comparison), function calls, CAST,
// NULL literals, IN lists, qualified references all land in follow-up
// commits.
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
type Resolver struct {
	analyzer *semantic.Analyzer
	scope    *semantic.Scope
}

// New constructs a Resolver bound to a scope. Nil analyzer or nil
// scope panics — the resolver has nothing to do without either.
func New(analyzer *semantic.Analyzer, scope *semantic.Scope) *Resolver {
	if analyzer == nil {
		panic("expr.New: analyzer is nil")
	}
	if scope == nil {
		panic("expr.New: scope is nil")
	}
	return &Resolver{analyzer: analyzer, scope: scope}
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
	case "INT", "BYTES":
		return cascades.TypeInt
	case "STRING", "ENUM":
		return cascades.TypeString
	case "BOOL":
		return cascades.TypeBool
	case "FLOAT":
		// Seed enum doesn't have Float yet; fall through to Unknown
		// rather than lie about INT representation.
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
// supported.
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
	}
	return nil, fmt.Errorf("expr.ResolveConstant: unsupported literal type %T", lit)
}
