package embedded

import "testing"

// Under CASE_SENSITIVE_IDENTIFIERS an unquoted identifier keeps its case, as
// Java's normalizeString does: every uid is quoted as written, and nothing
// else (keywords, literals, built-in function names, already quoted names)
// changes.
func TestCaseSensitiveIdentifiers(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ in, want string }{
		{`select col1 from Table1 where id = 1`, `select "col1" from "Table1" where "id" = 1`},
		{`SELECT t.Col FROM T1 AS t WHERE t.x > 'Abc'`, `SELECT "t"."Col" FROM "T1" AS "t" WHERE "t"."x" > 'Abc'`},
		{`select count(*) from "Q" where abs(v) > 0`, `select count(*) from "Q" where abs("v") > 0`},
		{`insert into TaBlE1 values (1, 'foo')`, `insert into "TaBlE1" values (1, 'foo')`},
		{`select f3(col1), cardinality(a) from t`, `select "f3"("col1"), cardinality("a") from "t"`},
		{`not sql at all (`, `not sql at all (`},
	} {
		if got := caseSensitiveIdentifiers(c.in); got != c.want {
			t.Errorf("caseSensitiveIdentifiers(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
