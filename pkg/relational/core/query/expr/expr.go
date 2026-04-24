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
