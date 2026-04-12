// Binary test-report generates a self-contained HTML test report from Bazel's
// Build Event Protocol (BEP) JSON output.
//
// Usage:
//
//	test-report <bep.jsonl>
//	test-report                     # reads .bazel-bep.jsonl
//
// The BEP file is produced by:
//
//	bazelisk test //... --build_event_json_file=bep.jsonl
//
// Each test target's test.xml (JUnit XML) is read via the file:// URIs in
// the BEP testResult events. Writes self-contained HTML to stdout.
//
// This tool is generic — it works with any Bazel project (Go, Java, C++, etc.)
// as long as the test runner produces JUnit XML output (which Bazel mandates).
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html/template"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---- BEP JSON types (subset we care about) ----

type bepEvent struct {
	ID          bepID          `json:"id"`
	TestResult  *bepTestResult `json:"testResult,omitempty"`
	TestSummary *bepSummary    `json:"testSummary,omitempty"`
}

type bepID struct {
	TestResult  *bepTestResultID  `json:"testResult,omitempty"`
	TestSummary *bepTestSummaryID `json:"testSummary,omitempty"`
}

type bepTestResultID struct {
	Label string `json:"label"`
}

type bepTestSummaryID struct {
	Label string `json:"label"`
}

type bepTestResult struct {
	TestActionOutput []bepOutputFile `json:"testActionOutput"`
	Status           string          `json:"status"` // PASSED, FAILED, TIMEOUT, FLAKY, etc.
	DurationMillis   string          `json:"testAttemptDurationMillis"`
}

type bepOutputFile struct {
	Name string `json:"name"` // "test.xml", "test.log"
	URI  string `json:"uri"`  // "file:///absolute/path"
}

type bepSummary struct {
	OverallStatus      string `json:"overallStatus"`
	TotalRunDurationMs string `json:"totalRunDurationMillis"`
}

// ---- JUnit XML types ----

type junitTestSuites struct {
	Suites []junitTestSuite `xml:"testsuite"`
}

type junitTestSuite struct {
	Name     string          `xml:"name,attr"`
	Tests    int             `xml:"tests,attr"`
	Failures int             `xml:"failures,attr"`
	Errors   int             `xml:"errors,attr"`
	Skipped  int             `xml:"skipped,attr"`
	Time     float64         `xml:"time,attr"`
	Cases    []junitTestCase `xml:"testcase"`
}

type junitTestCase struct {
	ClassName string        `xml:"classname,attr"`
	Name      string        `xml:"name,attr"`
	Time      float64       `xml:"time,attr"`
	Failure   *junitFailure `xml:"failure"`
	Error     *junitError   `xml:"error"`
	Skipped   *junitSkipped `xml:"skipped"`
}

type junitFailure struct {
	Message string `xml:"message,attr"`
	Body    string `xml:",chardata"`
}

type junitError struct {
	Message string `xml:"message,attr"`
	Body    string `xml:",chardata"`
}

type junitSkipped struct {
	Message string `xml:"message,attr"`
}

// ---- data model ----

type Status int

const (
	StatusPass Status = iota
	StatusFail
	StatusSkip
)

type TestResult struct {
	Name     string
	Status   Status
	Duration time.Duration
}

type TargetResult struct {
	Target    string // Bazel label, e.g. "//pkg/foo:foo_test"
	Tests     []TestResult
	SuiteTime time.Duration
}

func (t *TargetResult) Passed() int {
	n := 0
	for _, r := range t.Tests {
		if r.Status == StatusPass {
			n++
		}
	}
	return n
}

func (t *TargetResult) Failed() int {
	n := 0
	for _, r := range t.Tests {
		if r.Status == StatusFail {
			n++
		}
	}
	return n
}

func (t *TargetResult) Skipped() int {
	n := 0
	for _, r := range t.Tests {
		if r.Status == StatusSkip {
			n++
		}
	}
	return n
}

func (t *TargetResult) Total() int { return len(t.Tests) }

func (t *TargetResult) HasFailures() bool { return t.Failed() > 0 }

func (t *TargetResult) DisplayTime() string {
	d := t.SuiteTime
	if d == 0 {
		for _, r := range t.Tests {
			d += r.Duration
		}
	}
	return formatDuration(d)
}

func (t *TargetResult) SortedTests() []TestResult {
	sorted := make([]TestResult, len(t.Tests))
	copy(sorted, t.Tests)
	sort.SliceStable(sorted, func(i, j int) bool {
		si, sj := sorted[i].Status, sorted[j].Status
		if si == StatusFail && sj != StatusFail {
			return true
		}
		if si != StatusFail && sj == StatusFail {
			return false
		}
		return sorted[i].Duration > sorted[j].Duration
	})
	return sorted
}

