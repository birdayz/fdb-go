package values

// AndOrOp identifies the boolean connector. Mirrors Java's
// `AndOrValue.Operator` enum.
type AndOrOp int

const (
	// AndOrAnd is short-circuit logical AND with Kleene 3VL.
	AndOrAnd AndOrOp = iota
	// AndOrOr is short-circuit logical OR with Kleene 3VL.
	AndOrOr
)

// String returns the SQL keyword for explain / debug print.
func (op AndOrOp) String() string {
	switch op {
	case AndOrAnd:
		return "AND"
	case AndOrOr:
		return "OR"
	}
	return "INVALID"
}

// AndOrValue is the Value-layer AND/OR connector — binary boolean
// operator with Kleene three-valued logic semantics. Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.values.AndOrValue`.
//
// Java has parallel predicate-layer AndPredicate / OrPredicate (already
// ported); this Value-layer variant exists for cases where AND/OR
// appears in a NON-predicate context — typically SQL projections like
// `SELECT a AND b FROM t` where the connector itself is the row's
// emitted Value, not a filter.
//
// Result type: NotNullBoolean when both operands are NOT NULL, else
// NullableBoolean (per SQL Kleene rules — TRUE OR NULL = TRUE,
// FALSE AND NULL = FALSE, but TRUE AND NULL = NULL).
//
// Eval semantics (Kleene 3VL):
//
//	AND        | TRUE  FALSE NULL
//	-----------|-------------------
//	TRUE       | TRUE  FALSE NULL
//	FALSE      | FALSE FALSE FALSE
//	NULL       | NULL  FALSE NULL
//
//	OR         | TRUE  FALSE NULL
//	-----------|-------------------
//	TRUE       | TRUE  TRUE  TRUE
//	FALSE      | TRUE  FALSE NULL
//	NULL       | TRUE  NULL  NULL
//
// Short-circuit: if the LEFT operand evaluates to the dominant value
// (FALSE for AND, TRUE for OR), the right operand is not evaluated.
// Mirrors Java's eval-side optimisation. The right is evaluated for
// non-dominant left values (including NULL).
//
// Non-bool operand handling: if either operand evaluates to a non-
// bool / non-NULL value, eval returns nil (UNKNOWN — type-degraded).
type AndOrValue struct {
	Op    AndOrOp
	Left  Value
	Right Value
}

// NewAndOrValue constructs an AND/OR Value.
func NewAndOrValue(op AndOrOp, left, right Value) *AndOrValue {
	return &AndOrValue{Op: op, Left: left, Right: right}
}

// Children returns [left, right].
func (v *AndOrValue) Children() []Value {
	return []Value{v.Left, v.Right}
}

// Name returns the SQL keyword.
func (v *AndOrValue) Name() string { return v.Op.String() }

// Type returns NotNullBoolean iff BOTH operands have NOT NULL
// boolean types, else NullableBoolean. Mirrors Java's
// AndOrValue.getResultType which OR-reduces operand nullabilities.
//
// Rationale: when both operands are non-nullable booleans, the
// result is always TRUE or FALSE — never NULL. (NULL only enters
// the eval through a NULL operand, which can't happen with NOT NULL
// operand types.) The dispatch matches the conventional SQL
// type-inference for boolean connectors.
//
// Falls back to NullableBoolean if either operand is missing /
// non-boolean / nullable.
func (v *AndOrValue) Type() Type {
	if v.Left == nil || v.Right == nil {
		return NullableBoolean
	}
	lt := v.Left.Type()
	rt := v.Right.Type()
	if lt == nil || rt == nil {
		return NullableBoolean
	}
	if lt.Code() == TypeCodeBoolean && rt.Code() == TypeCodeBoolean &&
		!lt.IsNullable() && !rt.IsNullable() {
		return NotNullBoolean
	}
	return NullableBoolean
}

// Evaluate is the error-returning twin (RFC-091).
// Kleene short-circuit error semantics are preserved: a dominant
// LEFT (FALSE for AND, TRUE for OR) returns before the RIGHT operand
// is evaluated, so `FALSE AND <err>` → FALSE; `<err> AND FALSE` →
// error; `UNKNOWN AND <err>` → error.
func (v *AndOrValue) Evaluate(evalCtx any) (any, error) {
	if v.Left == nil || v.Right == nil {
		return nil, nil
	}
	left, err := v.Left.Evaluate(evalCtx)
	if err != nil {
		return nil, err
	}

	// Short-circuit on dominant left.
	switch v.Op {
	case AndOrAnd:
		if lb, ok := left.(bool); ok && !lb {
			return false, nil // FALSE AND ? = FALSE
		}
	case AndOrOr:
		if lb, ok := left.(bool); ok && lb {
			return true, nil // TRUE OR ? = TRUE
		}
	}

	right, err := v.Right.Evaluate(evalCtx)
	if err != nil {
		return nil, err
	}

	switch v.Op {
	case AndOrAnd:
		// AND truth table for the non-short-circuit cases:
		//   TRUE AND TRUE = TRUE
		//   TRUE AND FALSE = FALSE
		//   TRUE AND NULL = NULL
		//   NULL AND TRUE = NULL
		//   NULL AND FALSE = FALSE  (handled below)
		//   NULL AND NULL = NULL
		if rb, ok := right.(bool); ok && !rb {
			return false, nil // ? AND FALSE = FALSE
		}
		if left == nil || right == nil {
			return nil, nil
		}
		if lb, lok := left.(bool); lok {
			if rb, rok := right.(bool); rok {
				return lb && rb, nil
			}
		}
		return nil, nil
	case AndOrOr:
		// OR truth table for the non-short-circuit cases:
		//   FALSE OR TRUE = TRUE
		//   FALSE OR FALSE = FALSE
		//   FALSE OR NULL = NULL
		//   NULL OR TRUE = TRUE  (handled below)
		//   NULL OR FALSE = NULL
		//   NULL OR NULL = NULL
		if rb, ok := right.(bool); ok && rb {
			return true, nil // ? OR TRUE = TRUE
		}
		if left == nil || right == nil {
			return nil, nil
		}
		if lb, lok := left.(bool); lok {
			if rb, rok := right.(bool); rok {
				return lb || rb, nil
			}
		}
		return nil, nil
	}
	return nil, nil
}

// WithChildren returns a fresh AndOrValue with the given children.
// Caller is responsible for passing exactly 2 children; less raises
// out-of-bounds at access time.
func (v *AndOrValue) WithChildren(newChildren []Value) *AndOrValue {
	return NewAndOrValue(v.Op, newChildren[0], newChildren[1])
}
