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
//     parser must refuse, 20 that must fail at execution and do, 9 whose
//     polarity Java defines only against a version it pins, 2 include-only
//     fragments. None of these measure the engine.
//   - `unsupported-DDL:*` and `unsupported:*` — capabilities a later RFC-201
//     phase adds, plus the two directives ruling 2 declines permanently.
//     `value-index-as-select` at 42 files is the single largest blocker.
//   - `engine-gap:*` and `conformance:*` — divergences this run FOUND, each
//     pinned to its exact rejection in gaps.go and each booked.
//
// The `queries` count is the number of result-consuming configs actually
// asserted against the engine. It is the honest denominator behind `pass`.
const pinnedLedger = "pass=33 fail=0 skip=205 queries=518 " +
	"file_skips{conformance:go-accepts-what-java-rejects=1," +
	"engine-gap:array-literal-values=5,engine-gap:cast-array-literal=1," +
	"engine-gap:catalog-system-tables=2,engine-gap:correlated-exists-setop=1," +
	"engine-gap:derived-table-join-on=1,engine-gap:error-class=1," +
	"engine-gap:inline-values-table=1,engine-gap:nested-recursive-with=1," +
	"engine-gap:planner-declines=1,engine-gap:returning-dry-run=1," +
	"engine-gap:row-version-pseudocolumn=1,engine-gap:table-valued-function=1," +
	"engine-gap:typed-integer-literal=1,fragment=2,plan-assertion=7," +
	"polarity:fixed-version-meta=9,polarity:negative-execution=20," +
	"polarity:negative-parse=25,unsupported-DDL:other=6,unsupported-DDL:struct=39," +
	"unsupported-DDL:value-index-as-select=42,unsupported:continuation=1," +
	"unsupported:multi-cluster=2,unsupported:result-metadata=7," +
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
	"unsupported-DDL:value-index-as-select=42,unsupported:check-cache=86," +
	"unsupported:continuation=16,unsupported:debugger=3," +
	"unsupported:multi-cluster=2,unsupported:prepared=115," +
	"unsupported:result-metadata=65,unsupported:schema-command=16," +
	"unsupported:temporary-function=191}"

// pinnedFileTotal closes the ledger: every corpus file lands in exactly one of
// pass / fail / skip. Asserting the sum separately means a file that vanished
// from the run fails with an obvious message instead of a 2,000-column diff.
const pinnedFileTotal = 238
