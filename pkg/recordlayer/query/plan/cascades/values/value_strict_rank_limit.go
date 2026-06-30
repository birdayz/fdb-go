package values

import "fmt"

// StrictRankLimitValue computes the row cap of a STRICT distance-rank predicate
// `ROW_NUMBER() OVER (ORDER BY distance(...)) < K` whose K is a RUNTIME value (a
// bound parameter — RFC-156 parameterized vector rank limit). The number of
// admitted rows is max(0, K-1).
//
// It exists so the runtime LIMIT that bounds an ordered vector stream never
// expresses the strict adjustment with general CHECKED subtraction. `K - 1`
// underflows at K = math.MinInt64 (the ONLY K where it can), which an
// ArithmeticValue surfaces as an ArithmeticOverflowError — aborting a query that
// semantically selects NO rows (ROW_NUMBER() ≥ 1 is never < a K ≤ 1). This value
// mirrors the executor's scan-side rank-cap guard exactly: K ≤ 1 ⇒ 0 (computed
// WITHOUT subtracting, so K = math.MinInt64 cannot wrap K-1 to a huge positive),
// else K - 1 (K ≥ 2 ⇒ no underflow). NULL K propagates.
type StrictRankLimitValue struct {
	K Value
}

func (v *StrictRankLimitValue) Children() []Value { return []Value{v.K} }
func (v *StrictRankLimitValue) Name() string      { return "strict_rank_limit" }
func (v *StrictRankLimitValue) Type() Type        { return NullableLong }

func (v *StrictRankLimitValue) Evaluate(evalCtx any) (any, error) {
	kv, err := v.K.Evaluate(evalCtx)
	if err != nil {
		return nil, err
	}
	if kv == nil {
		return nil, nil // NULL propagates
	}
	k, ok := toInt64ForArith(kv)
	if !ok {
		return nil, &ScalarTypeMismatchError{
			Message: fmt.Sprintf("strict rank limit cap is not an integer: %T", kv),
		}
	}
	// A strict `< K` admits max(0, K-1) rows. Short-circuit K ≤ 1 to 0 BEFORE
	// subtracting (the only branch that could underflow), so K = math.MinInt64
	// can never wrap K-1 to a huge positive cap — identical to the scan-side
	// rank-cap guard in the executor.
	if k <= 1 {
		return int64(0), nil
	}
	return k - 1, nil
}

// WithChildren rebuilds the node around a rebased K so RebaseValue's generic
// recursion never drops a correlated K (a bound-parameter ConstantObjectValue).
func (v *StrictRankLimitValue) WithChildren(newChildren []Value) Value {
	if len(newChildren) != 1 {
		return v
	}
	return &StrictRankLimitValue{K: newChildren[0]}
}

// EqualsWithoutChildrenValue: the node has no own attributes beyond its child K
// (compared separately by ValuesStructurallyEqual's recursion), so two strict
// rank caps are node-equal iff they are the same concrete type.
func (v *StrictRankLimitValue) EqualsWithoutChildrenValue(other Value) bool {
	_, ok := other.(*StrictRankLimitValue)
	return ok
}

var (
	_ Value                     = (*StrictRankLimitValue)(nil)
	_ SelfEqualsWithoutChildren = (*StrictRankLimitValue)(nil)
)
