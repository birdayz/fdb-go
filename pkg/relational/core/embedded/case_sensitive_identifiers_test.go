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
		{`SELECT text_contains_all(body,'red'), TEXT_CONTAINS_ALL_WITHIN(body,'red blue',2) FROM docs`, `SELECT text_contains_all("body",'red'), TEXT_CONTAINS_ALL_WITHIN("body",'red blue',2) FROM "docs"`},
		{`SELECT "TEXT_CONTAINS_ALL"(body,'red') FROM docs`, `SELECT "TEXT_CONTAINS_ALL"("body",'red') FROM "docs"`},
		{`not sql at all (`, `not sql at all (`},
	} {
		if got := caseSensitiveIdentifiers(c.in); got != c.want {
			t.Errorf("caseSensitiveIdentifiers(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCaseSensitiveTextFunctionNamespace(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"TEXT_CONTAINS_ALL", "TEXT_CONTAINS_ANY", "TEXT_CONTAINS_PHRASE", "TEXT_CONTAINS_ALL_WITHIN", "TEXT_CONTAINS_PREFIX", "TEXT_CONTAINS_ALL_PREFIXES", "TEXT_CONTAINS_ANY_PREFIX"} {
		query := "SELECT " + name + "(BODY) FROM DOCS"
		for _, collision := range []bool{false, true} {
			calls := 0
			got := caseSensitiveIdentifiers(query, func(got string) bool {
				calls++
				if got != name {
					t.Fatalf("callback got %q want %q", got, name)
				}
				return collision
			})
			wantName := name
			if collision {
				wantName = `"` + name + `"`
			}
			want := "SELECT " + wantName + `("BODY") FROM "DOCS"`
			if got != want || calls != 1 {
				t.Fatalf("collision=%v got=%s calls=%d want=%s", collision, got, calls, want)
			}
		}
	}
	for _, query := range []string{`SELECT cardinality(A) FROM T`, `SELECT JAVA_CALL('x') FROM T`, `SELECT "TEXT_CONTAINS_ALL"(A) FROM T`, `not sql (`} {
		calls := 0
		caseSensitiveIdentifiers(query, func(string) bool { calls++; return true })
		if calls != 0 {
			t.Fatalf("%s: schema callback called %d times", query, calls)
		}
	}
}
