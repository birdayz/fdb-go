package embedded

import (
	"errors"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// TestDottedIdentifierGapsArePinned pins what Go answers today for a quoted
// identifier containing '.', the shapes Java's valid-identifiers.yamsql
// answers (TODO.md "A quoted identifier containing `.` breaks aggregate and
// GROUP BY queries"). Each row is a KNOWN GAP pinned as measured, so the fix
// reddens it on purpose: a row that starts planning, or plans without the
// sort, is the fix landing, and the row flips to the plan Java's shape asks
// for, with the TODO entry updated. A row that changes any other way is a new
// regression.
func TestDottedIdentifierGapsArePinned(t *testing.T) {
	t.Parallel()
	const dotted = `CREATE SCHEMA TEMPLATE x create table "foo.tableA"("foo.tableA.A1" bigint, "foo.tableA.A2" bigint, "foo.tableA.A3" bigint, primary key("foo.tableA.A1"))`
	const dottedColumn = `CREATE SCHEMA TEMPLATE x create table ta("x.a1" bigint, a2 bigint, primary key("x.a1"))`
	const plain = `CREATE SCHEMA TEMPLATE x create table ta(a1 bigint, a2 bigint, primary key(a1))`
	for _, c := range []struct {
		name, schema, q string
		plan            []string // substrings the plan holds, when it plans
		code            api.ErrorCode
	}{
		{
			"an aggregate grouped by a dotted column plans", dotted,
			`SELECT SUM("foo.tableA.A1") FROM "foo.tableA" GROUP BY "foo.tableA.A2"`,
			[]string{"StreamingAgg"},
			"",
		},
		{
			"GAP: the grouped dotted column projected", dotted,
			`SELECT "foo.tableA.A2", COUNT(*) FROM "foo.tableA" GROUP BY "foo.tableA.A2"`, nil, api.ErrCodeUndefinedColumn,
		},
		{
			"GAP: the aliased form", dotted,
			`SELECT t."foo.tableA.A2", SUM(t."foo.tableA.A1") FROM "foo.tableA" AS t GROUP BY t."foo.tableA.A2"`, nil, api.ErrCodeUndefinedColumn,
		},
		{
			"GAP: a HAVING over the dotted aggregate", dotted,
			`SELECT "foo.tableA.A2" AS k, MAX("foo.tableA.A3") FROM "foo.tableA" GROUP BY "foo.tableA.A2" HAVING MAX("foo.tableA.A3") > 1`, nil, api.ErrCodeUndefinedColumn,
		},
		{
			"GAP: ORDER BY a dotted primary key sorts in memory", dottedColumn,
			`SELECT "x.a1" FROM ta ORDER BY "x.a1"`,
			[]string{"InMemorySort"},
			"",
		},
		{
			"control: ORDER BY an undotted primary key is served by the scan", plain,
			`SELECT a1 FROM ta ORDER BY a1`,
			[]string{"Scan(TA)"},
			"",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			p, err := PlanQueryForTest(c.q, c.schema, nil)
			if c.code != "" {
				var ae *api.Error
				if !errors.As(err, &ae) || ae.Code != c.code {
					t.Fatalf("%s: got plan %q, err %v; pinned %s (a change here is the fix landing or a new regression)", c.q, p, err, c.code)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s: %v", c.q, err)
			}
			for _, want := range c.plan {
				if !strings.Contains(p, want) {
					t.Fatalf("%s: plan %s, pinned to hold %q", c.q, p, want)
				}
			}
			if strings.HasPrefix(c.name, "control") && strings.Contains(p, "InMemorySort") {
				t.Fatalf("control plan sorts: %s", p)
			}
		})
	}
}
