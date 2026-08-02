package embedded_test

import (
	"strings"
	"testing"

	"fdb.dev/pkg/relational/core/embedded"
)

// TestUnpredicatedFloatOrderByRetainsItsSort pins the shape that no predicated
// test can reach.
//
// Dropping a sort because the sort keys match the key columns by NAME proves
// the scan visits the right columns, not that it visits them in the order the
// query asked for. Those differ on a raw FLOAT/DOUBLE key: FDB tuple order puts
// the negative NaNs below -Inf, while the comparator makes every NaN greatest.
//
// The dimension here is the ABSENCE of a predicate. A query carrying
// `WHERE ... = ...` binds scan comparisons and routes through the range-set
// path, which splits the NaN blocks and is genuinely ordered. With no
// predicate a primary scan binds no range set at all, so nothing splits them
// and the sort has to stay. Every existing raw-NaN ordering test was
// predicated, so this case looked covered while being wrong.
//
// The second dimension is MULTI-COLUMN, and it is why the single-column case
// below is no longer worth buying: a float coordinate ordered only because all
// NaNs are logically tied orders itself and scrambles its successors, so
// splitting the coordinate could never help `ORDER BY v, w` — only the narrow
// `ORDER BY v`, which nothing measured.
func TestUnpredicatedFloatOrderByRetainsItsSort(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		ddl      string
		sql      string
		wantSort bool
		why      string
	}{
		{
			name:     "double primary key",
			ddl:      "CREATE TABLE TF (f DOUBLE NOT NULL, w BIGINT, PRIMARY KEY (f))",
			sql:      "SELECT f FROM TF ORDER BY f",
			wantSort: true,
			why:      "an unpredicated primary scan binds no range set, so the NaN blocks are never split",
		},
		{
			name:     "bigint primary key control",
			ddl:      "CREATE TABLE TL (i BIGINT NOT NULL, w BIGINT, PRIMARY KEY (i))",
			sql:      "SELECT i FROM TL ORDER BY i",
			wantSort: false,
			why:      "an integer key has no NaN region; this must keep eliding or the fix is a blanket pessimisation",
		},
		{
			name:     "double index, single column",
			ddl:      "CREATE TABLE TI (id BIGINT NOT NULL, v DOUBLE, w BIGINT, PRIMARY KEY (id)) CREATE INDEX IDXV ON TI (v)",
			sql:      "SELECT v FROM TI ORDER BY v",
			wantSort: true,
			why: "the DELIBERATE loss. The scan could split this coordinate into NULL/numbers/" +
				"the two NaN blocks and drop the sort, and briefly did; that cost four range " +
				"opens on every float-suffixed scan and changed 0 of 2489 golden plans, so it " +
				"was removed. Master does not elide it either. If this ever flips to false, the " +
				"split is back and needs a measurable benefit to justify its cost",
		},
		{
			name:     "double index, two columns",
			ddl:      "CREATE TABLE TJ (id BIGINT NOT NULL, v DOUBLE, w BIGINT, PRIMARY KEY (id)) CREATE INDEX IDXVW ON TJ (v, w)",
			sql:      "SELECT v, w FROM TJ ORDER BY v, w",
			wantSort: true,
			why:      "all NaN payloads are one logical value across many physical keys, so w is in payload order inside that tie",
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			plan, err := embedded.PlanPhysicalForTest(test.sql, test.ddl, nil)
			if err != nil {
				t.Fatalf("plan %q: %v", test.sql, err)
			}
			explained := plan.Explain()
			gotSort := strings.Contains(explained, "InMemorySort")
			if gotSort != test.wantSort {
				verb := "dropped its sort"
				if gotSort {
					verb = "kept a sort"
				}
				t.Fatalf("%s\n%s\nplan: %s\nwantSort=%v — %s",
					test.sql, verb, explained, test.wantSort, test.why)
			}
		})
	}
}
