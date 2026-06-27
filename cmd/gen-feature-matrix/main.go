// Command gen-feature-matrix regenerates FEATURE_MATRIX.md from the yamsql
// conformance corpus. Run from the repo root (e.g. via `just feature-matrix`).
package main

import (
	"flag"
	"log"
	"os"

	"fdb.dev/pkg/relational/conformance/yamsql"
)

func main() {
	testdata := flag.String("testdata", "pkg/relational/conformance/yamsql/testdata", "directory of *.yaml conformance scenarios")
	out := flag.String("out", "FEATURE_MATRIX.md", "output markdown path")
	flag.Parse()

	md, err := yamsql.GenerateFeatureMatrix(*testdata)
	if err != nil {
		log.Fatalf("generate feature matrix: %v", err)
	}
	if err := os.WriteFile(*out, []byte(md), 0o644); err != nil {
		log.Fatalf("write %s: %v", *out, err)
	}
	log.Printf("wrote %s", *out)
}
