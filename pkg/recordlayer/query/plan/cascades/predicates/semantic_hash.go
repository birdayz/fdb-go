package predicates

import (
	"io"
	"slices"
	"strconv"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/internal/fnv64"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// SemanticHashCode returns an ALIAS-INVARIANT structural hash of a
// QueryPredicate, consistent with alias-aware predicate equality
// (SemanticEqualsUnderAliasMap). Value-bearing predicates fold their Values
// via the alias-invariant values.SemanticHashCode; ExistentialValuePredicate's
// operand alias is EXCLUDED; compound predicates recurse via Children() (NOT alias-bearing
// Explain() text).
//
// Lives in the predicates package (RFC-040 040.1b relocation) so expressions
// (relational HashCodeWithoutChildren, 040.2) and cascades (memoEqual) can use
// it without an import cycle.
func SemanticHashCode(p QueryPredicate) uint64 {
	h := fnv64.New()
	writeSemanticHash(h, p)
	return h.Sum64()
}

// ConstantAgnosticHashCode is SemanticHashCode with constants hashed by type.
func ConstantAgnosticHashCode(p QueryPredicate) uint64 {
	h := fnv64.New()
	writeSemanticHash(values.ConstantAgnosticHash{Writer: h}, p)
	return h.Sum64()
}

func writeSemanticSetHash(h io.Writer, children []QueryPredicate) {
	hashes := make([]uint64, len(children))
	for i, child := range children {
		d := fnv64.New()
		if _, agnostic := h.(values.ConstantAgnosticHash); agnostic {
			writeSemanticHash(values.ConstantAgnosticHash{Writer: d}, child)
		} else {
			writeSemanticHash(d, child)
		}
		hashes[i] = d.Sum64()
	}
	slices.Sort(hashes)
	_, _ = io.WriteString(h, "[")
	for _, hash := range slices.Compact(hashes) {
		_, _ = io.WriteString(h, ";")
		fnv64.WriteHex(h, hash)
	}
	_, _ = io.WriteString(h, "]")
}

func writeDelimitedHashString(h io.Writer, value string) {
	fnv64.WriteInt(h, int64(len(value)))
	_, _ = io.WriteString(h, ":")
	_, _ = io.WriteString(h, value)
	_, _ = io.WriteString(h, ":")
}

func writeSemanticHash(h io.Writer, p QueryPredicate) {
	if p == nil {
		_, _ = io.WriteString(h, "<nilp>")
		return
	}
	if IsAtomic(p) {
		_, _ = io.WriteString(h, "atomic:")
	}
	switch t := p.(type) {
	case *ValuePredicate:
		_, _ = io.WriteString(h, "vp:")
		fnv64.WriteHex(h, values.SemanticHashCodeIn(h, t.Value))
	case *ComparisonPredicate:
		// The text-search comparand fields fold because both equality layers
		// compare them (see PredicateEquals). Length-delimit the strings so
		// "ab"+"c" cannot collide with "a"+"bc".
		// ParameterName is length-delimited.
		_, _ = io.WriteString(h, "cp:")
		fnv64.WriteInt(h, int64(t.Comparison.Type))
		_, _ = io.WriteString(h, ":")
		writeDelimitedHashString(h, t.Comparison.ParameterName)
		writeDelimitedHashString(h, t.Comparison.TextTokenizerName)
		writeDelimitedHashString(h, t.Comparison.TextAnalyzerName)
		fnv64.WriteInt(h, int64(t.Comparison.TextMaxDistance))
		_, _ = io.WriteString(h, ":")
		_, _ = io.WriteString(h, strconv.FormatBool(t.Comparison.TextStrictPrefix))
		_, _ = io.WriteString(h, ":")
		// DistanceRank comparands fold because both equality layers compare
		// them; the optional knobs fold a presence marker so nil ("index
		// default") and an explicit value cannot collide.
		fnv64.WriteHex(h, values.SemanticHashCodeIn(h, t.Comparison.QueryVector))
		_, _ = io.WriteString(h, ":")
		if t.Comparison.EfSearch != nil {
			_, _ = io.WriteString(h, "e")
			fnv64.WriteInt(h, int64(*t.Comparison.EfSearch))
			_, _ = io.WriteString(h, ":")
		} else {
			_, _ = io.WriteString(h, "-:")
		}
		if t.Comparison.IsReturningVectors != nil {
			_, _ = io.WriteString(h, "r")
			_, _ = io.WriteString(h, strconv.FormatBool(*t.Comparison.IsReturningVectors))
			_, _ = io.WriteString(h, ":")
		} else {
			_, _ = io.WriteString(h, "-:")
		}
		fnv64.WriteHex(h, values.SemanticHashCodeIn(h, t.Operand))
		_, _ = io.WriteString(h, "/")
		// Unary comparisons (IS [NOT] NULL) ignore Comparison.Operand at Eval
		// time and BOTH equality layers treat nil and Literal(nil) operands as
		// equivalent — folding the operand hash here split equal predicates
		// across buckets (equal⟹same-hash violation). Fold a fixed token.
		if t.Comparison.Type.IsUnary() {
			_, _ = io.WriteString(h, "u")
		} else {
			fnv64.WriteHex(h, values.SemanticHashCodeIn(h, t.Comparison.Operand))
		}
	case *PredicateWithValueAndRanges:
		// Java hashes only the value; ranges are unordered sets under equality.
		_, _ = io.WriteString(h, "ranges:")
		fnv64.WriteHex(h, values.SemanticHashCodeIn(h, t.value))
	case *ExistentialValuePredicate:
		// QuantifiedObjectValue operand's alias EXCLUDED — alias-invariant.
		// The operand's value hash (qov tag, alias-free) folds in too.
		_, _ = io.WriteString(h, "existential:")
		fnv64.WriteHex(h, values.SemanticHashCodeIn(h, t.Value))
	case *AndPredicate:
		_, _ = io.WriteString(h, "and")
		writeSemanticSetHash(h, t.SubPredicates)
		return
	case *OrPredicate:
		_, _ = io.WriteString(h, "or")
		writeSemanticSetHash(h, t.SubPredicates)
		return
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
