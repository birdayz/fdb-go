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
// NotPredicate when the surrounding context calls for it; Go keeps
// the layers separate — the only value→predicate bridge is the
// EXISTS one (predicates/existential_value_predicate.go).
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

// Type is NullableBoolean whatever the child: Java's NotValue does not
// override BooleanValue.getResultType, primitiveType(BOOLEAN), which is
// nullable (BooleanValue.java:40-42).
func (*NotValue) Type() Type { return NullableBoolean }

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
