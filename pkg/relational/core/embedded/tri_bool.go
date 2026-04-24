package embedded

// Kleene three-valued logic for SQL predicate evaluation.
//
// Predicate evaluators return triBool so NOT / AND / OR preserve
// SQL UNKNOWN (NULL) instead of collapsing it to FALSE. Only triTrue
// keeps a row at a WHERE / HAVING / ON boundary — both triFalse and
// triNull filter it out, matching SQL semantics.
//
// Destined for pkg/relational/core/eval/tri_bool.go per RFC 021
// Phase 1c; keeps working in the embedded package until the
// evaluator moves are done.

// triBool is a Kleene three-valued truth type used by predicate evaluators so
// that NOT/AND/OR preserve SQL UNKNOWN (NULL) instead of collapsing it to
// FALSE. In a WHERE/HAVING/ON boundary, only triTrue keeps the row — both
// triFalse and triNull filter it out, matching SQL semantics.
type triBool int8

const (
	triFalse triBool = iota
	triTrue
	triNull
)

func triFromBool(b bool) triBool {
	if b {
		return triTrue
	}
	return triFalse
}

// IsTrue reports whether the value is strictly TRUE. UNKNOWN is NOT true —
// this is the predicate-filter boundary: `if !t.IsTrue() { skip row }`.
func (t triBool) IsTrue() bool { return t == triTrue }

// Not implements SQL's NOT with UNKNOWN preservation: NOT TRUE = FALSE,
// NOT FALSE = TRUE, NOT UNKNOWN = UNKNOWN.
func (t triBool) Not() triBool {
	switch t {
	case triTrue:
		return triFalse
	case triFalse:
		return triTrue
	}
	return triNull
}

// triAnd implements SQL's AND: FALSE AND x = FALSE, otherwise UNKNOWN if either
// is UNKNOWN, else TRUE. Short-circuit on FALSE is done by the caller.
func triAnd(a, b triBool) triBool {
	if a == triFalse || b == triFalse {
		return triFalse
	}
	if a == triNull || b == triNull {
		return triNull
	}
	return triTrue
}

// triOr implements SQL's OR: TRUE OR x = TRUE, otherwise UNKNOWN if either is
// UNKNOWN, else FALSE. Short-circuit on TRUE is done by the caller.
func triOr(a, b triBool) triBool {
	if a == triTrue || b == triTrue {
		return triTrue
	}
	if a == triNull || b == triNull {
		return triNull
	}
	return triFalse
}
