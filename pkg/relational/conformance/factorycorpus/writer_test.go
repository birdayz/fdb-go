package factorycorpus_test

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/conformance/factorycorpus"
	"fdb.dev/pkg/relational/conformance/javayamsql"
	"fdb.dev/pkg/relational/conformance/yamsql"
)

// sampleFV keeps every synthetic scenario in this file inside ONE family, so a
// marshalled family file can hold several of them.
const sampleFV = "shape=single;idx=A;proj=star;where=cmp.gt;order=none"

func sampleHeader(name string, seed uint64, planShape string) factorycorpus.Header {
	return factorycorpus.Header{
		Name:          name,
		Generator:     "rowdiff-gen/1",
		Seed:          seed,
		QueryIndex:    2,
		Projection:    1,
		Date:          "2026-07-31",
		Blessing:      factorycorpus.BlessingMetamorphic,
		Oracles:       []string{"tlp", "second-plan"},
		FeatureVector: sampleFV,
		PlanShape:     planShape,
		DedupKey:      factorycorpus.DedupKeyOf(sampleFV, planShape),
	}
}

// sampleScenario deliberately carries every cell type the corpus can freeze,
// INCLUDING the ones that break a naive marshaller: negative zero at both
// float widths, a float with no fractional part, a string that looks like an
// integer, the empty string, a padded string, a NULL, and an empty result set.
func sampleScenario(name string, seed uint64, planShape string) *factorycorpus.Scenario {
	return &factorycorpus.Scenario{
		Header: sampleHeader(name, seed, planShape),
		Doc: &yamsql.Scenario{
			Name:           name,
			SchemaTemplate: "CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, d DOUBLE, e FLOAT, s STRING, PRIMARY KEY (id))\nCREATE INDEX idx_a ON t (a)",
			Setup:          []string{"INSERT INTO t VALUES (1, 5, -0.0, 0.1, 'alpha'), (2, NULL, 2.0, -0.0, '')"},
			Tests: []yamsql.Test{
				{
					Query:     "SELECT id, a, d, e, s FROM t",
					Unordered: true,
					Columns:   []string{"ID", "A", "D", "E", "S"},
					Rows: [][]any{
						// negZero() rather than the literal -0.0: an untyped Go
						// constant `-0.0` is exactly 0, so writing it here would
						// hand the test a POSITIVE zero and quietly stop probing
						// the case it exists for.
						{int64(1), int64(5), negZero(), float32(0.1), "alpha"},
						{int64(2), nil, 2.0, negZero32(), ""},
					},
				},
				{
					Query: "SELECT id FROM t WHERE a > 100",
					Rows:  [][]any{},
				},
				{
					Query:   "SELECT s FROM t WHERE id = 1",
					Columns: []string{"S"},
					Rows:    [][]any{{"7"}, {" c "}},
				},
				{
					Query:   "SELECT id, TRUE AS flag FROM t WHERE id = 1",
					Columns: []string{"ID", "FLAG"},
					Rows:    [][]any{{int64(1), true}},
				},
			},
		},
	}
}

