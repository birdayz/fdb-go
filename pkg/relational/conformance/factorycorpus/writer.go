package factorycorpus

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"fdb.dev/pkg/relational/conformance/yamsql"
)

// DedupKeyOf is the corpus's primary key: a digest over the feature vector and
// the plan shape. Neither half is sufficient alone — the feature vector says
// what the query ASKED (two different plans for the same shape are two
// different tests) and the plan shape says what the planner DID (explaindiff's
// ShapeOf is type-only, so a scan filtered on `a` and one filtered on `b`
// collapse to the same shape). A candidate is committed only if this key is
// new.
func DedupKeyOf(featureVector string, planShape string) string {
	h := sha256.New()
	h.Write([]byte(featureVector))
	h.Write([]byte{0})
	h.Write([]byte(planShape))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// ShapeDigest folds a plan shape rendering (explaindiff.ShapeOf output) into
// the fixed-width form the header carries. The full rendering is many lines
// and would dominate the header; the digest is stable because the rendering is
// (it holds Go type names and tree positions, nothing versioned).
func ShapeDigest(shape []string) string {
	h := sha256.New()
	for _, line := range shape {
		h.Write([]byte(line))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Marshal renders a header and scenario as the bytes of a committed corpus
// file.
//
// The contract this function exists for is BYTE-EXACT ROUND TRIPPING:
//
//	Marshal(Load(Marshal(h, s))) == Marshal(h, s)
//
// Without it a re-bless (§5.3) produces a diff full of reformatting noise and
// the reader cannot see which expectation actually moved — which is the only
// thing a re-bless review is for. Three details carry that contract, and each
// is a place a naive yaml.Marshal of the Scenario struct would break it:
//
//   - Optional fields are OMITTED, not zeroed. yamsql.Load rejects `rowcount`
//     on a query and `unordered` on a non-query, so a struct marshalled with
//     its zero values would not even reload.
//   - Every SQL string is emitted as a literal block scalar. A plain scalar is
//     line-wrapped by the emitter at ~80 columns and unwrapped on read, which
//     round-trips only as long as every break lands on a single space —
//     a property of the text, not of the format.
//   - Floats are rendered explicitly (canonicalFloat). Left to the emitter,
//     -0.0 prints as `-0`, reloads as an INT, and re-marshals as `0`: the
//     round trip fails, and it fails precisely on the signed-zero value this
//     repo has already shipped one wrong-rows bug on.
func Marshal(h Header, s *yamsql.Scenario) ([]byte, error) {
	if h.Name != s.Name {
		return nil, fmt.Errorf("header name %q != scenario name %q", h.Name, s.Name)
	}
	if s.SchemaTemplate == "" {
		return nil, fmt.Errorf("%s: schema_template is required", s.Name)
	}
	if len(s.Tests) == 0 {
		return nil, fmt.Errorf("%s: a committed scenario with no tests asserts nothing", s.Name)
	}

	doc := &yaml.Node{Kind: yaml.MappingNode}
	appendKV(doc, "name", strScalar(s.Name))
	tmpl, err := sqlScalar(s.SchemaTemplate)
	if err != nil {
		return nil, fmt.Errorf("%s: schema_template: %w", s.Name, err)
	}
	appendKV(doc, "schema_template", tmpl)

	if len(s.Setup) > 0 {
		seq := &yaml.Node{Kind: yaml.SequenceNode}
		for i, stmt := range s.Setup {
			n, err := sqlScalar(stmt)
			if err != nil {
				return nil, fmt.Errorf("%s: setup[%d]: %w", s.Name, i, err)
			}
			seq.Content = append(seq.Content, n)
		}
		appendKV(doc, "setup", seq)
	}

	tests := &yaml.Node{Kind: yaml.SequenceNode}
	for i := range s.Tests {
		n, err := testNode(&s.Tests[i])
		if err != nil {
			return nil, fmt.Errorf("%s: tests[%d]: %w", s.Name, i, err)
		}
		tests.Content = append(tests.Content, n)
	}
	appendKV(doc, "tests", tests)

	var body bytes.Buffer
	enc := yaml.NewEncoder(&body)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("%s: encode: %w", s.Name, err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("%s: close encoder: %w", s.Name, err)
	}
	return append([]byte(h.Render()), body.Bytes()...), nil
}

// testNode renders one Test, emitting only the fields that are set. The order
// is fixed (query, then the assertion fields) so two files describing the same
// test are byte-identical.
func testNode(t *yamsql.Test) (*yaml.Node, error) {
	if t.Exec != "" {
		return nil, fmt.Errorf("exec must already be normalized into query by the caller")
	}
	if t.Query == "" {
		return nil, fmt.Errorf("query is empty")
	}
	m := &yaml.Node{Kind: yaml.MappingNode}
	q, err := sqlScalar(t.Query)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	appendKV(m, "query", q)

	if len(t.Columns) > 0 {
		seq := &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
		for _, c := range t.Columns {
			seq.Content = append(seq.Content, strScalar(c))
		}
		appendKV(m, "columns", seq)
	}
	if t.Unordered {
		appendKV(m, "unordered", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"})
	}
	if code := t.EffectiveErrorCode(); code != "" {
		appendKV(m, "error_code", strScalar(code))
		return m, nil
	}
	rows := &yaml.Node{Kind: yaml.SequenceNode}
	for i, r := range t.Rows {
		row := &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
		for j, v := range r {
			n, err := valueScalar(v)
			if err != nil {
				return nil, fmt.Errorf("rows[%d][%d]: %w", i, j, err)
			}
			row.Content = append(row.Content, n)
		}
		rows.Content = append(rows.Content, row)
	}
	// An empty result set is a real, load-bearing expectation (the engine
	// returned nothing) and must be spelled `rows: []`, not omitted — omitting
	// it makes the test assert only that the query does not error.
	if len(t.Rows) == 0 {
		rows.Style = yaml.FlowStyle
	}
	appendKV(m, "rows", rows)
	return m, nil
}

func appendKV(m *yaml.Node, key string, val *yaml.Node) {
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		val)
}

func strScalar(s string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s, Style: yaml.DoubleQuotedStyle}
}

// sqlScalar renders a SQL statement as a literal block scalar (`|-`), the one
// YAML form that reproduces long text with no wrapping and no escaping.
//
// Two shapes cannot be spelled that way and are rejected rather than silently
// mangled: a line with trailing whitespace (a block scalar strips it on read)
// and a first line starting with a space (which needs an explicit indentation
// indicator). Neither can come out of the generator, so an occurrence is a
// generator bug and must be loud.
func sqlScalar(s string) (*yaml.Node, error) {
	if s == "" {
		return nil, fmt.Errorf("empty statement")
	}
	for i, line := range strings.Split(s, "\n") {
		if strings.TrimRight(line, " \t") != line {
			return nil, fmt.Errorf("line %d has trailing whitespace, which a block scalar strips on read: %q", i, line)
		}
	}
	if strings.HasPrefix(s, " ") || strings.HasPrefix(s, "\t") {
		return nil, fmt.Errorf("statement starts with whitespace: %q", s)
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s, Style: yaml.LiteralStyle}, nil
}

// valueScalar renders one expectation cell with an EXPLICIT tag, so reading it
// back yields a value of the same KIND it was written from. Left implicit, an
// unquoted `1` reloads as a number and the string `"1"` would too — and the
// runner's row diff is type-sensitive enough that the distinction decides
// whether a test passes.
//
// The integer and float families are both accepted, because the round trip is
// not type-preserving and cannot be: yaml.v3 decodes `!!int` into `int` when
// the destination is `any`, so a cell WRITTEN from an int64 comes BACK as an
// int. A writer that accepted only int64 could not re-marshal its own output —
// every re-bless would fail, and so would the canonical-form gate over the
// committed corpus. Normalizing the same families the runner's row diff
// normalizes keeps the two in agreement; the runner promotes int/int32/uint…
// to int64 and float32 to float64 before comparing, so a cell that survives
// here compares identically either way.
func valueScalar(v any) (*yaml.Node, error) {
	switch x := v.(type) {
	case nil:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "~"}, nil
	case bool:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(x)}, nil
	case int:
		return intScalar(int64(x)), nil
	case int32:
		return intScalar(int64(x)), nil
	case int64:
		return intScalar(x), nil
	case uint32:
		return intScalar(int64(x)), nil
	case uint64:
		// Values above MaxInt64 cannot round-trip through the runner's
		// int64 normalization, so they are refused rather than silently
		// wrapping to a negative expectation.
		if x > math.MaxInt64 {
			return nil, fmt.Errorf("value %d exceeds int64; the runner normalizes expectations to int64 and would compare a wrapped value", x)
		}
		return intScalar(int64(x)), nil
	case float32:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: canonicalFloat(float64(x))}, nil
	case float64:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: canonicalFloat(x)}, nil
	case string:
		return strScalar(x), nil
	default:
		return nil, fmt.Errorf("value %v has type %T, which the corpus format cannot freeze; the executor must normalize it first", v, v)
	}
}

func intScalar(v int64) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.FormatInt(v, 10)}
}

// canonicalFloat spells a float64 in a form YAML re-reads as a float64 with
// the identical bit pattern.
//
// strconv's shortest round-trip form is the right numeric text, but it is not
// always valid YAML float SYNTAX: `-0`, `3` and `1e+20` all reload as ints or
// (for the exponent form) depend on the parser's leniency. Appending `.0` when
// no `.` or `e` is present forces the float tag; the signed zero case is the
// one that matters, because `-0` → int 0 → `0` silently loses the sign of a
// value this repo has already shipped a wrong-rows bug on.
func canonicalFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return ".nan"
	case math.IsInf(f, 1):
		return ".inf"
	case math.IsInf(f, -1):
		return "-.inf"
	}
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return s
}
