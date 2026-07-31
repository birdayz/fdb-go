package factorycorpus_test

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"

	"fdb.dev/pkg/relational/conformance/factorycorpus"
	"fdb.dev/pkg/relational/conformance/yamsql"
)

func sampleHeader(name string) factorycorpus.Header {
	fv := "shape=single;idx=A;proj=star;where=cmp.gt;order=none"
	shape := "0123456789abcdef"
	return factorycorpus.Header{
		Name:          name,
		FormatVersion: factorycorpus.FormatVersion,
		Generator:     "rowdiff-gen/1",
		Seed:          4242,
		QueryIndex:    2,
		Projection:    1,
		Date:          "2026-07-31",
		Blessing:      factorycorpus.BlessingMetamorphic,
		Oracles:       []string{"tlp", "second-plan"},
		FeatureVector: fv,
		PlanShape:     shape,
		DedupKey:      factorycorpus.DedupKeyOf(fv, shape),
	}
}

// sampleScenario deliberately carries every cell type the corpus can freeze,
// INCLUDING the ones that break a naive marshaller: negative zero, a float
// with no fractional part, a string that looks like an integer, a string that
// looks like a YAML null, the empty string, and an empty result set.
func sampleScenario(name string) *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           name,
		SchemaTemplate: "CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, d DOUBLE, s STRING, PRIMARY KEY (id))\nCREATE INDEX idx_a ON t (a)",
		Setup:          []string{"INSERT INTO t VALUES (1, 5, -0.0, 'alpha'), (2, NULL, 2.0, '')"},
		Tests: []yamsql.Test{
			{
				Query:     "SELECT id, a, d, s FROM t",
				Unordered: true,
				Rows: [][]any{
					// negZero() rather than the literal -0.0: an untyped Go
					// constant `-0.0` is exactly 0, so writing it here would
					// hand the test a POSITIVE zero and quietly stop probing
					// the case it exists for.
					{int64(1), int64(5), negZero(), "alpha"},
					{int64(2), nil, 2.0, ""},
				},
			},
			{
				Query: "SELECT id FROM t WHERE a > 100",
				Rows:  [][]any{},
			},
			{
				Query: "SELECT s FROM t WHERE id = 1",
				Rows:  [][]any{{"7"}},
			},
			{
				Query:   "SELECT id, TRUE FROM t WHERE id = 1",
				Columns: []string{"ID", "_0"},
				Rows:    [][]any{{int64(1), true}},
			},
		},
	}
}

// TestWriterRoundTripIsByteExact is the writer's whole contract.
//
// Without it a re-bless (§5.3) produces a diff full of reformatting noise and
// a reader cannot see which expectation actually moved — which is the only
// thing a re-bless review is for. The cells above are chosen to break the
// obvious implementations: `-0.0` emitted as `-0` reloads as an int and
// re-marshals as `0`; the string `"7"` emitted unquoted reloads as an int;
// `rows: []` omitted turns a real empty-result expectation into "does not
// error".
func TestWriterRoundTripIsByteExact(t *testing.T) {
	t.Parallel()
	const name = "fc_roundtrip"
	h, s := sampleHeader(name), sampleScenario(name)

	first, err := factorycorpus.Marshal(h, s)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, name+".yaml")
	if err := os.WriteFile(path, first, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := factorycorpus.Load(path)
	if err != nil {
		t.Fatalf("Load of just-written file: %v\n---\n%s", err, first)
	}

	second, err := factorycorpus.Marshal(f.Header, f.Scenario)
	if err != nil {
		t.Fatalf("re-Marshal: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("Marshal(Load(Marshal(x))) != Marshal(x); a re-bless would diff on formatting instead of on expectations\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

// TestWriterPreservesCellTypes pins that the reloaded expectation holds the
// same Go values it was written from. Byte equality alone would be satisfied
// by a writer that consistently corrupts a value in both directions.
func TestWriterPreservesCellTypes(t *testing.T) {
	t.Parallel()
	const name = "fc_celltypes"
	h, s := sampleHeader(name), sampleScenario(name)
	data, err := factorycorpus.Marshal(h, s)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, name+".yaml")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := factorycorpus.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Scenario.Tests) != len(s.Tests) {
		t.Fatalf("test count %d, want %d", len(f.Scenario.Tests), len(s.Tests))
	}
	for i := range s.Tests {
		want, got := s.Tests[i], f.Scenario.Tests[i]
		if want.Query != got.Query {
			t.Errorf("tests[%d] query: got %q want %q", i, got.Query, want.Query)
		}
		if want.Unordered != got.Unordered {
			t.Errorf("tests[%d] unordered: got %v want %v", i, got.Unordered, want.Unordered)
		}
		// Compared by VALUE, not by Go type: yaml.v3 decodes `!!int` into `int`
		// when the destination is `any`, so a cell written from an int64 comes
		// back as an int. The runner normalizes both to int64 before diffing,
		// so what has to survive is the number and its kind, not its width.
		if d := diffCells(want.Rows, got.Rows); d != "" {
			t.Errorf("tests[%d] rows: %s", i, d)
		}
	}
	if f.Scenario.SchemaTemplate != s.SchemaTemplate {
		t.Errorf("schema_template: got %q want %q", f.Scenario.SchemaTemplate, s.SchemaTemplate)
	}
	if fmt.Sprint(f.Scenario.Setup) != fmt.Sprint(s.Setup) {
		t.Errorf("setup: got %#v want %#v", f.Scenario.Setup, s.Setup)
	}
}

// TestNegativeZeroSurvivesTheWriter is the narrowest of the round-trip's
// claims, kept as its own case because it is the one with a bug behind it: a
// signed-zero DISTINCT defect shipped in this repo under full green. The
// generator seeds -0.0 into the DOUBLE/FLOAT domains, so an expectation cell
// holding it is reachable, and a writer that flattens it to 0 silently deletes
// the only test that could catch the sign being dropped.
func TestNegativeZeroSurvivesTheWriter(t *testing.T) {
	t.Parallel()
	const name = "fc_negzero"
	h := sampleHeader(name)
	s := &yamsql.Scenario{
		Name:           name,
		SchemaTemplate: "CREATE TABLE t (id BIGINT NOT NULL, d DOUBLE, PRIMARY KEY (id))",
		Setup:          []string{"INSERT INTO t VALUES (1, -0.0)"},
		Tests:          []yamsql.Test{{Query: "SELECT d FROM t", Rows: [][]any{{negZero()}}}},
	}
	data, err := factorycorpus.Marshal(h, s)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, name+".yaml")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := factorycorpus.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got := f.Scenario.Tests[0].Rows[0][0]
	gotF, ok := got.(float64)
	if !ok {
		t.Fatalf("cell reloaded as %T (%v), not float64: the sign of a negative zero is lost the moment it becomes an int", got, got)
	}
	if !isNegZero(gotF) {
		t.Fatalf("cell reloaded as %v, want -0.0 with the sign bit intact", gotF)
	}
}

