package predicates

import (
	"hash/fnv"
	"io"
	"strconv"

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
		// Escape IS a discriminator (equality compares it, e.g. LIKE … ESCAPE).
		// The text-search comparand fields fold because both equality layers
		// compare them (see PredicateEquals). Length-delimit the strings so
		// "ab"+"c" cannot collide with "a"+"bc".
		// ParameterName is length-delimited so a name ending in a digit
		// cannot bleed into the escape-rune fold.
		_, _ = io.WriteString(h, "cp:"+strconv.Itoa(int(t.Comparison.Type))+":"+strconv.Itoa(len(t.Comparison.ParameterName))+":"+t.Comparison.ParameterName+":"+string(t.Comparison.Escape)+":")
		_, _ = io.WriteString(h, strconv.Itoa(len(t.Comparison.TextTokenizerName))+":"+t.Comparison.TextTokenizerName+":")
		_, _ = io.WriteString(h, strconv.Itoa(len(t.Comparison.TextAnalyzerName))+":"+t.Comparison.TextAnalyzerName+":")
		_, _ = io.WriteString(h, strconv.Itoa(t.Comparison.TextMaxDistance)+":")
		_, _ = io.WriteString(h, strconv.FormatBool(t.Comparison.TextStrictPrefix)+":")
		// DistanceRank comparands fold because both equality layers compare
		// them; the optional knobs fold a presence marker so nil ("index
		// default") and an explicit value cannot collide.
		_, _ = io.WriteString(h, strconv.FormatUint(values.SemanticHashCode(t.Comparison.QueryVector), 16)+":")
		if t.Comparison.EfSearch != nil {
			_, _ = io.WriteString(h, "e"+strconv.Itoa(*t.Comparison.EfSearch)+":")
		} else {
			_, _ = io.WriteString(h, "-:")
		}
		if t.Comparison.IsReturningVectors != nil {
			_, _ = io.WriteString(h, "r"+strconv.FormatBool(*t.Comparison.IsReturningVectors)+":")
		} else {
			_, _ = io.WriteString(h, "-:")
		}
		_, _ = io.WriteString(h, strconv.FormatUint(values.SemanticHashCode(t.Operand), 16))
		_, _ = io.WriteString(h, "/")
		// Unary comparisons (IS [NOT] NULL) ignore Comparison.Operand at Eval
		// time and BOTH equality layers treat nil and Literal(nil) operands as
		// equivalent — folding the operand hash here split equal predicates
		// across buckets (equal⟹same-hash violation). Fold a fixed token.
		if t.Comparison.Type.IsUnary() {
			_, _ = io.WriteString(h, "u")
		} else {
			_, _ = io.WriteString(h, strconv.FormatUint(values.SemanticHashCode(t.Comparison.Operand), 16))
		}
	case *ExistentialValuePredicate:
		// QuantifiedObjectValue operand's alias EXCLUDED — alias-invariant.
		// The operand's value hash (qov tag, alias-free) folds in too.
		_, _ = io.WriteString(h, "existential:"+strconv.FormatUint(values.SemanticHashCode(t.Value), 16))
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
