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
const pinnedLedger = "pass=129 fail=0 skip=126 queries=6613 file_skips{conformance:go-accepts-what-java-rejects=13,conformance:java-planner-bug=1,conformance:scan-choice-order=2,engine-gap:case-sensitive-identifiers=3,engine-gap:catalog-system-tables=3,engine-gap:comma-join-mixed-from=1,engine-gap:correlated-exists-setop=1,engine-gap:error-class=4,engine-gap:nested-recursive-with=2,engine-gap:planner-declines=2,engine-gap:star-group-by-expansion=1,fragment=2,plan-assertion=3,polarity:fixed-version-meta=11,polarity:negative-execution=31,polarity:negative-parse=25,unsupported:continuation=3,unsupported:multi-cluster=2,unsupported:result-metadata-nested=6,unsupported:schema-command=9,vacuous:all-assertions-skipped=1} inner_skips{conformance:go-accepts-what-java-rejects=13,conformance:java-planner-bug=1,conformance:scan-choice-order=2,engine-gap:case-sensitive-identifiers=3,engine-gap:catalog-system-tables=3,engine-gap:comma-join-mixed-from=1,engine-gap:correlated-exists-setop=1,engine-gap:error-class=4,engine-gap:nested-recursive-with=2,engine-gap:planner-declines=2,engine-gap:star-group-by-expansion=1,no-checks=7,plan-assertion=3203,polarity:negative-execution=31,unsupported:check-cache=218,unsupported:continuation=86,unsupported:debugger=3,unsupported:multi-cluster=2,unsupported:random-injection=25,unsupported:result-metadata-nested=190,unsupported:schema-command=18}"

// pinnedFileTotal closes the ledger: every corpus file lands in exactly one of
// pass / fail / skip. Asserting the sum separately means a file that vanished
// from the run fails with an obvious message instead of a 2,000-column diff.
const pinnedFileTotal = 255

// pinnedAssignmentDigest is sha256 over the sorted `path status class` lines.
const pinnedAssignmentDigest = "11a972bdd92a8e09e7546326aef3985b699303f22d624ee353188ff001c0bc56"
