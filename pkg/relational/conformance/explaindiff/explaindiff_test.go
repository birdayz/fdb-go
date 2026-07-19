package explaindiff_test

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	"fdb.dev/pkg/relational/conformance/explaindiff"
)

// corpusDir is the yamsql corpus, relative to this package. Bazel stages it
// via the go_test `data` attribute.
const corpusDir = "../yamsql/testdata"

// corpusOnce memoizes one full corpus planning pass. Planning all ~2400
// queries takes a couple of seconds; the assertion tests below inspect
// different dimensions of the SAME pass, so there is no reason to redo it
// per test. TestBaselineIsDeterministic deliberately does NOT use this — it
// needs two independent passes.
var (
	corpusOnce    sync.Once
	corpusEntries []explaindiff.Entry
	corpusStats   explaindiff.Stats
	corpusErr     error
)

func collectCorpus(t *testing.T) ([]explaindiff.Entry, explaindiff.Stats) {
	t.Helper()
	corpusOnce.Do(func() {
		corpusEntries, corpusStats, corpusErr = explaindiff.Collect(corpusDir)
	})
	if corpusErr != nil {
		t.Fatalf("collect corpus: %v", corpusErr)
	}
	return corpusEntries, corpusStats
}

// TestBaselineIsDeterministic is the load-bearing property: RFC-183 compares
// two baselines produced by two builds, so any nondeterminism inside ONE
// build makes every comparison meaningless. Generate the whole corpus twice
// in the same process and require byte equality.
//
// A failure here is never "flaky output" — it is map iteration, a pointer
// address, or genuine planner nondeterminism leaking into the plan, all of
// which are real bugs.
func TestBaselineIsDeterministic(t *testing.T) {
	t.Parallel()

	first, st1, err := explaindiff.GenerateBaseline(corpusDir)
	if err != nil {
		t.Fatalf("first baseline: %v", err)
	}
	second, st2, err := explaindiff.GenerateBaseline(corpusDir)
	if err != nil {
		t.Fatalf("second baseline: %v", err)
	}
	if st1 != st2 {
		t.Fatalf("stats differ across runs: %+v vs %+v", st1, st2)
	}
	if first != second {
		t.Fatalf("baseline is NOT deterministic:\n%s", firstDifferingLine(first, second))
	}
	if st1.Queries == 0 {
		t.Fatal("planned 0 queries — the corpus glob or the loader is broken")
	}
}

// TestBaselineRoundTrips proves Render and Parse are inverses. `diff` parses
// two baselines back into entries to report per-QUERY verdicts; if the parse
// silently dropped a field, a real plan change could diff as clean.
func TestBaselineRoundTrips(t *testing.T) {
	t.Parallel()

	entries, st := collectCorpus(t)
	text := explaindiff.Render(entries, st)

	parsed, err := explaindiff.Parse(text)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(parsed) != len(entries) {
		t.Fatalf("round-trip lost entries: rendered %d, parsed %d", len(entries), len(parsed))
	}
	if again := explaindiff.Render(parsed, st); again != text {
		t.Fatalf("round-trip is lossy:\n%s", firstDifferingLine(text, again))
	}
	// A round-trip of the SAME baseline must diff clean; anything else means
	// Diff is comparing a field Parse doesn't restore.
	if rep := explaindiff.Diff(entries, parsed); !rep.Clean() {
		t.Fatalf("self-diff is not clean: %d deltas, first %s", len(rep.Deltas), rep.Deltas[0].Key)
	}
}

// unexpectedPlanFailures pins the corpus queries that fail to plan although
// their stanza expects rows. It must stay EXACTLY this set.
//
// All four are INFORMATION_SCHEMA queries, and they are not planner gaps:
// cascades_generator.planSelect routes INFORMATION_SCHEMA off to a
// catalog-backed system-table handler that never enters Cascades, so there
// is no physical plan to render. They are recorded rather than filtered so
// the count reconciles with the corpus.
//
// Any OTHER entry appearing here is a query that stopped planning — the
// exact regression class RFC-183 P0's exit gate is looking for. Do not
// extend this list to make a red test green; fix the planner.
var unexpectedPlanFailures = []string{
	"information_schema.yaml#0",
	"information_schema.yaml#1",
	"information_schema.yaml#2",
	"information_schema.yaml#3",
}

// TestNoUnexpectedPlanFailures is the always-on sentinel the baseline files
// themselves cannot be (they are before/after artifacts, not committed).
// A row-level test cannot see this either: a query that stops planning fails
// its own scenario, but nothing tells you the corpus-wide set MOVED.
func TestNoUnexpectedPlanFailures(t *testing.T) {
	t.Parallel()

	entries, _ := collectCorpus(t)

	var got []string
	detail := map[string]string{}
	for _, e := range entries {
		if e.UnexpectedlyFailed() {
			got = append(got, e.Key())
			detail[e.Key()] = e.Plan
		}
	}
	sort.Strings(got)

	want := append([]string(nil), unexpectedPlanFailures...)
	sort.Strings(want)

	for _, k := range diffSets(got, want) {
		t.Errorf("query STOPPED PLANNING (not in the pinned allowlist): %s\n  %s", k, detail[k])
	}
	for _, k := range diffSets(want, got) {
		t.Errorf("pinned unexpected-failure %s now plans — good; remove it from unexpectedPlanFailures", k)
	}
}

