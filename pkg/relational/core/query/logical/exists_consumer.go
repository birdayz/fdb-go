package logical

import "fmt"

// ExistsConstraint is a requirement derived from a bound child. It is not a
// correlation property: the same child has the same requirement in every clause.
type ExistsConstraint uint8

const (
	ExistsAnyConsumer ExistsConstraint = iota
	ExistsPositivePredicateOnly
)

// ExistsConsumer describes a resolved use, not an assumed context at the child
// callback. Zero is deliberately not a positive-consumer default.
type ExistsConsumer uint8

const (
	ExistsPositivePredicate ExistsConsumer = 1 << iota
	ExistsNegativePredicate
	ExistsProjectedValue
)

// AdmitConsumer returns a copy carrying a proof for this use. Every use must
// satisfy the edge's constraint before its owner publishes the attachment.
func (e ExistsSubquery) AdmitConsumer(use ExistsConsumer) (ExistsSubquery, error) {
	if use != ExistsPositivePredicate && use != ExistsNegativePredicate && use != ExistsProjectedValue {
		return e, fmt.Errorf("EXISTS has an unclassified consumer")
	}
	switch e.Constraint {
	case ExistsAnyConsumer:
	case ExistsPositivePredicateOnly:
		if use != ExistsPositivePredicate {
			return e, fmt.Errorf("EXISTS over a nested-EXISTS subquery with an outer-only conjunct requires positive predicate consumption")
		}
	default:
		return e, fmt.Errorf("EXISTS has an unknown consumer constraint")
	}
	e.consumers |= use
	return e, nil
}

// ValidateAdmission checks the construction proof without reclassifying Values
// in the translator. Programmatic constrained edges must also call AdmitConsumer
// with an explicit resolved context; omitting it cannot bypass admission.
func (e ExistsSubquery) ValidateAdmission() error {
	if e.Constraint == ExistsAnyConsumer {
		return nil
	}
	if e.Constraint != ExistsPositivePredicateOnly || e.consumers != ExistsPositivePredicate {
		return fmt.Errorf("EXISTS consumer constraint has not been admitted by its owning clause")
	}
	return nil
}
