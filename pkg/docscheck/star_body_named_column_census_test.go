package docscheck

// A census of SQL that addresses a STAR-PROJECTED body column BY NAME.
//
// The shape: a CTE or derived-table body with no projection list — it is
// `SELECT *` — whose columns an enclosing level then names.
//
//	WITH c AS (SELECT * FROM t)            SELECT id FROM c WHERE grp = 10
//	SELECT d.id FROM (SELECT * FROM t) AS d WHERE d.amt = 300
//
// WHY THIS EXISTS. A proposal to make star-projected body columns UNNAMED and
// unresolvable was nearly implemented, on the reading that Java's
// SemanticAnalyzer.lookup skips an output attribute carrying no Identifier
// (SemanticAnalyzer.java:458-461). The skip is real; the inference is not —
// Java's star expansion NAMES its columns (expandStar returns a Star over
// `logicalTable.getOutput().nonEphemeralVisible()`, SemanticAnalyzer.java
// :321-367; Expressions.expanded() splices those NAMED members into the
// operator output, Expressions.java:78-84). Nothing in the tree could answer
// "how many queries would that break?", so the question was decided on an
// unverified premise. This census is that answer, kept runnable.
//
// SCOPE — TWO CORPORA, NOT THE REPO. It reads exactly:
//
//	pkg/relational/conformance/yamsql/testdata/*.yaml            (Go scenarios)
//	third_party/apple/.../yaml-tests/src/test/resources/**/*.yamsql (Java corpus)
//
// Both are already Bazel `data` deps of this target, so the test has bounded
// inputs and no repo staging. Every number it reports is scoped to those two
// corpora and the failure messages say so. It is NOT a whole-repo answer.
//
// WHAT IT DOES NOT COVER, deliberately, so nobody reads it as complete:
//   - Go test files carrying SQL as string literals — sqldriver's
//     star_body_cte_join_leg_fdb_test.go alone holds 29 such queries.
//   - Shapes GENERATED at runtime rather than written down:
//     conformance/rowdiff/gen.go's genDerivedQuery/derivedSQL emit
//     `SELECT <proj> FROM (SELECT * FROM t WHERE …) d` per run, so the harness
//     itself depends on this behaviour and no corpus scan can see it.
//   - Plan-shape goldens and any other derived artifact.
//
// The whole-tree sweep command lives in the PR body; this test complements it
// rather than replacing it.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// starBodyCensusFloors are the populations this census must find before any of
// its numbers mean anything.
//
// A census over an empty corpus reports a perfect zero, and a green from an
// empty set is this repo's dominant false positive. Both floors are therefore
// non-vacuity guards, not targets: they fail when the INSTRUMENT dies, and the
// direction of alarm is COLLAPSE. They sit well under the measured values so
// ordinary corpus churn does not trip them.
const (
	// minQueriesScanned floors the denominator — if query extraction breaks, or
	// a data dep is dropped from the BUILD target, the scan silently sees
	// nothing and every finding below reads as a clean zero.
	minQueriesScanned = 1500
	// minStarBodyNamed floors the finding itself. Measured at 67 across the two
	// corpora when this landed (21 Go scenarios + 46 Java corpus, of 5782
	// queries scanned).
	minStarBodyNamed = 50
)

