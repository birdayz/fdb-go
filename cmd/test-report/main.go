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
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"math"
	"net/url"
	"os"
	"sort"
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
	Target          string // Bazel label, e.g. "//pkg/foo:foo_test"
	Tests           []TestResult
	SuiteTime       time.Duration
	Cached          bool // true if every final run/shard result came from cache
	CachedResults   int
	ExecutedResults int
	Tree            *TreeNode // non-nil for Ginkgo targets with hierarchical specs
}

func (t *TargetResult) CachedLabel() string {
	if t.Cached {
		return "cached"
	}
	if t.CachedResults > 0 {
		return "mixed"
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
		return "—" // em dash — spec didn't run or no timing data
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

// ---- formatting ----

func formatDuration(d time.Duration) string {
	if d == 0 {
		return "—" // em dash
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

// ---- report metadata & main ----

type summaryData struct {
	Total           int
	Passed          int
	Failed          int
	Skipped         int
	Time            string
	CachedResults   int
	ExecutedResults int
}

type reportProvenance struct {
	SHA        string
	RunURL     string
	Lane       string
	Exclusions string
}

func (p reportProvenance) validate() error {
	if p == (reportProvenance{}) {
		return nil
	}
	if p.SHA == "" || p.RunURL == "" || strings.TrimSpace(p.Lane) == "" || strings.TrimSpace(p.Exclusions) == "" {
		return fmt.Errorf("report provenance requires sha, run-url, lane and exclusions together")
	}
	if _, err := hex.DecodeString(p.SHA); err != nil || len(p.SHA) != 40 && len(p.SHA) != 64 {
		return fmt.Errorf("report sha must be a full hexadecimal Git object ID")
	}
	u, err := url.Parse(p.RunURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return fmt.Errorf("report run-url must be an absolute HTTPS URL without user information")
	}
	return nil
}

type templateData struct {
	GeneratedAt string
	Provenance  reportProvenance
	Summary     summaryData
	Coverage    *CoverageReport // nil if no LCOV file provided
	Targets     []*TargetResult
}

func buildSummaryData(targets []*TargetResult) summaryData {
	var sumPassed, sumFailed, sumSkipped, cachedResults, executedResults int
	var totalTime time.Duration
	for _, t := range targets {
		sumPassed += t.Passed()
		sumFailed += t.Failed()
		sumSkipped += t.Skipped()
		cachedResults += t.CachedResults
		executedResults += t.ExecutedResults
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

	return summaryData{
		Total:           sumPassed + sumFailed + sumSkipped,
		Passed:          sumPassed,
		Failed:          sumFailed,
		Skipped:         sumSkipped,
		Time:            formatDuration(displayTime),
		CachedResults:   cachedResults,
		ExecutedResults: executedResults,
	}
}

func run() error {
	return runReport(os.Args[1:], os.Stdout)
}

func runReport(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("test-report", flag.ContinueOnError)
	coveragePath := flags.String("coverage", "", "path to LCOV coverage file (optional)")
	var provenance reportProvenance
	flags.StringVar(&provenance.SHA, "sha", "", "full tested Git object ID")
	flags.StringVar(&provenance.RunURL, "run-url", "", "HTTPS URL of the CI run")
	flags.StringVar(&provenance.Lane, "lane", "", "validation lane represented by this report")
	flags.StringVar(&provenance.Exclusions, "exclusions", "", "excluded targets and separately reported execution modes")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := provenance.validate(); err != nil {
		return err
	}

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
	if flags.NArg() > 0 {
		bepPaths = flags.Args()
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
		Provenance:  provenance,
		Summary:     buildSummaryData(targets),
		Coverage:    coverageReport,
		Targets:     targets,
	}

	return renderReportHTML(output, data)
}

func main() {
	if err := run(); err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintf(os.Stderr, "test-report: %v\n", err)
		os.Exit(1)
	}
}
