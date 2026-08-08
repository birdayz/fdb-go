package factory

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/conformance/factorycorpus"
)

// realisticManifest builds a manifest at the scale that actually broke the
// nightly: a whole-corpus census with one ByFeature entry and one ByKeyBlessing
// entry per scenario, feature vectors as long as the real ones.
func realisticManifest(scenarios int) Manifest {
	census := factorycorpus.Census{
		Scenarios:     scenarios,
		Tests:         scenarios * 3,
		ByFeature:     map[string]int{},
		ByBlessing:    map[string]int{"cross-engine": scenarios},
		ByKeyBlessing: map[string]string{},
	}
	for i := range scenarios {
		fv := fmt.Sprintf("shape=join2.comma;idx=A+B,B,C,D;proj=n16;"+
			"where=and(between.ge,cmp.eq,colcol.gt);order=desc+asc;variant=%d", i)
		census.ByFeature[fv] = 1
		census.ByKeyBlessing[fmt.Sprintf("dedupkey-%s-%06d", fv[:40], i)] = "cross-engine"
	}
	return Manifest{
		Date: "2026-08-07", Generator: "p1", SeedStart: 3900000, Seeds: 1000,
		Quota: 1000, BlessingMode: "cross-engine",
		CandidatesGen: 41234, CandidatesRun: 40011, Blessed: 3120, Committed: 812,
		DedupRejected: 2308, DedupPoints: 4931, NameCollisions: 0, Findings: 2,
		SkipsByReason: map[string]int{"second-plan infra": 28, "unsupported shape": 194, "dedup": 2308},
		SkipSamples: map[string][]string{
			"second-plan infra": {"seed 391: transport reset | mid-plan", "seed 402: deadline\nexceeded"},
		},
		Blessings:       map[string]int{"cross-engine": 780, "metamorphic": 32},
		BlessedByFamily: map[string]int{"unordered-agg": 32},
		Exemptions:      []string{"unordered-agg — pinned by X, retired by the ordering oracle"},
		Census:          census,
	}
}

// TestSummaryMarkdownFitsWhereTheManifestDoesNot is the regression for the
// nightly-factory red: "GraphQL: Body is too long, Body is too long (maximum is
// 65536 characters) (createPullRequest)".
//
// The two halves are both load-bearing and neither alone is the test. The
// manifest assertion is what keeps this honest: if the raw JSON ever stops
// exceeding the limit, this test would be passing for a reason unrelated to the
// bug, and it says so instead of quietly agreeing.
func TestSummaryMarkdownFitsWhereTheManifestDoesNot(t *testing.T) {
	t.Parallel()

	m := realisticManifest(5000)

	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if len(raw) <= PRBodyLimit {
		t.Fatalf("the raw manifest is %d chars, at or under the %d limit — the premise of this "+
			"test has evaporated. Either the census maps stopped being embedded, or the fixture "+
			"stopped resembling the corpus. Re-derive the fixture from a real manifest before "+
			"trusting the summary assertion below; as written it would now pass for the wrong reason.",
			len(raw), PRBodyLimit)
	}
	t.Logf("raw manifest JSON: %d chars (%.1fx the %d limit)",
		len(raw), float64(len(raw))/float64(PRBodyLimit), PRBodyLimit)

	body := m.SummaryMarkdown("https://github.com/o/r/actions/runs/123")
	if n := len([]rune(body)); n > PRBodyLimit {
		t.Fatalf("summary is %d chars, over GitHub's %d limit", n, PRBodyLimit)
	}
	t.Logf("summary: %d chars", len([]rune(body)))
}

