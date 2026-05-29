package predicates

import (
	"hash/fnv"
	"io"
	"strconv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// SemanticHashCode returns an ALIAS-INVARIANT structural hash of a
// QueryPredicate, consistent with alias-aware predicate equality
// (SemanticEqualsUnderAliasMap). Value-bearing predicates fold their Values
// via the alias-invariant values.SemanticHashCode; ExistsPredicate's alias is
// EXCLUDED; compound predicates recurse via Children() (NOT alias-bearing
// Explain() text).
//
// Lives in the predicates package (RFC-040 040.1b relocation) so expressions
// (relational HashCodeWithoutChildren, 040.2) and cascades (memoEqual) can use
// it without an import cycle.
func SemanticHashCode(p QueryPredicate) uint64 {
	h := fnv.New64a()
	writeSemanticHash(h, p)
	return h.Sum64()
}

func writeSemanticHash(h io.Writer, p QueryPredicate) {
	if p == nil {
		_, _ = io.WriteString(h, "<nilp>")
		return
	}
	switch t := p.(type) {
	case *ValuePredicate:
		_, _ = io.WriteString(h, "vp:"+strconv.FormatUint(values.SemanticHashCode(t.Value), 16))
	case *ComparisonPredicate:
		_, _ = io.WriteString(h, "cp:"+strconv.Itoa(int(t.Comparison.Type))+":"+t.Comparison.ParameterName+":")
		_, _ = io.WriteString(h, strconv.FormatUint(values.SemanticHashCode(t.Operand), 16))
		_, _ = io.WriteString(h, "/")
		_, _ = io.WriteString(h, strconv.FormatUint(values.SemanticHashCode(t.Comparison.Operand), 16))
	case *ExistsPredicate:
		// ExistentialAlias EXCLUDED — alias-invariant.
		_, _ = io.WriteString(h, "exists")
	case *AndPredicate:
		_, _ = io.WriteString(h, "and")
	case *OrPredicate:
		_, _ = io.WriteString(h, "or")
	case *NotPredicate:
		_, _ = io.WriteString(h, "not")
	default:
		// Non-alias-bearing predicate types: Explain() is a stable
		// structural discriminator.
		_, _ = io.WriteString(h, "p:"+p.Explain())
	}
	_, _ = io.WriteString(h, "[")
	for _, c := range p.Children() {
		_, _ = io.WriteString(h, ";")
		writeSemanticHash(h, c)
	}
	_, _ = io.WriteString(h, "]")
}
