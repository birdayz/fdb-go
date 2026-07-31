package javayamsql

import (
	"fmt"
	"regexp"
)

// The custom YAML tags the corpus uses.
//
// Two registries exist upstream and they are NOT the same set:
//
//   - Row/config tags, registered through the CustomTag ServiceLoader and
//     installed by CustomYamlConstructor. These appear in `result:` rows,
//     `supported_version:` values and so on.
//   - Parameter-injection tags, registered by QueryInterpreter's private
//     QueryParameterYamlConstructor. These appear only inside a `!!…!!`
//     segment of a query string.
//
// The two overlap but neither contains the other: `!not_null`/`!null`/`!ignore`/
// `!pos`/`!sc`/`!current_version` are row-only, while `!r`/`!a`/`!in`/`!n` are
// segment-only. Mixing them up is silently wrong in Java (an unknown tag inside
// a segment falls through to a plain scalar), so the two sets are kept apart
// here and checked against the context they appear in.
const (
	TagLong           = "!l"
	TagFloat          = "!f"
	TagBytes          = "!b"
	TagIgnore         = "!ignore"
	TagNull           = "!null"
	TagNotNull        = "!not_null"
	TagStringContains = "!sc"
	TagUUID           = "!uuid"
	TagPos            = "!pos"
	TagRandomStr      = "!randomStr"
	TagVector16       = "!v16"
	TagVector32       = "!v32"
	TagVector64       = "!v64"
	TagCurrentVersion = "!current_version"

	TagRandom  = "!r"
	TagArray   = "!a"
	TagInList  = "!in"
	TagNullArg = "!n"
)

// rowTags is the CustomTag ServiceLoader set installed by
// CustomYamlConstructor, i.e. every tag legal in a block/config/row position.
var rowTags = map[string]bool{
	TagLong: true, TagFloat: true, TagBytes: true, TagIgnore: true,
	TagNull: true, TagNotNull: true, TagStringContains: true, TagUUID: true,
	TagPos: true, TagRandomStr: true, TagVector16: true, TagVector32: true,
	TagVector64: true, TagCurrentVersion: true,
}

// segmentTags is QueryParameterYamlConstructor's set, i.e. every tag legal
// inside a `!!…!!` parameter-injection segment. Note it re-registers !l, !b and
// !f from the row set but deliberately not the matcher tags — a matcher has no
// meaning as a bound query parameter.
var segmentTags = map[string]bool{
	TagRandom: true, TagArray: true, TagInList: true, TagNullArg: true,
	TagUUID: true, TagVector16: true, TagVector32: true, TagVector64: true,
	TagRandomStr: true, TagLong: true, TagBytes: true, TagFloat: true,
}

// IsKnownTag reports whether tag is a custom tag this format defines anywhere.
func IsKnownTag(tag string) bool { return rowTags[tag] || segmentTags[tag] }

// IsRowTag reports whether tag is legal outside a parameter-injection segment.
func IsRowTag(tag string) bool { return rowTags[tag] }

// IsSegmentTag reports whether tag is legal inside a `!!…!!` segment.
func IsSegmentTag(tag string) bool { return segmentTags[tag] }

// KnownTags returns every custom tag, for census and drift reporting.
func KnownTags() []string {
	seen := make(map[string]bool, len(rowTags)+len(segmentTags))
	out := make([]string, 0, len(rowTags)+len(segmentTags))
	for _, m := range []map[string]bool{rowTags, segmentTags} {
		for t := range m {
			if !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
	}
	return out
}

// randomStrPattern mirrors RandomStringParser.PATTERN. Java validates the
// descriptor when the tag is *constructed*, so a malformed one is a parse
// error there and must be one here too.
var randomStrPattern = regexp.MustCompile(`(?i)^seed\s+\d+\s+length\s+\d+$`)

// validateTag enforces the shape checks Java performs at construction time.
// Checks Java defers to execution are deliberately not made here.
func validateTag(tag string, inner *Value, line int) error {
	switch tag {
	case TagNull, TagNotNull, TagIgnore, TagCurrentVersion:
		// Singleton matcher tags: IsNullTag/NotNullTag/IgnoreTag/CurrentVersionTag
		// all `return INSTANCE` without reading the node, so whatever follows the
		// tag is deliberately ignored. The corpus relies on that — a flow mapping
		// needs a key to hang the tag on, so it writes throwaway placeholders like
		// `{!null _}` and `{!ignore dc}`. Accepting any argument is required, not
		// lax.
	case TagPos:
		n, ok := inner.AsInt()
		if !ok {
			return fmt.Errorf("line %d: %s expects an integer column position, got %s", line, tag, inner.Kind)
		}
		if n < 1 {
			return fmt.Errorf("line %d: %s column positions are 1-based, got %d", line, tag, n)
		}
	case TagRandomStr:
		s, ok := inner.AsString()
		if !ok {
			return fmt.Errorf("line %d: %s expects a descriptor string, got %s", line, tag, inner.Kind)
		}
		if !randomStrPattern.MatchString(s) {
			return fmt.Errorf("line %d: invalid %s descriptor %q, expected \"seed <N> length <M>\"", line, tag, s)
		}
	case TagVector16, TagVector32, TagVector64:
		// AbstractVectorTag casts the node to a SequenceNode; anything else
		// throws. Inside a `!!…!!` segment the same cast applies.
		if inner.Kind != KindSeq {
			return fmt.Errorf("line %d: %s expects a sequence of components, got %s", line, tag, inner.Kind)
		}
	case TagUUID:
		if _, ok := inner.AsString(); !ok {
			return fmt.Errorf("line %d: %s expects a string value, got %s", line, tag, inner.Kind)
		}
	case TagInList:
		// ConstructInList asserts a mapping of exactly one entry.
		if inner.Kind != KindMap || len(inner.Map) != 1 {
			return fmt.Errorf("line %d: %s expects a set of exactly 1 element", line, tag)
		}
	case TagRandom:
		// ConstructRandom: a sequence of 1 or 2 (range), or a mapping (choice).
		if inner.Kind == KindSeq && len(inner.Seq) != 1 && len(inner.Seq) != 2 {
			return fmt.Errorf("line %d: %s expects a list of 1 or 2 elements, got %d", line, tag, len(inner.Seq))
		}
		if inner.Kind != KindSeq && inner.Kind != KindMap {
			return fmt.Errorf("line %d: %s expects a list or a set, got %s", line, tag, inner.Kind)
		}
	case TagArray:
		// ConstructArrayGenerator: a sequence of 1 or 2, or a mapping of exactly 2.
		if inner.Kind == KindSeq && len(inner.Seq) != 1 && len(inner.Seq) != 2 {
			return fmt.Errorf("line %d: %s expects a list of 1 or 2 elements, got %d", line, tag, len(inner.Seq))
		}
		if inner.Kind == KindMap && len(inner.Map) != 2 {
			return fmt.Errorf("line %d: %s expects a set to have 2 elements, got %d", line, tag, len(inner.Map))
		}
		if inner.Kind != KindSeq && inner.Kind != KindMap {
			return fmt.Errorf("line %d: %s expects a list or a set, got %s", line, tag, inner.Kind)
		}
	}
	return nil
}
