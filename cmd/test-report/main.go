// Binary test-report generates a self-contained HTML test report from Bazel test logs.
//
// Usage:
//
//	test-report [dir]
//
// If dir is omitted, defaults to bazel-testlogs/ in the current directory.
// Writes HTML to stdout.
package main

import (
	"bufio"
	"bytes"
	"fmt"
	"html/template"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---- data model ----

type Status int

const (
	StatusPass Status = iota
	StatusFail
	StatusSkip
)

func (s Status) String() string {
	switch s {
	case StatusPass:
		return "PASS"
	case StatusFail:
		return "FAIL"
	case StatusSkip:
		return "SKIP"
	default:
		return "UNKNOWN"
	}
}

type TestResult struct {
	Name     string
	Status   Status
	Duration time.Duration
}

type TargetResult struct {
	// Bazel target label, e.g. "pkg/recordlayer:recordlayer_test"
	Target string
	Tests  []TestResult
	// Total wall time reported by the test suite (Ginkgo "Ran N Specs in X.Xs")
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
		// sum individual test durations
		for _, r := range t.Tests {
			d += r.Duration
		}
	}
	return formatDuration(d)
}

// ---- regexps ----

var (
	// --- PASS: TestName (1.23s)
	// --- FAIL: TestName (0.45s)
	// --- SKIP: TestName (0.00s)
	reGoTest = regexp.MustCompile(`^--- (PASS|FAIL|SKIP): (\S+) \((\d+\.\d+)s\)$`)

	// Ran 2315 of 2317 Specs in 22.220 seconds
	reGinkgoRan = regexp.MustCompile(`^Ran (\d+) of (\d+) Specs in ([\d.]+) seconds`)

	// SUCCESS! -- 2315 Passed | 0 Failed | 0 Pending | 2 Skipped
	reGinkgoSuccess = regexp.MustCompile(`^SUCCESS!\s+--\s+(\d+) Passed\s*\|\s*(\d+) Failed\s*\|\s*(\d+) Pending\s*\|\s*(\d+) Skipped`)

	// FAIL! -- 10 Passed | 3 Failed | 0 Pending | 0 Skipped
	reGinkgoFail = regexp.MustCompile(`^FAIL!\s+--\s+(\d+) Passed\s*\|\s*(\d+) Failed\s*\|\s*(\d+) Pending\s*\|\s*(\d+) Skipped`)

	// Will run 2317 of 2317 specs  (Ginkgo preamble, not needed but useful to detect Ginkgo mode)
	reGinkgoWillRun = regexp.MustCompile(`^Will run \d+ of \d+ spec`)

	// ANSI escape code stripper
	reANSI = regexp.MustCompile(`\x1b\[[0-9;]*[mKHFJ]`)

	// Bazel "Executing tests from //target:name" preamble line
	reExecuting = regexp.MustCompile(`^Executing tests from //(.+)$`)
)

// stripANSI removes ANSI terminal escape sequences from s.
func stripANSI(s string) string {
	return reANSI.ReplaceAllString(s, "")
}

// parseDuration parses a string like "1.23" as seconds and returns a time.Duration.
func parseDuration(s string) time.Duration {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return time.Duration(f * float64(time.Second))
}

