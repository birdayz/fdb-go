package predicates

import (
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// PredicateWithValueAndRanges associates a Value with a set of
// RangeConstraints. The set represents a disjunction of range
// conjunctions — effectively a boolean predicate in DNF form on the
// associated Value.
//
// Used primarily for index matching: on the query side to represent a
// sargable (search-argument-able) predicate, on the candidate side to
// represent column constraints.
//
// Ports Java's
// `com.apple.foundationdb.record.query.plan.cascades.predicates.PredicateWithValueAndRanges`.
type PredicateWithValueAndRanges struct {
	value  values.Value
	ranges []*RangeConstraints
}

// NewPredicateWithValueAndRanges constructs a PredicateWithValueAndRanges.
func NewPredicateWithValueAndRanges(value values.Value, ranges []*RangeConstraints) *PredicateWithValueAndRanges {
	rc := make([]*RangeConstraints, len(ranges))
	copy(rc, ranges)
	return &PredicateWithValueAndRanges{
		value:  value,
		ranges: rc,
	}
}

// GetValue returns the Value associated with the range constraints.
func (p *PredicateWithValueAndRanges) GetValue() values.Value {
	return p.value
}

// GetRanges returns the set of RangeConstraints (disjunction of range
// conjunctions).
func (p *PredicateWithValueAndRanges) GetRanges() []*RangeConstraints {
	return p.ranges
}

// WithValue returns a new PredicateWithValueAndRanges with the given
// Value, keeping the same ranges.
func (p *PredicateWithValueAndRanges) WithValue(v values.Value) *PredicateWithValueAndRanges {
	return NewPredicateWithValueAndRanges(v, p.ranges)
}

// WithRanges returns a new PredicateWithValueAndRanges with the given
// ranges, keeping the same Value.
func (p *PredicateWithValueAndRanges) WithRanges(ranges []*RangeConstraints) *PredicateWithValueAndRanges {
	return NewPredicateWithValueAndRanges(p.value, ranges)
}

// GetComparisons returns all comparisons from all ranges, flattened.
func (p *PredicateWithValueAndRanges) GetComparisons() []Comparison {
	var result []Comparison
	for _, rc := range p.ranges {
		result = append(result, rc.GetComparisons()...)
	}
	return result
}

// GetCorrelatedTo returns the union of correlation identifiers from the
// value and all range constraints.
func (p *PredicateWithValueAndRanges) GetCorrelatedTo() map[values.CorrelationIdentifier]struct{} {
	out := values.GetCorrelatedToOfValue(p.value)
	if out == nil {
		out = map[values.CorrelationIdentifier]struct{}{}
	}
	for _, rc := range p.ranges {
		for alias := range rc.GetCorrelatedTo() {
			out[alias] = struct{}{}
		}
	}
	return out
}

// Explain returns a human-readable representation.
func (p *PredicateWithValueAndRanges) Explain() string {
	var sb strings.Builder
	sb.WriteString(values.ExplainValue(p.value))
	sb.WriteString(" IN {")
	for i, rc := range p.ranges {
		if i > 0 {
			sb.WriteString(" OR ")
		}
		comps := rc.GetComparisons()
		for j, c := range comps {
			if j > 0 {
				sb.WriteString(" AND ")
			}
			sb.WriteString(fmt.Sprintf("%s %v", c.Type.Symbol(), c.Operand))
		}
		if len(comps) == 0 {
			sb.WriteString("*")
		}
	}
	sb.WriteString("}")
	return sb.String()
}

// IsCompileTime reports whether all range constraints are compile-time
// evaluable.
func (p *PredicateWithValueAndRanges) IsCompileTime() bool {
	for _, rc := range p.ranges {
		if !rc.IsCompileTime() {
			return false
		}
	}
	return true
}

func (p *PredicateWithValueAndRanges) Children() []QueryPredicate { return nil }

func (p *PredicateWithValueAndRanges) Eval(_ any) (TriBool, error) { return TriUnknown, nil }

func (p *PredicateWithValueAndRanges) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("predwithvalueandranges|"))
	if p.value != nil {
		h.Write([]byte(values.ExplainValue(p.value)))
	}
	return h.Sum64()
}

var _ QueryPredicate = (*PredicateWithValueAndRanges)(nil)