func TestStarBodyColumnsAreAddressedByName(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)

	corpora := []struct {
		name string
		dir  string
		ext  string
	}{
		{"go-yamsql", filepath.Join(root, "pkg/relational/conformance/yamsql/testdata"), ".yaml"},
		{"java-corpus", filepath.Join(root, "third_party/apple/fdb-record-layer/yaml-tests/src/test/resources"), ".yamsql"},
	}

	type hit struct{ corpus, file, sql string }
	var hits []hit
	scanned := 0
	perCorpusScanned := map[string]int{}
	perCorpusHits := map[string]int{}

	for _, c := range corpora {
		err := filepath.Walk(c.dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || filepath.Ext(path) != c.ext {
				return nil
			}
			b, rErr := os.ReadFile(path)
			if rErr != nil {
				return rErr
			}
			for _, q := range extractYamlQueries(string(b)) {
				scanned++
				perCorpusScanned[c.name]++
				if starBodyColumnNamedOutside(q) {
					perCorpusHits[c.name]++
					rel, _ := filepath.Rel(root, path)
					hits = append(hits, hit{c.name, rel, q})
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s corpus at %s: %v\n\nThis corpus is a Bazel `data` dep of "+
				"//pkg/docscheck:docscheck_test. Under Bazel a missing dep is not a skip — "+
				"it is an empty scan that would make every number below a clean zero.",
				c.name, c.dir, err)
		}
	}

	sort.Slice(hits, func(i, j int) bool {
		if hits[i].file != hits[j].file {
			return hits[i].file < hits[j].file
		}
		return hits[i].sql < hits[j].sql
	})

	var b strings.Builder
	for _, c := range corpora {
		b.WriteString("\n  " + c.name + ": " +
			itoa(perCorpusHits[c.name]) + " star-body-by-name of " +
			itoa(perCorpusScanned[c.name]) + " queries scanned")
	}
	t.Logf("star-body-by-name census (SCOPE: the yamsql testdata and the vendored Java "+
		"corpus ONLY — not Go string literals, not rowdiff's runtime-generated shapes):%s"+
		"\n  TOTAL: %d of %d", b.String(), len(hits), scanned)

	// NON-VACUITY, checked before the finding is interpreted.
	if scanned < minQueriesScanned {
		t.Fatalf("the census scanned %d queries across the two corpora, want at least %d.\n\n"+
			"THE INSTRUMENT IS DEAD, not the corpus clean. Either query extraction stopped "+
			"matching the corpus format, or a `data` dep was dropped from this target so the "+
			"walk found no files. Every count below would read as a clean zero either way, "+
			"which is exactly the empty-set green this floor exists to prevent.",
			scanned, minQueriesScanned)
	}
	for _, c := range corpora {
		if perCorpusScanned[c.name] == 0 {
			t.Fatalf("the %s corpus contributed 0 queries (dir %s).\n\nThe TOTAL floor can be "+
				"satisfied by one corpus alone, so it cannot see a single corpus going dark. "+
				"This per-corpus guard can.", c.name, c.dir)
		}
	}

	if len(hits) < minStarBodyNamed {
		t.Fatalf("the census found %d queries addressing a star-projected body column by "+
			"name across the two corpora, want at least %d.\n\n"+
			"SCOPE: the yamsql testdata and the vendored Java corpus only.\n\n"+
			"A DROP HERE IS THE ALARM THIS TEST WAS BUILT FOR. Either the shape stopped "+
			"being expressible — which is the narrowing that was proposed and rejected, and "+
			"which Java's own corpus refutes (alias-tests.yamsql:250 pins resultMetadata for "+
			"`WITH cte AS (SELECT * FROM T1) SELECT c.id, c.a FROM cte AS c`) — or the "+
			"matcher stopped recognising it. Find out which before adjusting this floor.",
			len(hits), minStarBodyNamed)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	return string(d)
}

// queryLine matches a `- query:` entry in either corpus format.
var queryLine = regexp.MustCompile(`^\s*-?\s*query:\s*(.*)$`)

// extractYamlQueries pulls the SQL text out of `- query:` entries, handling both
// the inline form and YAML block scalars (`query: |`), which the Go scenarios
// use for multi-line SQL. Everything is flattened to one line: the matcher below
// reasons about token order, never about layout.
func extractYamlQueries(src string) []string {
	lines := strings.Split(src, "\n")
	var out []string
	for i := 0; i < len(lines); i++ {
		m := queryLine.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		rest := strings.TrimSpace(m[1])
		if rest != "" && rest != "|" && rest != ">" && rest != "|-" && rest != ">-" {
			out = append(out, rest)
			continue
		}
		// Block scalar: consume the following lines that are indented deeper
		// than the `query:` key itself.
		base := len(lines[i]) - len(strings.TrimLeft(lines[i], " "))
		var buf []string
		for j := i + 1; j < len(lines); j++ {
			if strings.TrimSpace(lines[j]) == "" {
				continue
			}
			ind := len(lines[j]) - len(strings.TrimLeft(lines[j], " "))
			if ind <= base {
				break
			}
			buf = append(buf, strings.TrimSpace(lines[j]))
			i = j
		}
		if len(buf) > 0 {
			out = append(out, strings.Join(buf, " "))
		}
	}
	return out
}

// starBodySpans returns every balanced-paren span whose contents begin with
// `SELECT *` / `SELECT DISTINCT *` — a projection-less body. Spans NEST, so
// this returns all of them and the caller decides which to use.
func starBodySpans(q string) [][2]int {
	var spans [][2]int
	for i := 0; i < len(q); i++ {
		if q[i] != '(' {
			continue
		}
		depth := 0
		for j := i; j < len(q); j++ {
			switch q[j] {
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					up := strings.Join(strings.Fields(strings.ToUpper(q[i+1:j])), " ")
					isStar := strings.HasPrefix(up, "SELECT *") || strings.HasPrefix(up, "SELECT DISTINCT *")
					if isStar && inBodyPosition(q, i) {
						spans = append(spans, [2]int{i, j + 1})
					}
					goto next
				}
			}
		}
	next:
	}
	return spans
}

