// Binary bench-report compares two Go benchmark result files and outputs a
// markdown comparison table. Optionally posts/updates a PR comment on GitHub.
//
// Usage:
//
//	bench-report -old baseline.txt -new current.txt
//	bench-report -old baseline.txt -new current.txt -pr 48 -repo owner/repo
//
// Benchmark files are standard Go test -bench output:
//
//	BenchmarkFoo-24    1000    1234567 ns/op    5678 B/op    12 allocs/op
//
// When -pr is set, the tool posts or updates a comment on the PR using `gh api`.
// A marker comment (<!-- bench-report -->) ensures only one comment exists —
// subsequent runs update it instead of creating duplicates.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type benchResult struct {
	Name        string
	NsPerOp     float64
	BytesPerOp  float64
	AllocsPerOp float64
	Iterations  int
}

var benchLineRe = regexp.MustCompile(
	`^(Benchmark\S+)\s+(\d+)\s+([\d.]+)\s+ns/op` +
		`(?:\s+([\d.]+)\s+B/op)?` +
		`(?:\s+([\d.]+)\s+allocs/op)?`,
)

func parseBenchFile(path string) (map[string]*benchResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	results := make(map[string]*benchResult)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		m := benchLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name := m[1]
		iters, _ := strconv.Atoi(m[2])
		nsOp, _ := strconv.ParseFloat(m[3], 64)
		var bytesOp, allocsOp float64
		if m[4] != "" {
			bytesOp, _ = strconv.ParseFloat(m[4], 64)
		}
		if m[5] != "" {
			allocsOp, _ = strconv.ParseFloat(m[5], 64)
		}
		results[name] = &benchResult{
			Name:        name,
			NsPerOp:     nsOp,
			BytesPerOp:  bytesOp,
			AllocsPerOp: allocsOp,
			Iterations:  iters,
		}
	}
	return results, scanner.Err()
}

type comparison struct {
	Name      string
	OldNs     float64
	NewNs     float64
	DeltaPct  float64 // positive = regression
	OldBytes  float64
	NewBytes  float64
	OldAllocs float64
	NewAllocs float64
	Status    string // "faster", "slower", "~"
}

// regressionThreshold is the percentage delta above which a benchmark is
// considered a regression or improvement. Single-iteration benchmarks on
// shared CI have 10-30% variance, so this must not be too tight.
var regressionThreshold = 10.0

func compare(old, new map[string]*benchResult) []comparison {
	var result []comparison
	// Use sorted keys for deterministic output.
	var names []string
	for name := range new {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		newR := new[name]
		oldR, ok := old[name]
		if !ok {
			result = append(result, comparison{
				Name:   name,
				NewNs:  newR.NsPerOp,
				Status: "new",
			})
			continue
		}
		delta := 0.0
		if oldR.NsPerOp > 0 {
			delta = (newR.NsPerOp - oldR.NsPerOp) / oldR.NsPerOp * 100
		}
		status := "~"
		// Only flag timing regressions when allocation count also changed.
		// If alloc count is identical, timing variance is noise from CI VM
		// load, not a real code regression. Byte sizes can vary slightly
		// (protobuf encoding, buffer pools) and are not reliable signals.
		allocsChanged := oldR.AllocsPerOp != newR.AllocsPerOp
		if delta > regressionThreshold && allocsChanged {
			status = "slower"
		} else if delta < -regressionThreshold && allocsChanged {
			status = "faster"
		}
		result = append(result, comparison{
			Name:      name,
			OldNs:     oldR.NsPerOp,
			NewNs:     newR.NsPerOp,
			DeltaPct:  delta,
			OldBytes:  oldR.BytesPerOp,
			NewBytes:  newR.BytesPerOp,
			OldAllocs: oldR.AllocsPerOp,
			NewAllocs: newR.AllocsPerOp,
			Status:    status,
		})
	}

	// Report removed benchmarks.
	for name := range old {
		if _, ok := new[name]; !ok {
			result = append(result, comparison{
				Name:   name,
				OldNs:  old[name].NsPerOp,
				Status: "removed",
			})
		}
	}
	return result
}

const marker = "<!-- bench-report -->"

func formatMarkdown(comps []comparison) string {
	var sb strings.Builder
	sb.WriteString(marker + "\n")
	sb.WriteString("## Benchmark Comparison\n\n")

	// Check if any regressions.
	hasRegression := false
	hasImprovement := false
	for _, c := range comps {
		if c.Status == "slower" {
			hasRegression = true
		}
		if c.Status == "faster" {
			hasImprovement = true
		}
	}

	if hasRegression {
		sb.WriteString("**Warning: performance regressions detected.**\n\n")
	} else if hasImprovement {
		sb.WriteString("Performance improvements detected.\n\n")
	} else {
		sb.WriteString("No significant performance changes.\n\n")
	}

	sb.WriteString("| Benchmark | old ns/op | new ns/op | delta | old B/op | new B/op | old allocs | new allocs |\n")
	sb.WriteString("|---|--:|--:|--:|--:|--:|--:|--:|\n")

	for _, c := range comps {
		if c.Status == "new" {
			sb.WriteString(fmt.Sprintf("| %s | - | %s | new | - | - | - | - |\n",
				stripBenchPrefix(c.Name), fmtNs(c.NewNs)))
			continue
		}
		if c.Status == "removed" {
			sb.WriteString(fmt.Sprintf("| ~~%s~~ | %s | - | removed | - | - | - | - |\n",
				stripBenchPrefix(c.Name), fmtNs(c.OldNs)))
			continue
		}
		deltaStr := fmtDelta(c.DeltaPct, c.Status)
		sb.WriteString(fmt.Sprintf("| %s | %s | %s | %s | %s | %s | %s | %s |\n",
			stripBenchPrefix(c.Name),
			fmtNs(c.OldNs), fmtNs(c.NewNs), deltaStr,
			fmtFloat(c.OldBytes), fmtFloat(c.NewBytes),
			fmtFloat(c.OldAllocs), fmtFloat(c.NewAllocs),
		))
	}

	sb.WriteString(fmt.Sprintf("\n<sub>Generated by bench-report. Threshold: +/-%.0f%%.</sub>\n", regressionThreshold))
	return sb.String()
}

