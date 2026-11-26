package plandiff

// RunQuery is one (schema, setup-DMLs, SELECT) case for the result-set diff
// harness; seedcorpus.SeedRunCorpus holds the baseline set.
// The conformance test runs each entry through BOTH engines (Java
// fdb-relational + Go embedded) and asserts byte-equivalent results.
//
// Java is the spec. The harness asserts:
//   - If Java succeeds: Go must also succeed AND produce byte-equal
//     column metadata and row values.
//   - If Java errors: Go must also error AND produce a byte-equal
//     core error message (Go's `api.Error.Message` ==
//     Java's `JavaError.Message`).
//
// The test author DOES NOT predict the error message. Whatever Java
// emits is the ground truth — Go must match. This catches both silent
// acceptance on one side AND Go's-message-drift-from-Java's at every
// query, without requiring per-entry expected-text annotations.
//
// Adding a new entry: just append {Name, SchemaTemplate, SetupSqls,
// Query}. No baseline RowSet, no expected-error-text. The harness
// figures out what Java does and pins Go to match it.
//
// Each entry's SetupSqls must produce deterministic state — SELECTs
// without ORDER BY can't be added until we trust both engines'
// row-order semantics match. Today every entry orders by primary key.
type RunQuery struct {
	Name           string
	SetupSqls      []string
	Query          string
	SchemaTemplate string
	// Divergence, when non-nil, marks this entry as a known
	// cross-engine divergence rather than a parity assertion. The
	// harness asserts Go's behaviour against the embedded
	// expectation but does NOT pin Java's actual behaviour — Java
	// may evolve (upstream fix), regress, or stay buggy without
	// breaking our test surface. See `divergence.go`.
	Divergence *Divergence
}
