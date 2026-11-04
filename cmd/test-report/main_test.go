package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type testBEPResult struct {
	label                 string
	run, shard, attempt   int
	status, xmlURI, cases string
}

func writeBEP(t *testing.T, results ...testBEPResult) string {
	t.Helper()
	var lines []string
	for _, r := range results {
		ev := map[string]any{
			"id": map[string]any{"testResult": map[string]any{
				"label": r.label, "run": r.run, "shard": r.shard, "attempt": r.attempt,
			}},
			"testResult": map[string]any{
				"status":                    r.status,
				"testAttemptDurationMillis": "10",
				"testActionOutput":          []map[string]string{{"name": "test.xml", "uri": r.xmlURI}},
			},
		}
		b, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(b))
	}
	path := filepath.Join(t.TempDir(), "bep.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeJUnit(t *testing.T, cases ...string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(`<testsuites><testsuite>`)
	for _, c := range cases {
		b.WriteString(`<testcase name="` + c + `"/>`)
	}
	b.WriteString(`</testsuite></testsuites>`)
	path := filepath.Join(t.TempDir(), "test.xml")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return "file://" + path
}

func TestParseBEPRejectsRemoteOnlyTestXML(t *testing.T) {
	t.Parallel()
	bep := writeBEP(t, testBEPResult{
		label: "//pkg:a_test", run: 1, shard: 1, attempt: 1, status: "PASSED",
		xmlURI: "bytestream://cache/blobs/abc/12",
	})
	if _, err := parseBEP(bep); err == nil || !strings.Contains(err.Error(), "not local") {
		t.Fatalf("parseBEP error = %v, want remote-only test.xml rejection", err)
	}
}

func TestParseBEPCountsEveryShardAndLastAttempt(t *testing.T) {
	t.Parallel()
	bep := writeBEP(t,
		testBEPResult{label: "//pkg:a_test", run: 1, shard: 1, attempt: 1, status: "PASSED", xmlURI: writeJUnit(t, "A1", "A2")},
		testBEPResult{label: "//pkg:a_test", run: 1, shard: 2, attempt: 1, status: "FAILED", xmlURI: writeJUnit(t, "stale")},
		testBEPResult{label: "//pkg:a_test", run: 1, shard: 2, attempt: 2, status: "PASSED", xmlURI: writeJUnit(t, "B1", "B2", "B3")},
	)
	targets, err := parseBEP(bep)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 {
		t.Fatalf("targets = %d, want 1", len(targets))
	}
	var names []string
	for _, tc := range targets[0].Tests {
		names = append(names, tc.Name)
	}
	if got, want := strings.Join(names, ","), "A1,A2,B1,B2,B3"; got != want {
		t.Fatalf("test cases = %s, want %s", got, want)
	}
}