// TestNoPlanPanics pins design principle 4 (never panic in library code)
// across the whole corpus. The dumper recovers panics into a marker so one
// bad query cannot abort the run, which makes it exactly the harness that
// can assert there are none.
func TestNoPlanPanics(t *testing.T) {
	t.Parallel()

	entries, _ := collectCorpus(t)
	for _, e := range entries {
		if e.Panicked() {
			t.Errorf("planner PANICKED on %s\n  sql: %s\n  %s", e.Key(), e.SQL, e.Plan)
		}
	}
}

// TestShapeAccompaniesEverySuccessfulPlan guards the invariant the diff's
// SHAPE tag depends on: a planned entry always carries a structural
// skeleton, and a failed one never does. Without it a shape-flip could
// register as a plain label change.
func TestShapeAccompaniesEverySuccessfulPlan(t *testing.T) {
	t.Parallel()

	entries, _ := collectCorpus(t)
	for _, e := range entries {
		switch {
		case e.Failed() && len(e.Shape) != 0:
			t.Errorf("%s: failure marker carries a shape: %v", e.Key(), e.Shape)
		case !e.Failed() && len(e.Shape) == 0:
			t.Errorf("%s: planned entry has no shape (plan: %s)", e.Key(), e.Plan)
		case !e.Failed() && e.Plan == "":
			t.Errorf("%s: planned entry rendered an empty plan line", e.Key())
		}
	}
}

// TestDiffClassifies exercises every verdict Diff can reach, on synthetic
// entries — the corpus cannot produce an ADDED/REMOVED/regressed pair on
// demand, and these classifications are what a reviewer reads first.
func TestDiffClassifies(t *testing.T) {
	t.Parallel()

	oldE := []explaindiff.Entry{
		{File: "a.yaml", Index: 0, SQL: "s0", Plan: "Scan(T)", Shape: []string{"RecordQueryScanPlan"}},
		{File: "a.yaml", Index: 1, SQL: "s1", Plan: "Scan(T)", Shape: []string{"RecordQueryScanPlan"}},
		{File: "a.yaml", Index: 2, SQL: "s2", Plan: "Scan(T)", Shape: []string{"RecordQueryScanPlan"}},
		{File: "a.yaml", Index: 3, SQL: "s3", Plan: "Scan(T)", Shape: []string{"RecordQueryScanPlan"}},
		{File: "a.yaml", Index: 4, SQL: "s4", Plan: "<PLAN-ERROR: nope›", ErrorPin: "42703"},
	}
	newE := []explaindiff.Entry{
		// 0 unchanged.
		{File: "a.yaml", Index: 0, SQL: "s0", Plan: "Scan(T)", Shape: []string{"RecordQueryScanPlan"}},
		// 1 structural flip.
		{
			File: "a.yaml", Index: 1, SQL: "s1", Plan: "Fetch(IndexScan(I))",
			Shape: []string{"RecordQueryFetchFromPartialRecordPlan", "  RecordQueryIndexPlan"},
		},
		// 2 label-only churn: same skeleton, different rendering.
		{File: "a.yaml", Index: 2, SQL: "s2", Plan: "Scan(T, [])", Shape: []string{"RecordQueryScanPlan"}},
		// 3 stopped planning.
		{File: "a.yaml", Index: 3, SQL: "s3", Plan: "<PLAN-ERROR: boom›"},
		// 4 recovered.
		{
			File: "a.yaml", Index: 4, SQL: "s4", Plan: "Scan(T)", ErrorPin: "42703",
			Shape: []string{"RecordQueryScanPlan"},
		},
		// 5 added.
		{File: "a.yaml", Index: 5, SQL: "s5", Plan: "Scan(U)", Shape: []string{"RecordQueryScanPlan"}},
	}
	// A key present only on the old side.
	oldE = append(oldE, explaindiff.Entry{File: "b.yaml", Index: 0, SQL: "gone", Plan: "Scan(V)"})

	rep := explaindiff.Diff(oldE, newE)

	if rep.Same != 1 {
		t.Errorf("Same = %d, want 1", rep.Same)
	}
	if rep.ShapeFlips != 1 {
		t.Errorf("ShapeFlips = %d, want 1 (only a.yaml#1 restructures)", rep.ShapeFlips)
	}
	if rep.Regressions != 1 {
		t.Errorf("Regressions = %d, want 1", rep.Regressions)
	}
	if rep.Recoveries != 1 {
		t.Errorf("Recoveries = %d, want 1", rep.Recoveries)
	}
	if rep.Clean() {
		t.Error("Clean() = true on a differing pair")
	}

	byKey := map[string]explaindiff.Delta{}
	for _, d := range rep.Deltas {
		byKey[d.Key] = d
	}
	if d := byKey["a.yaml#2"]; d.ShapeChanged {
		t.Error("a.yaml#2 is a label-only change; it must not be tagged as a shape flip")
	}
	if d := byKey["a.yaml#3"]; !d.RegressedToError {
		t.Error("a.yaml#3 stopped planning but was not flagged RegressedToError")
	}
	if d := byKey["a.yaml#5"]; d.Kind != explaindiff.Added {
		t.Errorf("a.yaml#5 kind = %v, want ADDED", d.Kind)
	}
	if d := byKey["b.yaml#0"]; d.Kind != explaindiff.Removed {
		t.Errorf("b.yaml#0 kind = %v, want REMOVED", d.Kind)
	}

	// Deltas are reported in (file, numeric index) order so the report is
	// itself diffable and file#2 sorts before file#10.
	var order []string
	for _, d := range rep.Deltas {
		order = append(order, d.Key)
	}
	want := []string{"a.yaml#1", "a.yaml#2", "a.yaml#3", "a.yaml#4", "a.yaml#5", "b.yaml#0"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("delta order = %v, want %v", order, want)
	}

	text := explaindiff.RenderDiff(rep)
	for _, must := range []string{"SHAPE", "STOPPED-PLANNING", "NOW-PLANS", "ADDED", "REMOVED"} {
		if !strings.Contains(text, must) {
			t.Errorf("rendered diff is missing the %q tag:\n%s", must, text)
		}
	}
}

