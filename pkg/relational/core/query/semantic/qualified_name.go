package semantic

import "strings"

// QualifiedName is a dot-separated SQL name like `schema.table.col`.
// The leaf (last segment) is the "simple name"; the preceding
// segments are the "qualifier". Both normalized together — callers
// never see a QualifiedName with one segment upper-cased and another
// in source case.
//
// Mirrors Java's Identifier-with-qualifier shape. The Go port breaks
// it off into its own type because:
//   - Slices aren't comparable, so bundling segments into Identifier
//     would lose the map-as-key ergonomics.
//   - Most callsites touch only the leaf — an unqualified Identifier
//     stays simple.
//
// Use QualifiedName when you need to preserve the qualifier chain
// (table aliases, schema-scoped tables). Use Identifier for bare
// column / alias references.
type QualifiedName struct {
	// segments is the normalized dotted path, leaf-last. Each entry
	// is an already-case-folded string (or case-preserved if the
	// source was quoted / caseSensitive).
	segments []string
	// wasQuotedPerSegment records the quoting flag for each segment.
	// Tracked per-segment because `schema."Weird"` mixes cases — the
	// schema segment folds, the table segment doesn't.
	wasQuotedPerSegment []bool
}

// ParseQualifiedName splits a raw dotted string into a QualifiedName.
// Each segment is normalized independently (stripped of its own
// surrounding quotes, case-folded unless caseSensitive or quoted).
//
// Semantics match Java's token-per-segment handling: `t."X"` parses
// as qualifier=[T], name="X" (first segment upper-cased because
// unquoted, second preserved because quoted).
//
// Quote-embedded dots are NOT handled (e.g. `"a.b".c` still splits
// on every dot). The ANTLR parser already tokenises individual
// identifiers, so callers feeding us pre-tokenised segments won't
// hit this edge — see FromSegments for that path.
func ParseQualifiedName(raw string, caseSensitive bool) QualifiedName {
	if raw == "" {
		return QualifiedName{}
	}
	parts := strings.Split(raw, ".")
	segs := make([]string, len(parts))
	quoted := make([]bool, len(parts))
	for i, p := range parts {
		if isQuoted(p, '"') || isQuoted(p, '\'') {
			segs[i] = p[1 : len(p)-1]
			quoted[i] = true
		} else if caseSensitive {
			segs[i] = p
		} else {
			segs[i] = strings.ToUpper(p)
		}
	}
	return QualifiedName{segments: segs, wasQuotedPerSegment: quoted}
}

// FromSegments builds a QualifiedName from already-normalized
// segments (each passed through NormalizeString or equivalent).
// Empty input returns the zero value.
func FromSegments(segments []string, caseSensitive bool) QualifiedName {
	if len(segments) == 0 {
		return QualifiedName{}
	}
	out := make([]string, len(segments))
	quoted := make([]bool, len(segments))
	for i, s := range segments {
		if isQuoted(s, '"') || isQuoted(s, '\'') {
			out[i] = s[1 : len(s)-1]
			quoted[i] = true
		} else if caseSensitive {
			out[i] = s
		} else {
			out[i] = strings.ToUpper(s)
		}
	}
	return QualifiedName{segments: out, wasQuotedPerSegment: quoted}
}

// Name returns the leaf (last) segment. For `schema.table.col` this
// is `col`. Zero QualifiedName returns the empty string.
func (q QualifiedName) Name() string {
	if len(q.segments) == 0 {
		return ""
	}
	return q.segments[len(q.segments)-1]
}

// Qualifier returns the segments before the leaf. For
// `schema.table.col` this returns [schema, table]. An unqualified
// name returns an empty slice (never nil).
func (q QualifiedName) Qualifier() []string {
	if len(q.segments) <= 1 {
		return []string{}
	}
	// Defensive copy so callers can't mutate our state.
	out := make([]string, len(q.segments)-1)
	copy(out, q.segments[:len(q.segments)-1])
	return out
}

// Segments returns all segments, leaf-last. Zero QualifiedName
// returns nil; otherwise a defensive copy.
func (q QualifiedName) Segments() []string {
	if len(q.segments) == 0 {
		return nil
	}
	out := make([]string, len(q.segments))
	copy(out, q.segments)
	return out
}

// IsQualified reports whether the name has at least one qualifier
// segment preceding the leaf.
func (q QualifiedName) IsQualified() bool {
	return len(q.segments) > 1
}

// IsZero reports whether q is the zero-value (empty) QualifiedName.
func (q QualifiedName) IsZero() bool {
	return len(q.segments) == 0
}

// String returns the canonical dotted representation. Suitable for
// map keys (two QualifiedNames with the same String value are equal
// under EqualsIgnoreQuoting).
func (q QualifiedName) String() string {
	return strings.Join(q.segments, ".")
}

// EqualsIgnoreQuoting compares segment-by-segment by normalized text
// only, ignoring per-segment quoting flags. This is the common
// "these target the same database object" semantics that Java's
// Identifier.equals uses.
func (q QualifiedName) EqualsIgnoreQuoting(other QualifiedName) bool {
	if len(q.segments) != len(other.segments) {
		return false
	}
	for i := range q.segments {
		if q.segments[i] != other.segments[i] {
			return false
		}
	}
	return true
}

// PrefixedWith reports whether q starts with prefix's segments.
// `schema.table.col` is prefixed with `schema.table`. Useful for
// resolving a bare column against a set of alias-qualified
// candidates.
func (q QualifiedName) PrefixedWith(prefix QualifiedName) bool {
	if len(prefix.segments) > len(q.segments) {
		return false
	}
	for i, s := range prefix.segments {
		if q.segments[i] != s {
			return false
		}
	}
	return true
}

// LeafIdentifier returns the leaf segment wrapped as an Identifier
// (preserving the leaf's wasQuoted flag). Useful when a caller
// resolved a qualified name and now wants to work with just the
// column name.
func (q QualifiedName) LeafIdentifier() Identifier {
	if len(q.segments) == 0 {
		return Identifier{}
	}
	last := len(q.segments) - 1
	return Identifier{name: q.segments[last], wasQuoted: q.wasQuotedPerSegment[last]}
}
