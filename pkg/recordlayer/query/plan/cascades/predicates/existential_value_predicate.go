package predicates

import (
	"hash/fnv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// ExistentialValuePredicate is the QueryPredicate-layer SQL EXISTS,
// ported from Java 4.12's
// `com.apple.foundationdb.record.query.plan.cascades.predicates.
// ExistentialValuePredicate`. It is "the existential quantifier's
// object is non-null ⇒ the subplan yielded ≥1 row".
//
// In Java it `extends ValuePredicate(value, comparison)` where value is
// a QuantifiedObjectValue and comparison is NullComparison(NOT_NULL).
// Go's ValuePredicate is a bare boolean-Value wrapper (no comparison),
// so the Java (value, comparison) pair maps to the same shape Go's
// ComparisonPredicate uses: an operand Value + a Comparison. This type
// carries exactly that, constrained to a *QuantifiedObjectValue operand
// and a ComparisonIsNotNull comparison.
//
//	EXISTS (SELECT ... FROM t WHERE ...)
//	  ↔  ExistentialValuePredicate{Value: QOV{αsubq}, NOT_NULL}
//
// This is the SINGLE EXISTS predicate representation (RFC-141): it
// replaces the deleted leaf-alias ExistsPredicate. A NOT-EXISTS is
// NotPredicate(ExistentialValuePredicate).
//
// Eval is a per-row UNKNOWN at this layer — actual EXISTS evaluation
// requires running the subplan, which the planner's semi-join rules
// (ImplementNestedLoopJoinRule) handle structurally. The QOV operand
// would eval to the bound existential row only inside the FlatMap/
// semi-join cursor, not in the generic per-row predicate eval.
type ExistentialValuePredicate struct {
	// Value is the operand — always a *QuantifiedObjectValue over the
	// existential quantifier. Kept as values.Value to mirror Java's
	// ValuePredicate.getValue() and because helpers expect the interface.
	Value values.Value
	// Comparison is always ComparisonIsNotNull (the NullComparison(NOT_NULL)).
	Comparison Comparison
}

// MustNewExistentialValuePredicate constructs the predicate. The value MUST
// be a *QuantifiedObjectValue; the `Must` prefix is the documented panic
// contract (Go's regexp.MustCompile convention, mapping to Java's
// Verify.verify(value instanceof QuantifiedObjectValue)) — a non-QOV operand
// is a programming error, not a runtime condition. Every call site holds a
// QOV by construction (a rebased / mapped / decorrelated QOV stays a QOV, and
// ExistsValueToQueryPredicate passes the ExistsValue's QOV child), so the
// precondition is a caller-guaranteed structural invariant, not input
// validation — which is why this asserts rather than returns an error.
func MustNewExistentialValuePredicate(value values.Value, comparison Comparison) *ExistentialValuePredicate {
	if _, ok := value.(*values.QuantifiedObjectValue); !ok {
		panic("MustNewExistentialValuePredicate: value must be a *QuantifiedObjectValue")
	}
	return &ExistentialValuePredicate{Value: value, Comparison: comparison}
}

// NewExistentialAlias is the convenience constructor over an existential
// alias: wraps it in a QuantifiedObjectValue with a NOT_NULL comparison.
func NewExistentialAlias(alias values.CorrelationIdentifier) *ExistentialValuePredicate {
	return &ExistentialValuePredicate{
		Value:      values.NewQuantifiedObjectValue(alias),
		Comparison: Comparison{Type: ComparisonIsNotNull},
	}
}

// GetExistentialAlias returns the QuantifiedObjectValue operand's
// correlation — the alias bound to the subquery. Mirrors Java's
// getQuantifierAlias().
func (p *ExistentialValuePredicate) GetExistentialAlias() values.CorrelationIdentifier {
	if qov, ok := p.Value.(*values.QuantifiedObjectValue); ok {
		return qov.Correlation
	}
	return values.CorrelationIdentifier{}
}

// Children returns the empty slice — leaf in the predicate tree (the
// QuantifiedObjectValue operand is a carried Value, not a child predicate).
func (*ExistentialValuePredicate) Children() []QueryPredicate { return []QueryPredicate{} }

// Eval returns TriUnknown — EXISTS isn't evaluated at the per-row
// predicate level; the planner's semi-join rules do the row-level test.
func (*ExistentialValuePredicate) Eval(any) (TriBool, error) { return TriUnknown, nil }

// GetCorrelatedTo returns the correlations of the operand Value — the
// existential alias.
func (p *ExistentialValuePredicate) GetCorrelatedTo() map[values.CorrelationIdentifier]struct{} {
	out := values.GetCorrelatedToOfValue(p.Value)
	if out == nil {
		return map[values.CorrelationIdentifier]struct{}{}
	}
	return out
}

// HashCodeWithoutChildren hashes the predicate kind. The operand alias is
// EXCLUDED (alias-invariant), consistent with the alias-invariant
// SemanticHashCode for the carried QuantifiedObjectValue.
func (p *ExistentialValuePredicate) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("existential|"))
	return h.Sum64()
}

