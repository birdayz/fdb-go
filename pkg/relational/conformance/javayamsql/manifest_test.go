package javayamsql_test

import (
	"testing"

	"fdb.dev/pkg/relational/conformance/javayamsql"
)

// TestManifestComposition pins the manifest's entry counts by polarity.
//
// These are ENTRY counts — how many files the manifest records with each
// polarity — and they are deliberately kept distinct from the corpus runner's
// LEDGER counts, which say what happened when those files ran. The two answer
// different questions and had already been fused once in prose: "42
// execution-level negatives" is this table, "20 booked as
// polarity:negative-execution" is the ledger, and the gap between them is the
// files an earlier skip claimed before their expected failure could be
// reached. Quoting either number as the other is the drift this pins.
func TestManifestComposition(t *testing.T) {
	t.Parallel()

	want := map[javayamsql.Polarity]int{
		javayamsql.NegativeParse:     25,
		javayamsql.NegativeExecution: 42,
		javayamsql.FixedVersionMeta:  9,
		javayamsql.Fragment:          2,
		javayamsql.Positive:          6,
	}

	got := javayamsql.Composition()
	total := 0
	for p, n := range got {
		total += n
		if want[p] != n {
			t.Errorf("manifest has %d %s entries, pinned baseline is %d", n, p, want[p])
		}
	}
	for p, n := range want {
		if _, ok := got[p]; !ok {
			t.Errorf("manifest has no %s entries, pinned baseline is %d", p, n)
		}
	}
	if total != len(javayamsql.Manifest) {
		t.Errorf("composition counts %d entries, manifest has %d", total, len(javayamsql.Manifest))
	}
	if total != 84 {
		t.Errorf("manifest has %d entries, pinned baseline is 84", total)
	}
}