// inBodyPosition reports whether the `(` at lp opens a relation-producing BODY
// — a derived table or a CTE definition — rather than some other parenthesised
// SELECT.
//
// This exists because a bare `SELECT *` is overwhelmingly common inside
// `EXISTS (…)`, where it projects nothing anyone can name. Counting those was a
// real defect in the first version of this matcher, caught by the unit cases
// below rather than by the corpus reading: it inflated the census with queries
// that would survive the narrowing untouched, and an inflated finding is worse
// than a missed one here — the whole point of the number is that it can be
// trusted.
//
// A body opens after FROM, JOIN, a FROM-list comma, or the AS of a CTE.
// Anything else (EXISTS, IN, a scalar comparison) is not a body.
func inBodyPosition(q string, lp int) bool {
	head := strings.TrimRight(q[:lp], " ")
	if head == "" {
		return false
	}
	if strings.HasSuffix(head, ",") {
		return true
	}
	fields := strings.Fields(strings.ToUpper(head))
	if len(fields) == 0 {
		return false
	}
	switch fields[len(fields)-1] {
	case "FROM", "JOIN", "AS":
		return true
	}
	return false
}

// innermostStarBodySpans keeps only the spans that contain no other star-body
// span — the DEEPEST projection-less bodies.
//
// Blanking the outermost span instead would swallow the very reference being
// counted. `SELECT * FROM (SELECT * FROM (SELECT * FROM t) AS x WHERE id = 5)
// AS y` is one span all the way out, so blanking it leaves `SELECT * FROM AS y`
// and the census would miss the whole nested class — which is a real Java shape
// (standard-tests.yamsql:284, field-index-tests-proto.yamsql:64). Blanking only
// `(SELECT * FROM t)` leaves `id` visible, which is the truth being measured.
func innermostStarBodySpans(spans [][2]int) [][2]int {
	var out [][2]int
	for _, s := range spans {
		nested := false
		for _, o := range spans {
			if o != s && o[0] >= s[0] && o[1] <= s[1] {
				nested = true
				break
			}
		}
		if !nested {
			out = append(out, s)
		}
	}
	return out
}

// sqlKeywords are the tokens that are never column references. Kept
// deliberately generous: a keyword wrongly omitted would INFLATE the census,
// and an inflated finding is the failure mode that matters here.
var sqlKeywords = map[string]bool{
	"select": true, "from": true, "where": true, "and": true, "or": true, "not": true,
	"as": true, "with": true, "order": true, "by": true, "group": true, "having": true,
	"join": true, "inner": true, "left": true, "right": true, "full": true, "outer": true,
	"on": true, "using": true, "distinct": true, "all": true, "union": true, "except": true,
	"intersect": true, "limit": true, "offset": true, "asc": true, "desc": true,
	"is": true, "null": true, "true": true, "false": true, "in": true, "exists": true,
	"between": true, "like": true, "case": true, "when": true, "then": true, "else": true,
	"end": true, "recursive": true, "at": true, "values": true, "cast": true,
	"nulls": true, "first": true, "last": true, "lateral": true, "cross": true,
}

// identToken matches a bare or dotted identifier, optionally double-quoted.
var identToken = regexp.MustCompile(`"[^"]+"|[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*`)

// starBodyColumnNamedOutside reports whether q contains a star-projected
// CTE/derived body AND names a column at some level OUTSIDE that body.
//
// Method, stated because the number depends on it: the INNERMOST star-body spans are
// blanked, then the remainder is tokenised. A token counts as a column
// reference unless it is a SQL keyword, a function name (immediately followed
// by `(`), or a NAME-POSITION token — the identifier right after FROM, JOIN or
// AS, which names a table, CTE or alias rather than a column. If anything
// survives that, an outer level named a column of the star body.
//
// `SELECT * FROM (SELECT * FROM t) AS d` therefore does NOT count: after
// blanking, only `d` remains and it is an AS-position name. `SELECT id FROM
// (SELECT * FROM t) AS d` does.
//
// The bias is deliberately toward UNDER-counting: a missed shape weakens the
// floor's evidence, while a false hit would inflate the very number this test
// exists to make trustworthy.
func starBodyColumnNamedOutside(q string) bool {
	flat := strings.Join(strings.Fields(q), " ")
	spans := innermostStarBodySpans(starBodySpans(flat))
	if len(spans) == 0 {
		return false
	}
	buf := []byte(flat)
	for _, s := range spans {
		for i := s[0]; i < s[1]; i++ {
			buf[i] = ' '
		}
	}
	remainder := string(buf)
	toks := identToken.FindAllStringIndex(remainder, -1)
	prev := ""
	for _, span := range toks {
		tok := remainder[span[0]:span[1]]
		low := strings.ToLower(strings.Trim(tok, `"`))
		bare := strings.ToLower(tok)
		// A function call: the identifier is immediately followed by `(`.
		rest := strings.TrimLeft(remainder[span[1]:], " ")
		isCall := strings.HasPrefix(rest, "(")
		switch {
		case sqlKeywords[bare], sqlKeywords[low]:
		case isCall:
		case prev == "from" || prev == "join" || prev == "as" || prev == "with":
		default:
			return true
		}
		if !sqlKeywords[bare] {
			prev = bare
			continue
		}
		prev = bare
	}
	return false
}

