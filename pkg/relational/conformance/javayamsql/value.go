// Package javayamsql parses Apple's `.yamsql` acceptance-test format — the
// multi-document YAML dialect driving the Java record layer's yaml-tests suite.
//
// The vendored corpus lives at third_party/apple/fdb-record-layer/ (see the
// README there). This package only *parses*; nothing here executes a query.
//
// The parser is deliberately STRICTER than Java's. Java probes its option maps
// with containsKey and silently ignores any key it does not recognise, so a
// typo'd or newly-added upstream directive reads as "absent" and the test
// quietly checks less than it claims to. Here every block key, command, config
// key and YAML tag must be in the known set or parsing fails. That is the whole
// point of the corpus gate: when a version bump introduces a directive, it must
// fail loudly and by name rather than being dropped on the floor.
package javayamsql

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// ValueKind discriminates [Value].
type ValueKind uint8

// The kinds a [Value] may take. KindTagged wraps any of the others with one of
// the corpus's custom YAML tags (see tags.go).
const (
	KindNull ValueKind = iota
	KindBool
	KindInt
	KindFloat
	KindString
	KindSeq
	KindMap
	KindTagged
)

func (k ValueKind) String() string {
	switch k {
	case KindNull:
		return "null"
	case KindBool:
		return "bool"
	case KindInt:
		return "int"
	case KindFloat:
		return "float"
	case KindString:
		return "string"
	case KindSeq:
		return "seq"
	case KindMap:
		return "map"
	case KindTagged:
		return "tagged"
	default:
		return fmt.Sprintf("ValueKind(%d)", uint8(k))
	}
}

// Entry is one key/value pair of a mapping, kept in source order.
//
// Order is load-bearing: a result row is a mapping whose entry order IS the
// column order (Java's Matchers.matchMap walks entrySet() with a positional
// counter). A Go map would destroy that, so mappings are slices.
type Entry struct {
	Key *Value
	Val *Value
}

// Value is a parsed YAML node with its custom tag preserved.
//
// Only the fields belonging to Kind are meaningful.
type Value struct {
	Kind ValueKind
	Line int
	Col  int

	Bool  bool
	Int   int64
	Float float64
	Str   string
	Seq   []*Value
	Map   []Entry

	// Tag and Inner are set only for KindTagged. Inner is never nil; a bare
	// tag with no argument (`!null`, `!not_null`, `!ignore`) wraps a KindNull.
	Tag   string
	Inner *Value
}

// IsNull reports whether v is the YAML null that Java's SafeConstructor turns
// into a Java null.
//
// This is the hinge of the whole row model. Java's Matchers.valueElseKey tests
// `entry.getValue() == null`, and SnakeYAML constructs Java null for an elided
// value (`{A: }`), an explicit `null`, and `~` alike — so all three are the
// same thing and all three mean "positional cell". A custom-tagged `!null` is
// NOT this: it constructs an IsNullMatcher object, which is non-null in Java
// and therefore an ordinary by-column-name cell that happens to expect SQL NULL.
func (v *Value) IsNull() bool { return v != nil && v.Kind == KindNull }

// Untag returns the value beneath any custom tag.
func (v *Value) Untag() *Value {
	for v != nil && v.Kind == KindTagged {
		v = v.Inner
	}
	return v
}

// TagName returns the custom tag on v, or "" if untagged.
func (v *Value) TagName() string {
	if v == nil || v.Kind != KindTagged {
		return ""
	}
	return v.Tag
}

// AsString returns the string content of a scalar, and whether v was one.
// A tagged scalar reports the tag's argument (`!sc "ell"` yields "ell").
func (v *Value) AsString() (string, bool) {
	u := v.Untag()
	if u == nil {
		return "", false
	}
	switch u.Kind {
	case KindString:
		return u.Str, true
	case KindInt:
		return strconv.FormatInt(u.Int, 10), true
	case KindBool:
		return strconv.FormatBool(u.Bool), true
	case KindFloat:
		return strconv.FormatFloat(u.Float, 'g', -1, 64), true
	default:
		return "", false
	}
}

// AsInt returns the integer content of a scalar, and whether v was one.
func (v *Value) AsInt() (int64, bool) {
	u := v.Untag()
	if u == nil || u.Kind != KindInt {
		return 0, false
	}
	return u.Int, true
}

// convertNode turns a decoded yaml.v3 node into a [Value], preserving custom
// tags. resolveAliases is handled by yaml.v3 for us: an AliasNode carries the
// anchor's node in .Alias.
func convertNode(n *yaml.Node) (*Value, error) {
	if n == nil {
		return &Value{Kind: KindNull}, nil
	}
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return &Value{Kind: KindNull, Line: n.Line, Col: n.Column}, nil
		}
		return convertNode(n.Content[0])
	case yaml.AliasNode:
		return convertNode(n.Alias)
	}

	// A custom tag is any tag that is not one of YAML's own `!!`-prefixed ones
	// and not the implicit-resolution sentinel. SnakeYAML dispatches these to
	// the CustomTag ServiceLoader registry; we validate the name against the
	// same closed set and keep the tag on the node.
	if tag := n.Tag; tag != "" && strings.HasPrefix(tag, "!") && !strings.HasPrefix(tag, "!!") {
		if !IsKnownTag(tag) {
			return nil, fmt.Errorf("line %d: unknown YAML tag %q", n.Line, tag)
		}
		inner, err := convertUntagged(n)
		if err != nil {
			return nil, err
		}
		if err := validateTag(tag, inner, n.Line); err != nil {
			return nil, err
		}
		return &Value{Kind: KindTagged, Line: n.Line, Col: n.Column, Tag: tag, Inner: inner}, nil
	}
	return convertUntagged(n)
}

