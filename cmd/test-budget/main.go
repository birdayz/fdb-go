// Command test-budget reports how much of its Bazel timeout budget each test
// target actually consumed, and fails when a target that RAN is close enough
// to its budget that the next growth will break it.
//
// It exists because a timeout breach is the worst possible place to learn that
// a suite outgrew its budget: by then the merge queue is already blocked, and
// the run that discovers it has burned the full budget to tell you. Two
// properties of Bazel conspire to hide the growth until exactly that moment:
//
//   - The action cache. A target whose inputs did not change is not executed
//     at all, so a lane can report green while executing a small fraction of
//     the targets it gates. A master run that executes 2 of 25 targets says
//     nothing about the 23 it skipped, and the first PR that touches a common
//     dependency executes all 25 cold.
//   - Bazel warns only in the other direction: `--test_verbose_timeout_warnings`
//     tells you a target's budget is too BIG. Nothing warns that it is nearly
//     too small.
//
// So this tool reads the run's Build Event Protocol stream, keeps the targets
// that genuinely executed (cache hits carry the duration of some earlier run
// on some earlier machine and would be noise at best), and compares each
// against the budget the BUILD file gave it.
//
// Usage:
//
//	bazel query 'tests(//scope/...)' --output=xml --xml:default_values > targets.xml
//	test-budget -bep .bazel-bep.jsonl -targets targets.xml [-threshold 0.6]
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

func main() {
	var (
		bepPath     = flag.String("bep", "", "path to a --build_event_json_file stream (repeat with commas for several)")
		targetsPath = flag.String("targets", "", "path to `bazel query --output=xml --xml:default_values` output for the tested scope")
		warn        = flag.Float64("warn", 0.70, "annotate when an executed target used more than this fraction of its budget")
		threshold   = flag.Float64("threshold", 0.90, "fail when an executed target used more than this fraction of its budget")
		slots       = flag.String("timeouts", "60,300,900,3600", "short,moderate,long,eternal budget seconds — must mirror .bazelrc's --test_timeout")
		label       = flag.String("lane", "", "name of the lane, used in the report header")
	)
	flag.Parse()

	if *bepPath == "" || *targetsPath == "" {
		fmt.Fprintln(os.Stderr, "test-budget: -bep and -targets are both required")
		os.Exit(2)
	}
	budgets, err := parseSlots(*slots)
	if err != nil {
		fmt.Fprintf(os.Stderr, "test-budget: %v\n", err)
		os.Exit(2)
	}

	targets, err := parseTargets(*targetsPath, budgets)
	if err != nil {
		fmt.Fprintf(os.Stderr, "test-budget: %v\n", err)
		os.Exit(2)
	}

	var runs []Run
	for _, p := range strings.Split(*bepPath, ",") {
		r, err := parseBEP(strings.TrimSpace(p))
		if err != nil {
			fmt.Fprintf(os.Stderr, "test-budget: %v\n", err)
			os.Exit(2)
		}
		runs = append(runs, r...)
	}

	if *warn > *threshold {
		fmt.Fprintf(os.Stderr, "test-budget: -warn (%.2f) is above -threshold (%.2f); the warning would never be reachable\n", *warn, *threshold)
		os.Exit(2)
	}

	report, warned, failed := Evaluate(runs, targets, *warn, *threshold)
	fmt.Print(RenderReport(report, *label, *warn, *threshold))
	// GitHub renders ::warning:: and ::error:: as job annotations.
	for _, r := range warned {
		fmt.Printf("::warning::%s used %.0f%% of its %s timeout budget (%s of %s). It is on course to time out; split it, shard it, or cut its work before it does.\n",
			r.Label, r.Fraction()*100, r.BudgetName, round(r.Duration), round(r.Budget))
	}
	if len(failed) > 0 {
		for _, r := range failed {
			fmt.Printf("::error::%s used %.0f%% of its %s timeout budget (%s of %s). This is the last run that fails FAST — the next one burns the whole budget and blocks the merge queue.\n",
				r.Label, r.Fraction()*100, r.BudgetName, round(r.Duration), round(r.Budget))
		}
		os.Exit(1)
	}
}

// Target is a test rule's timeout budget as declared in its BUILD file.
type Target struct {
	Label      string
	Budget     time.Duration
	BudgetName string // the timeout slot name — short/moderate/long/eternal
}

// Run is one target's outcome in a BEP stream.
type Run struct {
	Label    string
	Duration time.Duration
	Cached   bool
	Status   string
}

