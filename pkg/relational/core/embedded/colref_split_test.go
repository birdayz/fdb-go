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
		// A DOT inside a string literal resolves when something ENCLOSES it —
		// here the call's own parens put it at depth 1 whether or not the
		// quotes mean anything. The enclosure is the precondition, not the
		// literal; see the depth-0 limit two rows below.
		{
			`I.COUNT(CASE WHEN S='.' THEN 1 END)`, "I", `COUNT(CASE WHEN S='.' THEN 1 END)`,
			"a DOT inside an ENCLOSED literal is not a split point, via paren depth alone",
		},
		// An UNMATCHED paren inside a literal is inert like any other stray, so
		// this one is NOT a cost. Pinned because the sentence describing the
		// cost got this backwards once, saying any paren in a literal was
		// affected.
		{`X.Y || ')'`, "X", `Y || ')'`, "a stray paren inside a literal changes nothing"},
		// A STATED LIMIT — the one shape the quote decision costs, pinned so it
		// is known rather than discovered. A PAREN inside a string literal is
		// read as a real paren, closes the enclosing call early, and drops the
		// following dot to depth 0.
		//
		// It is the deliberate side of a measured trade. Treating quotes as
		// literal boundaries fixes this row and breaks `Q'.Z'` above, which is
		// reachable through ordinary delimited identifiers.
		//
		// THE OTHER HALF OF THAT TRADE WAS MIS-STATED HERE, and the correction
		// matters because it is what made the trade look one-sided. This said a
		// differential "found no literal-bearing name at all". It does find
		// them: 35 production names of 28,625 calls over the planning corpus
		// carry a quote — `CAST('0.0' AS BIGINT)`, `COALESCE(NAME,'unknown')`,
		// `SUM(CASEWHENSTATUS='open'…)`. The number the deletion actually rests
		// on is different and narrower: old and new disagree on exactly FOUR
		// inputs, and all four are rows in this file. Zero production decisions
		// change.
		{
			`I.COUNT(CASE WHEN S=')' THEN X.Y END)`, `I.COUNT(CASE WHEN S=')' THEN X`, `Y END)`,
			"KNOWN LIMIT 1 of 2: a MATCHED paren inside a literal closes the real one. If " +
				"this row ever needs to change, the fix is a structured name, not a " +
				"cleverer scan",
		},
		// THE SECOND LIMIT, and the one the prose missed. A literal at depth 0
		// has nothing enclosing it, so a dot inside it IS the last depth-0 dot.
		// This is the limit that is NOT remote: `CAST('0.0' AS BIGINT)` is a
		// real production name of exactly this shape, and it resolves only
		// because CAST's parens enclose it.
		// The sentence claiming "only a literal containing a PAREN is affected"
		// was written from the enclosed row above rather than from the rule.
		{
			`X.Y || '.'`, `X.Y || '`, `'`,
			"KNOWN LIMIT 2 of 2: an UNENCLOSED literal containing a dot splits inside itself",
		},
	} {
		got := parseColRef(tc.in)
		if got.table != tc.table || got.col != tc.col {
			t.Errorf("parseColRef(%q) = {table:%q col:%q}, want {table:%q col:%q} — %s",
				tc.in, got.table, got.col, tc.table, tc.col, tc.why)
		}
	}
}
