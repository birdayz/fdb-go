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
	"flag"
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

// ---- Ginkgo tree types ----

// TreeNode is a node in the Ginkgo container hierarchy.
type TreeNode struct {
	Name     string      // container text (e.g. "SaveRecord") or leaf name
	Children []*TreeNode // sub-containers
	Leaf     *TestResult // nil for containers, set for leaf specs
}

// TreeCounts returns aggregate pass/fail/skip counts for this node and all descendants.
func (n *TreeNode) TreeCounts() (passed, failed, skipped int) {
	if n.Leaf != nil {
		switch n.Leaf.Status {
		case StatusPass:
			return 1, 0, 0
		case StatusFail:
			return 0, 1, 0
		case StatusSkip:
			return 0, 0, 1
		}
		return 0, 0, 0
	}
	for _, c := range n.Children {
		p, f, s := c.TreeCounts()
		passed += p
		failed += f
		skipped += s
	}
	return
}

// TreeTotal returns the total spec count under this node.
func (n *TreeNode) TreeTotal() int {
	p, f, s := n.TreeCounts()
	return p + f + s
}

// TreeHasFailures returns true if any descendant spec failed.
func (n *TreeNode) TreeHasFailures() bool {
	_, f, _ := n.TreeCounts()
	return f > 0
}

// TreeBadgeClass returns the CSS class for this node's badge.
func (n *TreeNode) TreeBadgeClass() string {
	if n.TreeHasFailures() {
		return "badge-fail"
	}
	return "badge-pass"
}

// TreeBadgeText returns the badge text for this container node.
func (n *TreeNode) TreeBadgeText() string {
	p, f, s := n.TreeCounts()
	total := p + f + s
	if f > 0 {
		return fmt.Sprintf("%d/%d failed", f, total)
	}
	if s > 0 {
		return fmt.Sprintf("%d/%d passed, %d skipped", p, total, s)
	}
	return fmt.Sprintf("%d/%d passed", p, total)
}

// ginkgoJSONSpec is a single spec entry in ginkgo-report.json.
type ginkgoJSONSpec struct {
	Containers []string `json:"containers"`
	Name       string   `json:"name"`
	State      string   `json:"state"`
	DurationMs float64  `json:"duration_ms"`
}

// parseGinkgoTreeJSON reads a ginkgo-report.json file and returns both a flat
// list of TestResult (for summary counting) and a TreeNode hierarchy for
// rendering.
func parseGinkgoTreeJSON(path string) ([]TestResult, *TreeNode, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}

	var specs []ginkgoJSONSpec
	if err := json.Unmarshal(data, &specs); err != nil {
		return nil, nil, fmt.Errorf("parsing ginkgo JSON: %w", err)
	}

	if len(specs) == 0 {
		return nil, nil, nil
	}

	root := &TreeNode{Name: "root"}
	var results []TestResult

	for _, spec := range specs {
		status := StatusPass
		switch spec.State {
		case "failed", "panicked", "interrupted", "aborted":
			status = StatusFail
		case "skipped", "pending":
			status = StatusSkip
		}

		tr := TestResult{
			Name:     spec.Name,
			Status:   status,
			Duration: time.Duration(spec.DurationMs * float64(time.Millisecond)),
		}
		results = append(results, tr)

		// Build/find the path in the tree.
		node := root
		for _, container := range spec.Containers {
			found := false
			for _, child := range node.Children {
				if child.Name == container && child.Leaf == nil {
					node = child
					found = true
					break
				}
			}
			if !found {
				child := &TreeNode{Name: container}
				node.Children = append(node.Children, child)
				node = child
			}
		}

		// Add the leaf spec.
		leaf := &TreeNode{
			Name: spec.Name,
			Leaf: &tr,
		}
		node.Children = append(node.Children, leaf)
	}

	return results, root, nil
}

// ---- BEP JSON types (subset we care about) ----

type bepEvent struct {
	ID         bepID          `json:"id"`
	TestResult *bepTestResult `json:"testResult,omitempty"`
}

