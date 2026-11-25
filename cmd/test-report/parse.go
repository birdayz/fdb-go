package main

import (
	"bufio"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

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
	Label   string `json:"label"`
	Run     int    `json:"run"`
	Shard   int    `json:"shard"`
	Attempt int    `json:"attempt"`
}

type bepTestResult struct {
	TestActionOutput []bepOutputFile `json:"testActionOutput"`
	Status           string          `json:"status"` // PASSED, FAILED, TIMEOUT, FLAKY, etc.
	DurationMillis   string          `json:"testAttemptDurationMillis"`
	CachedLocally    bool            `json:"cachedLocally"`
	ExecutionInfo    struct {
		CachedRemotely bool `json:"cachedRemotely"`
	} `json:"executionInfo"`
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

// ---- BEP + JUnit parsing ----

type bepTargetInfo struct {
	label      string
	xmlURI     string
	status     string
	durationMs int64
	cached     bool
	attempt    int
}

type bepResultKey struct {
	label      string
	run, shard int
}

// parseBEP reads the BEP JSONL file and returns one TargetResult per test target.
func parseBEP(path string) ([]*TargetResult, error) {
	targets, err := scanBEPTargets(path)
	if err != nil {
		return nil, err
	}

	keys := sortedBEPKeys(targets)

	// Build one TargetResult per label from its JUnit XML files.
	var results []*TargetResult
	byLabel := make(map[string]*TargetResult)
	for _, key := range keys {
		info := targets[key]
		tr := byLabel[info.label]
		if tr == nil {
			tr = &TargetResult{Target: info.label, Cached: true}
			byLabel[info.label] = tr
			results = append(results, tr)
		}
		if err := populateTargetFromBEP(tr, info); err != nil {
			return nil, err
		}
	}

	return results, nil
}

func scanBEPTargets(path string) (map[bepResultKey]*bepTargetInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Collect the final attempt of every run and shard; each has its own test.xml.
	targets := make(map[bepResultKey]*bepTargetInfo)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4<<20), 4<<20) // 4MB max line — BEP events can be large
	for scanner.Scan() {
		var ev bepEvent
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue // skip malformed lines
		}
		if ev.ID.TestResult == nil || ev.TestResult == nil {
			continue
		}
		id := ev.ID.TestResult
		key := bepResultKey{label: id.Label, run: id.Run, shard: id.Shard}
		if prev := targets[key]; prev != nil && prev.attempt > id.Attempt {
			continue
		}
		info := &bepTargetInfo{
			label:   id.Label,
			status:  ev.TestResult.Status,
			cached:  ev.TestResult.CachedLocally || ev.TestResult.ExecutionInfo.CachedRemotely,
			attempt: id.Attempt,
		}
		if ms, _ := strconv.ParseInt(ev.TestResult.DurationMillis, 10, 64); ms > 0 {
			info.durationMs = ms
		}
		for _, out := range ev.TestResult.TestActionOutput {
			if out.Name == "test.xml" {
				info.xmlURI = out.URI
			}
		}
		targets[key] = info
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning BEP: %w", err)
	}
	return targets, nil
}

func sortedBEPKeys(targets map[bepResultKey]*bepTargetInfo) []bepResultKey {
	keys := make([]bepResultKey, 0, len(targets))
	for key := range targets {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.label != b.label {
			return a.label < b.label
		}
		if a.run != b.run {
			return a.run < b.run
		}
		return a.shard < b.shard
	})
	return keys
}

func populateTargetFromBEP(tr *TargetResult, info *bepTargetInfo) error {
	if info.xmlURI == "" {
		return fmt.Errorf("%s: BEP test result has no test.xml", info.label)
	}
	xmlPath, err := uriToPath(info.xmlURI)
	if err != nil {
		return fmt.Errorf("%s: %w", info.label, err)
	}
	tr.SuiteTime += time.Duration(info.durationMs) * time.Millisecond
	tr.Cached = tr.Cached && info.cached
	if info.cached {
		tr.CachedResults++
	} else {
		tr.ExecutedResults++
	}

	// Check for Ginkgo's tree-structured JSON report alongside the standard test.xml.
	// Ginkgo suites write to $TEST_UNDECLARED_OUTPUTS_DIR/ginkgo-report.json which
	// Bazel collects into test.outputs/ next to test.xml. This has individual spec
	// names, durations, and Describe/Context hierarchy — the standard test.xml only
	// sees the bootstrap function.
	ginkgoPath := filepath.Join(filepath.Dir(xmlPath), "test.outputs", "ginkgo-report.json")
	var cases []TestResult
	if ginkgoCases, tree, err := parseGinkgoTreeJSON(ginkgoPath); err == nil && len(ginkgoCases) > 0 {
		cases = ginkgoCases
		tr.Tree = tree
	} else {
		cases, err = parseJUnitXML(xmlPath)
		if err != nil {
			return fmt.Errorf("%s: %w", info.label, err)
		}
	}

	// A test binary with no test cases still has a target-level outcome.
	if len(cases) == 0 {
		status := StatusPass
		if info.status == "FAILED" || info.status == "TIMEOUT" {
			status = StatusFail
		}
		cases = []TestResult{{
			Name:     "(target)",
			Status:   status,
			Duration: time.Duration(info.durationMs) * time.Millisecond,
		}}
	}
	// One tree describes one result; a multi-shard or multi-run target renders its flat case list.
	if len(tr.Tests) > 0 {
		tr.Tree = nil
	}
	tr.Tests = append(tr.Tests, cases...)
	return nil
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
func uriToPath(uri string) (string, error) {
	path, ok := strings.CutPrefix(uri, "file://")
	if !ok {
		return "", fmt.Errorf("test.xml is not local (%s); download test logs before reporting", uri)
	}
	return path, nil
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
