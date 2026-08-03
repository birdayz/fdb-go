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
// The committed format is GENUINE `.yamsql` (RFC-201 §5.7, owner ruling
// 2026-08-01): one file per feature family (FamilyOf), each holding that
// family's scenarios as (schema_template, setup, test_block) document triples
// in the Java yaml-tests dialect — the same convention the vendored corpus
// uses (cte.yamsql holds the CTE tests). Provenance lives in comments, the one
// extension yamsql tolerates freely, so Java's own yaml-tests runner can
// execute a committed file verbatim; that is what makes cross-engine blessing
// literal rather than aspirational.
//
// The package therefore holds six things and no more:
//
//   - the FAMILY mapping (family.go): feature vector → family key → file
//     name, the grouping convention the loader cross-checks per scenario;
//   - the WRITER (writer.go), which emits a family file in a canonical form
//     that survives a Load/MarshalFamily round trip byte for byte, so a
//     re-bless produces a reviewable diff instead of reformatting noise;
//   - the HEADER (header.go), the §5.1 provenance contract every committed
//     scenario carries — generator version, seed, blessing oracle, feature
//     vector, plan shape, date — parseable without executing anything;
//   - the LOADER (load.go), which owns provenance and the provenance↔document
//     cross-checks over its OWN flat testdata/ directory, and delegates the
//     yamsql structure to javayamsql.Parse — the strict parser gating the
//     vendored Java corpus — so the factory corpus cannot drift from the
//     shared format without the build going red. Execution (run.go) delegates
//     to javacorpus.RunParsed for the same reason: one runner, not a fork;
//   - the CENSUS and RATCHET (census.go), the governance instrument of
//     RFC-201 §8: scenario count, test count, per-feature-vector counts and
//     the per-dedup-key blessing, computed from the files and gated so they
//     can only go up; and
//   - the RETIREMENT LEDGER (retirement.go and retirements/), the sole explicit
//     exception for a planner change that makes an old physical plan point
//     cease to exist. It binds the reason, date, RFC, exact base Git commit,
//     logical census, and full filename-plus-content tree before and after. CI
//     proves that commit is on the trusted target history and materializes it
//     to authenticate the old hashes; it authenticates the new hashes at the
//     ledger's unique first-add commit and, while newly proposed, against raw
//     proposed HEAD too. The checkout must exactly match those raw corpus and
//     ledger blobs, so Git filters cannot rewrite evidence. The new ledger's
//     BEFORE must equal the trusted target corpus, and every trusted scenario
//     remains byte-identical unless
//     that ledger authorizes the full transition; pure additions need no
//     exception. Later corpus growth therefore does not weaken or invalidate
//     older ledgers, and neither a synthetic/invented old side, nested corpus
//     file, balanced delete+add, nor unrelated edit can hitchhike on a reviewed
//     decrease.
//
// The producer lives in the sibling `factory` package and in
// `cmd/factory-run`; the split is what lets the corpus run with no dependency
// on the generator that wrote it.
package factorycorpus
