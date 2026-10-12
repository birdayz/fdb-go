package main

import (
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
