package javacorpus_test

// pinnedLedger is the MEASURED outcome of running all 240 vendored corpus
// files against the Go engine.
//
// It is a measurement, not a target. RFC-201 §8 makes it the public statement
// of what is and is not supported: `pass` is the count of files whose every
// assertion held, `fail` must stay zero, and each skip class is a named
// specification gap with a size. A phase that closes a gap moves counts between
// classes and updates this line in the same commit; a count moving for any
// other reason is exactly the drift this pin exists to catch.
const pinnedLedger = "pass=105 fail=0 skip=136 queries=4664 file_skips{conformance:go-accepts-what-java-rejects=8,conformance:java-planner-bug=1,conformance:scan-choice-order=1,engine-gap:case-sensitive-identifiers=2,engine-gap:catalog-system-tables=2,engine-gap:comma-join-mixed-from=1,engine-gap:correlated-exists-setop=1,engine-gap:dml-returning-result-set=3,engine-gap:error-class=4,engine-gap:nested-recursive-with=2,engine-gap:planner-declines=7,engine-gap:returning-dry-run=1,engine-gap:star-group-by-expansion=1,engine-gap:struct-query=1,engine-gap:table-valued-function=1,fragment=2,no-checks=1,plan-assertion=3,polarity:fixed-version-meta=9,polarity:negative-execution=31,polarity:negative-parse=25,unsupported-DDL:function=1,unsupported-DDL:other=1,unsupported:continuation=3,unsupported:multi-cluster=2,unsupported:result-metadata-nested=6,unsupported:schema-command=8,vacuous:all-assertions-skipped=8} inner_skips{conformance:go-accepts-what-java-rejects=8,conformance:java-planner-bug=1,conformance:scan-choice-order=1,engine-gap:case-sensitive-identifiers=2,engine-gap:catalog-system-tables=2,engine-gap:comma-join-mixed-from=1,engine-gap:correlated-exists-setop=1,engine-gap:dml-returning-result-set=3,engine-gap:error-class=4,engine-gap:nested-recursive-with=2,engine-gap:planner-declines=7,engine-gap:returning-dry-run=1,engine-gap:star-group-by-expansion=1,engine-gap:struct-query=1,engine-gap:table-valued-function=1,no-checks=8,plan-assertion=2042,polarity:negative-execution=31,unsupported-DDL:function=1,unsupported-DDL:other=1,unsupported:check-cache=197,unsupported:continuation=51,unsupported:debugger=3,unsupported:multi-cluster=2,unsupported:prepared=290,unsupported:random-injection=25,unsupported:result-metadata-nested=191,unsupported:schema-command=16}"

// pinnedFileTotal closes the ledger: every corpus file lands in exactly one of
// pass / fail / skip. Asserting the sum separately means a file that vanished
// from the run fails with an obvious message instead of a 2,000-column diff.
const pinnedFileTotal = 241

// pinnedAssignmentDigest is sha256 over the sorted `path status class` lines.
const pinnedAssignmentDigest = "210ae62947c0c2131f59bf3861c75d8b4ca72b81aa0aacd2c1024261ed9a801a"
