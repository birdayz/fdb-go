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
        Diff two baselines entry-by-entry. Exits 1 when they differ.
`)
}

func dump(args []string) {
	fs := flag.NewFlagSet("dump", flag.ExitOnError)
	testdata := fs.String("testdata", defaultCorpus, "directory of *.yaml conformance scenarios")
	out := fs.String("out", "", "output path (default: stdout)")
	if err := fs.Parse(args); err != nil {
		log.Fatal(err)
	}

	baseline, st, err := explaindiff.GenerateBaseline(*testdata)
	if err != nil {
		log.Fatalf("generate baseline: %v", err)
	}
	if *out == "" {
		fmt.Print(baseline)
		return
	}
	if err := os.WriteFile(*out, []byte(baseline), 0o644); err != nil {
		log.Fatalf("write %s: %v", *out, err)
	}
	log.Printf("wrote %s — %d files, %d queries planned, %d plan errors (%d without a corpus error pin), %d non-query stanzas skipped",
		*out, st.Files, st.Queries, st.PlanErrors, st.UnexpectedErrors, st.NonQuery)
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

	oldEntries, err := explaindiff.LoadBaseline(fs.Arg(0))
	if err != nil {
		log.Fatal(err)
	}
	newEntries, err := explaindiff.LoadBaseline(fs.Arg(1))
	if err != nil {
		log.Fatal(err)
	}

	rep := explaindiff.Diff(oldEntries, newEntries)
	fmt.Print(explaindiff.RenderDiff(rep))
	if !rep.Clean() {
		os.Exit(1)
	}
}