// formatDuration formats a duration as a human-readable string.
func formatDuration(d time.Duration) string {
	if d == 0 {
		return "—"
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

// parseTestLog parses a single test.log file and returns a TargetResult.
// The target label is derived from the log content or from the path.
func parseTestLog(path string, rootDir string) (*TargetResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	result := &TargetResult{}

	// Derive target from path relative to rootDir:
	// e.g. rootDir/pkg/recordlayer/recordlayer_test/test.log → pkg/recordlayer:recordlayer_test
	rel, err := filepath.Rel(rootDir, path)
	if err == nil {
		// rel = "pkg/recordlayer/recordlayer_test/test.log"
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) >= 2 {
			testPkg := parts[len(parts)-2]                     // "recordlayer_test"
			pkgPath := strings.Join(parts[:len(parts)-2], "/") // "pkg/recordlayer"
			result.Target = pkgPath + ":" + testPkg
		} else {
			result.Target = rel
		}
	} else {
		result.Target = path
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 1<<20) // 1MB max line — FDB test logs can have large stack traces
	lineNum := 0
	isGinkgo := false
	hasSuiteSummary := false

	var ginkgoPassed, ginkgoFailed, ginkgoSkipped int
	var suiteSeconds float64

	for scanner.Scan() {
		lineNum++
		raw := scanner.Text()
		line := stripANSI(raw)
		line = strings.TrimSpace(line)

		// Skip Bazel preamble: first 3 lines
		// Line 1: "exec ${PAGER:-/usr/bin/less} "$0" || exit 1"
		// Line 2: "Executing tests from //..."
		// Line 3: "---..."
		if lineNum == 1 {
			continue
		}
		if lineNum == 2 {
			// Try to extract the real target label
			if m := reExecuting.FindStringSubmatch(line); m != nil {
				result.Target = m[1]
			}
			continue
		}
		if lineNum == 3 && strings.HasPrefix(line, "---") {
			continue
		}

		// Detect Ginkgo mode
		if reGinkgoWillRun.MatchString(line) {
			isGinkgo = true
		}

		// Parse standard Go test lines (work in both modes)
		if m := reGoTest.FindStringSubmatch(line); m != nil {
			status := StatusPass
			switch m[1] {
			case "FAIL":
				status = StatusFail
			case "SKIP":
				status = StatusSkip
			}
			result.Tests = append(result.Tests, TestResult{
				Name:     m[2],
				Status:   status,
				Duration: parseDuration(m[3]),
			})
			continue
		}

		// Ginkgo "Ran N of M Specs in X.X seconds"
		if m := reGinkgoRan.FindStringSubmatch(line); m != nil {
			isGinkgo = true
			suiteSeconds, _ = strconv.ParseFloat(m[3], 64)
			result.SuiteTime = time.Duration(suiteSeconds * float64(time.Second))
			continue
		}

		// Ginkgo SUCCESS! summary
		if m := reGinkgoSuccess.FindStringSubmatch(line); m != nil {
			isGinkgo = true
			hasSuiteSummary = true
			ginkgoPassed, _ = strconv.Atoi(m[1])
			ginkgoFailed, _ = strconv.Atoi(m[2])
			// m[3] = pending (treat as skipped for display)
			ginkgoSkipped, _ = strconv.Atoi(m[4])
			pending, _ := strconv.Atoi(m[3])
			ginkgoSkipped += pending
			continue
		}

		// Ginkgo FAIL! summary
		if m := reGinkgoFail.FindStringSubmatch(line); m != nil {
			isGinkgo = true
			hasSuiteSummary = true
			ginkgoPassed, _ = strconv.Atoi(m[1])
			ginkgoFailed, _ = strconv.Atoi(m[2])
			ginkgoSkipped, _ = strconv.Atoi(m[4])
			pending, _ := strconv.Atoi(m[3])
			ginkgoSkipped += pending
			continue
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning %s: %w", path, err)
	}

	// If this is a Ginkgo suite and we got the summary, synthesize test entries
	// only if we didn't get individual test lines (most Ginkgo runs don't emit them).
	if isGinkgo && hasSuiteSummary && len(result.Tests) == 0 {
		// Synthesize one entry per category
		if ginkgoPassed > 0 {
			label := fmt.Sprintf("Suite (%d passed)", ginkgoPassed)
			result.Tests = append(result.Tests, TestResult{
				Name:     label,
				Status:   StatusPass,
				Duration: result.SuiteTime,
			})
		}
		if ginkgoFailed > 0 {
			label := fmt.Sprintf("Suite (%d failed)", ginkgoFailed)
			result.Tests = append(result.Tests, TestResult{
				Name:     label,
				Status:   StatusFail,
				Duration: 0,
			})
		}
		if ginkgoSkipped > 0 {
			label := fmt.Sprintf("Suite (%d skipped)", ginkgoSkipped)
			result.Tests = append(result.Tests, TestResult{
				Name:     label,
				Status:   StatusSkip,
				Duration: 0,
			})
		}
		return result, nil
	}

	// If Ginkgo + summary + we DO have individual test lines (e.g. from --ginkgo.v),
	// just use the individual lines — they're more useful.

	// If no parseable test lines at all, but the log ended with PASS:
	// synthesize a single "package" pass entry.
	if len(result.Tests) == 0 {
		// Re-read just to check for PASS/FAIL at end
		f.Seek(0, 0) //nolint:errcheck
		last := lastMeaningfulLine(f)
		if last == "PASS" {
			result.Tests = append(result.Tests, TestResult{
				Name:   "(package)",
				Status: StatusPass,
			})
		} else if last == "FAIL" {
			result.Tests = append(result.Tests, TestResult{
				Name:   "(package)",
				Status: StatusFail,
			})
		}
		// if truly empty log, leave Tests empty — will be omitted from report
	}

	return result, nil
}

// lastMeaningfulLine returns the last non-empty, non-whitespace line from f.
func lastMeaningfulLine(f *os.File) string {
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 1<<20) // 1MB max line — FDB test logs can have large stack traces
	last := ""
	for scanner.Scan() {
		line := strings.TrimSpace(stripANSI(scanner.Text()))
		if line != "" {
			last = line
		}
	}
	return last
}

// walk finds all test.log files under rootDir, returning them in sorted order.
// rootDir must already be a real (non-symlink) path — call filepath.EvalSymlinks
// before passing it here if needed (filepath.WalkDir does not follow symlinks
// on the root itself).
func walk(rootDir string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(rootDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == "test.log" {
			paths = append(paths, path)
		}
		return nil
	})
	return paths, err
}

