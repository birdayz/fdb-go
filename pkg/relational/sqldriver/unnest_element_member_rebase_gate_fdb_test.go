package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// TestFDB_UnnestElementMemberRebaseGate pins that the correlated-primary unnest's
// member-rebase arm selects on the ROOT ORDINAL alone, and that dropping the name
// comparison it used to also carry is behaviour-neutral.
//
// WHY THIS EXISTS AS A TEST RATHER THAN AN ARGUMENT. The arm originally read
//
//	len(Accessors) > 1 && Accessors[0].Ordinal == 0 &&
//	    strings.EqualFold(Accessors[0].Field, asAlias)
//
// and the EqualFold clause was removed, because the EXISTS scope's unnest source
// is a ONE-COLUMN virtual table — slot 0 IS the element, so the name adds no
// discrimination — and because a DISPLAY name deciding a binding is the failure
// this tree pins against. That reasoning is sound and it was still only an
// argument: a deleted discriminator with nothing pinning it is exactly the shape
// that silently comes back. This is the pin, in both directions.
//
// THE ADVERSARIAL SHAPE, which is the whole point. The outer table puts a STRUCT
// at ORDINAL 0, so `t.s.k` is a TWO-accessor path ROOTED AT ORDINAL 0 — the exact
// shape the gate now admits on ordinal evidence alone, differing from the element
// only in its name and its scope. It is also a live decoy rather than a mere
// look-alike: on a misrebase the path's remainder descends into the element
// instead, and accessor 1 lands on the element's OWN slot 0, which is `ek`. So a
// misrebase does not fail loudly — it reads `ek` where `s.k` was written.
//
// The data crosses the two values deliberately: row id=1 has s.k=99 and ek=10,
// row id=2 has s.k=10 and ek=30. So `WHERE t.s.k = 10` is true for id=2 only,
// and no answer that reads the element instead can also be `ID|2`.
//
// BOTH DIRECTIONS ARE MEASURED, because a probe that only passes proves nothing
// about what it would catch:
//
//   - REINSTATING the deleted `EqualFold(Accessors[0].Field, asAlias)` changes
//     NOTHING — all four unnest-member tests stay green (30 `=== RUN` lines).
//     The removal is behaviour-neutral, which is the claim this file exists to
//     pin.
//   - SIMULATING a misrebase (letting childful refs reach the member arm)
//     reddens this arm and `element_member_and_decoy_in_one_predicate`. So the
//     arms are live and discriminating, not inert passes.
//
// WHAT ACTUALLY EXCLUDES THE DECOY IS `fv.Child == nil`, NOT THE NAME — that is
// why removing the name check is safe, and it is the fact worth carrying: a
// correlated outer reference resolves through the parent scope and therefore
// carries a QOV child, while the element binding is childless in the EXISTS
// scope's one-column virtual table. The arity and ordinal clauses narrow within
// that; the childless-ness is what separates the two families.
func TestFDB_UnnestElementMemberRebaseGate(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	const dbPath = "/testdb_unnest_rebase_gate"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	// `souter` is declared FIRST in t so it occupies ORDINAL 0 — that placement is
	// load-bearing, not cosmetic. At any other ordinal `t.s.k` stops matching the
	// gate's `Accessors[0].Ordinal == 0` clause and the decoy stops being a decoy.
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE uerg_tmpl "+
			"CREATE TYPE AS STRUCT souter (k BIGINT) "+
			"CREATE TYPE AS STRUCT deep (dk BIGINT) "+
			"CREATE TYPE AS STRUCT elem (ek BIGINT, d deep) "+
			"CREATE TABLE t (s souter, id BIGINT, arr elem ARRAY, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE uerg_tmpl"); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// id=1: s.k=99, elements ek 10/20.   id=2: s.k=10, element ek 30.
	// The CROSSED values are deliberate — s.k=10 sits on the row whose elements
	// do NOT contain 10, and ek=10 sits on the row whose s.k is not 10.
	if _, err := db.ExecContext(ctx,
		"INSERT INTO t VALUES ((99), 1, [(10, (91)), (20, (92))]), ((10), 2, [(30, (93))])"); err != nil {
		t.Fatalf("INSERT t: %v", err)
	}

	cases := []struct {
		name   string
		sql    string
		want   string
		rearms string
	}{
		{
			// CONTROL: the decoy path resolves and reads correctly outside any
			// EXISTS. If this moves, the arms below are not about the rebase.
			name:   "control_outer_struct_member_outside_exists",
			sql:    "SELECT t.s.k FROM t ORDER BY t.s.k",
			want:   "K|10;99",
			rearms: "the outer STRUCT-at-ordinal-0 member stopped reading correctly at all",
		},
		{
			// THE ADVERSARIAL ARM. A two-accessor path rooted at ordinal 0 that is
			// NOT the element, referenced from inside the EXISTS body.
			name: "outer_struct_member_at_ordinal_zero_is_not_rebased_onto_element",
			sql:  "SELECT t.id FROM t WHERE EXISTS (SELECT x FROM t.arr AS x WHERE t.s.k = 10)",
			want: "ID|2",
			rearms: "THE MEMBER-REBASE ARM HAS CLAIMED A REFERENCE THAT IS NOT THE " +
				"ELEMENT. Any answer but `ID|2` means `t.s.k` was read against the " +
				"element row instead of the outer one. MEASURED under a simulated " +
				"misrebase (letting childful refs reach the arm): this arm reports " +
				"`ID|`, not `ID|1` — the descent lands on the element and yields no " +
				"match rather than the ek=10 collision one might predict, so do not " +
				"look for a specific wrong value, look for any departure from ID|2. " +
				"The fix is a discriminator stronger than the root ordinal — " +
				"identity, never the display name.",
		},
		{
			// The element member must still rebase — the arm has to keep firing
			// for what it is for, or the arm above passes by doing nothing.
			name:   "element_member_still_rebases",
			sql:    "SELECT t.id FROM t WHERE EXISTS (SELECT x FROM t.arr AS x WHERE x.ek = 20)",
			want:   "ID|1",
			rearms: "the member-rebase arm stopped firing; the decoy arm above then proves nothing",
		},
		{
			// BOTH in one predicate: the element member and the ordinal-0 decoy
			// must be routed differently within a single walk, which is where a
			// gate that keys on the wrong evidence collapses them.
			name:   "element_member_and_decoy_in_one_predicate",
			sql:    "SELECT t.id FROM t WHERE EXISTS (SELECT x FROM t.arr AS x WHERE x.ek = 20 AND t.s.k = 99)",
			want:   "ID|1",
			rearms: "the element member and an ordinal-0 outer struct member stopped being routed separately",
		},
		{
			// The complement of the arm above: same shape, contradictory
			// conjuncts, so a walk that collapsed the two references into one
			// would answer a row here.
			name: "element_member_and_decoy_contradictory",
			sql:  "SELECT t.id FROM t WHERE EXISTS (SELECT x FROM t.arr AS x WHERE x.ek = 10 AND t.s.k = 10)",
			want: "ID|",
			rearms: "a CONTRADICTORY pair (ek=10 holds only on id=1, s.k=10 only on id=2) " +
				"started answering — the two references are being read against the same row",
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
