package explaindiff_test

import (
	"fmt"
	"os"
	"path/filepath"
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

	b, err := explaindiff.Parse(text)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	parsed := b.Entries
	if len(parsed) != len(entries) {
		t.Fatalf("round-trip lost entries: rendered %d, parsed %d", len(entries), len(parsed))
	}
	// The header is data, not decoration: it must survive the round-trip too,
	// because Parse now enforces it.
	if b.Stats != st {
		t.Fatalf("round-trip lost the stats header: %+v vs %+v", b.Stats, st)
	}
	if again := explaindiff.Render(parsed, b.Stats); again != text {
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

// TestCorpusPlansDML is the sentinel for this harness's whole reason to exist:
// the dump must actually PLAN the DELETE/UPDATE stanzas, not skip them. A
// regression that reverted DML routing back to NonQuery would drop the count to
// zero and re-open the RFC-184 W2 blind spot — and every OTHER test here would
// stay green, because they assert on whatever entries exist. This one asserts
// the entries EXIST.
func TestCorpusPlansDML(t *testing.T) {
	t.Parallel()

	entries, st := collectCorpus(t)

	if st.DML == 0 {
		t.Fatal("no DELETE/UPDATE stanzas planned — the DML harness is not wired into the dump")
	}
	// Reconciliation: every entry is a SELECT-class or a DML-class plan; nothing
	// else produces an entry. If this drifts, the header's truncation check
	// (Queries+DML == entries) would start rejecting a valid dump.
	if st.Queries+st.DML != len(entries) {
		t.Fatalf("entry count %d != queries=%d + dml=%d", len(entries), st.Queries, st.DML)
	}

	// The DML plans must reach the two DML physical roots — a DELETE that only
	// ever rendered a scan (dropped delete wrapper) would be a silent data-loss
	// bug this shape pin catches.
	var sawDelete, sawUpdate bool
	for _, e := range entries {
		for _, s := range e.Shape {
			switch strings.TrimSpace(s) {
			case "RecordQueryDeletePlan":
				sawDelete = true
			case "RecordQueryUpdatePlan":
				sawUpdate = true
			}
		}
	}
	if !sawDelete {
		t.Error("no RecordQueryDeletePlan in the corpus dump — DELETE planning is broken or unrouted")
	}
	if !sawUpdate {
		t.Error("no RecordQueryUpdatePlan in the corpus dump — UPDATE planning is broken or unrouted")
	}
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

// header builds a well-formed baseline header declaring `queries` entries, so
// the body-level rejection tests below exercise the body and not the header.
func header(queries int) string {
	return fmt.Sprintf("# explain-baseline/v2\n# files=1 queries=%d dml=0 non_query=0 plan_errors=0 unexpected_errors=0\n#\n", queries)
}

// TestParseRejectsGarbage: a malformed baseline must fail loudly. Silently
// parsing to zero entries would make `diff` report a clean run against
// anything — the worst possible failure mode for a regression gate.
func TestParseRejectsGarbage(t *testing.T) {
	t.Parallel()

	for name, text := range map[string]string{
		"payload before header": header(1) + "sql:   SELECT 1\n",
		"unkeyed entry":         header(1) + "=== nokey\n",
		"non-numeric index":     header(1) + "=== a.yaml#x\n",
		"unknown line":          header(1) + "=== a.yaml#0\nwhat:  ever\n",
	} {
		if _, err := explaindiff.Parse(text); err == nil {
			t.Errorf("%s: Parse accepted malformed input %q", name, text)
		}
	}
}

// TestParseEnforcesHeader covers the gap that made formatVersion and the
// stats line decorative: both were WRITTEN and never READ, so a baseline from
// an older rendering, or one whose dump died mid-write, parsed happily and
// then diffed as a pile of phantom plan changes (or, worse, as CLEAN).
func TestParseEnforcesHeader(t *testing.T) {
	t.Parallel()

	body := "=== a.yaml#0\nsql:   SELECT 1\nplan:  Scan(T)\nshape: RecordQueryScanPlan\n"

	cases := map[string]struct {
		text string
		want string // substring the message must carry
	}{
		"stale format version": {
			text: "# explain-baseline/v0\n# files=1 queries=1 dml=0 non_query=0 plan_errors=0 unexpected_errors=0\n#\n" + body,
			want: "regenerate",
		},
		"no version header": {
			text: "# files=1 queries=1 dml=0 non_query=0 plan_errors=0 unexpected_errors=0\n#\n" + body,
			want: "format-version header",
		},
		"no stats header": {
			text: "# explain-baseline/v2\n#\n" + body,
			want: "stats header",
		},
		"malformed stats header": {
			text: "# explain-baseline/v2\n# files=1 queries=oops dml=0 non_query=0 plan_errors=0 unexpected_errors=0\n#\n" + body,
			want: "stats header",
		},
		// The truncation signal: the dump wrote its header, then died. The
		// tail is gone but nothing in the body says so.
		"header count exceeds entries": {
			text: header(2400) + body,
			want: "TRUNCATED",
		},
	}
	for name, tc := range cases {
		tc := tc
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b, err := explaindiff.Parse(tc.text)
			if err == nil {
				t.Fatalf("Parse accepted %d entries from a bad header", len(b.Entries))
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestParseAcceptsCurrentHeader is the positive half: the enforcement above
// must not reject the format the tool actually writes.
func TestParseAcceptsCurrentHeader(t *testing.T) {
	t.Parallel()

	b, err := explaindiff.Parse(header(1) + "=== a.yaml#0\nsql:   SELECT 1\nplan:  Scan(T)\nshape: RecordQueryScanPlan\n")
	if err != nil {
		t.Fatalf("Parse rejected a well-formed baseline: %v", err)
	}
	if b.Version != "explain-baseline/v2" {
		t.Errorf("Version = %q", b.Version)
	}
	if b.Stats.Files != 1 || b.Stats.Queries != 1 {
		t.Errorf("stats header not parsed: %+v", b.Stats)
	}
	if len(b.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(b.Entries))
	}
}

// synthBaseline builds an n-entry in-memory baseline at the given path.
func synthBaseline(path string, n int) *explaindiff.Baseline {
	b := &explaindiff.Baseline{Version: "explain-baseline/v2", Path: path}
	for i := 0; i < n; i++ {
		b.Entries = append(b.Entries, explaindiff.Entry{
			File: "a.yaml", Index: i, SQL: fmt.Sprintf("s%d", i),
			Plan: "Scan(T)", Shape: []string{"RecordQueryScanPlan"},
		})
	}
	b.Stats = explaindiff.Stats{Files: 1, Queries: n}
	return b
}

// TestValidateRejectsEmptyAndNearEmptyBaselines pins the harness's worst
// failure mode: a dump that covered NOTHING compared CLEAN and exited 0, so
// "no plan changed" was reported by a run that never looked at a plan.
func TestValidateRejectsEmptyAndNearEmptyBaselines(t *testing.T) {
	t.Parallel()

	full := synthBaseline("/tmp/new.txt", 2400)

	cases := map[string]struct {
		old, new *explaindiff.Baseline
		want     string
	}{
		"both empty": {synthBaseline("/tmp/a.txt", 0), synthBaseline("/tmp/b.txt", 0), "ZERO entries"},
		"old empty":  {synthBaseline("/tmp/a.txt", 0), full, "ZERO entries"},
		"new empty":  {full, synthBaseline("/tmp/b.txt", 0), "ZERO entries"},
		"both near-empty": {
			synthBaseline("/tmp/a.txt", explaindiff.MinBaselineEntries-1),
			synthBaseline("/tmp/b.txt", explaindiff.MinBaselineEntries-1),
			"floor is",
		},
	}
	for name, tc := range cases {
		tc := tc
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := explaindiff.ValidateDiffInputs(tc.old, tc.new)
			if err == nil {
				t.Fatal("ValidateDiffInputs accepted a baseline that proves nothing")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestValidateRejectsEntryCountDrift: a dump killed part-way through still
// parses (its header count is recomputed per run), so the only way to spot it
// is that its sibling holds far more entries. The rule is a 10% relative gap.
func TestValidateRejectsEntryCountDrift(t *testing.T) {
	t.Parallel()

	// 2400 vs 2000 is a 16.7% gap — an aborted dump, not a corpus edit.
	if err := explaindiff.ValidateDiffInputs(synthBaseline("/tmp/a.txt", 2400), synthBaseline("/tmp/b.txt", 2000)); err == nil {
		t.Error("ValidateDiffInputs accepted a 16.7% entry-count gap")
	}
	// Symmetric: growth is just as suspicious as shrinkage.
	if err := explaindiff.ValidateDiffInputs(synthBaseline("/tmp/a.txt", 2000), synthBaseline("/tmp/b.txt", 2400)); err == nil {
		t.Error("ValidateDiffInputs accepted a 16.7% entry-count gap in the growth direction")
	}
	// A real corpus edit adds a handful of stanzas. That must still diff.
	if err := explaindiff.ValidateDiffInputs(synthBaseline("/tmp/a.txt", 2400), synthBaseline("/tmp/b.txt", 2412)); err != nil {
		t.Errorf("ValidateDiffInputs rejected a plausible 12-stanza corpus addition: %v", err)
	}
}

// TestValidateRejectsSelfDiff covers the "diff OLD OLD" hole: comparing a
// file to itself always reported CLEAN, which reads exactly like the
// evidence the harness is supposed to produce.
func TestValidateRejectsSelfDiff(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "base.txt")
	content := explaindiff.Render(synthBaseline("", 2400).Entries, explaindiff.Stats{Files: 1, Queries: 2400})
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	load := func(p string) *explaindiff.Baseline {
		t.Helper()
		b, err := explaindiff.LoadBaseline(p)
		if err != nil {
			t.Fatalf("load %s: %v", p, err)
		}
		return b
	}

	t.Run("same path", func(t *testing.T) {
		t.Parallel()
		err := explaindiff.ValidateDiffInputs(load(path), load(path))
		if err == nil {
			t.Fatal("ValidateDiffInputs accepted a file diffed against itself")
		}
		if !strings.Contains(err.Error(), "itself") {
			t.Errorf("error %q does not say the file was compared to itself", err)
		}
	})

	// A relative/absolute or symlinked spelling of the same file is the same
	// operator error wearing a different name.
	t.Run("symlinked path", func(t *testing.T) {
		t.Parallel()
		link := filepath.Join(dir, "link.txt")
		if err := os.Symlink(path, link); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		if err := explaindiff.ValidateDiffInputs(load(path), load(link)); err == nil {
			t.Fatal("ValidateDiffInputs accepted a symlink to the same file")
		}
	})

	// The legitimate case the check must NOT break: two SEPARATE dumps of an
	// unchanged tree are byte-identical by construction, and comparing them
	// is the harness's own smoke test. The rejection keys on the SOURCE
	// file, never on the content.
	t.Run("distinct files with identical content", func(t *testing.T) {
		t.Parallel()
		twin := filepath.Join(dir, "twin.txt")
		if err := os.WriteFile(twin, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		oldB, newB := load(path), load(twin)
		if err := explaindiff.ValidateDiffInputs(oldB, newB); err != nil {
			t.Fatalf("two independent dumps of the same tree were rejected: %v", err)
		}
		if rep := explaindiff.Diff(oldB.Entries, newB.Entries); !rep.Clean() {
			t.Fatalf("byte-identical dumps did not diff clean: %d deltas", len(rep.Deltas))
		}
	})
}

// TestDiffDetectsCorpusInsertion is the mispairing regression. Inserting a
// stanza mid-file renumbers everything after it, so pairing on file#index
// alone compared query N against query N-1: the report printed one query's
// SQL beside another query's plans and tagged the mismatch SHAPE — a phantom
// planner regression that would send a reviewer chasing nothing.
func TestDiffDetectsCorpusInsertion(t *testing.T) {
	t.Parallel()

	scan := []string{"RecordQueryScanPlan"}
	fetch := []string{"RecordQueryFetchFromPartialRecordPlan", "  RecordQueryIndexPlan"}

	oldE := []explaindiff.Entry{
		{File: "a.yaml", Index: 0, SQL: "SELECT a FROM T", Plan: "Scan(T)", Shape: scan},
		{File: "a.yaml", Index: 1, SQL: "SELECT b FROM T", Plan: "Fetch(IndexScan(I))", Shape: fetch},
		{File: "a.yaml", Index: 2, SQL: "SELECT c FROM T", Plan: "Scan(T)", Shape: scan},
	}
	// A new stanza lands at index 1; the two originals shift down one slot.
	// Their PLANS are unchanged — nothing about the planner moved.
	newE := []explaindiff.Entry{
		{File: "a.yaml", Index: 0, SQL: "SELECT a FROM T", Plan: "Scan(T)", Shape: scan},
		{File: "a.yaml", Index: 1, SQL: "SELECT z FROM T", Plan: "Scan(T)", Shape: scan},
		{File: "a.yaml", Index: 2, SQL: "SELECT b FROM T", Plan: "Fetch(IndexScan(I))", Shape: fetch},
		{File: "a.yaml", Index: 3, SQL: "SELECT c FROM T", Plan: "Scan(T)", Shape: scan},
	}

	rep := explaindiff.Diff(oldE, newE)

	// The load-bearing assertion: no plan moved, so no shape flip may be
	// reported. Pairing on file#index alone reported two.
	if rep.ShapeFlips != 0 {
		t.Errorf("ShapeFlips = %d, want 0 — a corpus insertion is not a plan change", rep.ShapeFlips)
	}
	if rep.Regressions != 0 || rep.Recoveries != 0 {
		t.Errorf("Regressions=%d Recoveries=%d, want 0/0", rep.Regressions, rep.Recoveries)
	}
	// Positions 1 and 2 hold a different query on each side.
	if rep.Mispaired != 2 {
		t.Errorf("Mispaired = %d, want 2", rep.Mispaired)
	}

	var added, removed int
	for _, d := range rep.Deltas {
		switch d.Kind {
		case explaindiff.Added:
			added++
		case explaindiff.Removed:
			removed++
		default:
			t.Errorf("%s reported as CHANGED; a shifted entry is a different query: %+v", d.Key, d)
		}
	}
	// Old #1,#2 leave their slots; new #1,#2,#3 arrive in them.
	if removed != 2 || added != 3 {
		t.Errorf("removed=%d added=%d, want 2/3", removed, added)
	}

	// Every delta must carry its OWN query's SQL — the misattribution that
	// started this.
	for _, d := range rep.Deltas {
		plan := d.NewPlan
		if d.Kind == explaindiff.Removed {
			plan = d.OldPlan
		}
		want := map[string]string{
			"SELECT a FROM T": "Scan(T)",
			"SELECT b FROM T": "Fetch(IndexScan(I))",
			"SELECT c FROM T": "Scan(T)",
			"SELECT z FROM T": "Scan(T)",
		}[d.SQL]
		if plan != want {
			t.Errorf("%s: SQL %q printed beside plan %q (that query's plan is %q)", d.Key, d.SQL, plan, want)
		}
	}

	if !strings.Contains(explaindiff.RenderDiff(rep), "corpus positions hold a different query") {
		t.Error("the rendered diff does not warn that the corpus itself moved")
	}
}

// TestDiffPairsUnchangedSQL is the negative half of the mispairing fix: a
// genuine plan change on an unmoved query must still report as CHANGED, not
// dissolve into ADDED/REMOVED noise.
func TestDiffPairsUnchangedSQL(t *testing.T) {
	t.Parallel()

	oldE := []explaindiff.Entry{
		{File: "a.yaml", Index: 0, SQL: "SELECT a FROM T", Plan: "Scan(T)", Shape: []string{"RecordQueryScanPlan"}},
	}
	newE := []explaindiff.Entry{
		{
			File: "a.yaml", Index: 0, SQL: "SELECT a FROM T", Plan: "Fetch(IndexScan(I))",
			Shape: []string{"RecordQueryFetchFromPartialRecordPlan", "  RecordQueryIndexPlan"},
		},
	}
	rep := explaindiff.Diff(oldE, newE)
	if rep.Mispaired != 0 {
		t.Errorf("Mispaired = %d, want 0 — the query did not move", rep.Mispaired)
	}
	if rep.ShapeFlips != 1 || len(rep.Deltas) != 1 || rep.Deltas[0].Kind != explaindiff.Changed {
		t.Fatalf("a real plan flip on an unmoved query was not reported as CHANGED+SHAPE: %+v", rep)
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
