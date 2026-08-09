// Command factory-plan-census makes a factory-corpus plan-shape drift READABLE,
// and fails loudly on the drift classes that are regressions.
//
// The committed corpus records a plan-shape DIGEST, not a plan. That is enough
// to detect that something moved and useless for deciding whether the movement
// was an improvement, so a drift arrives as two hex strings and the only cheap
// response is to re-bless it. RETIREMENT_LEDGER.md states the cost of doing
// that ("a re-bless erases its own motive"), and this command is what makes the
// expensive response cheap instead.
//
// It was written after a re-bless that would have absorbed 183 scenarios losing
// a correlated equality index probe on the inner side of a nested-loop join —
// an O(N)-per-outer-row regression that every row-based test passes, because
// the rows are unchanged. Nothing in the suite could see it, and the digest
// drift looked exactly like the 3521 benign movements around it.
//
// TWO MODES.
//
//	dump    — one `<scenario>\t<plan>` line per committed scenario. Run it in a
//	          worktree at the base commit and again on the branch.
//	classify — read two dumps, partition the movement, and report NAMED COUNTS.
//
//	go run ./cmd/factory-plan-census dump -corpus <dir>            > before.txt
//	go run ./cmd/factory-plan-census classify before.txt after.txt
//
// classify EXITS NON-ZERO when a scenario loses an equality index probe. That
// is the regression class: an unbounded-range index scan replaced by a full
// scan reads the same rows with less indirection and is a legitimate cost
// choice, while an equality probe replaced by a full scan is not — and inside a
// join inner it is quadratic. Fetch-wrapper removal and COVERING acquisition are
// reported but never fail, being representation changes.
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"

	"fdb.dev/pkg/relational/conformance/factory"
	"fdb.dev/pkg/relational/conformance/factorycorpus"
	"fdb.dev/pkg/relational/core/embedded"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "dump":
		fs := os.Args[2:]
		corpus := ""
		for i := 0; i+1 < len(fs); i++ {
			if fs[i] == "-corpus" {
				corpus = fs[i+1]
			}
		}
		if corpus == "" {
			usage()
		}
		if err := dump(corpus); err != nil {
			fmt.Fprintln(os.Stderr, "factory-plan-census:", err)
			os.Exit(2)
		}
	case "classify":
		if len(os.Args) != 4 {
			usage()
		}
		os.Exit(classify(os.Args[2], os.Args[3]))
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:\n  factory-plan-census dump -corpus <dir>\n  factory-plan-census classify <before.txt> <after.txt>")
	os.Exit(2)
}

// dump plans every committed scenario through the SAME entry point the corpus's
// plan-shape digest is derived from (factory-rebless-plan-shapes uses
// TLPQueries()[1]), so a line here and a digest there describe one plan.
func dump(corpusDir string) error {
	files, err := factorycorpus.LoadDir(corpusDir)
	if err != nil {
		return err
	}
	bySeed := map[uint64][]*factorycorpus.Scenario{}
	var order []uint64
	for _, f := range files {
		s := f.Header.Seed
		if _, ok := bySeed[s]; !ok {
			order = append(order, s)
		}
		bySeed[s] = append(bySeed[s], f)
	}
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	for _, seed := range order {
		cands := factory.Candidates(seed)
		for _, s := range bySeed[seed] {
			var cand *factory.Candidate
			for i := range cands {
				if cands[i].QueryIndex == s.Header.QueryIndex && cands[i].ProjIndex == s.Header.Projection {
					cand = &cands[i]
					break
				}
			}
			if cand == nil {
				continue
			}
			queries := cand.TLPQueries()
			if len(queries) < 2 {
				continue
			}
			plan, perr := embedded.PlanPhysicalForTest(
				cand.Case.SQL(queries[1], cand.Projection), cand.Case.DDL(), nil)
			if perr != nil {
				fmt.Fprintf(out, "%s\t%s\n", s.Header.Name, planError)
				continue
			}
			fmt.Fprintf(out, "%s\t%s\n", s.Header.Name, plan.Explain())
		}
	}
	return nil
}

// planError is the dump line body for a scenario that failed to plan. It is
// written by dump and read back by the census, so it is one constant.
const planError = "PLAN-ERROR"

