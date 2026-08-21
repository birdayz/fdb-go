package embedded

import "testing"

// TestParseColRef_SplitsAtDepthZero drives the qualifier split directly,
// because every shape that breaks it is a shape the corpus does not contain: a
// DERIVED name carrying its own dots, a DELIMITED identifier carrying a stray
// paren or quote, and a STRING LITERAL carrying either. All of them reach this
// function as a flat string with no marker saying which characters are
// structure and which are content.
//
// THE ROWS ARE A HISTORY OF WRONG ANSWERS, which is why each carries the defect
// it pins rather than a description of the rule:
//
//   - `I.SUM(I.AMOUNT)` — a plain last-dot split cut this into table
//     `I.SUM(I` and column `AMOUNT)`, and that fragment was the result-set
//     LABEL for an unaliased correlated-scalar aggregate. The original defect.
//   - `D.A)B` — the defect the first fix introduced. Right-to-left, an
//     unmatched `)` reads as an opening nest, so the dot looked nested.
//   - `A(B.C` / `A(.COL` — the defect the SECOND fix introduced, in the mirror
//     direction: left-to-right, an unmatched `(` reads as an unclosed nest.
//     Neither direction is sound, which is why the pairs are matched first.
//   - `A'B.C` — the same stray-token failure on the quote axis, found only
//     after both paren directions were already pinned.
//   - `Q'.Z'` — the defect the quote fix introduced, and the one that settled
//     the question: pairing apostrophes as literal delimiters hid a qualifier
//     dot between two apostrophe-bearing identifiers, and joined derived
//     tables reported `Q'.Z'` as a whole column name.
//   - `I.COUNT(CASE WHEN S=')' THEN X.Y END)` — a STATED LIMIT, not a passing
//     case. See its comment: it is the measured cost of treating an apostrophe
//     as content.
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
		// QUOTES ARE IDENTIFIER CONTENT, and these four rows are why. An
		// apostrophe reaches this function far more often as part of a
		// delimited identifier than as a string-literal boundary, and nothing
		// local separates the two.
		{`D.A'B`, "D", `A'B`, "a quote in the COLUMN segment is content"},
		{`A'B.C`, `A'B`, "C", "a quote in the QUALIFIER segment is content too"},
		{`Q'.Z'`, `Q'`, `Z'`, "a quote in BOTH segments — a derived alias and its column alias, " +
			"which is the shape a literal-pairing pass broke: it paired the two apostrophes, " +
			"hid the qualifier dot inside the span, and Rows.Columns() over joined derived " +
			"tables reported [Q'.Z' R'.Z'] where [Z' Z'] is right"},
		{`A''B.C`, "A''B", "C", "a doubled quote is content as well; nothing here reads it as an escape"},
		// A DOT inside a string literal still resolves, and it costs nothing to
		// get right: the parens around the literal put that dot at depth 1
		// whether or not the quotes mean anything.
		{
			`I.COUNT(CASE WHEN S='.' THEN 1 END)`, "I", `COUNT(CASE WHEN S='.' THEN 1 END)`,
			"a DOT inside a literal is not a split point, via paren depth alone",
		},
		// A STATED LIMIT — the one shape the quote decision costs, pinned so it
		// is known rather than discovered. A PAREN inside a string literal is
		// read as a real paren, closes the enclosing call early, and drops the
		// following dot to depth 0.
		//
		// It is the deliberate side of a measured trade. Treating quotes as
		// literal boundaries fixes this row and breaks `Q'.Z'` above, which is
		// reachable through ordinary delimited identifiers; this row needs a
		// string literal to appear in a derived NAME, and a live differential
		// over 2,682,910 production calls found no literal-bearing name at all.
		// A measured regression against an unmeasured one goes only one way.
		{
			`I.COUNT(CASE WHEN S=')' THEN X.Y END)`, `I.COUNT(CASE WHEN S=')' THEN X`, `Y END)`,
			"KNOWN LIMIT: a paren inside a literal closes the real one. If this row ever " +
				"needs to change, the fix is a structured name, not a cleverer scan",
		},
	} {
		got := parseColRef(tc.in)
		if got.table != tc.table || got.col != tc.col {
			t.Errorf("parseColRef(%q) = {table:%q col:%q}, want {table:%q col:%q} — %s",
				tc.in, got.table, got.col, tc.table, tc.col, tc.why)
		}
	}
}