func writeGinkgoShard(t *testing.T, spec string) string {
	t.Helper()
	uri := writeJUnit(t, "bootstrap")
	dir := filepath.Join(filepath.Dir(strings.TrimPrefix(uri, "file://")), "test.outputs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	report := `[{"containers":["Suite"],"name":"` + spec + `","state":"passed","duration_ms":1}]`
	if err := os.WriteFile(filepath.Join(dir, "ginkgo-report.json"), []byte(report), 0o600); err != nil {
		t.Fatal(err)
	}
	return uri
}

func TestParseBEPMultiShardGinkgoRendersEveryCase(t *testing.T) {
	t.Parallel()
	bep := writeBEP(t,
		testBEPResult{label: "//pkg:g_test", run: 1, shard: 1, attempt: 1, status: "PASSED", xmlURI: writeGinkgoShard(t, "S1")},
		testBEPResult{label: "//pkg:g_test", run: 1, shard: 2, attempt: 1, status: "PASSED", xmlURI: writeGinkgoShard(t, "S2")},
	)
	targets, err := parseBEP(bep)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || len(targets[0].Tests) != 2 {
		t.Fatalf("got %d targets, want one with both shard specs", len(targets))
	}
	if targets[0].Tree != nil {
		t.Fatal("a single shard's Ginkgo tree would hide the other shard's specs")
	}
}

func TestParseBEPValidatesOnlyTheFinalAttempt(t *testing.T) {
	t.Parallel()
	remote := testBEPResult{label: "//pkg:r_test", run: 1, shard: 1, attempt: 1, status: "FAILED", xmlURI: "bytestream://cache/blobs/abc/12"}
	local := testBEPResult{label: "//pkg:r_test", run: 1, shard: 1, attempt: 2, status: "PASSED", xmlURI: writeJUnit(t, "R1")}
	for name, order := range map[string][]testBEPResult{"earlier first": {remote, local}, "later first": {local, remote}} {
		targets, err := parseBEP(writeBEP(t, order...))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(targets) != 1 || len(targets[0].Tests) != 1 || targets[0].Tests[0].Name != "R1" {
			t.Fatalf("%s: got %+v, want the final attempt's case R1", name, targets)
		}
	}
}

func TestReportProvenanceValidation(t *testing.T) {
	t.Parallel()
	valid := reportProvenance{
		SHA: strings.Repeat("a", 40), RunURL: "https://github.com/birdayz/fdb-go/actions/runs/123",
		Lane: "suite", Exclusions: "stress and separate jobs",
	}
	for _, tc := range []struct {
		name string
		edit func(*reportProvenance)
	}{
		{"missing SHA", func(p *reportProvenance) { p.SHA = "" }},
		{"short SHA", func(p *reportProvenance) { p.SHA = "abc123" }},
		{"nonhex SHA", func(p *reportProvenance) { p.SHA = strings.Repeat("z", 40) }},
		{"missing URL", func(p *reportProvenance) { p.RunURL = "" }},
		{"unsafe URL", func(p *reportProvenance) { p.RunURL = "javascript:alert(1)" }},
		{"HTTP URL", func(p *reportProvenance) { p.RunURL = "http://github.com/run/1" }},
		{"relative URL", func(p *reportProvenance) { p.RunURL = "/run/1" }},
		{"missing host", func(p *reportProvenance) { p.RunURL = "https:///run/1" }},
		{"port without host", func(p *reportProvenance) { p.RunURL = "https://:443/run/1" }},
		{"userinfo", func(p *reportProvenance) { p.RunURL = "https://user@github.com/run/1" }},
		{"malformed URL", func(p *reportProvenance) { p.RunURL = "https://[" }},
		{"missing lane", func(p *reportProvenance) { p.Lane = "" }},
		{"blank lane", func(p *reportProvenance) { p.Lane = " \t" }},
		{"missing exclusions", func(p *reportProvenance) { p.Exclusions = "" }},
		{"blank exclusions", func(p *reportProvenance) { p.Exclusions = " \t" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := valid
			tc.edit(&p)
			if err := p.validate(); err == nil {
				t.Fatalf("accepted invalid provenance: %+v", p)
			}
		})
	}
	for _, p := range []reportProvenance{{}, valid, {SHA: strings.Repeat("b", 64), RunURL: valid.RunURL, Lane: valid.Lane, Exclusions: valid.Exclusions}} {
		if err := p.validate(); err != nil {
			t.Errorf("valid provenance rejected: %+v: %v", p, err)
		}
	}
}

func TestReportRendersProvenanceAndCases(t *testing.T) {
	t.Parallel()
	bep := writeBEP(t, testBEPResult{label: "//pkg:proof_test", run: 1, shard: 1, attempt: 1, status: "PASSED", xmlURI: writeJUnit(t, "ProofCase")})
	sha := strings.Repeat("a", 40)
	var output bytes.Buffer
	if err := runReport([]string{
		"--sha", sha, "--run-url", "https://github.com/birdayz/fdb-go/actions/runs/123",
		"--lane", "suite <script>alert(1)</script>", "--exclusions", "stress & manual", bep,
	}, &output); err != nil {
		t.Fatal(err)
	}
	html := output.String()
	for _, want := range []string{sha, `href="https://github.com/birdayz/fdb-go/actions/runs/123"`, "suite &lt;script&gt;alert(1)&lt;/script&gt;", "stress &amp; manual", "Cached results are not fresh test executions.", "ProofCase", `stat-total"><span class="val">1</span>`} {
		if !strings.Contains(html, want) {
			t.Errorf("rendered report lacks %q", want)
		}
	}
	if strings.Contains(html, "suite <script>") || strings.Contains(html, "Unattributed report") {
		t.Fatal("report rendered unsafe or missing provenance")
	}
	output.Reset()
	if err := runReport([]string{bep}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Unattributed report") || !strings.Contains(output.String(), "ProofCase") {
		t.Fatal("local report must identify missing provenance and retain its cases")
	}
	for _, args := range [][]string{{"--unknown"}, {"--sha", sha, bep}} {
		output.Reset()
		if err := runReport(args, &output); err == nil || output.Len() != 0 {
			t.Fatalf("invalid flags %q produced error=%v and %d output bytes", args, err, output.Len())
		}
	}
}