// Result pairs a run with the budget its BUILD file declared.
type Result struct {
	Run
	Budget     time.Duration
	BudgetName string
}

// Fraction is the share of the budget the run consumed. A run with no known
// budget reports 0 so it can never trip the gate on a guess.
func (r Result) Fraction() float64 {
	if r.Budget <= 0 {
		return 0
	}
	return float64(r.Duration) / float64(r.Budget)
}

// Evaluate joins runs to budgets and returns every executed target sorted by
// budget utilisation, those past the warn line, and those past the fail line.
//
// TWO LINES, not one, and the reason is the failure mode this tool is meant to
// replace rather than reproduce. A single line low enough to give useful notice
// (~70%) sits right on top of targets that are healthy but big — measured on
// master, //pkg/relational/conformance/rowdiff runs 659s of a 900s budget, 73%,
// and passes every time. Failing there would block unrelated PRs on a busy
// afternoon, which is the disease, not the cure. A single line high enough to
// avoid that (~90%) gives almost no notice. So: warn early and loudly, fail
// only when the next run would plausibly hit the wall.
//
// CACHED RUNS ARE EXCLUDED, and that is the whole point of the tool rather
// than a detail: the duration Bazel reports for a cache hit is the duration of
// whatever run originally populated the cache, on whatever machine, under
// whatever contention. Judging a budget on it would report a stale number as
// if it were this run's, which is the same class of blindness the tool exists
// to remove.
func Evaluate(runs []Run, targets map[string]Target, warn, threshold float64) (all, warned, failed []Result) {
	for _, run := range runs {
		if run.Cached {
			continue
		}
		t := targets[run.Label]
		res := Result{Run: run, Budget: t.Budget, BudgetName: t.BudgetName}
		all = append(all, res)
		switch {
		case res.Fraction() > threshold:
			failed = append(failed, res)
		case res.Fraction() > warn:
			warned = append(warned, res)
		}
	}
	byUtilisation := func(s []Result) {
		sort.Slice(s, func(i, j int) bool {
			if s[i].Fraction() != s[j].Fraction() {
				return s[i].Fraction() > s[j].Fraction()
			}
			return s[i].Label < s[j].Label
		})
	}
	byUtilisation(all)
	byUtilisation(warned)
	byUtilisation(failed)
	return all, warned, failed
}

// RenderReport formats the utilisation table for a CI log.
func RenderReport(all []Result, lane string, warn, threshold float64) string {
	var b strings.Builder
	name := "test budget"
	if lane != "" {
		name = lane + " test budget"
	}
	fmt.Fprintf(&b, "%s — %d target(s) executed, warn %.0f%%, fail %.0f%%\n", name, len(all), warn*100, threshold*100)
	if len(all) == 0 {
		// Not an error: on a branch whose inputs are all cached this is the
		// normal, healthy outcome. It is printed so that a lane which gates
		// on targets it never executes is visible rather than implied.
		fmt.Fprintf(&b, "  (every target was a cache hit — this run measured no runtime)\n")
		return b.String()
	}
	for _, r := range all {
		if r.Budget <= 0 {
			fmt.Fprintf(&b, "  %6s   %-8s %s\n", round(r.Duration), "?", r.Label)
			continue
		}
		fmt.Fprintf(&b, "  %6s   %3.0f%% of %-8s %s\n", round(r.Duration), r.Fraction()*100, r.BudgetName, r.Label)
	}
	return b.String()
}

func round(d time.Duration) string { return d.Truncate(time.Second).String() }

func parseSlots(s string) (map[string]time.Duration, error) {
	parts := strings.Split(s, ",")
	if len(parts) != 4 {
		return nil, fmt.Errorf("-timeouts needs 4 comma-separated seconds (short,moderate,long,eternal), got %q", s)
	}
	names := []string{"short", "moderate", "long", "eternal"}
	out := map[string]time.Duration{}
	for i, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("-timeouts entry %q is not a positive number of seconds", p)
		}
		out[names[i]] = time.Duration(n) * time.Second
	}
	return out, nil
}

// sizeToTimeout is Bazel's mapping from a test's `size` to its default timeout
// slot (https://bazel.build/reference/be/common-definitions#test.size).
var sizeToTimeout = map[string]string{
	"small":    "short",
	"medium":   "moderate",
	"large":    "long",
	"enormous": "eternal",
}

