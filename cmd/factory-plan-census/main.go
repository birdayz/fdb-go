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
	"os"
	"regexp"
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
				fmt.Fprintf(out, "%s\tPLAN-ERROR\n", s.Header.Name)
				continue
			}
			fmt.Fprintf(out, "%s\t%s\n", s.Header.Name, plan.Explain())
		}
	}
	return nil
}

// eqIndexScan matches an index scan whose FIRST bound is an equality — a probe,
// as opposed to an unbounded or one-sided range.
var eqIndexScan = regexp.MustCompile(`IndexScan\([A-Z0-9_]+, \[=`)

var anyIndexScan = regexp.MustCompile(`IndexScan\(`)

// fullScan matches an UNBOUNDED primary scan. Scan(T, [=]) is a primary-key
// probe and is deliberately excluded: replacing a secondary-index probe with a
// PK probe is an improvement, not a regression.
var fullScan = regexp.MustCompile(`Scan\([A-Z0-9_]+\)`)

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
	if len(before) == 0 || len(after) == 0 {
		fmt.Fprintln(os.Stderr, "factory-plan-census: an empty dump proves nothing; refusing to report a verdict")
		return 2
	}

	var moved int
	var lostEqProbe, lostAnyIndex, gainedFullScan, newPlanError []string
	for name, b := range before {
		a, ok := after[name]
		if !ok || a == b {
			continue
		}
		moved++
		if a == "PLAN-ERROR" && b != "PLAN-ERROR" {
			newPlanError = append(newPlanError, name)
		}
		if eqIndexScan.MatchString(b) && !eqIndexScan.MatchString(a) {
			lostEqProbe = append(lostEqProbe, name)
		}
		if anyIndexScan.MatchString(b) && !anyIndexScan.MatchString(a) {
			lostAnyIndex = append(lostAnyIndex, name)
		}
		if !fullScan.MatchString(b) && fullScan.MatchString(a) {
			gainedFullScan = append(gainedFullScan, name)
		}
	}

	fmt.Printf("scenarios compared: %d\nplans moved:        %d\n\n", len(before), moved)
	fmt.Printf("  lost an EQUALITY index probe:   %d\n", len(lostEqProbe))
	fmt.Printf("  lost ALL index access:          %d\n", len(lostAnyIndex))
	fmt.Printf("  gained an UNBOUNDED full scan:  %d\n", len(gainedFullScan))
	fmt.Printf("  newly unplannable:              %d\n", len(newPlanError))

	fail := len(lostEqProbe) > 0 || len(newPlanError) > 0
	if !fail {
		fmt.Println("\nno regression class present; the movement is representation-only")
		return 0
	}
	fmt.Printf("\nREGRESSION: %d scenario(s) lost an equality index probe and %d became unplannable.\n",
		len(lostEqProbe), len(newPlanError))
	fmt.Println("The rows are unchanged, so no row-based test can see this — that is why it")
	fmt.Println("is checked here. Do NOT re-bless the corpus to make the drift go away; the")
	fmt.Println("digests are the only record that these plans were once index probes.")
	for i, n := range lostEqProbe {
		if i >= 10 {
			fmt.Printf("  ... and %d more\n", len(lostEqProbe)-10)
			break
		}
		fmt.Printf("  %s\n    before: %s\n    after:  %s\n", n, before[n], after[n])
	}
	return 1
}