func (r TestResult) DurationStr() string {
	if r.Duration == 0 {
		return "<1ms"
	}
	return formatDuration(r.Duration)
}

func (r TestResult) StatusIcon() template.HTML {
	switch r.Status {
	case StatusPass:
		return template.HTML(`<span class="icon pass">&#10003;</span>`)
	case StatusFail:
		return template.HTML(`<span class="icon fail">&#10007;</span>`)
	case StatusSkip:
		return template.HTML(`<span class="icon skip">&#8211;</span>`)
	default:
		return template.HTML(`<span class="icon">?</span>`)
	}
}

func (r TestResult) RowClass() string {
	switch r.Status {
	case StatusFail:
		return "row-fail"
	case StatusSkip:
		return "row-skip"
	default:
		return ""
	}
}

func (t *TargetResult) BadgeClass() string {
	if t.Failed() > 0 {
		return "badge-fail"
	}
	return "badge-pass"
}

func (t *TargetResult) BadgeText() string {
	if t.Failed() > 0 {
		return fmt.Sprintf("%d/%d failed  %s", t.Failed(), t.Total(), t.DisplayTime())
	}
	return fmt.Sprintf("%d/%d passed  %s", t.Passed(), t.Total(), t.DisplayTime())
}

// ---- BEP + JUnit parsing ----

// parseBEP reads the BEP JSONL file and returns one TargetResult per test target.
func parseBEP(path string) ([]*TargetResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Collect testResult events keyed by label.
	type targetInfo struct {
		label      string
		xmlPath    string
		logPath    string
		status     string
		durationMs int64
	}
	targets := make(map[string]*targetInfo)

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4<<20), 4<<20) // 4MB max line — BEP events can be large
	for scanner.Scan() {
		var ev bepEvent
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue // skip malformed lines
		}
		if ev.ID.TestResult != nil && ev.TestResult != nil {
			label := ev.ID.TestResult.Label
			info := &targetInfo{
				label:  label,
				status: ev.TestResult.Status,
			}
			if ms, _ := strconv.ParseInt(ev.TestResult.DurationMillis, 10, 64); ms > 0 {
				info.durationMs = ms
			}
			for _, out := range ev.TestResult.TestActionOutput {
				path := uriToPath(out.URI)
				switch out.Name {
				case "test.xml":
					info.xmlPath = path
				case "test.log":
					info.logPath = path
				}
			}
			targets[label] = info
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning BEP: %w", err)
	}

	// Build TargetResults from JUnit XML files.
	var results []*TargetResult
	for _, info := range targets {
		tr := &TargetResult{
			Target:    info.label,
			SuiteTime: time.Duration(info.durationMs) * time.Millisecond,
		}

		if info.xmlPath != "" {
			// Check for Ginkgo's per-spec JUnit report alongside the standard test.xml.
			// Ginkgo suites write to $TEST_UNDECLARED_OUTPUTS_DIR/ginkgo-report.xml which
			// Bazel collects into test.outputs/ next to test.xml. This has individual spec
			// names and durations — the standard test.xml only sees the bootstrap function.
			ginkgoPath := filepath.Join(filepath.Dir(info.xmlPath), "test.outputs", "ginkgo-report.xml")
			if ginkgoCases, err := parseJUnitXML(ginkgoPath); err == nil && len(ginkgoCases) > 0 {
				tr.Tests = ginkgoCases
			} else {
				cases, err := parseJUnitXML(info.xmlPath)
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: %s: %v\n", info.label, err)
				} else {
					tr.Tests = cases
				}
			}
		}

		// If no test cases from XML (e.g. XML missing or empty), synthesize from BEP status.
		if len(tr.Tests) == 0 {
			status := StatusPass
			if info.status == "FAILED" || info.status == "TIMEOUT" {
				status = StatusFail
			}
			tr.Tests = []TestResult{{
				Name:     "(target)",
				Status:   status,
				Duration: tr.SuiteTime,
			}}
		}

		results = append(results, tr)
	}

	return results, nil
}

// parseJUnitXML reads a JUnit XML file and returns individual test cases.
func parseJUnitXML(path string) ([]TestResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var suites junitTestSuites
	if err := xml.Unmarshal(data, &suites); err != nil {
		return nil, fmt.Errorf("parsing JUnit XML: %w", err)
	}

	var results []TestResult
	for _, suite := range suites.Suites {
		for _, tc := range suite.Cases {
			status := StatusPass
			if tc.Failure != nil || tc.Error != nil {
				status = StatusFail
			} else if tc.Skipped != nil {
				status = StatusSkip
			}
			results = append(results, TestResult{
				Name:     tc.Name,
				Status:   status,
				Duration: time.Duration(tc.Time * float64(time.Second)),
			})
		}
	}
	return results, nil
}