// TestStarBodyMatcherClassifies pins the census's MATCHER against known shapes,
// both directions.
//
// The corpus reading exercises whatever the corpus happens to contain, so it
// cannot tell a matcher that recognises the shape from one that has quietly
// stopped — the count would just drift, and a drifting count with a floor under
// it reads as fine. These cases drive each class explicitly, including the two
// the matcher is most likely to get wrong: the NESTED body (whose named
// reference sits inside the outer span) and the star-in, star-out query (which
// must NOT count, or the census inflates itself with queries that name nothing).
//
// Every positive is a real shape from one of the two corpora; every negative is
// a shape deliberately excluded, with the reason on the case.
func TestStarBodyMatcherClassifies(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		sql  string
		want bool
		why  string
	}{
		{
			name: "star CTE, columns named in SELECT and WHERE",
			sql:  "WITH c AS (SELECT * FROM sb) SELECT id, amt FROM c WHERE grp = 10",
			want: true,
			why:  "id/amt/grp are the star body's columns, named at the outer level",
		},
		{
			name: "star CTE, qualified by the CTE alias (Java alias-tests.yamsql:250)",
			sql:  "WITH cte AS (SELECT * FROM T1) SELECT c.id, c.a FROM cte AS c",
			want: true,
			why:  "a qualified reference to a star-body column is still a name reference",
		},
		{
			name: "star derived table, column in WHERE (Java versions-tests.yamsql:263)",
			sql:  `select "__ROW_VERSION", a.id from (select * from t1) a`,
			want: true,
			why:  "a.id names a star-body column through the derived table's alias",
		},
		{
			name: "NESTED star bodies, column named between them (Java standard-tests.yamsql:284)",
			sql:  "select * from (select * from (select * from T1) as x where ID = 5) as y",
			want: true,
			why: "THE CASE THE FIRST MATCHER MISSED: blanking the OUTER span swallows " +
				"`ID = 5`. Only the innermost body may be blanked",
		},
		{
			name: "star body over an unnest, columns named outside (Java array-join-at.yamsql:84)",
			sql:  `SELECT "id", "val", "at" FROM (SELECT * FROM T1, T1."arr1" AS "val" AT "at") AS subquery`,
			want: true,
			why:  "quoted identifiers must count exactly as bare ones do",
		},
		{
			name: "star in, star out — nothing named",
			sql:  "WITH c1 AS (SELECT * FROM t1) SELECT * FROM c1",
			want: false,
			why: "the outer level names no column, so this query would survive the " +
				"narrowing. Counting it would inflate the census with queries that " +
				"prove nothing",
		},
		{
			name: "star derived table, star out",
			sql:  "SELECT * FROM (SELECT * FROM t1) AS d",
			want: false,
			why:  "`d` is an AS-position alias, not a column reference",
		},
		{
			name: "no star body at all",
			sql:  "WITH c AS (SELECT id, amt FROM t) SELECT id FROM c WHERE amt > 1",
			want: false,
			why:  "an explicit projection list is not a star body — the census must not count it",
		},
		{
			name: "SELECT * in an EXISTS subquery is not a body",
			sql:  "SELECT fname FROM emp WHERE EXISTS (SELECT * FROM project WHERE emp_id = emp.id)",
			want: false,
			why: "a star inside EXISTS projects nothing anyone names; treating it as a " +
				"body would count most of the corpus",
		},
		{
			name: "aggregate over a star body counts its grouping column",
			sql:  "SELECT grp, COUNT(*) FROM (SELECT * FROM t WHERE amt > 1) AS d GROUP BY grp",
			want: true,
			why:  "grp is named outside; COUNT is a function name and must not itself count",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := starBodyColumnNamedOutside(tc.sql); got != tc.want {
				t.Fatalf("starBodyColumnNamedOutside(%q) = %v, want %v\n  why: %s",
					tc.sql, got, tc.want, tc.why)
			}
		})
	}
}