// convertUntagged converts n ignoring any custom tag on n itself (nested nodes
// keep theirs).
func convertUntagged(n *yaml.Node) (*Value, error) {
	switch n.Kind {
	case yaml.ScalarNode:
		return resolveScalar(n), nil
	case yaml.SequenceNode:
		out := &Value{Kind: KindSeq, Line: n.Line, Col: n.Column, Seq: make([]*Value, 0, len(n.Content))}
		for _, c := range n.Content {
			cv, err := convertNode(c)
			if err != nil {
				return nil, err
			}
			out.Seq = append(out.Seq, cv)
		}
		return out, nil
	case yaml.MappingNode:
		out := &Value{Kind: KindMap, Line: n.Line, Col: n.Column, Map: make([]Entry, 0, len(n.Content)/2)}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, err := convertNode(n.Content[i])
			if err != nil {
				return nil, err
			}
			v, err := convertNode(n.Content[i+1])
			if err != nil {
				return nil, err
			}
			out.Map = append(out.Map, Entry{Key: k, Val: v})
		}
		return out, nil
	default:
		return nil, fmt.Errorf("line %d: unsupported YAML node kind %d", n.Line, n.Kind)
	}
}

// resolveScalar resolves a scalar node's content.
//
// For an untagged scalar yaml.v3 has already run implicit resolution and left
// the result in n.Tag, so we trust it. For a custom-tagged scalar (`!l 10`) no
// implicit resolution happened — the tag replaced it — so we redo it against
// the YAML core schema, which is what SnakeYAML's SafeConstructor does when a
// CustomTag construct asks for the scalar's typed value.
func resolveScalar(n *yaml.Node) *Value {
	v := &Value{Line: n.Line, Col: n.Column}
	quoted := n.Style == yaml.SingleQuotedStyle || n.Style == yaml.DoubleQuotedStyle ||
		n.Style == yaml.LiteralStyle || n.Style == yaml.FoldedStyle

	switch n.Tag {
	case "!!null":
		v.Kind = KindNull
		return v
	case "!!bool":
		v.Kind = KindBool
		v.Bool = n.Value == "true" || n.Value == "True" || n.Value == "TRUE" ||
			n.Value == "y" || n.Value == "yes" || n.Value == "on"
		return v
	case "!!int":
		if i, err := strconv.ParseInt(n.Value, 0, 64); err == nil {
			v.Kind = KindInt
			v.Int = i
			return v
		}
		// Out of int64 range: keep full fidelity as a float, else as text.
		if f, err := strconv.ParseFloat(n.Value, 64); err == nil {
			v.Kind = KindFloat
			v.Float = f
			return v
		}
		v.Kind = KindString
		v.Str = n.Value
		return v
	case "!!float":
		if f, err := parseYAMLFloat(n.Value); err == nil {
			v.Kind = KindFloat
			v.Float = f
			return v
		}
		v.Kind = KindString
		v.Str = n.Value
		return v
	case "!!str":
		v.Kind = KindString
		v.Str = n.Value
		return v
	}

	if quoted {
		v.Kind = KindString
		v.Str = n.Value
		return v
	}
	return resolvePlain(n.Value, n.Line, n.Column)
}

// resolvePlain applies YAML core-schema implicit resolution to a plain scalar.
func resolvePlain(raw string, line, col int) *Value {
	v := &Value{Line: line, Col: col}
	switch raw {
	case "", "~", "null", "Null", "NULL":
		v.Kind = KindNull
		return v
	case "true", "True", "TRUE":
		v.Kind, v.Bool = KindBool, true
		return v
	case "false", "False", "FALSE":
		v.Kind, v.Bool = KindBool, false
		return v
	}
	if i, err := strconv.ParseInt(raw, 0, 64); err == nil {
		v.Kind, v.Int = KindInt, i
		return v
	}
	if f, err := parseYAMLFloat(raw); err == nil {
		v.Kind, v.Float = KindFloat, f
		return v
	}
	v.Kind, v.Str = KindString, raw
	return v
}

// parseYAMLFloat accepts Go float syntax plus YAML's .inf/.nan spellings.
func parseYAMLFloat(raw string) (float64, error) {
	switch strings.ToLower(raw) {
	case ".inf", "+.inf":
		return math.Inf(1), nil
	case "-.inf":
		return math.Inf(-1), nil
	case ".nan":
		return math.NaN(), nil
	}
	return strconv.ParseFloat(raw, 64)
}
