// Command corpus-mutation-proof measures what a corpus actually DETECTS,
// rather than what it covers.
//
// It exists to settle one question that a coverage number cannot: if
// cmd/factory-prune drops 79% of the committed factory corpus, does the
// remaining 21% still catch every bug the whole corpus caught? Line coverage
// answers "was this code executed", which is not the same claim — a scenario
// can execute a line and assert nothing that depends on it.
//
// METHOD. For each mutation: patch one engine source file, PROVE the patch
// landed, build, run the FULL corpus once, and record which scenarios failed.
// The pruned corpus is a strict SUBSET of the full one, so "does the pruned
// corpus catch this mutation" is answered by intersecting that failure set
// with the keep list — no second run needed, and the answer is exact rather
// than an approximation of one.
//
// WHY THE PATCH IS VERIFIED. A mutation that silently fails to apply produces
// a green run, and a green run is exactly what "the corpus did not catch it"
// also looks like. Worse, an un-applied mutation makes the corpus look
// PERFECT — every scenario passes — so the failure mode flatters the thing
// under test. Every mutation therefore reports its occurrence count before and
// after, and a mutation whose count did not move is a hard error, never a
// datum. The build result is read separately from the test result for the same
// reason: a mutation that does not compile makes the target report
// "Executed 0 out of 1", which a grep for failures renders as silence.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
)

// Mutation is one deliberate engine defect.
type Mutation struct {
	Name string `json:"name"`
	// Why records what class of bug this stands in for, so a result table is
	// readable without reading the diff.
	Why  string `json:"why"`
	File string `json:"file"`
	From string `json:"from"`
	To   string `json:"to"`
}

type result struct {
	Mutation
	occurrencesBefore int
	occurrencesAfter  int
	built             bool
	ranScenarios      int
	failedAll         []string
	failedKept        []string
	err               string
}

func main() {
	var (
		mutFile  = flag.String("mutations", "", "JSON file describing the mutations (required)")
		keepFile = flag.String("keep-list", "", "scenario names retained by factory-prune (required)")
		target   = flag.String("target", "//pkg/relational/conformance/factorycorpus/full:full_test", "corpus test target")
		only     = flag.String("only", "", "run just this mutation by name")
	)
	flag.Parse()
	if *mutFile == "" || *keepFile == "" {
		fmt.Fprintln(os.Stderr, "corpus-mutation-proof: -mutations and -keep-list are both required")
		os.Exit(2)
	}

	muts, err := loadMutations(*mutFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load mutations: %v\n", err)
		os.Exit(1)
	}
	keep, err := loadKeep(*keepFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load keep list: %v\n", err)
		os.Exit(1)
	}
	if len(keep) == 0 {
		fmt.Fprintln(os.Stderr, "keep list is EMPTY — every mutation would report 'pruned corpus missed it'")
		os.Exit(1)
	}
	fmt.Printf("keep list: %d scenarios\n", len(keep))

	var results []result
	for _, m := range muts {
		if *only != "" && m.Name != *only {
			continue
		}
		fmt.Printf("\n=== mutation %s ===\n", m.Name)
		results = append(results, run(m, keep, *target))
	}
	if len(results) == 0 {
		fmt.Fprintln(os.Stderr, "no mutations ran — the -only filter matched nothing")
		os.Exit(1)
	}
	report(results)
}

