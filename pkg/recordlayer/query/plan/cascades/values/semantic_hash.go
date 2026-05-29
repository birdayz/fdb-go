package values

import (
	"fmt"
	"hash/fnv"
	"io"
	"strconv"
)

// SemanticHashCode returns an ALIAS-INVARIANT structural hash of a Value: the
// contract (Java Correlated.semanticHashCode) is
//
//	SemanticEqualsUnderAliasMap(a, b, m)  ⟹  SemanticHashCode(a) == SemanticHashCode(b)
//
// for ANY alias map m — so the hash must NOT depend on specific quantifier-alias
// names. Correlation-bearing leaf Values (QuantifiedObjectValue, QuantifiedRecord,
// Object, ConstantObject, Exists, ScalarSubquery, UnmatchedAggregate,
// IndexEntryObject, JoinMerge) hash to a per-type tag with the alias EXCLUDED;
// value-bearing leaves (ConstantValue, BooleanValue, ParameterValue) fold their
// literal; structural Values fold a type tag + children.
//
// Lives in the values package (RFC-040 040.1b relocation) so both expressions
// (for relational EqualsWithoutChildren/HashCodeWithoutChildren, 040.2) and
// cascades (memoEqual) can use it without an import cycle. Inert until those
// call sites switch to it.
func SemanticHashCode(v Value) uint64 {
	h := fnv.New64a()
	writeSemanticHash(h, v)
	return h.Sum64()
}

func writeSemanticHash(h io.Writer, v Value) {
	if v == nil {
		_, _ = io.WriteString(h, "<nil>")
		return
	}
	switch t := v.(type) {
	// Correlation-bearing leaves: per-type tag ONLY, alias excluded.
	case *QuantifiedObjectValue:
		_, _ = io.WriteString(h, "qov")
	case *QuantifiedRecordValue:
		_, _ = io.WriteString(h, "qrv")
	case *ObjectValue:
		_, _ = io.WriteString(h, "obj")
	case *ConstantObjectValue:
		_, _ = io.WriteString(h, "cov")
	case *ExistsValue:
		_, _ = io.WriteString(h, "exists")
	case *ScalarSubqueryValue:
		_, _ = io.WriteString(h, "scalarsubquery")
	case *UnmatchedAggregateValue:
		_, _ = io.WriteString(h, "unmatchedagg")
	case *IndexEntryObjectValue:
		_, _ = io.WriteString(h, "indexentryobj")
	case *JoinMergeResultValue:
		_, _ = io.WriteString(h, "joinmerge")
	case *FieldValue:
		_, _ = io.WriteString(h, "field:"+t.Field)
	// Value-bearing leaves: the literal MUST be in the hash (their
	// EqualsWithoutChildren distinguishes different literals).
	case *ConstantValue:
		_, _ = fmt.Fprintf(h, "const:%T=%v", t.Value, t.Value)
	case *BooleanValue:
		if t.Value == nil {
			_, _ = io.WriteString(h, "bool:nil")
		} else {
			_, _ = fmt.Fprintf(h, "bool:%v", *t.Value)
		}
	case *ParameterValue:
		_, _ = fmt.Fprintf(h, "param:%d:%s", t.Ordinal, t.ParamName)
	case *NullValue:
		_, _ = io.WriteString(h, "null")
	default:
		_, _ = io.WriteString(h, "v:"+v.Name())
	}
	children := v.Children()
	_, _ = io.WriteString(h, "(")
	_, _ = io.WriteString(h, strconv.Itoa(len(children)))
	for _, c := range children {
		_, _ = io.WriteString(h, ",")
		writeSemanticHash(h, c)
	}
	_, _ = io.WriteString(h, ")")
}
