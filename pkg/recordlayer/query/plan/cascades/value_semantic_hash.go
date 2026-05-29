package cascades

import (
	"hash/fnv"
	"io"
	"strconv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// ValueSemanticHashCode returns an ALIAS-INVARIANT structural hash of a
// Value: the contract (Java Correlated.semanticHashCode) is
//
//	ValueSemanticEquals(a, b, veq).IsTrue()  ⟹  ValueSemanticHashCode(a) == ValueSemanticHashCode(b)
//
// for ANY ValueEquivalence veq — so the hash must NOT depend on specific
// quantifier-alias names. Correlation-bearing leaf Values (QuantifiedObject,
// QuantifiedRecord, Object, ExistsValue, …) are veq-equal to a same-type peer
// under the right alias map, so they hash to a per-type tag with the alias
// EXCLUDED. Structural Values fold a type tag + their children's semantic
// hashes.
//
// This is the 040.0 sub-foundation of RFC-040: it does not exist in Go today
// (Values have no hash method). It is inert until predicate/expression hashing
// is switched to use it (RFC-040 040.2..N) — building it first lets that
// switch be consistency-fuzzed.
//
// NOTE (RFC-040 completeness audit): the structural `default` arm folds
// Name()+children, which is consistent for every Value whose equality is
// alias-INSENSITIVE. The only soundness requirement is that EVERY
// correlation-bearing Value type is handled explicitly below (alias excluded).
// The exhaustive per-type audit against the `values.EqualsWithoutChildren`
// switch + a registry/completeness test + the consistency fuzz are 040.0's
// completion criteria.
func ValueSemanticHashCode(v values.Value) uint64 {
	h := fnv.New64a()
	writeValueSemanticHash(h, v)
	return h.Sum64()
}

func writeValueSemanticHash(h io.Writer, v values.Value) {
	if v == nil {
		_, _ = io.WriteString(h, "<nil>")
		return
	}
	// Correlation-bearing leaves: hash a per-type tag ONLY — the alias is
	// excluded so two same-type references hash equal (they are veq-equal
	// under the appropriate AliasMap). Mirrors Java's BASE_HASH-only
	// hashCodeWithoutChildren on QuantifiedObjectValue et al.
	switch v.(type) {
	case *values.QuantifiedObjectValue:
		_, _ = io.WriteString(h, "qov")
	case *values.QuantifiedRecordValue:
		_, _ = io.WriteString(h, "qrv")
	case *values.ObjectValue:
		_, _ = io.WriteString(h, "obj")
	case *values.ConstantObjectValue:
		_, _ = io.WriteString(h, "cov")
	case *values.ExistsValue:
		_, _ = io.WriteString(h, "exists")
	case *values.ScalarSubqueryValue:
		_, _ = io.WriteString(h, "scalarsubquery")
	case *values.UnmatchedAggregateValue:
		_, _ = io.WriteString(h, "unmatchedagg")
	case *values.IndexEntryObjectValue:
		_, _ = io.WriteString(h, "indexentryobj")
	case *values.JoinMergeResultValue:
		_, _ = io.WriteString(h, "joinmerge")
	case *values.FieldValue:
		// Field path is alias-invariant; the (possibly QOV) child is
		// folded below via Children().
		fv := v.(*values.FieldValue)
		_, _ = io.WriteString(h, "field:"+fv.Field)
	default:
		// Structural Values: a type tag (Name) keeps types apart; the
		// concrete shape (operator, constant, etc.) is distinguished by
		// the tag + children. Consistent for alias-INSENSITIVE equality.
		_, _ = io.WriteString(h, "v:"+v.Name())
	}
	children := v.Children()
	_, _ = io.WriteString(h, "(")
	_, _ = io.WriteString(h, strconv.Itoa(len(children)))
	for _, c := range children {
		_, _ = io.WriteString(h, ",")
		writeValueSemanticHash(h, c)
	}
	_, _ = io.WriteString(h, ")")
}
