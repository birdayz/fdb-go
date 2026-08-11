package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// TestFDB_UnnestElementMemberInExists drives a struct-element member reference
// on a lateral array-unnest alias from inside an EXISTS — the shape Java answers
// with rows (valid-identifiers.yamsql:221,226 drive the flat and the two-level
// member on a lateral struct-element alias inside an EXISTS body).
//
// Go raised 42703 on both, because the element's declared struct fields were
// carried onto exactly ONE of the scopes that bind an unnest element (the SELECT
// scope). The other three — the correlated subquery's OUTER scope, a correlated
// subquery's inner JOIN leg, and the correlated PRIMARY unnest inside an EXISTS
// body — minted a fieldless element column, so no descent could enter it. Java
// has no such split: the unnest quantifier's flowed object type IS the element
// type, so the element's fields are the quantifier's own attributes
// (LogicalOperator.generateCorrelatedFieldAccess).
//
// TWO independent directions are covered, and they are separate code:
//
//   - the member INSIDE the EXISTS body over a correlated primary unnest
//     (`EXISTS (SELECT x FROM t.arr AS x WHERE x.ek = …)`), which needs both the
//     scope mint and a translator rebase onto the Explode's flowed element;
//   - the member of an OUTER unnest referenced FROM the EXISTS body
//     (`FROM t, t.arr AS x WHERE EXISTS (… AND x.ek = …)`), which needs the
//     outer-scope mint and the seed bake for a MULTI-ACCESSOR element ref.
//
// The second one is why the row assertions matter more than a plan assertion:
// with the outer-scope mint alone the reference resolved and then silently
// evaluated to zero rows, because the bake and its own safety net both keyed on
// a single-accessor predicate and waved a member ref through.
//
// The controls are load-bearing. A green from a dead family (no rows, no
// unnest, or a gate refusing everything) would be indistinguishable from the
// fix working, so each EXISTS arm is paired with the same reference in a shape
// that never depended on the fix.
func TestFDB_UnnestElementMemberInExists(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	const dbPath = "/testdb_unnest_elem_exists"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE ueie_tmpl "+
			"CREATE TYPE AS STRUCT deep (dk BIGINT) "+
			"CREATE TYPE AS STRUCT elem (ek BIGINT, d deep) "+
			"CREATE TABLE t (id BIGINT, arr elem ARRAY, PRIMARY KEY (id)) "+
			"CREATE TABLE u (id2 BIGINT, tags BIGINT ARRAY, PRIMARY KEY (id2))"); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE ueie_tmpl"); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// t: id=1 carries ek 10 (dk 91) and ek 20 (dk 92); id=2 carries ek 30 (dk 93).
	if _, err := db.ExecContext(ctx,
		"INSERT INTO t VALUES (1, [(10, (91)), (20, (92))]), (2, [(30, (93))])"); err != nil {
		t.Fatalf("INSERT t: %v", err)
	}
	// u is the SCALAR-element twin: its element alias needs no struct fields,
	// so it answered before this change and must still answer after.
	if _, err := db.ExecContext(ctx,
		"INSERT INTO u VALUES (1, [5, 6]), (2, [7])"); err != nil {
		t.Fatalf("INSERT u: %v", err)
	}

	cases := []struct {
		name   string
		sql    string
		want   string
		rearms string
	}{
		// ---- controls: the same references where they always resolved ----
		{
			name: "control_flat_member_outside_exists",
			sql:  "SELECT x.ek FROM t, t.arr AS x ORDER BY x.ek",
			want: "EK|10;20;30",
			rearms: "the SELECT-scope element mint changed; every EXISTS arm below " +
				"is only interpretable against this control",
		},
		{
			name:   "control_two_level_member_outside_exists",
			sql:    "SELECT x.d.dk FROM t, t.arr AS x ORDER BY x.d.dk",
			want:   "DK|91;92;93",
			rearms: "the two-level descent through an unnest element changed",
		},
		{
			// The predicate shape of the outer-scope arms, with the member
			// OUTSIDE the EXISTS. Isolates "the member evaluates" from "the
			// member evaluates under the semi-join".
			name:   "control_two_level_member_beside_exists",
			sql:    "SELECT x.ek FROM t, t.arr AS x WHERE EXISTS (SELECT 1 FROM t AS m WHERE m.id = 1) AND x.d.dk = 91",
			want:   "EK|10",
			rearms: "an unnest member predicate SIBLING to an EXISTS changed",
		},
		{
			// A REAL outer column in the exact predicate shape the outer-scope
			// arms use. Proves the outer-only conjunct route itself is healthy,
			// so an empty result there is about the ELEMENT, not the route.
			name:   "control_real_outer_column_in_exists_body",
			sql:    "SELECT t.id FROM t WHERE EXISTS (SELECT 1 FROM t AS m WHERE m.id = 1 AND t.id = 1)",
			want:   "ID|1",
			rearms: "the outer-only-conjunct route changed; the element arms below measure it",
		},
		{
			// The SCALAR-element twin of the outer-scope arms. Its element ref is
			// single-accessor, so it was already baked correctly; it pins that the
			// multi-accessor widening did not disturb the single-accessor path.
			name:   "control_scalar_element_in_exists_body",
			sql:    "SELECT tg FROM u, u.tags AS tg WHERE EXISTS (SELECT 1 FROM u AS m WHERE m.id2 = 1 AND tg = 5)",
			want:   "TG|5",
			rearms: "the SINGLE-accessor element seed bake changed",
		},

		// ---- direction 1: the member INSIDE a correlated primary unnest ----
		{
			name:   "flat_member_in_exists",
			sql:    "SELECT t.id FROM t WHERE EXISTS (SELECT x FROM t.arr AS x WHERE x.ek = 20)",
			want:   "ID|1",
			rearms: "the correlated-primary unnest element mint or its Explode rebase changed",
		},
		{
			name:   "two_level_member_in_exists",
			sql:    "SELECT t.id FROM t WHERE EXISTS (SELECT x FROM t.arr AS x WHERE x.d.dk = 93)",
			want:   "ID|2",
			rearms: "the two-level descent on a correlated-primary unnest element changed",
		},
		{
			// Both depths in one predicate: the descent must be applied per
			// reference, not once for the whole tree.
			name:   "both_depths_in_exists",
			sql:    "SELECT t.id FROM t WHERE EXISTS (SELECT x FROM t.arr AS x WHERE x.d.dk = 91 AND x.ek = 10)",
			want:   "ID|1",
			rearms: "mixing a flat and a two-level element member in one EXISTS predicate changed",
		},
		{
			name:   "flat_member_in_not_exists",
			sql:    "SELECT t.id FROM t WHERE NOT EXISTS (SELECT x FROM t.arr AS x WHERE x.ek = 20)",
			want:   "ID|2",
			rearms: "NOT EXISTS over a correlated-primary unnest element member changed",
		},

		// ---- direction 2: the OUTER unnest's member, referenced from the body ----
		{
			// The outer-only conjunct: this is the arm that returned ZERO rows
			// silently once the reference resolved.
			name:   "outer_element_flat_member_in_exists_body",
			sql:    "SELECT x.ek FROM t, t.arr AS x WHERE EXISTS (SELECT 1 FROM t AS m WHERE m.id = 1 AND x.ek = 10)",
			want:   "EK|10",
			rearms: "the MULTI-ACCESSOR element seed bake (or its safety net) changed",
		},
		{
			name:   "outer_element_two_level_member_in_exists_body",
			sql:    "SELECT x.ek FROM t, t.arr AS x WHERE EXISTS (SELECT 1 FROM t AS m WHERE m.id = 1 AND x.d.dk = 91)",
			want:   "EK|10",
			rearms: "the two-level MULTI-ACCESSOR element seed bake changed",
		},
		{
			// A genuine CORRELATION conjunct (outer element = inner column),
			// which routes through the join predicate rather than the
			// outer-only lift. A different channel, same reference.
			name:   "outer_element_correlated_conjunct",
			sql:    "SELECT x.ek FROM t, t.arr AS x WHERE EXISTS (SELECT 1 FROM t AS m WHERE m.id * 10 = x.ek) ORDER BY x.ek",
			want:   "EK|10;20",
			rearms: "the join-predicate channel for an outer unnest element changed",
		},
		{
			// A TRUE empty, asserted so the arms above cannot be read as "every
			// element predicate answers something". dk is 91/92/93 and m.id*10
			// is 10/20, so nothing matches.
			name:   "outer_element_two_level_true_empty",
			sql:    "SELECT x.ek FROM t, t.arr AS x WHERE EXISTS (SELECT 1 FROM t AS m WHERE m.id * 10 = x.d.dk)",
			want:   "EK|",
			rearms: "a two-level element correlation that matches NOTHING started matching",
		},

		{
			// A CORRELATED EXISTS whose inner FROM carries the unnest as a JOIN
			// LEG (not the primary). That leg's inner-scope binding is minted by
			// a THIRD site, distinct from the two directions above. o.id*30 is
			// 30 or 60 and the elements are 10/20/30, so only o.id=1 matches —
			// a discriminating value, not a blanket true.
			name:   "inner_join_leg_element_member_in_correlated_exists",
			sql:    "SELECT o.id FROM t AS o WHERE EXISTS (SELECT 1 FROM t AS m, m.arr AS x WHERE x.ek = o.id * 30)",
			want:   "ID|1",
			rearms: "the correlated-EXISTS inner JOIN-LEG unnest element mint changed",
		},

		// ---- a live divergence this change does NOT close ----
		{
			// Java answers this with rows (`select * from t."level0.field1" as x
			// where …`, valid-identifiers.yamsql:221). Go refuses the correlated
			// primary unnest unless the EXISTS body projects exactly the bare
			// element alias, so `SELECT *` / `SELECT 1` / `SELECT x.ek` are all
			// declined. That gate is upstream of everything this test measures —
			// it is the reason the arms above spell the projection `SELECT x` —
			// and it is a loud refusal, never a wrong answer.
			name:   "projection_gate_still_refuses_star_divergence_sentinel",
			sql:    "SELECT t.id FROM t WHERE EXISTS (SELECT * FROM t.arr AS x WHERE x.ek = 20)",
			want:   "ERROR: 0AF00: correlated array EXISTS currently requires projecting its element alias",
			rearms: "the correlated-primary EXISTS projection gate was widened; assert rows (ID|1) here, matching Java",
		},
	}

	for i := range cases {
		tc := cases[i]
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := runShape(t, ctx, db, tc.sql); got != tc.want {
				t.Fatalf("query %q\n   got: %s\n  want: %s\n  RE-ARMED IF THIS CHANGES: %s",
					tc.sql, got, tc.want, tc.rearms)
			}
		})
	}
}