type bepID struct {
	TestResult *bepTestResultID `json:"testResult,omitempty"`
}

type bepTestResultID struct {
	Label string `json:"label"`
}

type bepTestResult struct {
	TestActionOutput []bepOutputFile `json:"testActionOutput"`
	Status           string          `json:"status"` // PASSED, FAILED, TIMEOUT, FLAKY, etc.
	DurationMillis   string          `json:"testAttemptDurationMillis"`
	CachedLocally    bool            `json:"cachedLocally"`
}

type bepOutputFile struct {
	Name string `json:"name"` // "test.xml", "test.log"
	URI  string `json:"uri"`  // "file:///absolute/path"
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
	Cached    bool      // true if Bazel served this target from cache
	Tree      *TreeNode // non-nil for Ginkgo targets with hierarchical specs
}

func (t *TargetResult) CachedLabel() string {
	if t.Cached {
		return "cached"
	}
	return "executed"
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
		return "\u2014" // em dash — spec didn't run or no timing data
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
		status     string
		durationMs int64
		cached     bool
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
				cached: ev.TestResult.CachedLocally,
			}
			if ms, _ := strconv.ParseInt(ev.TestResult.DurationMillis, 10, 64); ms > 0 {
				info.durationMs = ms
			}
			for _, out := range ev.TestResult.TestActionOutput {
				if out.Name == "test.xml" {
					info.xmlPath = uriToPath(out.URI)
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
			Cached:    info.cached,
		}

		if info.xmlPath != "" {
			// Check for Ginkgo's tree-structured JSON report alongside the standard test.xml.
			// Ginkgo suites write to $TEST_UNDECLARED_OUTPUTS_DIR/ginkgo-report.json which
			// Bazel collects into test.outputs/ next to test.xml. This has individual spec
			// names, durations, and Describe/Context hierarchy — the standard test.xml only
			// sees the bootstrap function.
			ginkgoPath := filepath.Join(filepath.Dir(info.xmlPath), "test.outputs", "ginkgo-report.json")
			if ginkgoCases, tree, err := parseGinkgoTreeJSON(ginkgoPath); err == nil && len(ginkgoCases) > 0 {
				tr.Tests = ginkgoCases
				tr.Tree = tree
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
			// Skip Ginkgo infrastructure nodes — not real tests.
			if isGinkgoInfraNode(tc.Name) {
				continue
			}
			status := StatusPass
			if tc.Failure != nil || tc.Error != nil {
				status = StatusFail
			} else if tc.Skipped != nil {
				status = StatusSkip
			}
			name := tc.Name
			// Strip Ginkgo node type prefix — every spec has [It] and it's noise.
			for _, prefix := range []string{"[It] ", "[Measure] "} {
				name = strings.TrimPrefix(name, prefix)
			}
			results = append(results, TestResult{
				Name:     name,
				Status:   status,
				Duration: time.Duration(tc.Time * float64(time.Second)),
			})
		}
	}
	return results, nil
}

