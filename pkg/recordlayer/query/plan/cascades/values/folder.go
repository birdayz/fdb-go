package values

// ExpressionFolder is the testable surface for plan-time constant
// folding of standalone Values. Implementations: DefaultFolder
// (production — composes SimplifyValue with EvaluateConstant), and
// any caller-provided test fake that returns canned answers without
// invoking real simplification.
//
// Why an interface, not a free function: callers like
// `embedded.foldConstantProjections` need to be unit-testable without
// constructing a real catalog + Resolver + metadata. With an injected
// folder, the routing logic ("did we already fold this slot? is the
// slice big enough?") can be exercised against a fake folder that
// returns whatever the test wants. Per RFC-025 §"Closing the leaks".
//
// Contract: Fold returns (foldedValue, true) when v is a row-context-
// independent Value whose evaluation produces a Go-native scalar that
// LiteralValue can faithfully re-wrap. Returns (nil, false) on a
// non-foldable input — a FieldValue, a ParameterValue, an
// AggregateValue, or any composite containing those. Nil v returns
// (nil, false) — the boundary so callers don't need to nil-guard.
type ExpressionFolder interface {
	Fold(v Value) (any, bool)
}

// DefaultFolder returns the production ExpressionFolder. Its Fold
// runs SimplifyValue first (so partial folds compose: `name + (1+2)`
// simplifies to `name + 3` even though the result isn't constant) and
// then EvaluateConstant for the all-constant case. Failure modes
// surface as ok=false, never panic.
func DefaultFolder() ExpressionFolder { return defaultFolder{} }

type defaultFolder struct{}

func (defaultFolder) Fold(v Value) (any, bool) {
	if v == nil {
		return nil, false
	}
	simplified := SimplifyValue(v)
	return EvaluateConstant(simplified)
}
