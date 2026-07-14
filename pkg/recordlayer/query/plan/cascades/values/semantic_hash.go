package values

import (
	"fmt"
	"hash/fnv"
	"io"
	"strconv"
	"strings"
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

// SelfSemanticHash lets a Value implemented outside this package contribute its
// own discriminator to the semantic hash — the hash analog of
// SelfEqualsWithoutChildren (a type the writeSemanticHash switch can't reach
// would otherwise collide into the bare Name() bucket). The returned value MUST be
// derived from the same non-child attributes EqualsWithoutChildrenValue compares
// (and be alias-free), so the equal⟹same-hash memo invariant holds.
type SelfSemanticHash interface {
	SemanticHashDiscriminator() uint64
}

// ptrHashToken renders an optional-config pointer field for writeSemanticHash:
// "nil" when unset, the pointee otherwise — keeps nil vs &v distinct in the
// hash exactly as ptrEqual keeps them distinct in equality (RFC-176 §3). "nil"
// cannot collide with a rendered pointee: the config fields are *int / *bool,
// whose renderings are digits / true / false — enforced by the constraint, not this comment.
func ptrHashToken[T int | bool](p *T) string {
	if p == nil {
		return "nil"
	}
	return fmt.Sprint(*p)
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
		// alias excluded; ConstantID IS a discriminator (equality compares it).
		_, _ = io.WriteString(h, "cov:"+t.ConstantID)
	case *ExistsValue:
		// Transparent composite (RFC-141): type tag here, the child
		// QuantifiedObjectValue is folded by the Children() loop in the
		// common tail (alias-excluded, so the hash stays alias-invariant).
		_, _ = io.WriteString(h, "exists")
	case *ScalarSubqueryValue:
		_, _ = io.WriteString(h, "scalarsubquery")
	case *UnmatchedAggregateValue:
		_, _ = io.WriteString(h, "unmatchedagg")
	case *IndexEntryObjectValue:
		// alias excluded; Source (KEY/VALUE/OTHER) AND OrdinalPath are both
		// discriminators (equality compares both). Source must be folded so
		// KEY[p] and VALUE[p] — which Evaluate resolves against different
		// tuples — do not collide in the memo, matching Java's planHash which
		// folds (ordinalPath, source).
		_, _ = fmt.Fprintf(h, "indexentryobj:%d:%v", t.Source, t.OrdinalPath)
	// Structural types whose EqualsWithoutChildren compares a non-alias
	// discriminator (op / target type / name) the bare Name() default would
	// drop — fold it so the hash matches equality's resolution (RFC-040
	// hash-quality). All alias-free.
	case *ArithmeticValue:
		_, _ = fmt.Fprintf(h, "arith:%v", t.Op)
	case *AggregateValue:
		_, _ = fmt.Fprintf(h, "agg:%v", t.Op)
	case *AndOrValue:
		_, _ = fmt.Fprintf(h, "andor:%v", t.Op)
	case *IndexOnlyAggregateValue:
		_, _ = fmt.Fprintf(h, "idxagg:%v", t.Op)
	case *ScalarFunctionValue:
		_, _ = io.WriteString(h, "scalarfn:"+t.FuncName)
	case *CastValue:
		_, _ = fmt.Fprintf(h, "cast:%v", t.Target)
	case *PromoteValue:
		_, _ = fmt.Fprintf(h, "promote:%v", t.Target)
	case *ThrowsValue:
		_, _ = fmt.Fprintf(h, "throws:%v", t.ResultType)
	case *RecordConstructorValue:
		_, _ = io.WriteString(h, "record:")
		for _, f := range t.Fields {
			_, _ = io.WriteString(h, f.Name+",")
		}
	case *FieldValue:
		// RFC-173 S3-W3: a BAKED node's identity is its ordinal PATH alone
		// (Java ResolvedAccessor.equals compares getOrdinal() only,
		// FieldValue.java:675-689; the display name is rendering, not
		// identity) — so the hash folds ONLY the per-step ordinals. Mixing
		// the name in (as the coexistence window did) would break
		// equal ⟹ same-hash for the alias-mapped twins the flip makes equal.
		// Lazy nodes keep the name bucket ('#' doubled, same escape as
		// ExplainValue, so a field literally named "X#0" cannot collide with
		// an ordinal discriminator); baked vs lazy are UNEQUAL by contract,
		// and their prefixes differ. FrontierPinned is NOT hashed (excluded
		// from identity).
		if t.Resolved != nil {
			_, _ = io.WriteString(h, "fieldpath:")
			for _, acc := range t.Resolved.Accessors {
				_, _ = fmt.Fprintf(h, "#%d", acc.Ordinal)
			}
		} else {
			_, _ = io.WriteString(h, "field:"+strings.ReplaceAll(t.Field, "#", "##"))
		}
	// Windowed/vector family (RFC-176 P1): fold the same discriminator set the
	// EqualsWithoutChildren arms compare — Metric + EfSearch +
	// IsReturningVectors for DistanceRowNumberValue, EfSearch +
	// IsReturningVectors for RowNumberValue / RowNumberHighOrderValue — so
	// hash and equality resolve identity at the same granularity.
	case *DistanceRowNumberValue:
		// Before this arm existed, the generic "v:"+Name() bucket was FINER
		// than equality for this type (RFC-176 §2): Name() embeds the Metric
		// while the equality arm was type-assertion-only, so two
		// different-metric values compared equal yet hashed apart — a live
		// equal⟹same-hash violation (a hash-first memo lookup misses the
		// "equal" member and inserts a duplicate). Fixed by tightening both
		// sides to the same discriminator set in one commit.
		_, _ = fmt.Fprintf(h, "distrownum:%d:ef=%s:rv=%s",
			t.Metric, ptrHashToken(t.EfSearch), ptrHashToken(t.IsReturningVectors))
	case *RowNumberValue:
		_, _ = fmt.Fprintf(h, "rownum:ef=%s:rv=%s",
			ptrHashToken(t.EfSearch), ptrHashToken(t.IsReturningVectors))
	case *RowNumberHighOrderValue:
		_, _ = fmt.Fprintf(h, "rownumho:ef=%s:rv=%s",
			ptrHashToken(t.EfSearch), ptrHashToken(t.IsReturningVectors))
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
		// A Value defined outside this package (the type switch can't reach it)
		// can fold its own discriminator via SelfSemanticHash — the hash analog of
		// SelfEqualsWithoutChildren. Without it every such instance shares the bare
		// Name() bucket (e.g. all predicateValues → "v:predicate"), degrading memo
		// lookup. equal⟹same-hash holds iff the discriminator is consistent with
		// EqualsWithoutChildrenValue (the implementer's contract).
		if sh, ok := v.(SelfSemanticHash); ok {
			_, _ = fmt.Fprintf(h, "v:%s:%x", v.Name(), sh.SemanticHashDiscriminator())
		} else {
			_, _ = io.WriteString(h, "v:"+v.Name())
		}
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
