package factory_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/relational/conformance/rowdiff"
)

// TestFDB_NestedCorpusCollisionIsAuthored is the corpus's own non-differential
// regression net for the defect class it exists to catch.
//
// Every one of the 150 committed nested scenarios is blessed by `tlp` and
// `second-plan`, and BOTH are differential. Neither can see a uniform
// wrong-leaf read in the nested field accessor:
//
//   - TLP partitions the WHERE across four renderings while the SELECT LIST is
//     identical in all four, so a projection that reads the wrong leaf reads it
//     wrongly and identically on every side of the partition and the partition
//     still reassembles;
//   - second-plan varies the access path, but genNestedTable deliberately keeps
//     nested paths OUT of the index pool (a nested-path index is refused at
//     DDL), so both plans deserialize the struct through the same accessor and
//     agree with each other about the wrong value.
//
// So the exact shape the nested work exists for — `A`, `N.A` and `N.DP.A`
// sharing a last segment, where "a wrong-column read lived exactly here" — is
// the one the corpus's own oracles are structurally blind to. Until now it was
// covered only by nested_axis_fdb_test.go, over a DIFFERENT schema (`nt`, three
// hand-written rows). That is real coverage but it is borrowed: nothing tied
// T_RDN, the schema the 150 committed scenarios actually run against, to an
// expectation a human wrote down.
//
// This is that pin, and it is AUTHORED on purpose. The rows are written here,
// not derived from an oracle, so no engine bug can move both the expectation
// and the result the same way. The values are chosen so a wrong-column read
// cannot look like a right one: each depth lives in its own decade, so `A`, the
// depth-2 `N.A` and the depth-3 `N.DP.A` of one row differ by two orders of
// magnitude and any confusion between them is unmistakable in the failure
// message rather than being a plausible-looking number.
func TestFDB_NestedCorpusCollisionIsAuthored(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// The schema comes from the GENERATOR, not from a copy of it written here.
	// A hand-copied DDL would keep passing after genNestedTable changed shape,
	// which is precisely the drift this is supposed to catch: the pin has to be
	// over the table the committed corpus runs against.
	c := rowdiff.GenerateNested(1)
	if !c.Table.IsNested() || c.Table.Name != "T_RDN" {
		t.Fatalf("the nested generator no longer produces T_RDN with a struct column: %+v", c.Table)
	}
	db := openFactorySchema(t, ctx, "zznestcollide", c.DDL())

	// Physical column order is (id, a, n, c, s, f, d) with the struct literal
	// positional in declaration order (n.a, n.b, n.s, (n.dp.a, n.dp.s)).
	const insert = "INSERT INTO T_RDN VALUES " +
		"(1, 10, (100, 1000, 'sn100', (10000, 'sd10000')), 1, 'sf1', true, 1.5), " +
		"(2, 20, (200, 2000, 'sn200', (20000, 'sd20000')), 2, 'sf2', false, 2.5), " +
		"(3, 30, (300, 3000, 'sn300', (30000, 'sd30000')), 3, 'sf3', true, 3.5)"
	if _, err := db.ExecContext(ctx, insert); err != nil {
		t.Fatalf("insert: %v", err)
	}

	for _, tc := range []struct {
		name  string
		query string
		want  []string
	}{
		{
			// THE collision projection: the committed corpus's `collision`
			// projection variant, {ID, A, N.A, N.DP.A}, over the schema it runs
			// against. Three columns share the last segment `A` at three depths.
			name: "collision projection, three depths of one last segment",
			query: "SELECT id, a AS c_a, n.a AS c_n_a, n.dp.a AS c_n_dp_a " +
				"FROM T_RDN ORDER BY id",
			want: []string{"1|10|100|10000", "2|20|200|20000", "3|30|300|30000"},
		},
		{
			// The same collision on the STRING domain, which reaches the other
			// deserialization path. `S`, `N.S` and `N.DP.S` share a last segment
			// exactly as the BIGINT trio does.
			name: "collision projection on the string leaves",
			query: "SELECT id, s AS c_s, n.s AS c_n_s, n.dp.s AS c_n_dp_s " +
				"FROM T_RDN ORDER BY id",
			want: []string{"1|sf1|sn100|sd10000", "2|sf2|sn200|sd20000", "3|sf3|sn300|sd30000"},
		},
		{
			// Two leaves of ONE struct root, adjacent — the duplicate-label
			// shape. Aliased, because unaliased these come back as two columns
			// named A and the name-keyed comparison collapses them.
			name:  "two leaves of one root",
			query: "SELECT id, n.a AS c_n_a, n.b AS c_n_b FROM T_RDN ORDER BY id",
			want:  []string{"1|100|1000", "2|200|2000", "3|300|3000"},
		},
		{
			// Reversed against the table's declaration order, which is what
			// catches an output list built from the TABLE rather than from the
			// QUERY. With the decades above, a table-ordered result would read
			// `10000|100|10` and be obvious.
			name: "reversed against declaration order",
			query: "SELECT id, n.dp.a AS c_n_dp_a, n.a AS c_n_a, a AS c_a " +
				"FROM T_RDN ORDER BY id",
			want: []string{"1|10000|100|10", "2|20000|200|20", "3|30000|300|30"},
		},
		{
			// The collision read through a WHERE on the deepest leaf, so the
			// residual filter and the projection have to agree about which `A`
			// they mean. If they disagree this returns the wrong ROW, not just
			// the wrong column.
			name: "collision under a depth-3 predicate",
			query: "SELECT id, a AS c_a, n.a AS c_n_a, n.dp.a AS c_n_dp_a " +
				"FROM T_RDN WHERE n.dp.a = 20000 ORDER BY id",
			want: []string{"2|20|200|20000"},
		},
	} {
		tc := tc
		// Deliberately NOT t.Parallel(): a parallel subtest runs after its
		// parent returns, and the parent's deferred cancel would have torn the
		// context down first — every arm then fails with "context canceled",
		// which looks exactly like an engine refusal.
		t.Run(tc.name, func(t *testing.T) {
			got, err := queryRowStrings(ctx, db, tc.query)
			if err != nil {
				t.Fatalf("%s\n  %v", tc.query, err)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("WRONG ROWS — the authored expectation and the engine disagree, and this expectation "+
					"was written by hand rather than by an oracle, so the engine is what moved.\n  %s\n  want %v\n  got  %v",
					tc.query, tc.want, got)
			}
		})
	}
}