// TestDiffCleanOnIdenticalInput is the negative half of TestDiffClassifies:
// the harness must not cry wolf, or the whole gate gets ignored.
func TestDiffCleanOnIdenticalInput(t *testing.T) {
	t.Parallel()

	e := []explaindiff.Entry{
		{File: "a.yaml", Index: 0, SQL: "s", Plan: "Scan(T)", Shape: []string{"RecordQueryScanPlan"}},
		{File: "a.yaml", Index: 10, SQL: "s", Plan: "Scan(T)", Shape: []string{"RecordQueryScanPlan"}},
	}
	rep := explaindiff.Diff(e, e)
	if !rep.Clean() {
		t.Fatalf("identical baselines diffed as %d deltas", len(rep.Deltas))
	}
	if !strings.Contains(explaindiff.RenderDiff(rep), "CLEAN") {
		t.Error("a clean report must say so")
	}
}

// TestParseRejectsGarbage: a malformed baseline must fail loudly. Silently
// parsing to zero entries would make `diff` report a clean run against
// anything — the worst possible failure mode for a regression gate.
func TestParseRejectsGarbage(t *testing.T) {
	t.Parallel()

	for name, text := range map[string]string{
		"payload before header": "sql:   SELECT 1\n",
		"unkeyed entry":         "=== nokey\n",
		"non-numeric index":     "=== a.yaml#x\n",
		"unknown line":          "=== a.yaml#0\nwhat:  ever\n",
	} {
		if _, err := explaindiff.Parse(text); err == nil {
			t.Errorf("%s: Parse accepted malformed input %q", name, text)
		}
	}
}

// TestPlanErrorMarkersAreSingleLine guards the format's one-line-per-field
// invariant against multi-line planner errors: a marker that smuggled a
// newline would break Parse and split one query across two diff hunks.
func TestPlanErrorMarkersAreSingleLine(t *testing.T) {
	t.Parallel()

	entries, st := collectCorpus(t)
	for _, e := range entries {
		if strings.ContainsAny(e.Plan, "\n\r") {
			t.Errorf("%s: plan line contains a newline: %q", e.Key(), e.Plan)
		}
		if strings.ContainsAny(e.SQL, "\n\r") {
			t.Errorf("%s: sql line contains a newline: %q", e.Key(), e.SQL)
		}
	}
	// Every rendered line must be structural or prefixed — the property
	// Parse relies on.
	for _, line := range strings.Split(explaindiff.Render(entries, st), "\n") {
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "=== ") ||
			strings.HasPrefix(line, "sql:   ") || strings.HasPrefix(line, "plan:  ") ||
			strings.HasPrefix(line, "shape: ") {
			continue
		}
		t.Fatalf("unrecognized rendered line: %q", line)
	}
}

// diffSets returns the elements of a that are not in b. Both must be sorted.
func diffSets(a, b []string) []string {
	in := make(map[string]bool, len(b))
	for _, s := range b {
		in[s] = true
	}
	var out []string
	for _, s := range a {
		if !in[s] {
			out = append(out, s)
		}
	}
	return out
}

// firstDifferingLine renders the first line where two baselines diverge,
// with context — a 700KB byte-diff message is unreadable otherwise.
func firstDifferingLine(a, b string) string {
	al := strings.Split(a, "\n")
	bl := strings.Split(b, "\n")
	for i := 0; i < len(al) && i < len(bl); i++ {
		if al[i] != bl[i] {
			var ctx strings.Builder
			for j := max(0, i-3); j < i; j++ {
				ctx.WriteString("  " + al[j] + "\n")
			}
			ctx.WriteString("- " + al[i] + "\n+ " + bl[i] + "\n")
			return ctx.String()
		}
	}
	return fmt.Sprintf("one baseline is a prefix of the other (lines: %d vs %d)", len(al), len(bl))
}
