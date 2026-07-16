package plans

import (
	"encoding/binary"
	"io"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
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

// scanComparisonRangesEqual reports whether two per-column comparison-range
// lists are identical in shape AND COMPARAND — the operand values that define
// the index/scan key range. Range SHAPE alone (equality/inequality/empty) is
// NOT identity: `[= 5]` and `[= 7]` share a shape but read DIFFERENT key
// ranges, so collapsing them in the memo (a leaf dedups by
// HashCodeWithoutChildren + EqualsWithoutChildren) materializes the
// wrong-comparand scan. Mirrors Java RecordQueryIndexPlan.equalsWithoutChildren
// → Objects.equals(scanParameters), which compares the FULL scan parameters
// (comparisons + comparands). Operand comparison is the plans-package semantic
// primitive (semanticValueEquals), consistent with every other plan node's
// Value identity.
func scanComparisonRangesEqual(a, b []*predicates.ComparisonRange) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !comparisonRangeEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

func comparisonRangeEqual(a, b *predicates.ComparisonRange) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.GetRangeType() != b.GetRangeType() {
		return false
	}
	switch {
	case a.IsEquality():
		return comparisonEqual(a.GetEqualityComparison(), b.GetEqualityComparison())
	case a.IsInequality():
		ac, bc := a.GetInequalityComparisons(), b.GetInequalityComparisons()
		if len(ac) != len(bc) {
			return false
		}
		for i := range ac {
			if !comparisonEqual(ac[i], bc[i]) {
				return false
			}
		}
		return true
	default: // empty range — the universe, no comparand to distinguish
		return true
	}
}

// comparisonEqual compares two comparisons in the dimensions that define an
// index/scan key range: operator type, LIKE escape, parameter binding, the
// text-search comparand fields, and the comparand operand (semantic,
// alias-invariant-hash-compatible). A parameter-bound comparand may be carried
// in ParameterName (Java's ParameterComparison) or in Operand (a
// ParameterValue); folding both covers either representation.
//
// The text-search fields (Java's TextComparison / TextWithMaxDistanceComparison
// / TextContainsAllPrefixesComparison families) are part of the comparand: two
// TEXT_CONTAINS comparisons over the same field differing ONLY in tokenizer,
// analyzer, max-distance, or strict-prefix read DIFFERENT results, so they are
// distinct identities — the exact F21 memo-poisoning class, for text comparands.
// Java's RecordQueryIndexPlan.equalsWithoutChildren compares
// Objects.equals(scanParameters), i.e. the WHOLE comparand including these
// subclass fields.
//
// The DistanceRank comparand fields (QueryVector / EfSearch / IsReturningVectors)
// are deliberately NOT folded here: a DistanceRank comparison is always lowered
// to a RecordQueryVectorIndexPlan (never carried in a ComparisonRange that
// reaches this helper), and that plan disambiguates them at the plan level
// (RecordQueryVectorIndexPlan.EqualsWithoutChildren compares queryVector via
// ValuesStructurallyEqual, plus efSearch / isReturningVectors) — Go's
// architectural equivalent of Java's DistanceRankValueComparison.equals().
func comparisonEqual(a, b *predicates.Comparison) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Type != b.Type || a.Escape != b.Escape || a.ParameterName != b.ParameterName {
		return false
	}
	if a.TextTokenizerName != b.TextTokenizerName ||
		a.TextAnalyzerName != b.TextAnalyzerName ||
		a.TextMaxDistance != b.TextMaxDistance ||
		a.TextStrictPrefix != b.TextStrictPrefix {
		return false
	}
	return semanticValueEquals(a.Operand, b.Operand)
}

// writeScanComparisonRangesHash folds a comparison-range list into a plan's
// HashCodeWithoutChildren stream, the hash analog of scanComparisonRangesEqual:
// two lists that are scanComparisonRangesEqual fold identically (the
// equal⟹same-hash memo invariant). Each range folds its shape byte AND its
// comparison comparands (via writeValueHash / SemanticHashCode).
func writeScanComparisonRangesHash(w io.Writer, ranges []*predicates.ComparisonRange) {
	for _, cr := range ranges {
		writeComparisonRangeHash(w, cr)
	}
}

func writeComparisonRangeHash(w io.Writer, cr *predicates.ComparisonRange) {
	if cr == nil {
		_, _ = w.Write([]byte{0})
		return
	}
	// Shape byte is offset by 1 so it never collides with the nil marker (0).
	_, _ = w.Write([]byte{byte(cr.GetRangeType()) + 1})
	switch {
	case cr.IsEquality():
		writeComparisonHash(w, cr.GetEqualityComparison())
	case cr.IsInequality():
		for _, c := range cr.GetInequalityComparisons() {
			writeComparisonHash(w, c)
		}
	}
}

func writeComparisonHash(w io.Writer, c *predicates.Comparison) {
	if c == nil {
		_, _ = w.Write([]byte{0})
		return
	}
	var buf [8]byte
	_, _ = w.Write([]byte{1})
	binary.BigEndian.PutUint64(buf[:], uint64(c.Type))
	_, _ = w.Write(buf[:])
	binary.BigEndian.PutUint64(buf[:], uint64(c.Escape))
	_, _ = w.Write(buf[:])
	_, _ = io.WriteString(w, c.ParameterName)
	_, _ = w.Write([]byte{0})
	// Text-search comparand fields — folded so equal⟹same-hash holds with
	// comparisonEqual (which compares this same set). Length-delimited (0 byte
	// terminator) so "ab"+"c" can't collide with "a"+"bc".
	_, _ = io.WriteString(w, c.TextTokenizerName)
	_, _ = w.Write([]byte{0})
	_, _ = io.WriteString(w, c.TextAnalyzerName)
	_, _ = w.Write([]byte{0})
	binary.BigEndian.PutUint64(buf[:], uint64(c.TextMaxDistance))
	_, _ = w.Write(buf[:])
	if c.TextStrictPrefix {
		_, _ = w.Write([]byte{1})
	} else {
		_, _ = w.Write([]byte{0})
	}
	writeValueHash(w, c.Operand)
}
