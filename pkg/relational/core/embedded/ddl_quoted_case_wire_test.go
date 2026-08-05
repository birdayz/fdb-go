package embedded

import (
	"testing"
)

// Pins the STORED name of a mixed-case quoted DDL column.
//
// This is the wire half of a refuted watch-list claim. Measurement (the
// cross-engine QuotedIdentifierCaseJavaProbe) showed that Go resolves
// `"KeepCase"`, `"KEEPCASE"` and bare `KeepCase` all to the same column and
// reports it as `KEEPCASE`, while Java resolves only the exact spelling and
// raises 42703 for the others, reporting `KeepCase`. That is a divergence in
// NAME RESOLUTION. Whether it is ALSO a wire divergence turns on one question
// this test answers and nothing else did: does the fold reach the stored
// descriptor, or does it live only in the read-side row layout?
//
// It must be asked of the REAL DDL path. An earlier attempt asserted this
// against a hand-built descriptor and proved nothing — the test constructed a
// field named "KeepCase" and then checked that it was named "KeepCase". Here
// the descriptor comes from parsing actual `CREATE TABLE` text, so the answer
// is the schema builder's, not the test's.
//
// If this goes RED with the field reading KEEPCASE, the fold has reached stored
// metadata: a Go-created table and a Java-created table would no longer carry
// the same field name for the same DDL, and the divergence would have been
// promoted from a read-side name quirk into a wire-format break. That is the
// escalation this test exists to trigger.
func TestDDL_QuotedColumnKeepsCaseInStoredDescriptor(t *testing.T) {
	t.Parallel()
	tmpl, err := buildSchemaTemplateFromDDL(
		`CREATE TABLE qcase (id BIGINT, "KeepCase" BIGINT, plain BIGINT, PRIMARY KEY (id))`)
	if err != nil {
		t.Fatalf("mixed-case quoted column must build: %v", err)
	}
	rt := tmpl.Underlying().GetRecordType("QCASE")
	if rt == nil || rt.Descriptor == nil {
		t.Fatal("record type QCASE has no descriptor")
	}
	fields := rt.Descriptor.Fields()

	got := make([]string, 0, fields.Len())
	for i := 0; i < fields.Len(); i++ {
		got = append(got, string(fields.Get(i).Name()))
	}

	// The quoted column keeps its declared case; the UNQUOTED sibling folds to
	// upper. Both are asserted together because it is the CONTRAST that carries
	// the meaning — a descriptor where everything happened to be upper-case
	// would satisfy a `plain`-only check while telling us nothing about quoting.
	want := []string{"ID", "KeepCase", "PLAIN"}
	if len(got) != len(want) {
		t.Fatalf("descriptor fields = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("descriptor field %d = %q, want %q — if the quoted column now stores folded, "+
				"the read-side case divergence has become a WIRE divergence from Java and must be "+
				"escalated, not absorbed into this expectation (full sequence: %v)", i, got[i], want[i], got)
		}
	}
}
