package cascades

import "fdb.dev/pkg/recordlayer/query/plan/cascades/values"

// ValueEquivalence defines axiomatic equality relationships between
// Values beyond structural equality. Structural equality
// (Value.semanticEquals) compares two values within the same scope.
// ValueEquivalence enables cross-scope comparisons by declaring that
// certain values are considered equal under a mapping (e.g., two
// QuantifiedObjectValues are equal if their aliases are mapped via an
// AliasMap).
//
// Ports Java's com.apple.foundationdb.record.query.plan.cascades.ValueEquivalence.
type ValueEquivalence interface {
	// IsDefinedEqual reports whether two values are axiomatically equal
	// under this equivalence. Returns a ConstrainedBoolean that may
	// carry a QueryPlanConstraint (the equality holds only if the
	// constraint is satisfied at plan execution time).
	IsDefinedEqual(left, right values.Value) ConstrainedBoolean

	// IsDefinedEqualAlias reports whether two correlation identifiers
	// are axiomatically equal under this equivalence.
	IsDefinedEqualAlias(left, right values.CorrelationIdentifier) ConstrainedBoolean
}

// ConstrainedBoolean is a boolean result that may carry an optional
// QueryPlanConstraint. Ports Java's ConstrainedBoolean.
type ConstrainedBoolean struct {
	Value      bool
	Constraint *QueryPlanConstraint
}

// AlwaysTrue returns a ConstrainedBoolean that is unconditionally true.
func AlwaysTrue() ConstrainedBoolean {
	return ConstrainedBoolean{Value: true}
}

// FalseValue returns a ConstrainedBoolean that is false.
func FalseValue() ConstrainedBoolean {
	return ConstrainedBoolean{Value: false}
}

// TrueWithConstraint returns a ConstrainedBoolean that is true only
// if the given constraint holds.
func TrueWithConstraint(c *QueryPlanConstraint) ConstrainedBoolean {
	return ConstrainedBoolean{Value: true, Constraint: c}
}

// IsTrue reports whether the boolean value is true (regardless of
// constraint). Ports Java's ConstrainedBoolean.isTrue().
func (cb ConstrainedBoolean) IsTrue() bool { return cb.Value }

// IsFalse reports whether the boolean value is false. Ports Java's
// ConstrainedBoolean.isFalse().
func (cb ConstrainedBoolean) IsFalse() bool { return !cb.Value }

// ComposeWithOther composes this ConstrainedBoolean with another via
// AND semantics: if either is false, the result is false. If both are
// true, constraints are merged. Ports Java's
// ConstrainedBoolean.composeWithOther().
func (cb ConstrainedBoolean) ComposeWithOther(other ConstrainedBoolean) ConstrainedBoolean {
	if cb.IsFalse() {
		return FalseValue()
	}
	if other.IsFalse() {
		return FalseValue()
	}
	if cb.Constraint == nil {
		return other
	}
	if other.Constraint == nil {
		return cb
	}
	return TrueWithConstraint(cb.Constraint)
}

// Filter returns false if the ConstrainedBoolean is false; otherwise
// returns the result of evaluating the predicate fn. Ports Java's
// ConstrainedBoolean.filter().
func (cb ConstrainedBoolean) Filter(fn func() ConstrainedBoolean) ConstrainedBoolean {
	if cb.IsFalse() {
		return FalseValue()
	}
	return cb.ComposeWithOther(fn())
}

// emptyValueEquivalence is the baseline: all comparisons return false.
type emptyValueEquivalence struct{}

func (emptyValueEquivalence) IsDefinedEqual(values.Value, values.Value) ConstrainedBoolean {
	return FalseValue()
}

func (emptyValueEquivalence) IsDefinedEqualAlias(values.CorrelationIdentifier, values.CorrelationIdentifier) ConstrainedBoolean {
	return FalseValue()
}

// EmptyValueEquivalence returns a ValueEquivalence where no values are
// considered equal. Ports Java's ValueEquivalence.empty().
func EmptyValueEquivalence() ValueEquivalence {
	return emptyValueEquivalence{}
}

// AliasMapValueEquivalence maps QuantifiedObjectValues via an AliasMap
// to enable cross-scope value comparison. Two values are equal if they
// are both QuantifiedObjectValues and their aliases are mapped in the
// AliasMap.
//
// Ports Java's ValueEquivalence.AliasMapBackedValueEquivalence.
type AliasMapValueEquivalence struct {
	aliasMap *AliasMap
}

// NewAliasMapValueEquivalence creates a ValueEquivalence backed by
// the given AliasMap.
func NewAliasMapValueEquivalence(am *AliasMap) *AliasMapValueEquivalence {
	return &AliasMapValueEquivalence{aliasMap: am}
}

// IsDefinedEqual checks if two values are QuantifiedObjectValues with
// aliases mapped in the underlying AliasMap.
func (e *AliasMapValueEquivalence) IsDefinedEqual(left, right values.Value) ConstrainedBoolean {
	lqov, lok := left.(*values.QuantifiedObjectValue)
	rqov, rok := right.(*values.QuantifiedObjectValue)
	if !lok || !rok {
		return FalseValue()
	}
	return e.IsDefinedEqualAlias(lqov.Correlation, rqov.Correlation)
}

// IsDefinedEqualAlias checks if two correlation identifiers are mapped
// in the underlying AliasMap.
func (e *AliasMapValueEquivalence) IsDefinedEqualAlias(left, right values.CorrelationIdentifier) ConstrainedBoolean {
	if e.aliasMap == nil {
		return FalseValue()
	}
	if e.aliasMap.ContainsMapping(left, right) {
		return AlwaysTrue()
	}
	return FalseValue()
}

var (
	_ ValueEquivalence = emptyValueEquivalence{}
	_ ValueEquivalence = (*AliasMapValueEquivalence)(nil)
)