// eqIndexScan matches an index scan whose FIRST bound is an equality — a probe,
// as opposed to an unbounded or one-sided range.
var eqIndexScan = regexp.MustCompile(`IndexScan\([A-Z0-9_]+, \[=`)

var anyIndexScan = regexp.MustCompile(`IndexScan\(`)

// fullScan matches an UNBOUNDED primary scan. Scan(T, [=]) is a primary-key
// probe and is deliberately excluded: replacing a secondary-index probe with a
// PK probe is an improvement, not a regression.
var fullScan = regexp.MustCompile(`Scan\([A-Z0-9_]+\)`)

// census is the classified movement between two dumps. It is produced by a
// PURE function over two explicit maps so every arm can be driven directly: a
// full-corpus run exercises only the arms the corpus happens to reach, and an
// arm that is rare today reads as a FINDING the first time it fires rather than
// as the untested branch it actually is.
type census struct {
	compared int
	moved    int

	// The two REGRESSION classes. Their steady state is zero and the alarm is
	// GROWTH above zero — see exitCode.
	lostEqProbe  []string
	newPlanError []string

	// The two REPRESENTATION classes. These are reported and never fail:
	// an unbounded-range index scan replaced by a full scan reads the same
	// rows with less indirection and is a legitimate cost choice.
	lostAnyIndex   []string
	gainedFullScan []string
}

// refusal is a population problem, not a verdict. The census must separate
// three states — passed, failed, and NEVER RAN — because collapsing the third
// into the first fails OPEN, and an empty or truncated dump is exactly the
// input that renders "never ran" as "no regression class present".
type refusal struct{ msg string }

func (r *refusal) Error() string { return "refusing to report a verdict: " + r.msg }

// checkPopulation rejects every dump pair whose examined population cannot
// support a verdict. Each arm names the direction of its alarm, because the
// expected value differs per arm: a dump is expected to be the FULL corpus, so
// the alarm on the compared population is SHRINKAGE, whereas the alarm on the
// regression classes is growth.
func checkPopulation(before, after map[string]string) error {
	if len(before) == 0 {
		return &refusal{"the BEFORE dump is empty; an empty population cannot distinguish 'every plan is clean' from 'nothing was planned'. Expected population is the full corpus; the alarm here is SHRINKAGE"}
	}
	if len(after) == 0 {
		return &refusal{"the AFTER dump is empty; an empty population cannot distinguish 'every plan is clean' from 'nothing was planned'. Expected population is the full corpus; the alarm here is SHRINKAGE"}
	}
	// Both dumps are produced from the SAME committed corpus, one worktree at
	// the base commit and one on the branch, so the two name sets are expected
	// to be IDENTICAL. Any asymmetry is an artifact of how the dumps were made
	// — a truncated redirect, a racing writer, two different corpus dirs — and
	// it is never a signal about plans. The two directions are checked
	// SEPARATELY because they fail differently and a guard covering one is not
	// a guard covering the other.
	onlyBefore := missingFrom(before, after)
	onlyAfter := missingFrom(after, before)
	if len(onlyBefore) == len(before) && len(onlyAfter) == len(after) {
		return &refusal{fmt.Sprintf(
			"the two dumps share NO scenario name (%d before, %d after); zero scenarios would be compared, and a zero examined population prints the same green as a clean corpus. Expected population is the full corpus; the alarm here is SHRINKAGE",
			len(before), len(after))}
	}
	if len(onlyBefore) > 0 {
		return &refusal{fmt.Sprintf(
			"the AFTER dump is missing %d of %d BEFORE scenarios (first: %s); those scenarios would be SKIPPED, so a regression hiding among them never surfaces and the run prints a green over a silently smaller population. Expected population is the full corpus; the alarm here is SHRINKAGE",
			len(onlyBefore), len(before), onlyBefore[0])}
	}
	if len(onlyAfter) > 0 {
		return &refusal{fmt.Sprintf(
			"the BEFORE dump is missing %d of %d AFTER scenarios (first: %s); the two dumps do not describe the same corpus, so the movement between them is not attributable to the branch. Expected population is the full corpus; the alarm here is SHRINKAGE on the before side",
			len(onlyAfter), len(after), onlyAfter[0])}
	}
	return nil
}