// ---- HTML generation ----

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

func (t *TargetResult) SortedTests() []TestResult {
	sorted := make([]TestResult, len(t.Tests))
	copy(sorted, t.Tests)
	sort.SliceStable(sorted, func(i, j int) bool {
		si, sj := sorted[i].Status, sorted[j].Status
		// Failures first
		if si == StatusFail && sj != StatusFail {
			return true
		}
		if si != StatusFail && sj == StatusFail {
			return false
		}
		// Then by duration descending
		return sorted[i].Duration > sorted[j].Duration
	})
	return sorted
}

func (r TestResult) DurationStr() string {
	if r.Duration == 0 {
		return "—"
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
    <span class="target-label">//{{.Target}}</span>
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

<footer>Generated by test-report &middot; {{.GeneratedAt}}</footer>

</body>
</html>
`

func run() error {
	rootDir := "bazel-testlogs"
	if len(os.Args) > 1 {
		rootDir = os.Args[1]
	}

	if _, err := os.Stat(rootDir); os.IsNotExist(err) {
		return fmt.Errorf("directory %q not found — run 'bazelisk test //...' first to populate test logs", rootDir)
	}

	// Resolve symlinks so walk() and parseTestLog() operate on consistent paths.
	resolvedRoot, err := filepath.EvalSymlinks(rootDir)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", rootDir, err)
	}

	logFiles, err := walk(resolvedRoot)
	if err != nil {
		return fmt.Errorf("walking %s: %w", rootDir, err)
	}
	if len(logFiles) == 0 {
		return fmt.Errorf("no test.log files found under %s", rootDir)
	}

	var targets []*TargetResult
	for _, lf := range logFiles {
		tr, err := parseTestLog(lf, resolvedRoot)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: %v\n", err)
			continue
		}
		if len(tr.Tests) == 0 {
			continue
		}
		targets = append(targets, tr)
	}

	// Sort: targets with failures first, then alphabetically.
	sort.SliceStable(targets, func(i, j int) bool {
		fi, fj := targets[i].HasFailures(), targets[j].HasFailures()
		if fi != fj {
			return fi
		}
		return targets[i].Target < targets[j].Target
	})

	// Build summary
	var sumPassed, sumFailed, sumSkipped int
	var totalTime time.Duration
	for _, t := range targets {
		sumPassed += t.Passed()
		sumFailed += t.Failed()
		sumSkipped += t.Skipped()
		// Use suite time if available, else sum individual
		if t.SuiteTime > 0 {
			totalTime += t.SuiteTime
		} else {
			for _, r := range t.Tests {
				totalTime += r.Duration
			}
		}
	}

	// Round total time up to nearest second for display if large
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