// TestWriterRoundTripIsByteExact is the writer's whole contract.
//
// Without it a re-bless (§5.3) produces a diff full of reformatting noise and
// a reader cannot see which expectation actually moved — which is the only
// thing a re-bless review is for. The file holds TWO scenarios handed to the
// writer in the WRONG order, so the round trip also pins the derived ordering
// that makes a nightly append deterministic.
func TestWriterRoundTripIsByteExact(t *testing.T) {
	t.Parallel()
	entries := []*factorycorpus.Scenario{
		sampleScenario("fc_roundtrip_b", 9, "1111111111111111"),
		sampleScenario("fc_roundtrip_a", 4, "2222222222222222"),
	}

	first, err := factorycorpus.MarshalFamily(entries)
	if err != nil {
		t.Fatalf("MarshalFamily: %v", err)
	}

	path := filepath.Join(t.TempDir(), factorycorpus.FamilyFileName(factorycorpus.FamilyOf(sampleFV)))
	if err := os.WriteFile(path, first, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := factorycorpus.Load(path)
	if err != nil {
		t.Fatalf("Load of just-written file: %v\n---\n%s", err, first)
	}
	if len(f.Scenarios) != 2 {
		t.Fatalf("loaded %d scenarios, wrote 2", len(f.Scenarios))
	}
	if f.Scenarios[0].Header.Name != "fc_roundtrip_a" {
		t.Errorf("scenarios are not sorted by seed: first is %s", f.Scenarios[0].Header.Name)
	}

	second, err := factorycorpus.MarshalFamily(f.Scenarios)
	if err != nil {
		t.Fatalf("re-Marshal: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("MarshalFamily(Load(MarshalFamily(x))) != MarshalFamily(x); a re-bless would diff on formatting instead of on expectations\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

// TestEmittedFamilyFileIsCleanYamsql is the emitted-format gate at the unit
// level: whatever the writer produces must parse through the SAME strict
// parser that gates the vendored Java corpus, with no inert directives — a key
// the Java runner would silently ignore is a key the writer must never emit.
// (The committed corpus has the same gate over every file in
// TestCommittedCorpusIsCleanYamsql.)
func TestEmittedFamilyFileIsCleanYamsql(t *testing.T) {
	t.Parallel()
	data, err := factorycorpus.MarshalFamily([]*factorycorpus.Scenario{
		sampleScenario("fc_yamsql_gate", 3, "3333333333333333"),
	})
	if err != nil {
		t.Fatal(err)
	}
	file, err := javayamsql.Parse("fc_yamsql_gate.yamsql", data)
	if err != nil {
		t.Fatalf("the javayamsql parser rejects the writer's output: %v\n---\n%s", err, data)
	}
	if len(file.Blocks) != 3 {
		t.Fatalf("one scenario emitted %d blocks, want the (schema_template, setup, test_block) triple", len(file.Blocks))
	}
	for _, blk := range file.Blocks {
		if len(blk.Inert) > 0 {
			t.Errorf("emitted %s block carries an inert directive %s — Java would silently ignore it", blk.Kind, blk.Inert[0])
		}
	}
}

// TestWriterPreservesCellTypes pins that the reloaded expectation holds the
// same Go values it was written from. Byte equality alone would be satisfied
// by a writer that consistently corrupts a value in both directions.
func TestWriterPreservesCellTypes(t *testing.T) {
	t.Parallel()
	src := sampleScenario("fc_celltypes", 6, "4444444444444444")
	data, err := factorycorpus.MarshalFamily([]*factorycorpus.Scenario{src})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), factorycorpus.FamilyFileName(factorycorpus.FamilyOf(sampleFV)))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := factorycorpus.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got := f.Scenarios[0].Doc
	want := src.Doc
	if len(got.Tests) != len(want.Tests) {
		t.Fatalf("test count %d, want %d", len(got.Tests), len(want.Tests))
	}
	for i := range want.Tests {
		w, g := want.Tests[i], got.Tests[i]
		if w.Query != g.Query {
			t.Errorf("tests[%d] query: got %q want %q", i, g.Query, w.Query)
		}
		if w.Unordered != g.Unordered {
			t.Errorf("tests[%d] unordered: got %v want %v", i, g.Unordered, w.Unordered)
		}
		if d := diffCells(w.Rows, g.Rows); d != "" {
			t.Errorf("tests[%d] rows: %s", i, d)
		}
	}
	if got.SchemaTemplate != want.SchemaTemplate {
		t.Errorf("schema_template: got %q want %q", got.SchemaTemplate, want.SchemaTemplate)
	}
	if fmt.Sprint(got.Setup) != fmt.Sprint(want.Setup) {
		t.Errorf("setup: got %#v want %#v", got.Setup, want.Setup)
	}
}

// TestNegativeZeroSurvivesTheWriter is the narrowest of the round-trip's
// claims, kept as its own case because it is the one with a bug behind it: a
// signed-zero DISTINCT defect shipped in this repo under full green. The
// generator seeds -0.0 into the DOUBLE/FLOAT domains, so an expectation cell
// holding it is reachable, and a writer that flattens it to 0 silently deletes
// the only test that could catch the sign being dropped. Both float widths are
// probed: DOUBLE freezes as a plain YAML float, FLOAT under the `!f` tag.
func TestNegativeZeroSurvivesTheWriter(t *testing.T) {
	t.Parallel()
	sc := sampleScenario("fc_negzero", 8, "5555555555555555")
	sc.Doc.Tests = []yamsql.Test{{
		Query:   "SELECT d, e FROM t WHERE id = 1",
		Columns: []string{"D", "E"},
		Rows:    [][]any{{negZero(), negZero32()}},
	}}
	data, err := factorycorpus.MarshalFamily([]*factorycorpus.Scenario{sc})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), factorycorpus.FamilyFileName(factorycorpus.FamilyOf(sampleFV)))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := factorycorpus.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	row := f.Scenarios[0].Doc.Tests[0].Rows[0]
	d, ok := row[0].(float64)
	if !ok {
		t.Fatalf("DOUBLE cell reloaded as %T (%v), not float64: the sign of a negative zero is lost the moment it becomes an int", row[0], row[0])
	}
	if !(d == 0 && math.Signbit(d)) {
		t.Fatalf("DOUBLE cell reloaded as %v, want -0.0 with the sign bit intact", d)
	}
	e, ok := row[1].(float32)
	if !ok {
		t.Fatalf("FLOAT cell reloaded as %T (%v), not float32", row[1], row[1])
	}
	if !(e == 0 && math.Signbit(float64(e))) {
		t.Fatalf("FLOAT cell reloaded as %v, want -0.0 with the sign bit intact", e)
	}
}