// missingFrom returns the sorted names present in a but absent from b.
func missingFrom(a, b map[string]string) []string {
	var out []string
	for name := range a {
		if _, ok := b[name]; !ok {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// takeCensus classifies the movement. A scenario absent from after cannot
// reach this — checkPopulation has already refused — so `compared` is the whole
// BEFORE population and no arm is computed over a partial one.
func takeCensus(before, after map[string]string) (*census, error) {
	if err := checkPopulation(before, after); err != nil {
		return nil, err
	}
	c := &census{}
	for name, b := range before {
		a := after[name]
		c.compared++
		if a == b {
			continue
		}
		c.moved++
		if a == planError && b != planError {
			c.newPlanError = append(c.newPlanError, name)
		}
		if eqIndexScan.MatchString(b) && !eqIndexScan.MatchString(a) {
			c.lostEqProbe = append(c.lostEqProbe, name)
		}
		if anyIndexScan.MatchString(b) && !anyIndexScan.MatchString(a) {
			c.lostAnyIndex = append(c.lostAnyIndex, name)
		}
		if !fullScan.MatchString(b) && fullScan.MatchString(a) {
			c.gainedFullScan = append(c.gainedFullScan, name)
		}
	}
	for _, s := range [][]string{c.lostEqProbe, c.lostAnyIndex, c.gainedFullScan, c.newPlanError} {
		sort.Strings(s)
	}
	return c, nil
}

// exitCode is the whole verdict, isolated so every class can be driven through
// it. Only the two regression classes fail; the representation classes are
// reported and return 0.
func (c *census) exitCode() int {
	if len(c.lostEqProbe) > 0 || len(c.newPlanError) > 0 {
		return 1
	}
	return 0
}

func (c *census) report(w io.Writer, before, after map[string]string) {
	fmt.Fprintf(w, "scenarios compared: %d\nplans moved:        %d\n\n", c.compared, c.moved)
	fmt.Fprintf(w, "  lost an EQUALITY index probe:   %d\n", len(c.lostEqProbe))
	fmt.Fprintf(w, "  lost ALL index access:          %d\n", len(c.lostAnyIndex))
	fmt.Fprintf(w, "  gained an UNBOUNDED full scan:  %d\n", len(c.gainedFullScan))
	fmt.Fprintf(w, "  newly unplannable:              %d\n", len(c.newPlanError))

	if c.exitCode() == 0 {
		fmt.Fprintln(w, "\nno regression class present; the movement is representation-only")
		return
	}
	fmt.Fprintf(w, "\nREGRESSION: %d scenario(s) lost an equality index probe and %d became unplannable.\n",
		len(c.lostEqProbe), len(c.newPlanError))
	fmt.Fprintln(w, "Both classes are expected to be ZERO in steady state, so the alarm here is")
	fmt.Fprintln(w, "GROWTH above zero, not shrinkage.")
	fmt.Fprintln(w, "The rows are unchanged, so no row-based test can see this — that is why it")
	fmt.Fprintln(w, "is checked here. Do NOT re-bless the corpus to make the drift go away; the")
	fmt.Fprintln(w, "digests are the only record that these plans were once index probes.")
	for i, n := range c.lostEqProbe {
		if i >= 10 {
			fmt.Fprintf(w, "  ... and %d more\n", len(c.lostEqProbe)-10)
			break
		}
		fmt.Fprintf(w, "  %s\n    before: %s\n    after:  %s\n", n, before[n], after[n])
	}
}

func load(p string) (map[string]string, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	m := map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		parts := strings.SplitN(sc.Text(), "\t", 2)
		if len(parts) == 2 {
			m[parts[0]] = parts[1]
		}
	}
	return m, sc.Err()
}

func classify(beforePath, afterPath string) int {
	before, err := load(beforePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "factory-plan-census:", err)
		return 2
	}
	after, err := load(afterPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "factory-plan-census:", err)
		return 2
	}
	c, err := takeCensus(before, after)
	if err != nil {
		fmt.Fprintln(os.Stderr, "factory-plan-census:", err)
		return 2
	}
	c.report(os.Stdout, before, after)
	return c.exitCode()
}
