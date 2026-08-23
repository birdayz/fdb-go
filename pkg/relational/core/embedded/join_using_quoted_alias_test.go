package embedded

import (
	"strings"
	"testing"
)

// A `JOIN … USING (col)` desugars into a synthesized ON expression whose
// terms qualify the columns by the two source aliases. Those aliases are
// stored NORMALIZED (NormalizeIdentifier: unquoted folded UPPER, quoted
// verbatim), so splicing them into SQL text bare re-normalizes them: a
// quoted-DDL alias `"e"` (stored `e`) folded to `E` and the synthesized ON
// failed with `no FROM source aliased as E` — join-tests-outer.yamsql's
// `LEFT JOIN "dept" "d" USING ("id")` rows died there the moment the file's
// schema DDL started building (RFC-202 S5's quoted-case fix). The desugar
// double-quotes the stored alias so it round-trips exactly.
func TestJoinUsing_QuotedAliasesResolve(t *testing.T) {
	t.Parallel()
	const schema = `
CREATE TABLE "emp" ("id" BIGINT, "fname" STRING, PRIMARY KEY ("id"))
CREATE TABLE "dept" ("id" BIGINT, "name" STRING, PRIMARY KEY ("id"))
`
	for _, q := range []string{
		// The exact corpus shape: quoted tables, quoted aliases, quoted
		// USING column.
		`SELECT "e"."fname", "d"."name" FROM "emp" "e" LEFT JOIN "dept" "d" USING ("id")`,
		// Unquoted aliases must keep resolving after the re-quoting (their
		// stored form is the folded name, which quotes back to itself).
		`SELECT e."fname", d."name" FROM "emp" e LEFT JOIN "dept" d USING ("id")`,
	} {
		plan, err := PlanQueryForTest(q, schema, nil)
		if err != nil {
			t.Errorf("%s: %v", q, err)
			continue
		}
		if !strings.Contains(plan, "FlatMap") && !strings.Contains(plan, "Join") {
			t.Errorf("%s planned without a join operator:\n%s", q, plan)
		}
	}
}