// uriToPath converts a file:// URI to a filesystem path.
func uriToPath(uri string) string {
	return strings.TrimPrefix(uri, "file://")
}

// ---- formatting ----

func formatDuration(d time.Duration) string {
	if d == 0 {
		return "\u2014" // em dash
	}
	if d < time.Millisecond {
		return "0.00s"
	}
	if d < time.Second {
		return fmt.Sprintf("%.0fms", float64(d)/float64(time.Millisecond))
	}
	if d < time.Minute {
		return fmt.Sprintf("%.3fs", d.Seconds())
	}
	m := int(d.Minutes())
	s := d.Seconds() - float64(m)*60
	return fmt.Sprintf("%dm%.1fs", m, s)
}

// ---- HTML template ----

type summaryData struct {
	Total   int
	Passed  int
	Failed  int
	Skipped int
	Time    string
}

type templateData struct {
	GeneratedAt string
	Summary     summaryData
	Targets     []*TargetResult
}

const htmlTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Test Report</title>
<style>
*, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }

body {
  font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
  font-size: 14px;
  background: #f5f6fa;
  color: #2c3e50;
  min-height: 100vh;
}

header {
  background: #1a1f2e;
  color: #ecf0f1;
  padding: 20px 32px;
  display: flex;
  align-items: baseline;
  gap: 16px;
}
header h1 { font-size: 22px; font-weight: 600; letter-spacing: 0.03em; }
header .timestamp { font-size: 12px; color: #8fa3b1; }

.summary-bar {
  background: #fff;
  border-bottom: 1px solid #dde1ec;
  padding: 14px 32px;
  display: flex;
  gap: 28px;
  align-items: center;
  flex-wrap: wrap;
}
.summary-bar .stat { display: flex; flex-direction: column; align-items: center; min-width: 70px; }
.summary-bar .stat .val { font-size: 26px; font-weight: 700; line-height: 1.1; }
.summary-bar .stat .lbl { font-size: 11px; text-transform: uppercase; letter-spacing: 0.07em; color: #8fa3b1; margin-top: 2px; }
.stat-total .val  { color: #2c3e50; }
.stat-pass  .val  { color: #27ae60; }
.stat-fail  .val  { color: #e74c3c; }
.stat-skip  .val  { color: #f39c12; }
.stat-time  .val  { color: #2980b9; font-size: 20px; }
.summary-divider { width: 1px; height: 40px; background: #dde1ec; }

.targets { padding: 24px 32px; display: flex; flex-direction: column; gap: 12px; }

details {
  background: #fff;
  border: 1px solid #dde1ec;
  border-radius: 6px;
  overflow: hidden;
  box-shadow: 0 1px 3px rgba(0,0,0,0.04);
}
details.has-fail { border-left: 4px solid #e74c3c; }
details.has-pass { border-left: 4px solid #27ae60; }

summary {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 12px 16px;
  cursor: pointer;
  user-select: none;
  list-style: none;
  gap: 12px;
}
summary::-webkit-details-marker { display: none; }
summary::marker { display: none; }
summary:hover { background: #f8f9ff; }

.target-label {
  font-family: "SFMono-Regular", Consolas, "Liberation Mono", Menlo, monospace;
  font-size: 13px;
  font-weight: 500;
  color: #2c3e50;
  flex: 1;
  word-break: break-all;
}

.chevron {
  font-size: 11px;
  color: #8fa3b1;
  transition: transform 0.15s ease;
  flex-shrink: 0;
}
details[open] .chevron { transform: rotate(90deg); }

.badge {
  display: inline-flex;
  align-items: center;
  padding: 3px 10px;
  border-radius: 12px;
  font-size: 12px;
  font-weight: 600;
  flex-shrink: 0;
}
.badge-pass { background: #eafaf1; color: #1e8449; }
.badge-fail { background: #fdedec; color: #c0392b; }

table {
  width: 100%;
  border-collapse: collapse;
}
th {
  background: #f8f9ff;
  padding: 8px 16px;
  text-align: left;
  font-size: 11px;
  text-transform: uppercase;
  letter-spacing: 0.06em;
  color: #8fa3b1;
  border-bottom: 1px solid #dde1ec;
  font-weight: 600;
}
td {
  padding: 7px 16px;
  border-bottom: 1px solid #f0f1f7;
  vertical-align: middle;
}
tr:last-child td { border-bottom: none; }
tr.row-fail td { background: #fff8f8; }
tr.row-skip td { background: #fffdf5; color: #7f8c8d; }
tr:hover td { background: #f8f9ff; }

.col-name { font-family: "SFMono-Regular", Consolas, "Liberation Mono", Menlo, monospace; font-size: 12px; word-break: break-all; }
.col-status { text-align: center; width: 60px; }
.col-duration { text-align: right; width: 90px; font-family: "SFMono-Regular", Consolas, "Liberation Mono", Menlo, monospace; font-size: 12px; color: #7f8c8d; }

.icon { font-size: 15px; font-weight: 700; }
.icon.pass { color: #27ae60; }
.icon.fail { color: #e74c3c; }
.icon.skip { color: #f39c12; }

footer {
  padding: 20px 32px;
  text-align: center;
  color: #8fa3b1;
  font-size: 11px;
  border-top: 1px solid #dde1ec;
  background: #fff;
  margin-top: 12px;
}
</style>
</head>
<body>

<header>
  <h1>Test Report</h1>
  <span class="timestamp">{{.GeneratedAt}}</span>
</header>

<div class="summary-bar">
  <div class="stat stat-total"><span class="val">{{.Summary.Total}}</span><span class="lbl">Total</span></div>
  <div class="summary-divider"></div>
  <div class="stat stat-pass"><span class="val">{{.Summary.Passed}}</span><span class="lbl">Passed</span></div>
  <div class="stat stat-fail"><span class="val">{{.Summary.Failed}}</span><span class="lbl">Failed</span></div>
  <div class="stat stat-skip"><span class="val">{{.Summary.Skipped}}</span><span class="lbl">Skipped</span></div>
  <div class="summary-divider"></div>
  <div class="stat stat-time"><span class="val">{{.Summary.Time}}</span><span class="lbl">Total Time</span></div>
</div>

<div class="targets">
{{- range .Targets}}
<details {{if .HasFailures}}class="has-fail" open{{else}}class="has-pass"{{end}}>
  <summary>
    <span class="target-label">{{.Target}}</span>
    <span class="badge {{.BadgeClass}}">{{.BadgeText}}</span>
    <span class="chevron">&#9654;</span>
  </summary>
  {{if .Tests}}
  <table>
    <thead>
      <tr><th>Test</th><th style="text-align:center">Status</th><th style="text-align:right">Duration</th></tr>
    </thead>
    <tbody>
    {{- range .SortedTests}}
      <tr class="{{.RowClass}}">
        <td class="col-name">{{.Name}}</td>
        <td class="col-status">{{.StatusIcon}}</td>
        <td class="col-duration">{{.DurationStr}}</td>
      </tr>
    {{- end}}
    </tbody>
  </table>
  {{end}}
</details>
{{- end}}
</div>

<footer>Generated by bazel-test-report &middot; {{.GeneratedAt}}</footer>

</body>
</html>
`

// ---- main ----

func run() error {
	bepPath := ".bazel-bep.jsonl"
	if len(os.Args) > 1 {
		bepPath = os.Args[1]
	}

	if _, err := os.Stat(bepPath); os.IsNotExist(err) {
		return fmt.Errorf("BEP file %q not found — run 'bazelisk test //... --build_event_json_file=%s' first", bepPath, bepPath)
	}

	targets, err := parseBEP(bepPath)
	if err != nil {
		return fmt.Errorf("parsing BEP: %w", err)
	}
	if len(targets) == 0 {
		return fmt.Errorf("no test results found in %s", bepPath)
	}

	// Sort: failures first, then alphabetically.
	sort.SliceStable(targets, func(i, j int) bool {
		fi, fj := targets[i].HasFailures(), targets[j].HasFailures()
		if fi != fj {
			return fi
		}
		return targets[i].Target < targets[j].Target
	})

	// Build summary.
	var sumPassed, sumFailed, sumSkipped int
	var totalTime time.Duration
	for _, t := range targets {
		sumPassed += t.Passed()
		sumFailed += t.Failed()
		sumSkipped += t.Skipped()
		if t.SuiteTime > 0 {
			totalTime += t.SuiteTime
		} else {
			for _, r := range t.Tests {
				totalTime += r.Duration
			}
		}
	}

	displayTime := totalTime
	if totalTime > time.Minute {
		displayTime = time.Duration(math.Round(totalTime.Seconds())) * time.Second
	}

	data := templateData{
		GeneratedAt: time.Now().Format("2006-01-02 15:04:05"),
		Summary: summaryData{
			Total:   sumPassed + sumFailed + sumSkipped,
			Passed:  sumPassed,
			Failed:  sumFailed,
			Skipped: sumSkipped,
			Time:    formatDuration(displayTime),
		},
		Targets: targets,
	}

	tmpl, err := template.New("report").Parse(htmlTemplate)
	if err != nil {
		return fmt.Errorf("parsing template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return fmt.Errorf("executing template: %w", err)
	}

	_, err = os.Stdout.Write(buf.Bytes())
	return err
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "test-report: %v\n", err)
		os.Exit(1)
	}
}