// Explain renders the SQL-ish form.
func (p *ExistentialValuePredicate) Explain() string {
	return "EXISTS(" + p.GetExistentialAlias().Name() + ")"
}

var _ QueryPredicate = (*ExistentialValuePredicate)(nil)

// ExistsValueToQueryPredicate is the bridge from the value-layer EXISTS
// (values.ExistsValue) to the predicate-layer EXISTS, mirroring Java's
// ExistsValue.toQueryPredicate() → ExistentialValuePredicate(child,
// NullComparison(NOT_NULL)). It lives in the predicates package because
// the values package cannot import predicates (import cycle: predicates
// imports values). The child must be a *QuantifiedObjectValue.
func ExistsValueToQueryPredicate(ev *values.ExistsValue) QueryPredicate {
	return MustNewExistentialValuePredicate(ev.GetChild(), Comparison{Type: ComparisonIsNotNull})
}

// IsExistentialPredicate reports whether p is the existential semi-join
// shape — an ExistentialValuePredicate (a QuantifiedObjectValue operand
// with a NOT_NULL comparison) — and if so returns the existential alias.
// This is the single detection point all EXISTS call sites use, instead
// of type-switching on a dedicated leaf type (RFC-141: one mechanism).
//
// It also recognises a bare ComparisonPredicate with the same shape (QOV
// operand + ComparisonIsNotNull), which is what ExistentialValuePredicate
// lowers to as a residual predicate (Java's toResidualPredicate).
func IsExistentialPredicate(p QueryPredicate) (values.CorrelationIdentifier, bool) {
	switch pred := p.(type) {
	case *ExistentialValuePredicate:
		// An existential semi-join is specifically "the existential QOV IS NOT NULL". The
		// exported constructor permits any comparison (mirroring Java's), so guard on NOT_NULL
		// here too — consistent with the *ComparisonPredicate branch — lest an IS NULL (or other)
		// comparison over the QOV be mis-planned as a positive EXISTS.
		if pred.Comparison.Type != ComparisonIsNotNull {
			return values.CorrelationIdentifier{}, false
		}
		return pred.GetExistentialAlias(), true
	case *ComparisonPredicate:
		if pred.Comparison.Type != ComparisonIsNotNull {
			return values.CorrelationIdentifier{}, false
		}
		if qov, ok := pred.Operand.(*values.QuantifiedObjectValue); ok {
			return qov.Correlation, true
		}
	}
	return values.CorrelationIdentifier{}, false
}

// IsNotExistentialPredicate reports whether p is NotPredicate wrapping an
// existential predicate (the NOT EXISTS shape), returning the alias.
func IsNotExistentialPredicate(p QueryPredicate) (values.CorrelationIdentifier, bool) {
	not, ok := p.(*NotPredicate)
	if !ok {
		return values.CorrelationIdentifier{}, false
	}
	ch := not.Children()
	if len(ch) != 1 {
		return values.CorrelationIdentifier{}, false
	}
	return IsExistentialPredicate(ch[0])
}

// ContainsExistentialPredicate reports whether the predicate tree rooted at p
// contains an existential semi-join predicate ANYWHERE in its subtree — an
// *ExistentialValuePredicate, or the bare QuantifiedObjectValue-IS-NOT-NULL
// *ComparisonPredicate it lowers to (the same shape IsExistentialPredicate
// recognises). A structural (typed-node) walk, not text matching.
//
// This is the RFC-141 R4 convergence backstop primitive. EXISTS can
// appear at any depth in a WHERE predicate tree, and only a TOP-LEVEL (or
// single-NOT-wrapped) existential is a directly-handled semi-join shape that
// the NLJ rule lowers to a FirstOrDefault + residual filter. An existential
// buried under any OTHER wrapper — `NOT (NOT EXISTS(...))`, `EXISTS(...) OR p`,
// arbitrary AND/OR/NOT nesting that is not the direct/single-NOT shape — falls
// into the regular-predicate bucket where the empty FirstOrDefault's NULL
// default is never removed and every outer row silently passes. The guard uses
// this to DETECT any such buried existential and REJECT the query cleanly rather
// than mis-evaluate it.
func ContainsExistentialPredicate(p QueryPredicate) bool {
	found := false
	WalkPredicate(p, func(node QueryPredicate) bool {
		if _, ok := IsExistentialPredicate(node); ok {
			found = true
			return false
		}
		return true
	})
	return found
}