// TestSummaryMarkdownDropsNoCount is the ANTI-SILENCE half, and it is the more
// important of the two: a summary that fits by dropping numbers has replaced a
// loud failure with a quiet one. Every scalar the ledger reports must appear.
func TestSummaryMarkdownDropsNoCount(t *testing.T) {
	t.Parallel()

	m := realisticManifest(5000)
	body := m.SummaryMarkdown("https://github.com/o/r/actions/runs/123")

	for _, want := range []struct{ what, substr string }{
		{"date", "2026-08-07"},
		{"generator", "p1"},
		{"seed range", "3900000..3900999"},
		{"quota", "| quota | 1000 |"},
		{"blessing mode", "cross-engine"},
		{"candidates generated", "| candidates generated | 41234 |"},
		{"candidates executed", "| candidates executed | 40011 |"},
		{"blessed", "| blessed | 3120 |"},
		{"committed", "| **committed** | **812** |"},
		{"dedup rejected", "| dedup rejected | 2308 |"},
		{"dedup points", "| dedup points after | 4931 |"},
		{"name collisions", "| name collisions | 0 |"},
		{"findings", "| findings | 2 |"},
		{"skip class", "| second-plan infra | 28 |"},
		{"skip sample", "seed 391: transport reset"},
		{"blessing mix", "| metamorphic | 32 |"},
		{"exemption family count", "| unordered-agg | 32 |"},
		{"exemption text", "retired by the ordering oracle"},
		{"census scenarios", "| scenarios | 5000 |"},
		{"census tests", "| tests | 15000 |"},
		{"distinct feature vectors", "| distinct feature vectors | 5000 |"},
		{"keyed blessings", "| keyed blessings | 5000 |"},
		{"pointer to the full manifest", "factory-findings"},
		{"actionable run link", "https://github.com/o/r/actions/runs/123"},
	} {
		if !strings.Contains(body, want.substr) {
			t.Errorf("summary omits the %s: no %q.\n"+
				"A summary that fits by dropping a number is worse than the failed PR it replaces — "+
				"a failed PR is loud, a missing count is not. Render it, or shrink something else.",
				want.what, want.substr)
		}
	}

	// The two per-scenario maps are what must NOT be inlined, and the reader is
	// told which ones by name rather than left to notice the absence.
	for _, banned := range []string{"variant=4999", "dedupkey-shape=join2"} {
		if strings.Contains(body, banned) {
			t.Errorf("summary inlined per-scenario census data (%q) — that is the ~640KB "+
				"whole-corpus state this change exists to keep out of the body", banned)
		}
	}
	if !strings.Contains(body, "by_feature") || !strings.Contains(body, "by_key_blessing") {
		t.Errorf("summary must NAME the two omitted maps; silently omitting them is the failure mode")
	}
}

// TestSummaryMarkdownIsDeterministic pins the map ordering. Go randomizes map
// iteration, so an unsorted ledger reshuffles between runs and every re-run of
// the nightly produces a PR body that diffs against itself.
func TestSummaryMarkdownIsDeterministic(t *testing.T) {
	t.Parallel()

	m := realisticManifest(200)
	first := m.SummaryMarkdown("")
	for i := range 20 {
		if got := m.SummaryMarkdown(""); got != first {
			t.Fatalf("summary differs on render %d — a map is being iterated unsorted", i)
		}
	}
	if strings.Contains(first, "](") {
		t.Errorf("with no run URL the summary must not emit a dangling link; got a markdown link")
	}
}

// TestTruncateMarksTheCut pins the last-resort guard in both directions: under
// the limit it must not touch the body, and over it must cut AND say so.
func TestTruncateMarksTheCut(t *testing.T) {
	t.Parallel()

	short := "already small"
	if got := Truncate(short, 100); got != short {
		t.Errorf("Truncate altered a body under the limit: %q", got)
	}

	// Multi-byte on purpose: GitHub counts characters, and a byte-offset cut
	// would both overcount the budget and be able to split a rune.
	long := strings.Repeat("é", 5000)
	got := Truncate(long, 1000)
	if n := len([]rune(got)); n > 1000 {
		t.Errorf("Truncate returned %d runes, over the 1000 limit", n)
	}
	if !strings.Contains(got, "truncated") {
		t.Errorf("Truncate cut silently; the marker is mandatory:\n%q", got)
	}
	if !strings.Contains(got, "factory-findings") {
		t.Errorf("the truncation marker must point at where the full content lives")
	}
	if !strings.HasPrefix(got, "é") || strings.Contains(got, "�") {
		t.Errorf("Truncate produced invalid UTF-8 — it cut by byte rather than by rune")
	}
}

// TestOneLineKeepsTheTableIntact pins the cell escaping. A skip sample is a
// verbatim engine message: it can and does contain newlines and pipes, and an
// unescaped one silently restructures the markdown table rather than merely
// looking wrong.
func TestOneLineKeepsTheTableIntact(t *testing.T) {
	t.Parallel()

	got := oneLine("seed 402: deadline\nexceeded | at stage 3\r\n")
	if strings.ContainsAny(got, "\n\r") {
		t.Errorf("oneLine left a line break in a table cell: %q", got)
	}
	// Every pipe must be backslash-escaped. Asserting on the escaped substring
	// alone would not work: "\\|" trivially contains "|", so a "no raw pipe"
	// check can never fail. Count instead — pipes and escaped pipes must match.
	if pipes, escaped := strings.Count(got, "|"), strings.Count(got, "\\|"); pipes != escaped {
		t.Errorf("oneLine left %d unescaped pipe(s), which open new table columns: %q",
			pipes-escaped, got)
	}
	if !strings.Contains(got, "deadline exceeded") {
		t.Errorf("oneLine dropped content instead of flattening it: %q", got)
	}
}
