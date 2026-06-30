// Command gen-sql-coverage regenerates the RFC-165 SQL conformance ledgers from
// the yamsql conformance corpus. Run from the repo root (e.g. via
// `just sql-coverage`).
//
//   - SQL_COVERAGE.md          (Ledger B) — measured corpus coverage, fully
//     derived by classifying every test case's declared outcome.
//   - SQL_ANSI_CONFORMANCE.md  (Ledger A) — the ANSI-standard scorecard; the
//     roster + Java? facts are hand-authored, Go?/completeness are derived from
//     `# ansi:` corpus tags (RFC-165 §4).
package main

import (
	"flag"
	"log"
	"os"

	"fdb.dev/pkg/relational/conformance/yamsql"
)

func main() {
	testdata := flag.String("testdata", "pkg/relational/conformance/yamsql/testdata", "directory of *.yaml conformance scenarios")
	coverageOut := flag.String("coverage-out", "SQL_COVERAGE.md", "output path for the measured coverage report (Ledger B)")
	ansiOut := flag.String("ansi-out", "SQL_ANSI_CONFORMANCE.md", "output path for the ANSI conformance scorecard (Ledger A)")
	flag.Parse()

	coverage, err := yamsql.GenerateCoverageReport(*testdata)
	if err != nil {
		log.Fatalf("generate coverage report: %v", err)
	}
	if err := os.WriteFile(*coverageOut, []byte(coverage), 0o644); err != nil {
		log.Fatalf("write %s: %v", *coverageOut, err)
	}
	log.Printf("wrote %s", *coverageOut)

	ansi, err := yamsql.GenerateAnsiLedger(*testdata)
	if err != nil {
		log.Fatalf("generate ANSI ledger: %v", err)
	}
	if err := os.WriteFile(*ansiOut, []byte(ansi), 0o644); err != nil {
		log.Fatalf("write %s: %v", *ansiOut, err)
	}
	log.Printf("wrote %s", *ansiOut)
}