// TestFDB_UnnestElementMemberInExistsConvertedSentinel is the CONVERSION of a
// divergence sentinel that used to pin `42703` for a struct-element member
// reference inside an EXISTS body. Its own instructions were to convert it —
// never delete it — the day the refusal lifted, because it is the only pin on
// the element-bake site the refusal was keeping unreachable. Both halves landed
// together, so it now asserts the rows Java returns.
//
// Kept BESIDE the table-driven test above rather than folded into it, because
// its shape is genuinely different and neither subsumes the other: a three-leg
// outer (`t JOIN v ON …, t.arr AS x`) with the element member correlated to a
// SEPARATE inner table (`EXISTS (SELECT 1 FROM u WHERE u.uk = x.d.dk)`). That is
// the buried-conjunct channel; the arms above drive the outer-only and
// correlated-primary channels.
//
// The anti-vacuity arm it was built around is retained in purpose: a bare SCALAR
// element through the same buried-conjunct path must answer first, so a green
// here cannot come from a family that stopped planning.
func TestFDB_UnnestElementMemberInExistsConvertedSentinel(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_uelem")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_uelem")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE uelem_tmpl "+
		"CREATE TYPE AS STRUCT deep (dk BIGINT) "+
		"CREATE TYPE AS STRUCT elem (ek BIGINT, d deep) "+
		"CREATE TABLE t (id BIGINT, sarr BIGINT ARRAY, arr elem ARRAY, PRIMARY KEY(id)) "+
		"CREATE TABLE v (vid BIGINT, vk BIGINT, PRIMARY KEY(vid)) "+
		"CREATE TABLE u (uk BIGINT, PRIMARY KEY(uk))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_uelem/s WITH TEMPLATE uelem_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_uelem?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mustExec(t, db, ctx, "INSERT INTO t VALUES (1, [100, 200], [(10, (100)), (20, (200))]), (2, [300], [(30, (300))])")
	mustExec(t, db, ctx, "INSERT INTO v VALUES (1, 7), (2, 8)")
	// uk=10 is ADDED BY THE CONVERSION. The original data made the FLAT arm's
	// converted answer EMPTY (ek is 10/20/30, uk was 100/300), and an arm that
	// asserts an empty result cannot distinguish "resolves and matches nothing"
	// from "silently drops every row" — which is the exact failure mode the
	// element bake had. 10 matches ek only: it is absent from sarr (100/200/300)
	// and from the dk values (100/200/300), so the other two arms are unchanged.
	mustExec(t, db, ctx, "INSERT INTO u VALUES (10), (100), (300)")

	run := func(q string) ([]string, error) {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var a sql.NullInt64
			if err := rows.Scan(&a); err != nil {
				return nil, err
			}
			if a.Valid {
				out = append(out, fmt.Sprint(a.Int64))
			} else {
				out = append(out, "NULL")
			}
		}
		return out, rows.Err()
	}

	// --- the channel is LIVE (anti-vacuity: a green must not come from a family
	// that stopped planning).
	const bareElem = "SELECT t.id FROM t JOIN v ON t.id = v.vid, t.sarr AS x " +
		"WHERE EXISTS (SELECT 1 FROM u WHERE u.uk = x) ORDER BY t.id"
	got, err := run(bareElem)
	if err != nil {
		t.Fatalf("the buried-conjunct element channel no longer plans, so every arm "+
			"below is vacuous — this test would report success because NOTHING "+
			"runs.\n  %s\n  %v", bareElem, err)
	}
	if strings.Join(got, ",") != "1,2" {
		t.Errorf("bare scalar element through the buried conjunct = %v, want [1 2]", got)
	}

	// --- members resolve outside the EXISTS. Retained as the control it always
	// was: it is what says an arm below failing is about the EXISTS scope rather
	// than about struct elements generally.
	const outsideExists = "SELECT x.d.dk FROM t, t.arr AS x ORDER BY x.d.dk"
	nested, err := run(outsideExists)
	if err != nil {
		t.Errorf("a NESTED element member must still resolve outside an EXISTS.\n  %s\n  %v",
			outsideExists, err)
	} else if strings.Join(nested, ",") != "100,200,300" {
		t.Errorf("nested element projection = %v, want [100 200 300]", nested)
	}

	// --- and BOTH member forms now ANSWER inside the EXISTS, flat and nested.
	// These are the converted arms. They assert VALUES, not absence-of-error:
	// the way this breaks is a silent empty, so "no error" would pass with the
	// defect fully present.
	for _, tc := range []struct{ name, query, want string }{
		{
			// uk=10 matches ek=10, which belongs to t.id=1 only.
			"flat member",
			"SELECT t.id FROM t JOIN v ON t.id = v.vid, t.arr AS x " +
				"WHERE EXISTS (SELECT 1 FROM u WHERE u.uk = x.ek) ORDER BY t.id",
			"1",
		},
		{
			// uk=100 matches dk=100 (t.id=1) and uk=300 matches dk=300 (t.id=2).
			"nested member",
			"SELECT t.id FROM t JOIN v ON t.id = v.vid, t.arr AS x " +
				"WHERE EXISTS (SELECT 1 FROM u WHERE u.uk = x.d.dk) ORDER BY t.id",
			"1,2",
		},
	} {
		rows, err := run(tc.query)
		if err != nil {
			t.Errorf("%s: an element member inside an EXISTS must RESOLVE — this is the "+
				"Java-conformant behaviour (valid-identifiers.yamsql:221,226 return rows "+
				"for both forms). A 42703 here is the divergence reopening.\n  %s\n  %v",
				tc.name, tc.query, err)
			continue
		}
		if strings.Join(rows, ",") != tc.want {
			t.Errorf("%s = %v, want [%s].\nAn EMPTY result here is the SILENT direction: "+
				"the element reference resolved and then failed to bake onto the seed, so "+
				"the comparison went NULL and EXISTS dropped every row. Check that "+
				"bakeUnnestElementRefOrdinal still keys on RootIsLegRelativeUnpinned() and "+
				"still fuses Accessors[1:].", tc.name, rows, tc.want)
		}
	}
}
