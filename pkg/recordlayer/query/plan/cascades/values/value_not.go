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

func (*NotValue) Type() ValueType { return TypeBool }
func (*NotValue) Name() string    { return "not" }

func (n *NotValue) Evaluate(evalCtx any) any {
	if n.Child == nil {
		return nil
	}
	v := n.Child.Evaluate(evalCtx)
	if v == nil {
		return nil
	}
	if b, ok := v.(bool); ok {
		return !b
	}
	// Type mismatch — degrade to UNKNOWN.
	return nil
}
