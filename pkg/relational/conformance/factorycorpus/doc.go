// Package factorycorpus is the home of the RFC-201 §5 generation factory's
// COMMITTED output: machine-generated, oracle-blessed yamsql scenarios that
// live in the repository as permanent suite content.
//
// The distinction from every other generative harness here is that nothing in
// this package generates anything. rowdiff, the fuzz targets and the chaos
// suite all re-derive their cases on every run; if their generator breaks, the
// coverage silently evaporates. A file under testdata/ is a frozen expectation:
// it keeps testing the engine even if the generator, the oracle infrastructure
// and the Java server are all broken or gone, it bisects with git, and it
// converts oracle agreement — a moment-in-time fact — into a regression pin, a
// permanent one.
//
// The package therefore holds four things and no more:
//
//   - the WRITER (writer.go), which emits a scenario in a canonical yamsql
//     form that survives a Load/Marshal round trip byte for byte, so a
//     re-bless produces a reviewable diff instead of reformatting noise;
//   - the HEADER (header.go), the §5.1 provenance contract every generated
//     file carries — generator version, seed, blessing oracle, feature vector,
//     plan shape, date — parseable without executing anything;
//   - the LOADER (load.go), which is deliberately its OWN loader over its OWN
//     flat testdata/ directory. Six places glob one level of
//     `pkg/relational/conformance/yamsql/testdata` — the runner
//     (yamsql/runner_test.go), SQL_COVERAGE.md (yamsql/coverage.go),
//     FEATURE_MATRIX.md (yamsql/featurematrix.go), the ANSI ledger twice
//     (yamsql/ansiledger.go: the evidence collector and the tagged-case
//     lister) and the EXPLAIN baseline (explaindiff/explaindiff.go) — and a
//     factory batch must not silently land in the documents
//     those ledgers generate — machine volume would drown the hand-authored
//     evidence they exist to report. Validation is NOT re-implemented: the
//     loader delegates to yamsql.Load, inheriting its strict KnownFields
//     decoding and every assertion-cannot-be-silently-dropped check;
//   - the CENSUS and RATCHET (census.go), the governance instrument of
//     RFC-201 §8: scenario count, test count and per-feature-vector counts,
//     computed from the files and gated so they can only go up.
//
// The producer lives in the sibling `factory` package and in
// `cmd/factory-run`; the split is what lets the corpus run with no dependency
// on the generator that wrote it.
package factorycorpus