// TestWriterRefusesDuplicateColumnLabels pins the by-name cell precondition:
// Java resolves cell names case-insensitively against the result set, so a
// repeated label would silently match the FIRST column twice and the second
// column's value would never be asserted at all.
func TestWriterRefusesDuplicateColumnLabels(t *testing.T) {
	t.Parallel()
	sc := sampleScenario("fc_dupcols", 5, "6666666666666666")
	sc.Doc.Tests = []yamsql.Test{{
		Query:   "SELECT a, a FROM t",
		Columns: []string{"A", "a"},
		Rows:    [][]any{{int64(1), int64(1)}},
	}}
	if _, err := factorycorpus.MarshalFamily([]*factorycorpus.Scenario{sc}); err == nil {
		t.Fatal("a scenario with case-insensitively duplicate column labels marshalled clean; " +
			"its second column would never be asserted")
	}
}

// negZero returns a genuine negative zero. The Go constant expression `-0.0`
// is exactly 0 — untyped constants have no signed zero — so a test that spells
// it that way silently probes positive zero instead.
func negZero() float64 { return math.Copysign(0, -1) }

func negZero32() float32 { return float32(math.Copysign(0, -1)) }

// diffCells compares expectation rows by value and kind, including the float
// width and the sign bit.
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
		return fmt.Sprintf("float32:%v/%t", float64(x), math.Signbit(float64(x)))
	case float64:
		// The sign bit is part of the value: a float expectation that loses it
		// silently stops testing signed zero.
		return fmt.Sprintf("float64:%v/%t", x, math.Signbit(x))
	default:
		return fmt.Sprintf("%T:%v", v, v)
	}
}

// TestHeaderRejectsIncompleteProvenance pins that a provenance block is
// all-or-nothing. A header whose fields default silently keeps describing a
// scenario it no longer describes, and the census keeps reporting a confident
// number computed from absent data.
func TestHeaderRejectsIncompleteProvenance(t *testing.T) {
	t.Parallel()
	full := strings.Split(strings.TrimRight(sampleHeader("fc_hdr", 7, "0123456789abcdef").Render(), "\n"), "\n")
	for _, key := range []string{
		"scenario", "generator", "seed", "query-index", "projection",
		"date", "blessing", "oracles", "feature-vector", "plan-shape", "dedup-key",
	} {
		key := key
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			var lines []string
			for _, line := range full {
				if strings.HasPrefix(line, "# "+key+":") {
					continue
				}
				lines = append(lines, line)
			}
			if _, err := factorycorpus.ParseHeader(lines); err == nil {
				t.Fatalf("provenance without %q parsed clean; a missing key must never default silently", key)
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
	h := sampleHeader("fc_stale", 7, "0123456789abcdef")
	h.FeatureVector = "shape=single;idx=none;proj=star;where=cmp.eq;order=none"
	lines := strings.Split(strings.TrimRight(h.Render(), "\n"), "\n")
	if _, err := factorycorpus.ParseHeader(lines); err == nil {
		t.Fatal("a header whose dedup-key does not digest its own (feature-vector, plan-shape) parsed clean")
	}
}
