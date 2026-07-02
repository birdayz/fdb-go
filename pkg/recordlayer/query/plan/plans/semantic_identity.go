package plans

import (
	"encoding/binary"
	"io"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// semanticValueEquals is the Value-comparison primitive for plan-level
// EqualsWithoutChildren (RFC-176 P2): values.SemanticEqualsUnderAliasMap under
// the EMPTY alias map. Plan identity is keyed on the semantic structure of the
// plan's result/projection Values — Java's model (RecordQueryMapPlan.
// equalsWithoutChildren → semanticEqualsForResults) — never on explain
// renderings, which are for humans and carry no identity guarantee.
//
// The empty map is deliberate: plan-level EqualsWithoutChildren has no map
// parameter, the physical wrappers receive one and discard it
// (physical_map_wrapper.go), and the memo interns leaves under the empty map
// (memo.go) — so empty-map comparison preserves the alias-literal
// discrimination the previous rendering compare had (an unmapped alias maps to
// itself). Threading the wrappers' actual alias maps down (Java-faithful
// alias-AWARE physical dedup) changes memo unification and is a named
// follow-up of RFC-176 §3, not part of the P2 migration.
func semanticValueEquals(a, b values.Value) bool {
	return values.SemanticEqualsUnderAliasMap(a, b, nil)
}

// writeValueHash folds a Value's alias-invariant values.SemanticHashCode into
// a plan's HashCodeWithoutChildren stream as 8 fixed-width big-endian bytes
// (fixed width, so no separator is needed between consecutive Values). Pairs
// with semanticValueEquals: semantic equality under ANY alias map implies
// equal SemanticHashCode, so the plan-level equal⟹same-hash memo invariant
// holds by construction. The hash stays alias-invariant while equality is
// alias-map-relative — alias-renamed twins share a hash bucket and equality
// under the caller's map decides; a hash-first memo lookup can therefore
// never miss an equal member. nil is well-defined (SemanticHashCode folds a
// distinct nil token), so callers fold optional Values unconditionally.
func writeValueHash(w io.Writer, v values.Value) {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], values.SemanticHashCode(v))
	_, _ = w.Write(buf[:])
}