func run(m Mutation, keep map[string]bool, target string) result {
	r := result{Mutation: m}
	orig, err := os.ReadFile(m.File)
	if err != nil {
		r.err = fmt.Sprintf("read %s: %v", m.File, err)
		return r
	}
	src := string(orig)
	r.occurrencesBefore = strings.Count(src, m.From)
	if r.occurrencesBefore == 0 {
		r.err = fmt.Sprintf("pattern not found in %s — the source moved and this mutation is stale", m.File)
		return r
	}
	// Require a UNIQUE match. With several occurrences, "replace the first" is
	// a mutation whose location depends on file order, so the result cannot be
	// attributed to the site the mutation claims to be about.
	if r.occurrencesBefore > 1 {
		r.err = fmt.Sprintf("pattern occurs %d times in %s — ambiguous; narrow it to a unique site",
			r.occurrencesBefore, m.File)
		return r
	}
	mutated := strings.Replace(src, m.From, m.To, 1)
	if mutated == src {
		r.err = "replacement produced identical source"
		return r
	}
	if err := os.WriteFile(m.File, []byte(mutated), 0o644); err != nil {
		r.err = fmt.Sprintf("write: %v", err)
		return r
	}
	// Always restore, including on panic, or the tree is left mutated and every
	// later measurement in this process and the next is quietly wrong.
	defer os.WriteFile(m.File, orig, 0o644) //nolint:errcheck

	after, _ := os.ReadFile(m.File)
	r.occurrencesAfter = strings.Count(string(after), m.From)
	if r.occurrencesAfter >= r.occurrencesBefore {
		r.err = fmt.Sprintf("mutation did NOT land: %q still occurs %d times (was %d)",
			m.From, r.occurrencesAfter, r.occurrencesBefore)
		return r
	}
	fmt.Printf("  patch landed: %q %d -> %d occurrences\n", m.From, r.occurrencesBefore, r.occurrencesAfter)

	out, built, scenarios := runCorpus(target)
	r.built = built
	r.ranScenarios = scenarios
	if !built {
		r.err = "mutated tree did not build — the target ran zero tests, which is not a detection result"
		return r
	}
	r.failedAll = parseFailures(out)
	for _, n := range r.failedAll {
		if keep[n] {
			r.failedKept = append(r.failedKept, n)
		}
	}
	fmt.Printf("  scenarios run: %d, failed: %d (of which kept by prune: %d)\n",
		r.ranScenarios, len(r.failedAll), len(r.failedKept))
	return r
}

var (
	failRE  = regexp.MustCompile(`--- FAIL: TestFDB_FactoryCorpusFull/([^\s]+)`)
	countRE = regexp.MustCompile(`executing the full committed corpus: (\d+) scenarios`)
)

func runCorpus(target string) (output string, built bool, scenarios int) {
	cmd := exec.Command("bazelisk", "test", target,
		"--test_output=streamed", "--test_arg=--test.v",
		"--nocache_test_results", "--test_timeout=3600")
	b, _ := cmd.CombinedOutput()
	out := string(b)
	// Read the BUILD result separately from the test result. A mutation that
	// does not compile reports "Executed 0 out of 1", and a grep for failures
	// finds nothing in it — which is indistinguishable from a clean pass.
	built = !strings.Contains(out, "FAILED TO BUILD") &&
		!strings.Contains(out, "Executed 0 out of 1")
	if mm := countRE.FindStringSubmatch(out); mm != nil {
		fmt.Sscanf(mm[1], "%d", &scenarios)
	}
	return out, built, scenarios
}

func parseFailures(out string) []string {
	seen := map[string]bool{}
	for _, m := range failRE.FindAllStringSubmatch(out, -1) {
		seen[m[1]] = true
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func report(rs []result) {
	fmt.Println("\n================ MUTATION DETECTION ================")
	fmt.Printf("%-28s %-9s %8s %8s  %s\n", "mutation", "landed", "full", "pruned", "verdict")
	var lost, clean, broken int
	for _, r := range rs {
		if r.err != "" {
			fmt.Printf("%-28s %-9s %8s %8s  ERROR: %s\n", r.Name, "-", "-", "-", r.err)
			broken++
			continue
		}
		landed := fmt.Sprintf("%d->%d", r.occurrencesBefore, r.occurrencesAfter)
		verdict := "both detect"
		switch {
		case len(r.failedAll) == 0:
			// Neither corpus caught it. That is a statement about the CORPUS,
			// not about pruning, and it must not be scored as a pruning win.
			verdict = "NEITHER detects (corpus gap, not a prune result)"
		case len(r.failedKept) == 0:
			verdict = "PRUNE LOSES IT"
			lost++
		default:
			clean++
		}
		fmt.Printf("%-28s %-9s %8d %8d  %s\n", r.Name, landed, len(r.failedAll), len(r.failedKept), verdict)
	}
	fmt.Println("---------------------------------------------------")
	fmt.Printf("both detect: %d   prune loses: %d   harness errors: %d\n", clean, lost, broken)
	if broken > 0 {
		fmt.Println("NOTE: a harness error is NOT a passing result — those mutations proved nothing.")
	}
	if lost > 0 {
		os.Exit(1)
	}
}

func loadMutations(path string) ([]Mutation, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []Mutation
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("mutation file is empty")
	}
	return out, nil
}

func loadKeep(path string) (map[string]bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out[l] = true
		}
	}
	return out, nil
}
