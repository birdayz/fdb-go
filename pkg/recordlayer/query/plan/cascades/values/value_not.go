package values

// NotValue is the Value-layer NOT — the boolean negation of a single
// child Value. Mirrors Java's `com.apple.foundationdb.record.query.
// plan.cascades.values.NotValue`.
//
// Why a Value-layer NOT in addition to the predicate-layer NotPredicate:
// boolean negation appears in non-predicate contexts too — e.g.
// `SELECT NOT(active) FROM t` where the result column carries a
// nullable boolean, not a 3VL truth value the predicate system can
// route. Cascades rules that float between Value and QueryPredicate
// representations need a Value-shaped NOT so the rebuild stays a
// Value tree. Java's NotValue.toQueryPredicate() bridges back to
// NotPredicate when the surrounding context calls for it; the seed
// keeps the layers separate for now (no toQueryPredicate yet).
//
// Evaluate semantics — Kleene 3VL:
//   - NOT TRUE = FALSE
//   - NOT FALSE = TRUE
//   - NOT NULL = NULL (NULL propagates)
//   - NOT non-bool = nil (UNKNOWN — degraded type mismatch)
//
// Type is always TypeBool (NOT is a boolean operator).
type NotValue struct {
	Child Value
}

// NewNotValue constructs a NotValue.
func NewNotValue(child Value) *NotValue { return &NotValue{Child: child} }

func (n *NotValue) Children() []Value {
	if n.Child == nil {
		return []Value{}
	}
	return []Value{n.Child}
}

func (*NotValue) Name() string { return "not" }

// Type preserves the child's nullability — NOT of a nullable boolean
// is a nullable boolean (NOT of NULL is NULL). When the child is nil
// or its Type isn't a boolean shape, fall back to NullableBoolean
// (NOT is always boolean-shaped at the Value layer).
func (n *NotValue) Type() Type {
	if n.Child == nil {
		return NullableBoolean
	}
	ct := n.Child.Type()
	if ct == nil {
		return NullableBoolean
	}
	if ct.Code() == TypeCodeBoolean {
		return ct
	}
	return NullableBoolean
}

// Evaluate is the error-returning twin (RFC-091).
func (n *NotValue) Evaluate(evalCtx any) (any, error) {
	if n.Child == nil {
		return nil, nil
	}
	v, err := n.Child.Evaluate(evalCtx)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, nil
	}
	if b, ok := v.(bool); ok {
		return !b, nil
	}
	// Type mismatch — degrade to UNKNOWN.
	return nil, nil
}