// stripXMLDecl removes a leading `<?xml ... ?>` declaration.
//
// `bazel query --output=xml` writes `version="1.1"`, and Go's encoding/xml
// refuses any version but 1.0 outright ("unsupported version"). The document
// body is plain XML that Go parses fine; only the declaration offends. Nothing
// downstream reads the declaration, so dropping it is lossless.
func stripXMLDecl(b []byte) []byte {
	trimmed := bytes.TrimLeft(b, " \t\r\n")
	if !bytes.HasPrefix(trimmed, []byte("<?xml")) {
		return b
	}
	end := bytes.Index(trimmed, []byte("?>"))
	if end < 0 {
		return b
	}
	return trimmed[end+2:]
}

type queryXML struct {
	Rules []struct {
		Name    string `xml:"name,attr"`
		Class   string `xml:"class,attr"`
		Strings []struct {
			Name  string `xml:"name,attr"`
			Value string `xml:"value,attr"`
		} `xml:"string"`
	} `xml:"rule"`
}

func parseTargets(path string, budgets map[string]time.Duration) (map[string]Target, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read targets: %w", err)
	}
	var q queryXML
	if err := xml.Unmarshal(stripXMLDecl(raw), &q); err != nil {
		return nil, fmt.Errorf("parse targets xml: %w", err)
	}
	out := map[string]Target{}
	for _, r := range q.Rules {
		var size, timeout string
		for _, s := range r.Strings {
			switch s.Name {
			case "size":
				size = s.Value
			case "timeout":
				timeout = s.Value
			}
		}
		// An explicit `timeout` wins over the one `size` implies — the same
		// precedence Bazel itself applies.
		slot := timeout
		if slot == "" {
			slot = sizeToTimeout[size]
		}
		d, ok := budgets[slot]
		if !ok {
			continue
		}
		out[r.Name] = Target{Label: r.Name, Budget: d, BudgetName: slot}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s named no test rule with a resolvable timeout — a budget gate over zero targets passes vacuously", path)
	}
	return out, nil
}

type bepLine struct {
	ID struct {
		TestSummary *struct {
			Label string `json:"label"`
		} `json:"testSummary"`
		TestResult *struct {
			Label string `json:"label"`
		} `json:"testResult"`
	} `json:"id"`
	TestSummary *struct {
		TotalRunDurationMillis string `json:"totalRunDurationMillis"`
		OverallStatus          string `json:"overallStatus"`
	} `json:"testSummary"`
	TestResult *struct {
		CachedLocally             bool   `json:"cachedLocally"`
		TestAttemptDurationMillis string `json:"testAttemptDurationMillis"`
		ExecutionInfo             *struct {
			CachedRemotely bool `json:"cachedRemotely"`
		} `json:"executionInfo"`
	} `json:"testResult"`
}

// parseBEP folds a BEP stream into one Run per label. Duration comes from the
// testSummary event, which already accounts for shards and retries; the
// cached flag comes from the testResult attempts, because testSummary does not
// carry one. A target is treated as cached only when EVERY attempt was a cache
// hit — one real execution is a real measurement.
func parseBEP(path string) ([]Run, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read bep: %w", err)
	}
	defer f.Close()

	type acc struct {
		duration  time.Duration
		status    string
		attempts  int
		cacheHits int
		seen      bool
	}
	byLabel := map[string]*acc{}
	get := func(label string) *acc {
		a, ok := byLabel[label]
		if !ok {
			a = &acc{}
			byLabel[label] = a
		}
		return a
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev bepLine
		if err := json.Unmarshal(line, &ev); err != nil {
			continue // BEP streams carry event kinds this tool does not model.
		}
		switch {
		case ev.ID.TestSummary != nil && ev.TestSummary != nil:
			a := get(ev.ID.TestSummary.Label)
			a.seen = true
			a.duration = millis(ev.TestSummary.TotalRunDurationMillis)
			a.status = ev.TestSummary.OverallStatus
		case ev.ID.TestResult != nil && ev.TestResult != nil:
			a := get(ev.ID.TestResult.Label)
			a.seen = true
			a.attempts++
			if ev.TestResult.CachedLocally || (ev.TestResult.ExecutionInfo != nil && ev.TestResult.ExecutionInfo.CachedRemotely) {
				a.cacheHits++
			}
			if a.duration == 0 {
				a.duration = millis(ev.TestResult.TestAttemptDurationMillis)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read bep %s: %w", path, err)
	}

	var out []Run
	for label, a := range byLabel {
		if !a.seen {
			continue
		}
		out = append(out, Run{
			Label:    label,
			Duration: a.duration,
			Cached:   a.attempts > 0 && a.cacheHits == a.attempts,
			Status:   a.status,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out, nil
}

func millis(s string) time.Duration {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Millisecond
}
