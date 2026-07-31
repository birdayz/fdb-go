package javacorpus_test

// pinnedLedger is the MEASURED outcome of running all 238 vendored corpus
// files against the Go engine.
//
// It is a measurement, not a target. RFC-201 §8 makes it the public statement
// of what is and is not supported: `pass` is the count of files whose every
// assertion held, `fail` must stay zero, and each skip class is a named
// specification gap with a size. A phase that closes a gap moves counts between
// classes and updates this line in the same commit; a count moving for any
// other reason is exactly the drift this pin exists to catch.
//
// Read it as three groups:
//
//   - `polarity:*` and `fragment` — the corpus's own meta-tests. 25 files the
//     parser must refuse, 25 that must fail at execution and do, 9 whose
//     polarity Java defines only against a version it pins, 2 include-only
//     fragments. None of these measure the engine.
//   - `unsupported-DDL:*` and `unsupported:*` — capabilities a later RFC-201
//     phase adds, plus the two directives ruling 2 declines permanently.
//     `value-index-as-select` at 42 files is the single largest blocker.
//   - `engine-gap:*` and `conformance:*` — divergences this run FOUND, each
//     pinned to its exact rejection in gaps.go and each booked.
//
// `queries` counts result-consuming configs actually asserted against the
// engine. It is the honest denominator behind `pass`, and it deliberately
// EXCLUDES `noChecks` queries: those execute but assert nothing, so counting
// them would let a file whose only query is config-less report a pass.
const pinnedLedger = "pass=32 fail=0 skip=206 queries=487 " +
	"file_skips{conformance:go-accepts-what-java-rejects=1," +
	"engine-gap:array-literal-values=5,engine-gap:cast-array-literal=1," +
	"engine-gap:catalog-system-tables=2,engine-gap:correlated-exists-setop=1," +
	"engine-gap:derived-table-join-on=1,engine-gap:error-class=1," +
	"engine-gap:inline-values-table=1,engine-gap:nested-recursive-with=1," +
	"engine-gap:planner-declines=1,engine-gap:returning-dry-run=1," +
	"engine-gap:row-version-pseudocolumn=1,engine-gap:table-valued-function=1," +
	"engine-gap:typed-integer-literal=1,fragment=2,no-checks=1,plan-assertion=7," +
	"polarity:fixed-version-meta=9,polarity:negative-execution=25," +
	"polarity:negative-parse=25,unsupported-DDL:other=6,unsupported-DDL:struct=39," +
	"unsupported-DDL:value-index-as-select=42,unsupported:continuation=3," +
	"unsupported:multi-cluster=2," +
	"unsupported:schema-command=8,unsupported:temporary-function=17," +
	"vacuous:all-assertions-skipped=1} " +
	"inner_skips{conformance:go-accepts-what-java-rejects=1," +
	"engine-gap:array-literal-values=5,engine-gap:cast-array-literal=1," +
	"engine-gap:catalog-system-tables=2,engine-gap:correlated-exists-setop=1," +
	"engine-gap:derived-table-join-on=1,engine-gap:error-class=1," +
	"engine-gap:inline-values-table=1,engine-gap:nested-recursive-with=1," +
	"engine-gap:planner-declines=1,engine-gap:returning-dry-run=1," +
	"engine-gap:row-version-pseudocolumn=1,engine-gap:table-valued-function=1," +
	"engine-gap:typed-integer-literal=1,no-checks=8,plan-assertion=212," +
	"unsupported-DDL:other=6,unsupported-DDL:struct=39," +
	"unsupported-DDL:value-index-as-select=42,unsupported:check-cache=94," +
	"unsupported:continuation=16,unsupported:debugger=3," +
	"unsupported:multi-cluster=2,unsupported:prepared=124," +
	"unsupported:schema-command=16," +
	"unsupported:temporary-function=191}"

// pinnedFileTotal closes the ledger: every corpus file lands in exactly one of
// pass / fail / skip. Asserting the sum separately means a file that vanished
// from the run fails with an obvious message instead of a 2,000-column diff.
const pinnedFileTotal = 238

// pinnedAssignmentDigest is sha256 over the sorted `path status class` lines.
//
// It exists because the counts above are blind to a SWAP: two files trading
// classes leaves every total identical, so the census stays green while the
// corpus's meaning changes underneath it. The digest is deliberately opaque —
// on mismatch the test dumps the full assignment, which is the artefact worth
// diffing.
const pinnedAssignmentDigest = "0d1f15ab4266e4e9a8a373eea2cbdba7460e749d2b61bf42c98f259365681de8"
