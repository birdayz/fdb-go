// Command dst-generate runs metamorphic equivalence scenarios (hand-written or LLM-generated)
// through the deterministic SQL engine over SimFDB and reports findings. It is the "judge" half of
// the LLM-adversarial loop (RFC-179 Tier 2): an LLM proposes scenarios — schemas, data, and groups
// of queries it asserts are equivalent — as JSON; this tool executes them and reports where the
// engine returns DIFFERENT rows for equivalent queries.
//
//	go build -o /tmp/dst-generate ./cmd/dst-generate
//	/tmp/dst-generate -dir ./generated/     # check a directory of *.json scenarios
//	/tmp/dst-generate                        # check the hand-written seed corpus
//
// Findings are split: INEQUIVALENCE (the engine disagreed with itself on two equivalent queries —
// a REAL finding: engine bug OR a wrong LLM equivalence claim, triage it) vs ERRORED (a query the
// engine could not plan — usually the LLM emitting unsupported SQL, low signal).
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"fdb.dev/pkg/simfdb/hunt/metamorphic"
)

func main() {
	dir := flag.String("dir", "", "directory of scenario JSON files (empty = hand-written seed corpus)")
	flag.Parse()

	scenarios := metamorphic.SeedCorpus()
	if *dir != "" {
		loaded, err := metamorphic.LoadDir(*dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "dst-generate: load %s: %v\n", *dir, err)
			os.Exit(2)
		}
		scenarios = loaded
	}

	var groups, setupErrs, inequiv, errored int
	var findings []metamorphic.Violation
	for _, s := range scenarios {
		groups += len(s.Groups)
		viols, err := metamorphic.Check(s)
		if err != nil {
			setupErrs++
			fmt.Printf("SETUP-ERR  %s: %v\n", s.Name, err)
			continue
		}
		for _, v := range viols {
			if strings.Contains(v.Detail, "NOT equivalent") {
				inequiv++
				findings = append(findings, v)
			} else {
				errored++
			}
		}
	}

	// The real signal first.
	for _, v := range findings {
		fmt.Printf("\n*** INEQUIVALENCE *** %s\n", v)
	}
	fmt.Printf("\nsummary: %d scenarios, %d groups | %d INEQUIVALENCE (real) | %d errored (unsupported SQL) | %d setup-err\n",
		len(scenarios), groups, inequiv, errored, setupErrs)
	if inequiv > 0 {
		fmt.Println("→ triage each INEQUIVALENCE: engine bug, or a wrong equivalence claim?")
		os.Exit(1)
	}
}
