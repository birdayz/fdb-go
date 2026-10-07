package javacorpus_test

// pinnedLedger is the MEASURED outcome of running all 255 vendored corpus
// files against the Go engine.
//
// It is a measurement, not a target. RFC-201 §8 makes it the public statement
// of what is and is not supported: `pass` is the count of files whose every
// assertion held, `fail` must stay zero, and each skip class is a named
// specification gap with a size. A phase that closes a gap moves counts between
// classes and updates this line in the same commit; a count moving for any
// other reason is exactly the drift this pin exists to catch.
const pinnedLedger = "pass=140 fail=0 skip=115 queries=8790 file_skips{conformance:go-accepts-what-java-rejects=14,conformance:java-disabled=1,conformance:java-planner-bug=2,engine-gap:case-sensitive-identifiers=3,engine-gap:catalog-system-tables=3,engine-gap:comma-join-mixed-from=1,engine-gap:error-class=3,engine-gap:nested-recursive-with=2,engine-gap:star-group-by-expansion=1,fragment=2,plan-assertion=4,polarity:fixed-version-meta=11,polarity:negative-execution=37,polarity:negative-parse=25,unsupported:continuation=3,unsupported:multi-cluster=2,vacuous:all-assertions-skipped=1} inner_skips{conformance:go-accepts-what-java-rejects=14,conformance:java-disabled=1,conformance:java-planner-bug=2,engine-gap:case-sensitive-identifiers=3,engine-gap:catalog-system-tables=3,engine-gap:comma-join-mixed-from=1,engine-gap:error-class=3,engine-gap:nested-recursive-with=2,engine-gap:star-group-by-expansion=1,no-checks=8,plan-assertion=4606,polarity:negative-execution=37,unsupported:continuation=104,unsupported:debugger=3,unsupported:multi-cluster=2,unsupported:random-injection=30}"

// pinnedFileTotal closes the ledger: every corpus file lands in exactly one of
// pass / fail / skip. Asserting the sum separately means a file that vanished
// from the run fails with an obvious message instead of a 2,000-column diff.
const pinnedFileTotal = 255

// pinnedAssignmentDigest is sha256 over the sorted `path status class` lines.
const pinnedAssignmentDigest = "0d9af835519592c2f12c1114f45a0681d5d89908b77e88270d8f54b7880a133b"
