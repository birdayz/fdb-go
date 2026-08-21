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
		{"D.A(B", "D", "A(B", "an unmatched OPEN paren after the dot swallows nothing"},
		{"A(.COL", "A(", "COL", "an unmatched OPEN paren BEFORE the dot must not swallow it either — " +
			"the mirror of the D.A)B row, and the row a one-pass forward counter fails"},
		{"A(B.C", "A(B", "C", "the same stray open with text after it; a forward counter " +
			"returns this unqualified, a backward counter gets it right, and only " +
			"matching the pairs first satisfies this row AND D.A)B together"},
		{"A)B", "", "A)B", "unqualified, unmatched close"},
		// A `)` inside a STRING LITERAL is not a paren. Reachable because the
		// operand mint carries literals verbatim into these derived names.
		{
			`I.COUNT(CASE WHEN S=')' THEN X.Y END)`, "I", `COUNT(CASE WHEN S=')' THEN X.Y END)`,
			"a close paren inside a literal must not close the real one",
		},
		{
			`I.COUNT(CASE WHEN S='.' THEN 1 END)`, "I", `COUNT(CASE WHEN S='.' THEN 1 END)`,
			"a DOT inside a literal is not a split point",
		},
		{`D.A'B`, "D", `A'B`, "an unterminated quote must not swallow the rest of the name"},
		{`A'B.C`, `A'B`, "C", "the MIRROR of the row above on the quote axis: a stray quote " +
			"BEFORE the dot made this UNQUALIFIED, because an unterminated quote was allowed " +
			"to open a literal and swallow the dot. Both paren directions were pinned and " +
			"only one quote direction was. It splits like A(B.C, at the last depth-0 dot"},
		{`A''B.C`, "A''B", "C", "a `''` pair is a CLOSED span, so the dot after it is still a split point"},
	} {
		got := parseColRef(tc.in)
		if got.table != tc.table || got.col != tc.col {
			t.Errorf("parseColRef(%q) = {table:%q col:%q}, want {table:%q col:%q} — %s",
				tc.in, got.table, got.col, tc.table, tc.col, tc.why)
		}
	}
}
