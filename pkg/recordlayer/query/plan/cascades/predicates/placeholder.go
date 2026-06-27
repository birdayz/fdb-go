package predicates

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// Placeholder is a QueryPredicate representing a sargable parameter
// slot in a candidate expression tree. During index matching, the
// matching rules check whether query predicates can "fill" a
// placeholder. Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.predicates.Placeholder`.
//
// A Placeholder carries:
//   - ParameterAlias: uniquely identifies this slot in the candidate's
//     parameter list (maps to MatchCandidate.GetSargableAliases()).
//   - Value: the value expression being constrained (e.g. a FieldValue
//     representing a column).
//   - CompRange: the comparison range constraint on this placeholder.
//     An empty range means unconstrained.
//
// Placeholder is a leaf predicate — it has no children.
type Placeholder struct {
	// ParameterAlias uniquely identifies this placeholder in the
	// candidate's parameter list.
	ParameterAlias values.CorrelationIdentifier

	// Value is the value expression being constrained (e.g. a
	// FieldValue representing a column).
	Value values.Value

	// CompRange is the comparison range constraint on this
	// placeholder. Empty range = unconstrained.
	CompRange *ComparisonRange
}

// NewPlaceholder constructs a Placeholder with the given alias and
// value, and an initially empty ComparisonRange.
func NewPlaceholder(parameterAlias values.CorrelationIdentifier, value values.Value) *Placeholder {
	return &Placeholder{
		ParameterAlias: parameterAlias,
		Value:          value,
		CompRange:      EmptyComparisonRange(),
	}
}

// GetParameterAlias returns the alias that identifies this placeholder
// in the candidate's parameter list.
func (p *Placeholder) GetParameterAlias() values.CorrelationIdentifier {
	return p.ParameterAlias
}

// GetValue returns the value expression being constrained.
func (p *Placeholder) GetValue() values.Value {
	return p.Value
}

// GetComparisonRange returns the comparison range constraint.
func (p *Placeholder) GetComparisonRange() *ComparisonRange {
	return p.CompRange
}

// WithRange returns a new Placeholder with the given comparison range.
// The original is not modified.
func (p *Placeholder) WithRange(cr *ComparisonRange) *Placeholder {
	return &Placeholder{
		ParameterAlias: p.ParameterAlias,
		Value:          p.Value,
		CompRange:      cr,
	}
}

// IsConstraining reports whether this placeholder has a non-empty
// range — i.e. whether it actually constrains the value.
func (p *Placeholder) IsConstraining() bool {
	return p.CompRange != nil && !p.CompRange.IsEmpty()
}

// --- QueryPredicate interface -----------------------------------------

// Children returns an empty slice — Placeholder is a leaf predicate.
func (*Placeholder) Children() []QueryPredicate { return []QueryPredicate{} }

// Eval returns TriUnknown — placeholders are planning-time constructs
// and are never evaluated at runtime.
func (*Placeholder) Eval(_ any) (TriBool, error) { return TriUnknown, nil }

// Explain renders a textual form: "Placeholder(alias, value)".
func (p *Placeholder) Explain() string {
	valExplain := "<nil>"
	if p.Value != nil {
		valExplain = values.ExplainValue(p.Value)
	}
	return fmt.Sprintf("Placeholder(%s, %s)", p.ParameterAlias.String(), valExplain)
}

// --- Additional methods -----------------------------------------------

// Negate returns the placeholder itself — placeholders don't negate.
// They are structural slots, not boolean expressions.
func (p *Placeholder) Negate() QueryPredicate {
	return p
}

// GetCorrelatedTo returns the set of correlation identifiers this
// placeholder references. The parameter alias is always included;
// additionally, any correlations from the carried Value are merged in.
func (p *Placeholder) GetCorrelatedTo() map[values.CorrelationIdentifier]struct{} {
	out := map[values.CorrelationIdentifier]struct{}{
		p.ParameterAlias: {},
	}
	if p.Value != nil {
		for k := range values.GetCorrelatedToOfValue(p.Value) {
			out[k] = struct{}{}
		}
	}
	return out
}
