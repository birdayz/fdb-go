package embedded

import (
	"errors"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// TestDottedIdentifierGapsArePinned pins what Go answers today for a quoted
// identifier containing '.', the shapes Java's valid-identifiers.yamsql
// answers. Each GAP row is a known gap pinned as measured, so the fix
// reddens it on purpose: a row that starts planning, or plans without the
// sort, is the fix landing, and the row flips to the plan Java's shape asks
// for, with the TODO entry ("Quoted identifiers holding dots") updated. A row that changes any other way is a new
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
		sortFree        bool // the plan must not sort in memory
	}{
		{
			"an aggregate grouped by a dotted column plans", dotted,
			`SELECT SUM("foo.tableA.A1") FROM "foo.tableA" GROUP BY "foo.tableA.A2"`,
			[]string{"StreamingAgg"},
			"",
			false,
		},
		{
			"the grouped dotted column projected", dotted,
			`SELECT "foo.tableA.A2", COUNT(*) FROM "foo.tableA" GROUP BY "foo.tableA.A2"`,
			[]string{"StreamingAgg"},
			"",
			false,
		},
		{
			"GAP: the aliased form", dotted,
			`SELECT t."foo.tableA.A2", SUM(t."foo.tableA.A1") FROM "foo.tableA" AS t GROUP BY t."foo.tableA.A2"`, nil, api.ErrCodeUndefinedColumn, false,
		},
		{
			"a HAVING over the dotted aggregate", dotted,
			`SELECT "foo.tableA.A2" AS k, MAX("foo.tableA.A3") FROM "foo.tableA" GROUP BY "foo.tableA.A2" HAVING MAX("foo.tableA.A3") > 1`,
			[]string{"StreamingAgg"},
			"",
			false,
		},
		// Was a GAP: the primary-key column names the planner resolved against
		// the row layout were the STORED spelling x__2a1, the layout names the
		// field x.a1, and the scan's ordering never resolved. The names are
		// decoded where metadata enters the planner (layoutNames,
		// coveredPrimaryKeyColumns), as Java's ScalarTranslationVisitor reads
		// toUserIdentifier.
		{
			"ORDER BY a dotted primary key is served by the scan", dottedColumn,
			`SELECT "x.a1" FROM ta ORDER BY "x.a1"`,
			[]string{"Scan(TA)"},
			"",
			true,
		},
		{
			"control: ORDER BY an undotted primary key is served by the scan", plain,
			`SELECT a1 FROM ta ORDER BY a1`,
			[]string{"Scan(TA)"},
			"",
			true,
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
			if c.sortFree && strings.Contains(p, "InMemorySort") {
				t.Fatalf("%s: plan sorts in memory: %s", c.q, p)
			}
		})
	}
}
