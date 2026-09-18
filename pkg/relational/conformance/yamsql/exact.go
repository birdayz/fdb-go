package yamsql

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Scalar is a driver value with an explicit carrier and representation. Value
// is textual even for numbers: float64 carries exactly 16 IEEE hexadecimal digits.
// Null has no payload; an empty string or byte string has a present empty payload.
type Scalar struct {
	Kind  string  `yaml:"kind"`
	Value *string `yaml:"value,omitempty"`
}

// UnmarshalYAML keeps the tagged codec strict even inside a custom decoder.
func (s *Scalar) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("scalar must be a tagged mapping")
	}
	*s = Scalar{}
	seen := make(map[string]bool)
	for i := 0; i < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if k.Kind != yaml.ScalarNode || k.Tag != "!!str" || seen[k.Value] {
			return fmt.Errorf("invalid or duplicate scalar field %q", k.Value)
		}
		seen[k.Value] = true
		if v.Kind != yaml.ScalarNode || v.Tag != "!!str" {
			return fmt.Errorf("scalar %s must be a string", k.Value)
		}
		switch k.Value {
		case "kind":
			s.Kind = v.Value
		case "value":
			value := v.Value
			s.Value = &value
		default:
			return fmt.Errorf("unknown scalar field %q", k.Value)
		}
	}
	_, err := s.decode()
	return err
}

func (s Scalar) decode() (any, error) {
	if s.Kind == "null" {
		if s.Value != nil {
			return nil, fmt.Errorf("null scalar must not have a payload")
		}
		return nil, nil
	}
	if s.Value == nil {
		return nil, fmt.Errorf("%q scalar requires a payload", s.Kind)
	}
	v := *s.Value
	switch s.Kind {
	case "int64":
		x, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("int64 scalar: %w", err)
		}
		return x, nil
	case "float64":
		if len(v) != 16 {
			return nil, fmt.Errorf("float64 scalar requires 16 hexadecimal digits")
		}
		b, err := hex.DecodeString(v)
		if err != nil {
			return nil, fmt.Errorf("float64 scalar: %w", err)
		}
		var bits uint64
		for _, c := range b {
			bits = bits<<8 | uint64(c)
		}
		return math.Float64frombits(bits), nil
	case "string":
		if !utf8.ValidString(v) {
			return nil, fmt.Errorf("string scalar must be valid UTF-8; use bytes for arbitrary data")
		}
		return v, nil
	case "bool":
		if v != "true" && v != "false" {
			return nil, fmt.Errorf("bool scalar requires true or false")
		}
		return v == "true", nil
	case "bytes":
		b, err := hex.DecodeString(v)
		if err != nil {
			return nil, fmt.Errorf("bytes scalar: %w", err)
		}
		return b, nil
	default:
		return nil, fmt.Errorf("unknown scalar kind %q", s.Kind)
	}
}

func decodeScalars(values []Scalar) ([]any, error) {
	out := make([]any, len(values))
	for i, value := range values {
		v, err := value.decode()
		if err != nil {
			return nil, fmt.Errorf("value[%d]: %w", i, err)
		}
		out[i] = v
	}
	return out, nil
}

func exactValueEqual(want, got any) bool {
	switch w := want.(type) {
	case nil:
		return got == nil
	case int64:
		g, ok := got.(int64)
		return ok && w == g
	case float64:
		g, ok := got.(float64)
		return ok && math.Float64bits(w) == math.Float64bits(g)
	case string:
		g, ok := got.(string)
		return ok && w == g
	case bool:
		g, ok := got.(bool)
		return ok && w == g
	case []byte:
		g, ok := got.([]byte)
		return ok && bytes.Equal(w, g)
	default:
		return false
	}
}

func exactRowEqual(want, got []any) bool {
	if len(want) != len(got) {
		return false
	}
	for i := range want {
		if !exactValueEqual(want[i], got[i]) {
			return false
		}
	}
	return true
}

func diffExactRows(want [][]Scalar, got [][]any, unordered bool) string {
	if len(want) != len(got) {
		return fmt.Sprintf("exact row count: expected %d, got %d", len(want), len(got))
	}
	used := make([]bool, len(got))
	for i, row := range want {
		decoded, err := decodeScalars(row)
		if err != nil {
			return fmt.Sprintf("exact row %d: %v", i, err)
		}
		if !unordered {
			if !exactRowEqual(decoded, got[i]) {
				return fmt.Sprintf("exact row %d mismatch: expected tagged %v, got %#v", i, row, got[i])
			}
			continue
		}
		found := false
		for j := range got {
			if !used[j] && exactRowEqual(decoded, got[j]) {
				used[j], found = true, true
				break
			}
		}
		if !found {
			return fmt.Sprintf("exact multiset missing row %d: %v", i, row)
		}
	}
	return ""
}

// String renders the tag and payload, including float bits, in mismatch reports.
func (s Scalar) String() string {
	if s.Value == nil {
		return s.Kind
	}
	return fmt.Sprintf("%s(%q)", s.Kind, *s.Value)
}

// MarshalYAML quotes payloads explicitly. yaml.v3's block-scalar encoder loses
// one newline for strings consisting only of newlines ("\n" becomes empty), so
// its automatic style selection cannot carry exact string expectations.
func (s Scalar) MarshalYAML() (any, error) {
	if _, err := s.decode(); err != nil {
		return nil, err
	}
	text := func(value string) *yaml.Node {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value, Style: yaml.DoubleQuotedStyle}
	}
	node := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{text("kind"), text(s.Kind)}}
	if s.Value != nil {
		node.Content = append(node.Content, text("value"), text(*s.Value))
	}
	return node, nil
}
