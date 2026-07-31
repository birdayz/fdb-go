package javayamsql

import (
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// ParseSegments extracts the `!!…!!` parameter-injection segments from a query.
//
// This mirrors QueryInterpreter.getInjections: scan for `!!`, find the next
// `!!` after it, and load the text between them as its own little YAML document
// using the parameter-injection tag set. Delimiters do not nest and are not
// escapable, so the scan is a plain left-to-right pairing — a query containing
// an odd number of `!!` is malformed, which Java asserts on.
//
// Phase 0 stops at representation. Binding an unbound generator to a value
// needs a seeded Random and belongs to execution.
func ParseSegments(query string, line int) ([]Segment, error) {
	var out []Segment
	cursor := 0
	for {
		start := strings.Index(query[cursor:], "!!")
		if start < 0 {
			return out, nil
		}
		start += cursor
		end := strings.Index(query[start+2:], "!!")
		if end < 0 {
			return nil, &ParseError{
				Line: line,
				Err:  fmt.Errorf("parameter injection not formed correctly in query %q", query),
			}
		}
		end += start + 2
		body := query[start+2 : end]
		val, err := parseSegmentBody(body, line)
		if err != nil {
			return nil, err
		}
		out = append(out, Segment{
			Raw:    query[start : end+2],
			Body:   body,
			Value:  val,
			Kind:   segmentKind(val),
			Offset: start,
		})
		cursor = end + 2
	}
}

// parseSegmentBody loads one segment's mini-YAML.
func parseSegmentBody(body string, line int) (*Value, error) {
	if strings.TrimSpace(body) == "" {
		return nil, &ParseError{Line: line, Err: errors.New("empty parameter injection segment")}
	}
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(body), &node); err != nil {
		return nil, &ParseError{Line: line, Err: fmt.Errorf("parameter injection %q: %w", body, err)}
	}
	v, err := convertNode(&node)
	if err != nil {
		return nil, &ParseError{Line: line, Err: fmt.Errorf("parameter injection %q: %w", body, err)}
	}
	if err := checkSegmentTags(v, body, line); err != nil {
		return nil, err
	}
	return v, nil
}

// checkSegmentTags rejects tags that exist in the corpus but are not registered
// on QueryParameterYamlConstructor.
//
// This is the one place the two tag registries actually diverge in effect: an
// unregistered tag inside a segment does NOT raise in Java, it falls through
// to `new PrimitiveParameter(super.constructObject(node))` and the tag is lost,
// so `!! !null !!` would silently bind whatever the bare scalar resolves to.
// Flagging it keeps a real upstream authoring mistake visible.
func checkSegmentTags(v *Value, body string, line int) error {
	if v == nil {
		return nil
	}
	if v.Kind == KindTagged {
		if !IsSegmentTag(v.Tag) {
			return &ParseError{
				Line: line,
				Err:  fmt.Errorf("parameter injection %q: tag %s is not valid inside a !!…!! segment", body, v.Tag),
			}
		}
		return checkSegmentTags(v.Inner, body, line)
	}
	for _, c := range v.Seq {
		if err := checkSegmentTags(c, body, line); err != nil {
			return err
		}
	}
	for _, e := range v.Map {
		if err := checkSegmentTags(e.Key, body, line); err != nil {
			return err
		}
		if err := checkSegmentTags(e.Val, body, line); err != nil {
			return err
		}
	}
	return nil
}

// segmentKind reports whether a segment needs a Random to become a value.
// !r and !a are the generators; everything else resolves to a literal.
func segmentKind(v *Value) SegmentKind {
	switch v.TagName() {
	case TagRandom, TagArray:
		return SegmentUnbound
	}
	// !in wraps a single element that may itself be a generator.
	if v.TagName() == TagInList && len(v.Untag().Map) == 1 {
		if segmentKind(v.Untag().Map[0].Key) == SegmentUnbound {
			return SegmentUnbound
		}
	}
	return SegmentLiteral
}