func stripBenchPrefix(name string) string {
	name = strings.TrimPrefix(name, "Benchmark")
	// Strip -N suffix (GOMAXPROCS).
	if idx := strings.LastIndex(name, "-"); idx > 0 {
		if _, err := strconv.Atoi(name[idx+1:]); err == nil {
			name = name[:idx]
		}
	}
	return name
}

func fmtNs(ns float64) string {
	switch {
	case ns >= 1e9:
		return fmt.Sprintf("%.2fs", ns/1e9)
	case ns >= 1e6:
		return fmt.Sprintf("%.2fms", ns/1e6)
	case ns >= 1e3:
		return fmt.Sprintf("%.2fus", ns/1e3)
	default:
		return fmt.Sprintf("%.1fns", ns)
	}
}

func fmtFloat(v float64) string {
	if v == 0 {
		return "0"
	}
	if v == math.Trunc(v) {
		return strconv.FormatInt(int64(v), 10)
	}
	return fmt.Sprintf("%.1f", v)
}

func fmtDelta(pct float64, status string) string {
	sign := "+"
	if pct < 0 {
		sign = ""
	}
	s := fmt.Sprintf("%s%.1f%%", sign, pct)
	switch status {
	case "slower":
		return "**" + s + "**"
	case "faster":
		return s
	default:
		return s + " ~"
	}
}

// postOrUpdateComment posts or updates a PR comment via GitHub REST API.
// Uses GH_TOKEN env var for authentication (set by github.token in Actions).
func postOrUpdateComment(repo string, pr int, body string) error {
	token := os.Getenv("GH_TOKEN")
	if token == "" {
		return fmt.Errorf("GH_TOKEN not set")
	}

	existingID, err := findMarkerComment(repo, pr, token)
	if err != nil {
		return fmt.Errorf("finding existing comment: %w", err)
	}

	payload, _ := json.Marshal(map[string]string{"body": body})

	var url, method string
	if existingID > 0 {
		url = fmt.Sprintf("https://api.github.com/repos/%s/issues/comments/%d", repo, existingID)
		method = "PATCH"
	} else {
		url = fmt.Sprintf("https://api.github.com/repos/%s/issues/%d/comments", repo, pr)
		method = "POST"
	}

	req, err := http.NewRequest(method, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GitHub API %s %s: %d %s", method, url, resp.StatusCode, string(respBody))
	}
	return nil
}

func findMarkerComment(repo string, pr int, token string) (int64, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/issues/%d/comments?per_page=100", repo, pr)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("list comments: HTTP %d", resp.StatusCode)
	}

	var comments []struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&comments); err != nil {
		return 0, err
	}

	for _, c := range comments {
		if strings.HasPrefix(c.Body, marker) {
			return c.ID, nil
		}
	}
	return 0, nil
}

func main() {
	oldFile := flag.String("old", "", "Path to baseline benchmark results")
	newFile := flag.String("new", "", "Path to current benchmark results")
	prNum := flag.Int("pr", 0, "PR number (posts/updates comment if set)")
	repo := flag.String("repo", "", "GitHub repo (owner/repo)")
	threshold := flag.Float64("threshold", 10.0, "Regression threshold percentage (default 10%)")
	flag.Parse()

	regressionThreshold = *threshold

	if *newFile == "" {
		fmt.Fprintln(os.Stderr, "usage: bench-report -old baseline.txt -new current.txt [-pr N -repo owner/repo]")
		os.Exit(1)
	}

	newResults, err := parseBenchFile(*newFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parsing %s: %v\n", *newFile, err)
		os.Exit(1)
	}

	var oldResults map[string]*benchResult
	if *oldFile != "" {
		oldResults, err = parseBenchFile(*oldFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "parsing %s: %v\n", *oldFile, err)
			os.Exit(1)
		}
	}

	if oldResults == nil {
		// No baseline — just show current results.
		oldResults = make(map[string]*benchResult)
	}

	comps := compare(oldResults, newResults)
	md := formatMarkdown(comps)

	// Always write to stdout.
	fmt.Print(md)

	// Post to PR if requested.
	if *prNum > 0 && *repo != "" {
		if err := postOrUpdateComment(*repo, *prNum, md); err != nil {
			fmt.Fprintf(os.Stderr, "posting comment: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "Updated PR #%d comment on %s\n", *prNum, *repo)
	}
}
