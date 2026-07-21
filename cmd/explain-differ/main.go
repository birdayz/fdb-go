// Command explain-differ dumps the planned PHYSICAL plan shape of every query
// in the yamsql conformance corpus to a stable text baseline, and diffs two
// such baselines.
//
// RFC-183 §6 deliverable: removing the nil-inner shell plans changes plan
// hashes and costs, which can silently flip which plan wins. A flipped-but-
// still-correct plan passes every row-level test, so the corpus-wide plan
// diff is the only check that can see it.
//
// Typical use — before/after a planner change:
//
//	git stash                                              # or a worktree on master
//	go run ./cmd/explain-differ dump -out /tmp/before.txt
//	git stash pop
//	go run ./cmd/explain-differ dump -out /tmp/after.txt
//	go run ./cmd/explain-differ diff /tmp/before.txt /tmp/after.txt
//
// `diff` exits 1 when the baselines differ, so it drops straight into CI or
// a pre-merge gate.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/relational/conformance/explaindiff"
)

const defaultCorpus = "pkg/relational/conformance/yamsql/testdata"

func main() {
	log.SetFlags(0)
	log.SetPrefix("explain-differ: ")

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "dump":
		dump(os.Args[2:])
	case "diff":
		diff(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		log.Printf("unknown subcommand %q", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  explain-differ dump [-testdata DIR] [-out FILE]
        Plan every query in the yamsql corpus and write the baseline.
        -out defaults to stdout. No FDB required.

  explain-differ diff OLD NEW
        Diff two baselines entry-by-entry.
        Exit 0 = clean, 1 = the baselines differ, 2 = unusable input
        (same file twice, empty/truncated baseline, stale format version).
`)
}

func dump(args []string) {
	fs := flag.NewFlagSet("dump", flag.ExitOnError)
	testdata := fs.String("testdata", defaultCorpus, "directory of *.yaml conformance scenarios")
	out := fs.String("out", "", "output path (default: stdout)")
	if err := fs.Parse(args); err != nil {
		log.Fatal(err)
	}

	// RFC-183 plan-reachability tally over the whole corpus. The differ is the
	// only harness that plans every corpus query without FDB, which makes it
	// the natural place to measure — but the two answer different questions and
	// must not be confused: the baseline compares EXTRACTED PLANS, so it stays
	// green through reachability defects by construction (extraction reads the
	// plan and never consults the memo).
	//
	// The collector is constructed HERE and handed down, rather than switched
	// on through package state inside cascades. Either env var arms it; nil
	// means the planner accumulates nothing.
	reachOut := os.Getenv("RFC183_REACHABILITY_OUT")
	var reach *cascades.ReachabilityCollector
	if reachOut != "" || os.Getenv("RFC183_REACHABILITY") != "" {
		reach = cascades.NewReachabilityCollector()
	}

	baseline, st, err := explaindiff.GenerateBaselineWithReachability(*testdata, reach)
	if err != nil {
		log.Fatalf("generate baseline: %v", err)
	}
	if reachOut != "" {
		if err := os.WriteFile(reachOut, []byte(reach.Report(40)), 0o644); err != nil {
			log.Fatalf("write reachability report %s: %v", reachOut, err)
		}
		fmt.Fprintf(os.Stderr, "explain-differ: reachability -> %s (%d unreachable)\n",
			reachOut, reach.Count())
	}
	if *out == "" {
		fmt.Print(baseline)
		return
	}
	if err := os.WriteFile(*out, []byte(baseline), 0o644); err != nil {
		log.Fatalf("write %s: %v", *out, err)
	}
	log.Printf("wrote %s — %d files, %d queries + %d DML planned, %d plan errors (%d without a corpus error pin), %d non-query stanzas skipped",
		*out, st.Files, st.Queries, st.DML, st.PlanErrors, st.UnexpectedErrors, st.NonQuery)
}

func diff(args []string) {
	fs := flag.NewFlagSet("diff", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		log.Fatal(err)
	}
	if fs.NArg() != 2 {
		log.Print("diff needs exactly two baseline files")
		usage()
		os.Exit(2)
	}

	oldB, err := explaindiff.LoadBaseline(fs.Arg(0))
	if err != nil {
		log.Print(err)
		os.Exit(2)
	}
	newB, err := explaindiff.LoadBaseline(fs.Arg(1))
	if err != nil {
		log.Print(err)
		os.Exit(2)
	}
	// Exit 2, not 1: an unusable input pair is an operator error, distinct
	// from "the baselines legitimately differ". A CI gate that treats every
	// non-zero exit the same still stops; one that reads the code can tell
	// a real drift from a broken invocation.
	if err := explaindiff.ValidateDiffInputs(oldB, newB); err != nil {
		log.Print(err)
		os.Exit(2)
	}

	rep := explaindiff.Diff(oldB.Entries, newB.Entries)
	fmt.Print(explaindiff.RenderDiff(rep))
	if !rep.Clean() {
		os.Exit(1)
	}
}
