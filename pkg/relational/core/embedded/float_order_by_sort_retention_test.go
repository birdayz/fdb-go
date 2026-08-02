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
// The second dimension is MULTI-COLUMN. A float coordinate that is ordered
// only because all NaNs are logically tied orders itself and scrambles its
// successors: within the tie the next column comes back in NaN-payload order.
// So `ORDER BY v` over a float index may drop its sort, while `ORDER BY v, w`
// may not.
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
			wantSort: false,
			why:      "an index scan binds a range set unconditionally and returns NULL/numbers/NaN blocks in logical order",
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
