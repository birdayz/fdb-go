package predicates

import (
	"fmt"
	"hash/fnv"
	"io"
	"strconv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// StructuralHash is the hash analog of StructurallyEqual: two predicates that are
// StructurallyEqual produce the same hash (the equal⟹same-hash invariant the memo
// relies on). It mirrors StructurallyEqual's comparison exactly — the same
// discriminators, values.SemanticHashCode for operand Values (itself the hash
// analog of values.ValuesStructurallyEqual), recursion for And/Or/Not, and the
// Explain() fallback StructurallyEqual uses in its own default arm.
//
// KEEP IN SYNC with StructurallyEqual (structural_equal.go): any new case there
// needs the matching case here, or equal predicates could hash differently.
func StructuralHash(p QueryPredicate) uint64 {
	h := fnv.New64a()
	writeStructuralHash(h, p)
	return h.Sum64()
}

func writeStructuralHash(h io.Writer, p QueryPredicate) {
	if p == nil {
		_, _ = io.WriteString(h, "nil")
		return
	}
	writeU64 := func(u uint64) { _, _ = io.WriteString(h, strconv.FormatUint(u, 16)+":") }
	switch t := p.(type) {
	case *ComparisonPredicate:
		_, _ = fmt.Fprintf(h, "cmp:%v:%v:", t.Comparison.Type, t.Comparison.Escape)
		writeU64(values.SemanticHashCode(t.Operand))
		writeU64(values.SemanticHashCode(t.Comparison.Operand))
	case *ValuePredicate:
		_, _ = io.WriteString(h, "vp:")
		writeU64(values.SemanticHashCode(t.Value))
	case *ConstantPredicate:
		_, _ = fmt.Fprintf(h, "cp:%v", t.Value)
	case *ExistentialValuePredicate:
		_, _ = fmt.Fprintf(h, "evp:%v:", t.Comparison.Type)
		writeU64(values.SemanticHashCode(t.Value))
	case *Placeholder:
		_, _ = fmt.Fprintf(h, "ph:%v:", t.ParameterAlias)
		writeU64(values.SemanticHashCode(t.Value))
	case *AndPredicate:
		_, _ = io.WriteString(h, "and(")
		for _, sp := range t.SubPredicates {
			writeStructuralHash(h, sp)
			_, _ = io.WriteString(h, ",")
		}
		_, _ = io.WriteString(h, ")")
	case *OrPredicate:
		_, _ = io.WriteString(h, "or(")
		for _, sp := range t.SubPredicates {
			writeStructuralHash(h, sp)
			_, _ = io.WriteString(h, ",")
		}
		_, _ = io.WriteString(h, ")")
	case *NotPredicate:
		_, _ = io.WriteString(h, "not(")
		writeStructuralHash(h, t.Child)
		_, _ = io.WriteString(h, ")")
	default:
		// StructurallyEqual's default arm compares Explain() == Explain(), so the
		// hash must fold Explain() to stay consistent for these predicates.
		_, _ = io.WriteString(h, "expl:"+p.Explain())
	}
}
