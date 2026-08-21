package embedded

import "testing"

// TestParseColRef_SplitsAtDepthZero drives the qualifier split directly,
// because the two shapes that break it are shapes the corpus does not contain:
// a DERIVED name carrying its own dots, and a DELIMITED identifier carrying an
// unmatched paren. Both reach this function as a flat string with no marker
// saying which dots are structure and which are content.
//
// The `AMOUNT)` row is the defect that started this: a plain last-dot split cut
// `I.SUM(I.AMOUNT)` into table `I.SUM(I` and column `AMOUNT)`, and that fragment
// was the result-set LABEL for an unaliased correlated-scalar aggregate.
//
// The `D.A)B` row is the defect the FIX introduced, which is why it is here
// rather than in a comment: scanning right-to-left, an unmatched `)` reads as an
// opening nest, so the dot after `D` looked nested and the whole string came
// back as one unqualified name. Neither direction is obviously right; only the
// forward one satisfies both rows.
func TestParseColRef_SplitsAtDepthZero(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in         string
		table, col string
		why        string
	}{
		{"COL", "", "COL", "unqualified"},
		{"D1.COL", "D1", "COL", "the ordinary qualified reference"},
		{"A.B.C", "A.B", "C", "multi-segment: the LAST depth-0 dot splits"},
		{"SUM(A.B)", "", "SUM(A.B)", "a derived name is not qualified by its own inner dot"},
		{"I.SUM(I.AMOUNT)", "I", "SUM(I.AMOUNT)", "the shape that produced the `AMOUNT)` label"},
		{"D.f(x.y)", "D", "f(x.y)", "same, with a lower-case delimited name"},
		{"D.A)B", "D", "A)B", "an UNMATCHED close paren is content, not a nest"},
		{"D.A(B", "D", "A(B", "an unmatched OPEN paren swallows nothing after it"},
		{"A)B", "", "A)B", "unqualified, unmatched close"},
	} {
		got := parseColRef(tc.in)
		if got.table != tc.table || got.col != tc.col {
			t.Errorf("parseColRef(%q) = {table:%q col:%q}, want {table:%q col:%q} — %s",
				tc.in, got.table, got.col, tc.table, tc.col, tc.why)
		}
	}
}
