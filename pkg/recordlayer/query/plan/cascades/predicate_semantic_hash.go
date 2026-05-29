package cascades

import (
	"hash/fnv"
	"io"
	"strconv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
)

// PredicateSemanticHashCode returns an ALIAS-INVARIANT structural hash of a
// QueryPredicate, consistent with alias-aware predicate equality (the same
// contract as ValueSemanticHashCode: semantically-equal-under-some-alias-map
// predicates must hash equal). Value-bearing predicates fold their Values via
// the alias-invariant ValueSemanticHashCode; the alias on ExistsPredicate is
// EXCLUDED; compound predicates recurse via Children() — NOT via Explain(),
// whose text embeds alias names and would make the hash alias-sensitive.
//
// RFC-040 040.0 (predicate half). Inert until predicate HashCodeWithoutChildren
// is switched to it (040.2). Covers the alias-relevant predicate types
// explicitly; the audit + fuzz (extended to predicates) is 040.0's completion.
func PredicateSemanticHashCode(p predicates.QueryPredicate) uint64 {
	h := fnv.New64a()
	writePredicateSemanticHash(h, p)
	return h.Sum64()
}

func writePredicateSemanticHash(h io.Writer, p predicates.QueryPredicate) {
	if p == nil {
		_, _ = io.WriteString(h, "<nilp>")
		return
	}
	switch t := p.(type) {
	case *predicates.ValuePredicate:
		_, _ = io.WriteString(h, "vp:")
		writeValueSemanticHash(h, t.Value)
	case *predicates.ComparisonPredicate:
		_, _ = io.WriteString(h, "cp:"+strconv.Itoa(int(t.Comparison.Type))+":"+t.Comparison.ParameterName+":")
		writeValueSemanticHash(h, t.Operand)
		_, _ = io.WriteString(h, "/")
		writeValueSemanticHash(h, t.Comparison.Operand) // nil for IS [NOT] NULL → "<nil>"
	case *predicates.ExistsPredicate:
		// ExistentialAlias EXCLUDED — alias-invariant.
		_, _ = io.WriteString(h, "exists")
	case *predicates.AndPredicate:
		_, _ = io.WriteString(h, "and")
	case *predicates.OrPredicate:
		_, _ = io.WriteString(h, "or")
	case *predicates.NotPredicate:
		_, _ = io.WriteString(h, "not")
	default:
		// Non-alias-bearing predicate types (no quantifier-alias in their
		// node info): Explain() is a stable structural discriminator.
		_, _ = io.WriteString(h, "p:"+p.Explain())
	}
	// Recurse child predicates (And/Or/Not and any compound).
	_, _ = io.WriteString(h, "[")
	for _, c := range p.Children() {
		_, _ = io.WriteString(h, ";")
		writePredicateSemanticHash(h, c)
	}
	_, _ = io.WriteString(h, "]")
}