func isNegZero(f float64) bool { return f == 0 && math.Signbit(f) }

// negZero returns a genuine negative zero. The Go constant expression `-0.0`
// is exactly 0 — untyped constants have no signed zero — so a test that spells
// it that way silently probes positive zero instead.
func negZero() float64 { return math.Copysign(0, -1) }

// diffCells compares expectation rows by value and kind, tolerating the
// int64/int and float64/float32 widening the YAML round trip performs.
func diffCells(want, got [][]any) string {
	if len(want) != len(got) {
		return fmt.Sprintf("row count %d, want %d", len(got), len(want))
	}
	for i := range want {
		if len(want[i]) != len(got[i]) {
			return fmt.Sprintf("row %d width %d, want %d", i, len(got[i]), len(want[i]))
		}
		for j := range want[i] {
			w, g := normCell(want[i][j]), normCell(got[i][j])
			if w != g {
				return fmt.Sprintf("row %d col %d: got %s, want %s", i, j, g, w)
			}
		}
	}
	return ""
}

func normCell(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return fmt.Sprintf("bool:%t", x)
	case string:
		return "str:" + x
	case int:
		return fmt.Sprintf("int:%d", int64(x))
	case int32:
		return fmt.Sprintf("int:%d", int64(x))
	case int64:
		return fmt.Sprintf("int:%d", x)
	case float32:
		return fmt.Sprintf("float:%v/%t", float64(x), math.Signbit(float64(x)))
	case float64:
		// The sign bit is part of the value: a float expectation that loses it
		// silently stops testing signed zero.
		return fmt.Sprintf("float:%v/%t", x, math.Signbit(x))
	default:
		return fmt.Sprintf("%T:%v", v, v)
	}
}

// TestHeaderRejectsIncompleteProvenance pins that a header is all-or-nothing.
// A header whose fields default silently keeps describing a file it no longer
// describes, and the census keeps reporting a confident number computed from
// absent data.
func TestHeaderRejectsIncompleteProvenance(t *testing.T) {
	t.Parallel()
	full := sampleHeader("fc_hdr").Render()
	for _, key := range []string{
		"format-version", "generator", "seed", "query-index", "projection",
		"date", "blessing", "oracles", "feature-vector", "plan-shape", "dedup-key",
	} {
		key := key
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			var b bytes.Buffer
			for _, line := range splitLines(full) {
				if hasKey(line, key) {
					continue
				}
				b.WriteString(line)
				b.WriteString("\n")
			}
			if _, err := factorycorpus.ParseHeader(b.Bytes()); err == nil {
				t.Fatalf("header without %q parsed clean; a missing provenance key must never default silently", key)
			}
		})
	}
}

// TestHeaderRejectsStaleDedupKey pins the cross-check between the recorded key
// and the fields it is a digest of. Without it, hand-editing a feature vector
// leaves the key pointing at the old point and the dedup set admits a
// duplicate.
func TestHeaderRejectsStaleDedupKey(t *testing.T) {
	t.Parallel()
	h := sampleHeader("fc_stale")
	h.FeatureVector = "shape=single;idx=none;proj=star;where=cmp.eq;order=none"
	if _, err := factorycorpus.ParseHeader([]byte(h.Render())); err == nil {
		t.Fatal("a header whose dedup-key does not digest its own (feature-vector, plan-shape) parsed clean")
	}
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func hasKey(line, key string) bool {
	prefix := "# " + key + ":"
	return len(line) >= len(prefix) && line[:len(prefix)] == prefix
}
