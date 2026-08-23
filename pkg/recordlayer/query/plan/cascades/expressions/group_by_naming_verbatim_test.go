package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestAggregateResultColumnName_OperandNameIsVerbatim drives every arm of the
// aggregate naming authority with an OperandName carrying a DELIMITED spelling.
//
// The corpus reading cannot substitute for this. A quoted identifier reaches
// only SUM and MIN/MAX under the current 2593-query corpus, so a fold
// reintroduced on AVG, on COUNT, or on the default arm would ship green — and
// the arm that goes untested longest is the one the next change makes live.
//
// Two properties, separable, both required: the operand must not be folded (a
// column the user really declared as `qty` is named `qty`), and the function
// symbol must still read upper — it is written as a literal, so nothing has to
// fold it, which is exactly why the blanket fold that did was removable.
func TestAggregateResultColumnName_OperandNameIsVerbatim(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		fn   AggregateFunction
		want string
	}{
		{AggCount, "COUNT(qty)"},
		{AggSum, "SUM(qty)"},
		{AggMin, "MIN(qty)"},
		{AggMax, "MAX(qty)"},
		{AggAvg, "AVG(qty)"},
		{AggregateFunction(99), "AGG(qty)"},
	} {
		got := AggregateResultColumnName(AggregateSpec{Function: tc.fn, OperandName: "qty"})
		if got != tc.want {
			t.Errorf("AggregateResultColumnName(%v, %q) = %q, want %q — a fold on this arm "+
				"renames a column the user declared", tc.fn, "qty", got, tc.want)
		}
	}

	// A COMPOUND operand arrives already canonical from the sole mint
	// (embedded.aggOperandCanonicalText): non-identifier tokens upper, delimited
	// identifiers verbatim, whitespace already gone. This authority must not
	// re-derive any part of that — the space-strip that used to sit here was the
	// visible half of a repair whose other half was the fold.
	if got := AggregateResultColumnName(AggregateSpec{
		Function: AggSum, OperandName: "KeepCase*PLAIN",
	}); got != "SUM(KeepCase*PLAIN)" {
		t.Errorf("compound operand = %q, want SUM(KeepCase*PLAIN)", got)
	}
	// A name that legitimately contains a space is CARRIED, not repaired. The
	// old space-strip could not tell a name apart from a source slice.
	if got := AggregateResultColumnName(AggregateSpec{
		Function: AggMax, OperandName: "two words",
	}); got != "MAX(two words)" {
		t.Errorf("spaced operand = %q, want MAX(two words)", got)
	}
}

// TestGroupByOutputColumnNames_AliasIsVerbatim pins the alias arm of the output
// row's naming authority.
//
// It is the downstream guard the translator's alias mint leans on. That mint's
// fold could not be driven by any query shape that could be constructed —
// instrumented, it is taken three times across the whole corpus and every one
// carries an already-upper canonical name — so what protects its removal is
// this: a fold reintroduced anywhere upstream arrives here and is published.
func TestGroupByOutputColumnNames_AliasIsVerbatim(t *testing.T) {
	t.Parallel()

	keys := []values.Value{testField("KeepCase", values.NotNullLong)}
	aggs := []AggregateSpec{
		{Function: AggSum, OperandName: "PLAIN", Alias: "total"},
		{Function: AggMax, OperandName: "PLAIN"},
	}
	got := GroupByOutputColumnNames(keys, aggs)
	want := []string{AggregateKeyColumnName(keys[0]), "total", "MAX(PLAIN)"}
	if len(got) != len(want) {
		t.Fatalf("GroupByOutputColumnNames = %v, want %v", got, want)
	}
	// The key half is compared against the key authority rather than a literal
	// because the two share no derivation — the key renders through
	// ColumnNameValue and the aggregate halves do not — so this is a genuine
	// cross-check and not a pair that moves together.
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("column %d = %q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}
	// And assert the VALUE the alias must carry, not only that it equals
	// something: an equality against a folded expectation would hold under the
	// fold this pins.
	if got[1] != "total" {
		t.Errorf("alias published as %q, want the authored spelling %q", got[1], "total")
	}
}