// isGinkgoInfraNode returns true for Ginkgo setup/teardown nodes that
// should be excluded from test reports (they're infrastructure, not tests).
func isGinkgoInfraNode(name string) bool {
	switch name {
	case "[BeforeSuite]", "[AfterSuite]",
		"[SynchronizedBeforeSuite]", "[SynchronizedAfterSuite]",
		"[ReportAfterSuite]", "[ReportBeforeSuite]",
		"[BeforeAll]", "[AfterAll]",
		"[DeferCleanup]":
		return true
	}
	return false
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

// ---- LCOV coverage types ----

// FileCoverage holds line coverage data for a single source file.
type FileCoverage struct {
	Path     string
	LinesHit int
	Lines    int
}

// PackageCoverage holds aggregated coverage for a package (directory).
type PackageCoverage struct {
	Package  string
	LinesHit int
	Lines    int
}

// Percent returns the coverage percentage.
func (p *PackageCoverage) Percent() float64 {
	if p.Lines == 0 {
		return 0
	}
	return float64(p.LinesHit) / float64(p.Lines) * 100
}

// PercentStr returns the coverage percentage formatted as a string.
func (p *PackageCoverage) PercentStr() string {
	return fmt.Sprintf("%.1f%%", p.Percent())
}

// BarWidth returns the CSS width percentage for the coverage bar.
func (p *PackageCoverage) BarWidth() string {
	return fmt.Sprintf("%.1f%%", p.Percent())
}

// CoverageReport holds the parsed LCOV data aggregated by package.
type CoverageReport struct {
	Packages   []*PackageCoverage
	TotalHit   int
	TotalLines int
}

// Percent returns the overall coverage percentage.
func (c *CoverageReport) Percent() float64 {
	if c.TotalLines == 0 {
		return 0
	}
	return float64(c.TotalHit) / float64(c.TotalLines) * 100
}

// PercentStr returns the overall coverage percentage as a string.
func (c *CoverageReport) PercentStr() string {
	return fmt.Sprintf("%.1f%%", c.Percent())
}

// CoverageColor returns the CSS color class for the overall coverage.
func (c *CoverageReport) CoverageColor() string {
	pct := c.Percent()
	if pct >= 80 {
		return "#27ae60" // green
	}
	if pct >= 60 {
		return "#f39c12" // yellow
	}
	return "#e74c3c" // red
}

// parseLCOV reads an LCOV file and returns a CoverageReport.
// Skips files in gen/ directories and test files (*_test.go).
func parseLCOV(path string) (*CoverageReport, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Parse per-file coverage.
	var files []FileCoverage
	var currentFile string
	var linesHit, linesTotal int

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "SF:"):
			currentFile = strings.TrimPrefix(line, "SF:")
			linesHit = 0
			linesTotal = 0
		case strings.HasPrefix(line, "DA:"):
			// DA:linenum,execcount
			parts := strings.SplitN(strings.TrimPrefix(line, "DA:"), ",", 2)
			if len(parts) == 2 {
				linesTotal++
				if count, err := strconv.Atoi(parts[1]); err == nil && count > 0 {
					linesHit++
				}
			}
		case line == "end_of_record":
			if currentFile != "" {
				files = append(files, FileCoverage{
					Path:     currentFile,
					LinesHit: linesHit,
					Lines:    linesTotal,
				})
				currentFile = ""
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning LCOV: %w", err)
	}

	// Aggregate by package, skipping gen/ and _test.go.
	pkgMap := make(map[string]*PackageCoverage)
	for _, fc := range files {
		if strings.HasPrefix(fc.Path, "gen/") || strings.Contains(fc.Path, "/gen/") {
			continue
		}
		if strings.HasSuffix(fc.Path, "_test.go") {
			continue
		}
		pkg := filepath.Dir(fc.Path)
		pc, ok := pkgMap[pkg]
		if !ok {
			pc = &PackageCoverage{Package: pkg}
			pkgMap[pkg] = pc
		}
		pc.LinesHit += fc.LinesHit
		pc.Lines += fc.Lines
	}

	// Sort packages alphabetically.
	packages := make([]*PackageCoverage, 0, len(pkgMap))
	for _, pc := range pkgMap {
		packages = append(packages, pc)
	}
	sort.Slice(packages, func(i, j int) bool {
		return packages[i].Package < packages[j].Package
	})

	// Compute totals.
	var totalHit, totalLines int
	for _, pc := range packages {
		totalHit += pc.LinesHit
		totalLines += pc.Lines
	}

	return &CoverageReport{
		Packages:   packages,
		TotalHit:   totalHit,
		TotalLines: totalLines,
	}, nil
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
	Coverage    *CoverageReport // nil if no LCOV file provided
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
.badge-cached { background: #eef0f5; color: #8fa3b1; font-weight: 400; }

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

/* Tree styles for Ginkgo hierarchical specs */
.tree-container { padding: 8px 0; }
.tree-node { padding-left: 20px; }
.tree-node details {
  border: none;
  border-radius: 0;
  box-shadow: none;
  border-left: 2px solid #dde1ec;
  margin: 2px 0;
}
.tree-node details.has-fail { border-left: 2px solid #e74c3c; }
.tree-node details.has-pass { border-left: 2px solid #27ae60; }
.tree-node summary { padding: 6px 12px; }
.tree-node .container-name {
  font-family: "SFMono-Regular", Consolas, "Liberation Mono", Menlo, monospace;
  font-size: 13px;
  font-weight: 500;
  color: #2c3e50;
  flex: 1;
}
.tree-leaf {
  display: flex;
  align-items: center;
  padding: 4px 12px;
  gap: 8px;
  border-bottom: 1px solid #f0f1f7;
}
.tree-leaf:last-child { border-bottom: none; }
.tree-leaf.row-fail { background: #fff8f8; }
.tree-leaf.row-skip { background: #fffdf5; color: #7f8c8d; }
.tree-leaf .col-name { flex: 1; }
.tree-leaf .col-status { width: 40px; text-align: center; flex-shrink: 0; }
.tree-leaf .col-duration { width: 80px; text-align: right; flex-shrink: 0; font-family: "SFMono-Regular", Consolas, "Liberation Mono", Menlo, monospace; font-size: 12px; color: #7f8c8d; }

/* Coverage section */
.coverage-section {
  margin: 0 32px;
  background: #fff;
  border: 1px solid #dde1ec;
  border-radius: 6px;
  overflow: hidden;
  box-shadow: 0 1px 3px rgba(0,0,0,0.04);
}
.coverage-section summary {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 12px 16px;
  cursor: pointer;
  user-select: none;
  list-style: none;
  gap: 12px;
}
.coverage-section summary::-webkit-details-marker { display: none; }
.coverage-section summary::marker { display: none; }
.coverage-section summary:hover { background: #f8f9ff; }
.coverage-section .section-title {
  font-size: 14px;
  font-weight: 600;
  color: #2c3e50;
  flex: 1;
}
.coverage-bar-cell {
  width: 200px;
}
.coverage-bar {
  height: 10px;
  background: #ecf0f1;
  border-radius: 5px;
  overflow: hidden;
}
.coverage-bar-fill {
  height: 100%;
  border-radius: 5px;
  background: #27ae60;
}
.coverage-table { width: 100%; border-collapse: collapse; }
.coverage-table th {
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
.coverage-table td {
  padding: 7px 16px;
  border-bottom: 1px solid #f0f1f7;
  vertical-align: middle;
}
.coverage-table tr:last-child td { border-bottom: none; }
.coverage-table .col-pkg {
  font-family: "SFMono-Regular", Consolas, "Liberation Mono", Menlo, monospace;
  font-size: 12px;
}
.coverage-table .col-num {
  text-align: right;
  width: 80px;
  font-family: "SFMono-Regular", Consolas, "Liberation Mono", Menlo, monospace;
  font-size: 12px;
  color: #7f8c8d;
}
.coverage-table .col-pct {
  text-align: right;
  width: 80px;
  font-family: "SFMono-Regular", Consolas, "Liberation Mono", Menlo, monospace;
  font-size: 12px;
  font-weight: 600;
}
.coverage-table .col-bar { width: 200px; }

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
{{- if .Coverage}}
  <div class="summary-divider"></div>
  <div class="stat"><span class="val" style="color: {{.Coverage.CoverageColor}}">{{.Coverage.PercentStr}}</span><span class="lbl">Coverage</span></div>
{{- end}}
</div>

{{- if .Coverage}}
<div style="padding: 24px 32px 0 32px;">
<details class="coverage-section">
  <summary>
    <span class="section-title">Coverage by Package</span>
    <span class="badge badge-pass" style="background: {{.Coverage.CoverageColor}}22; color: {{.Coverage.CoverageColor}}">{{.Coverage.TotalHit}}/{{.Coverage.TotalLines}} lines &mdash; {{.Coverage.PercentStr}}</span>
    <span class="chevron">&#9654;</span>
  </summary>
  <table class="coverage-table">
    <thead>
      <tr>
        <th>Package</th>
        <th style="text-align:right">Lines</th>
        <th style="text-align:right">Hit</th>
        <th style="text-align:right">Coverage</th>
        <th></th>
      </tr>
    </thead>
    <tbody>
    {{- range .Coverage.Packages}}
      <tr>
        <td class="col-pkg">{{.Package}}</td>
        <td class="col-num">{{.Lines}}</td>
        <td class="col-num">{{.LinesHit}}</td>
        <td class="col-pct">{{.PercentStr}}</td>
        <td class="col-bar"><div class="coverage-bar"><div class="coverage-bar-fill" style="width: {{.BarWidth}}"></div></div></td>
      </tr>
    {{- end}}
    </tbody>
  </table>
</details>
</div>
{{- end}}

<div class="targets">
{{- range .Targets}}
<details {{if .HasFailures}}class="has-fail" open{{else}}class="has-pass"{{end}}>
  <summary>
    <span class="target-label">{{.Target}}</span>
    <span class="badge {{.BadgeClass}}">{{.BadgeText}}</span>
    {{if .Cached}}<span class="badge badge-cached">cached</span>{{end}}
    <span class="chevron">&#9654;</span>
  </summary>
  {{- if .Tree}}
  <div class="tree-container">
    {{- range .Tree.Children}}
    {{template "treenode" .}}
    {{- end}}
  </div>
  {{- else if .Tests}}
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
  {{- end}}
</details>
{{- end}}
</div>

<footer>Generated by bazel-test-report &middot; {{.GeneratedAt}}</footer>

</body>
</html>

{{define "treenode"}}
{{- if .Leaf}}
<div class="tree-leaf {{.Leaf.RowClass}}">
  <span class="col-name">{{.Leaf.Name}}</span>
  <span class="col-status">{{.Leaf.StatusIcon}}</span>
  <span class="col-duration">{{.Leaf.DurationStr}}</span>
</div>
{{- else}}
<div class="tree-node">
  <details {{if .TreeHasFailures}}class="has-fail" open{{else}}class="has-pass"{{end}}>
    <summary>
      <span class="container-name">{{.Name}}</span>
      <span class="badge {{.TreeBadgeClass}}">{{.TreeBadgeText}}</span>
      <span class="chevron">&#9654;</span>
    </summary>
    {{- range .Children}}
    {{template "treenode" .}}
    {{- end}}
  </details>
</div>
{{- end}}
{{end}}
`

// ---- main ----

func run() error {
	coveragePath := flag.String("coverage", "", "path to LCOV coverage file (optional)")
	flag.Parse()

	// Accept one or more BEP file paths. Each `bazelisk test`
	// invocation writes its own BEP, and the global
	// `test --build_event_json_file=` setting in .bazelrc means
	// successive invocations OVERWRITE the same default file. CI
	// works around this by passing each invocation an explicit path
	// and forwarding all of them here. Targets from each BEP are
	// concatenated; duplicate target names (e.g. a test re-run under
	// the race detector) appear as separate rows so each suite's
	// outcome is visible. Default path stays `.bazel-bep.jsonl` so
	// local single-invocation use is unchanged.
	bepPaths := []string{".bazel-bep.jsonl"}
	if flag.NArg() > 0 {
		bepPaths = flag.Args()
	}

	var targets []*TargetResult
	for _, bepPath := range bepPaths {
		if _, err := os.Stat(bepPath); os.IsNotExist(err) {
			return fmt.Errorf("BEP file %q not found — run 'bazelisk test //... --build_event_json_file=%s' first", bepPath, bepPath)
		}
		bepTargets, err := parseBEP(bepPath)
		if err != nil {
			return fmt.Errorf("parsing BEP %q: %w", bepPath, err)
		}
		targets = append(targets, bepTargets...)
	}
	if len(targets) == 0 {
		return fmt.Errorf("no test results found in %v", bepPaths)
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

	// Parse LCOV coverage if provided.
	var coverageReport *CoverageReport
	if *coveragePath != "" {
		cr, err := parseLCOV(*coveragePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: coverage: %v\n", err)
		} else {
			coverageReport = cr
		}
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
		Coverage: coverageReport,
		Targets:  targets,
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
