package query

import "testing"

// TestNameResolvesInColumns drives every arm of the group-key resolution gate.
//
// It had ZERO tests when it was changed — twice, in opposite directions, both
// wrong. That is the whole reason this file exists: the gate is four lines, it
// sits in front of a fail-closed refusal, and neither of its failure modes is a
// wrong answer, so nothing in the suite moved either time.
//
// The rule the rows encode is "a gate folds exactly as the read it guards
// folds". The guarded read is values' fieldRequestByName, which compares
// `fields[i].name == request.name` — byte-exact — so this must be byte-exact
// too. Both historical mistakes are pinned as rows rather than described:
//
//   - folding only the KEY (`strings.ToUpper(key)` against raw columns) refused
//     a verbatim key over its own verbatim column, on a query Java answers;
//   - folding BOTH sides is strictly WIDER than the guarded read, so it admits
//     a key the read will then miss, and — now that two case-differing names can
//     coexist as distinct slots — reports "resolves" for a key matching two.
func TestNameResolvesInColumns(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		key  string
		cols []string
		want bool
		why  string
	}{
		{
			name: "exact match resolves",
			key:  "REGION", cols: []string{"ID", "REGION"}, want: true,
			why: "the ordinary case, and the one that must never stop working",
		},
		{
			name: "verbatim key over its own verbatim column",
			key:  "Region", cols: []string{"ID", "Region"}, want: true,
			why: "folding only the key refused this and hard-failed a query Java answers",
		},
		{
			name: "upper key does NOT reach a mixed-case column",
			key:  "REGION", cols: []string{"Region"}, want: false,
			why: "the guarded read is byte-exact, so admitting this here only moves " +
				"the failure deeper; a folding gate answered true",
		},
		{
			name: "mixed key does NOT reach an upper column",
			key:  "Region", cols: []string{"REGION"}, want: false,
			why: "the mirror of the row above — the widening is symmetric and so is the refusal",
		},
		{
			name: "two case-differing columns are two slots, and the exact one resolves",
			key:  "REGION", cols: []string{"Region", "REGION"}, want: true,
			why: "exactly ONE exact match; a folding gate matched both and still returned a " +
				"single bool, reporting resolution for an ambiguous row",
		},
		{
			name: "duplicate EXACT columns do not resolve",
			key:  "REGION", cols: []string{"REGION", "REGION"}, want: false,
			why: "the guarded read counts matches and refuses >1 with FieldAmbiguousName, so a " +
				"first-match bool admits here exactly what the read refuses below — the same " +
				"failure the fold produced, surviving at byte-exactness. This is the arm the " +
				"case-differing row above CANNOT drive, because that row has one exact match",
		},
		{
			name: "duplicates elsewhere do not affect an unambiguous key",
			key:  "ID", cols: []string{"REGION", "REGION", "ID"}, want: true,
			why: "the count is per-KEY, not per-row: a row carrying some other name twice " +
				"must not refuse a name it carries once",
		},
		{
			name: "absent name does not resolve",
			key:  "MISSING", cols: []string{"ID", "REGION"}, want: false,
			why: "the refusal path — this is what the gate exists to produce",
		},
		{
			name: "no columns at all",
			key:  "REGION", cols: nil, want: false,
			why: "an empty row resolves nothing; a loop that fell through to true would " +
				"be the empty-set false positive",
		},
		{
			name: "an empty key DOES match a single empty column name",
			key:  "", cols: []string{""}, want: true,
			why: "documents the degenerate case rather than leaving it to be discovered: " +
				"the comparison is on the strings it is given, with no emptiness special " +
				"case. The name of this row said `does not match` while asserting true — " +
				"a row whose NAME contradicts its expectation reads as correct on the day " +
				"the behaviour flips and the row reddens",
		},
	} {
		if got := nameResolvesInColumns(tc.key, tc.cols); got != tc.want {
			t.Errorf("%s: nameResolvesInColumns(%q, %v) = %v, want %v — %s",
				tc.name, tc.key, tc.cols, got, tc.want, tc.why)
		}
	}
}
