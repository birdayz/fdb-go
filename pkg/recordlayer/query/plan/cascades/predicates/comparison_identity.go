package predicates

import (
	"encoding/binary"
	"io"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// comparisonIdentityFields is the list of Comparison fields that participate in
// structural identity, named as strings so TestComparisonIdentityFoldsEveryField
// can check this set against the struct by reflection.
//
// It exists because the set was wrong: StructurallyEqual folded Type, Escape and
// Operand and ignored the other eight, so two TEXT_CONTAINS_ALL predicates
// differing ONLY in tokenizer — which read different index data — compared equal
// and hashed identically. StructuralHash mirrored the same three fields, so the
// equal-implies-same-hash invariant HELD while both sides were wrong, and no
// pairwise test could have seen it.
//
// The same defect was found and fixed one layer up, in plans.comparisonEqual;
// the fix never reached here. A list checked against reflection is what stops
// that happening a third time.
var comparisonIdentityFields = map[string]string{
	"Type":               "the operator itself",
	"Operand":            "the right-hand comparand",
	"Escape":             "LIKE's escape character changes which strings match",
	"ParameterName":      "two bound parameters are different comparisons",
	"TextTokenizerName":  "Java's TextComparison.equals folds tokenizerName; different tokenizers read different index data",
	"TextAnalyzerName":   "as above, the analyzer half",
	"TextMaxDistance":    "TEXT_CONTAINS_ALL_WITHIN's window",
	"TextStrictPrefix":   "changes which prefixes match",
	"QueryVector":        "the vector being searched for",
	"EfSearch":           "search breadth changes which neighbours are returned",
	"IsReturningVectors": "changes the shape of what the comparison yields",
}

// comparisonIdentityEqual reports whether two Comparisons denote the same
// comparison. Every field in comparisonIdentityFields is folded; adding a field
// to Comparison without adding it here fails
// TestComparisonIdentityFoldsEveryField.
func comparisonIdentityEqual(a, b Comparison) bool {
	if a.Type != b.Type ||
		a.Escape != b.Escape ||
		a.ParameterName != b.ParameterName {
		return false
	}
	if a.TextTokenizerName != b.TextTokenizerName ||
		a.TextAnalyzerName != b.TextAnalyzerName ||
		a.TextMaxDistance != b.TextMaxDistance ||
		a.TextStrictPrefix != b.TextStrictPrefix {
		return false
	}
	if !intPtrEqual(a.EfSearch, b.EfSearch) ||
		!boolPtrEqual(a.IsReturningVectors, b.IsReturningVectors) {
		return false
	}
	if !values.ValuesStructurallyEqual(a.QueryVector, b.QueryVector) {
		return false
	}
	return values.ValuesStructurallyEqual(a.Operand, b.Operand)
}

// intPtrEqual and boolPtrEqual compare POINTEES, not pointers. EfSearch and
// IsReturningVectors are pointers to carry three-state presence, so comparing
// the pointers themselves would call two separately-built comparisons with the
// same setting unequal and defeat memo dedup entirely.
func intPtrEqual(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func boolPtrEqual(a, b *bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// writeComparisonIdentity folds the same field set into a hash stream, so that
// comparisonIdentityEqual implies an identical fold. Strings are terminated
// with a 0 byte rather than concatenated, so "ab"+"c" cannot collide with
// "a"+"bc".
func writeComparisonIdentity(h io.Writer, c Comparison) {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(c.Type))
	_, _ = h.Write(buf[:])
	binary.BigEndian.PutUint64(buf[:], uint64(c.Escape))
	_, _ = h.Write(buf[:])
	writeDelimited(h, c.ParameterName)
	writeDelimited(h, c.TextTokenizerName)
	writeDelimited(h, c.TextAnalyzerName)
	binary.BigEndian.PutUint64(buf[:], uint64(c.TextMaxDistance))
	_, _ = h.Write(buf[:])
	writeTriByte(h, &c.TextStrictPrefix)
	if c.EfSearch == nil {
		_, _ = h.Write([]byte{0})
	} else {
		_, _ = h.Write([]byte{1})
		binary.BigEndian.PutUint64(buf[:], uint64(*c.EfSearch))
		_, _ = h.Write(buf[:])
	}
	writeTriByte(h, c.IsReturningVectors)
	binary.BigEndian.PutUint64(buf[:], values.SemanticHashCode(c.QueryVector))
	_, _ = h.Write(buf[:])
	binary.BigEndian.PutUint64(buf[:], values.SemanticHashCode(c.Operand))
	_, _ = h.Write(buf[:])
}

func writeDelimited(h io.Writer, s string) {
	_, _ = io.WriteString(h, s)
	_, _ = h.Write([]byte{0})
}

// writeTriByte folds a *bool as three distinct states — absent, false, true —
// so a nil pointer cannot fold identically to a pointer at false.
func writeTriByte(h io.Writer, b *bool) {
	switch {
	case b == nil:
		_, _ = h.Write([]byte{0})
	case *b:
		_, _ = h.Write([]byte{2})
	default:
		_, _ = h.Write([]byte{1})
	}
}
