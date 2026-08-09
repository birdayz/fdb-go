package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// TestFDB_UnqualifiedRefBesideAProjectedExistsOverAJoin locates a live
// execution failure by DIFFERENTIAL, and its point is as much what it EXCLUDES
// as what it reproduces.
//
// The shape was escalated as a struct-typed-column defect: "the positional
// merge does not model a struct-typed column", to be fixed by carrying a real
// record type where Go erases one. THE STRUCT IS NOT INVOLVED. Case
// unqualified_scalar_join_projectedExists below fails identically on a plain
// BIGINT, and case unqualified_structRoot_join_projectedExists's qualified twin
// passes on the very same struct column. A fix aimed at the type would have
// left the defect standing while looking principled.
//
// What actually discriminates is the QUALIFIER. Every case here holds three of
// the four factors fixed and moves one:
//
//	qualified   + projected EXISTS + join -> OK      (both types)
//	UNQUALIFIED + projected EXISTS + join -> FAILS   (both types)
//	UNQUALIFIED + projected EXISTS + NO join -> OK
//	UNQUALIFIED + NO projected EXISTS + join -> OK
//	UNQUALIFIED multi-accessor + projected EXISTS + join -> OK
//
// So the failure needs all of: an unqualified reference, a SINGLE accessor, a
// PROJECTED EXISTS, and a JOIN. Drop any one and it goes away. An EXISTS in
// WHERE rather than in the SELECT list is not enough either.
//
// The passing cases are CONTROLS, not decoration: a decline whose control also
// fails is uninterpretable, and each failing case here has a control that
// differs from it in exactly one factor.
//
// Every case asserts the COLUMN VALUES in order — the escalation that produced
// this investigation reported a wrong first column, and a row COUNT reads
// identically whether the column is right or wrong. Order is asserted too
// because these fixtures are deterministic; the claim is that a count alone is
// insufficient, not that order is unchecked.
func TestFDB_UnqualifiedRefBesideAProjectedExistsOverAJoin(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_unqual_pe")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_unqual_pe")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE unqual_pe_tmpl "+
		"CREATE TYPE AS STRUCT nst (sk BIGINT, co BIGINT) "+
		"CREATE TABLE t1(id BIGINT, n nst, PRIMARY KEY(id)) "+
		"CREATE TABLE t2(id BIGINT, t1_id BIGINT, PRIMARY KEY(id)) "+
		"CREATE TABLE t3(id BIGINT, t1_id BIGINT, PRIMARY KEY(id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_unqual_pe/s WITH TEMPLATE unqual_pe_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_unqual_pe?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mustExec(t, db, ctx, "INSERT INTO t1 VALUES (1, (50, 1)), (2, (40, 2)), (3, (30, 3))")
	mustExec(t, db, ctx, "INSERT INTO t2 VALUES (100, 1), (200, 3)")
	mustExec(t, db, ctx, "INSERT INTO t3 VALUES (900, 1), (901, 2), (902, 3)")

	const exists = "EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS h"
	const join = " FROM t1 JOIN t3 ON t3.t1_id = t1.id"

	// t1_id is a plain BIGINT and belongs only to t3, so it can be written
	// unqualified. n is the struct root. The pairs below differ ONLY in whether
	// the second projected column carries a table qualifier.
	cases := []struct {
		name string
		q    string
		// want is the full rendered result. Every arm here now has one: the
		// three that used to be tripwires on the unresolvable-ordinal failure
		// assert the values their qualified twins return, which is exactly the
		// replacement their tripwire messages named.
		want string
	}{
		// --- THE TWO THAT USED TO FAIL, on DIFFERENT column types. Each is
		// byte-for-byte its qualified twin below, which is the claim: bare and
		// qualified now resolve by one rule. ---
		{
			name: "unqualified_scalar_join_projectedExists",
			q:    "SELECT t1.id, t1_id, " + exists + join,
			want: "[[1 1 true] [2 2 false] [3 3 true]]",
		},
		{
			name: "unqualified_structRoot_join_projectedExists",
			q:    "SELECT t1.id, n, " + exists + join,
			want: "[[1 struct[50 1] true] [2 struct[40 2] false] [3 struct[30 3] true]]",
		},
		// --- their qualified twins: ONE factor moved, both pass ---
		{
			name: "CONTROL_qualified_scalar_join_projectedExists",
			q:    "SELECT t1.id, t3.t1_id, " + exists + join,
			want: "[[1 1 true] [2 2 false] [3 3 true]]",
		},
		{
			name: "CONTROL_qualified_structRoot_join_projectedExists",
			q:    "SELECT t1.id, t1.n, " + exists + join,
			want: "[[1 struct[50 1] true] [2 struct[40 2] false] [3 struct[30 3] true]]",
		},
		// --- drop the projected EXISTS: both pass unqualified ---
		{
			name: "CONTROL_unqualified_scalar_join_noExists",
			q:    "SELECT t1.id, t1_id" + join,
			want: "[[1 1] [2 2] [3 3]]",
		},
		{
			name: "CONTROL_unqualified_structRoot_join_noExists",
			q:    "SELECT t1.id, n" + join,
			want: "[[1 struct[50 1]] [2 struct[40 2]] [3 struct[30 3]]]",
		},
		// A third projected column that is NOT an EXISTS also passes, so it is
		// the EXISTS and not the arity of the SELECT list.
		{
			name: "CONTROL_unqualified_structRoot_join_thirdColumnNotExists",
			q:    "SELECT t1.id, n, 1 AS h" + join,
			want: "[[1 struct[50 1] 1] [2 struct[40 2] 1] [3 struct[30 3] 1]]",
		},
		// An EXISTS in WHERE rather than projected also passes, so it is the
		// PROJECTION of the EXISTS that matters, not its presence.
		{
			name: "CONTROL_unqualified_structRoot_join_existsInWhereNotProjected",
			q: "SELECT t1.id, n" + join +
				" WHERE EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id)",
			want: "[[1 struct[50 1]] [3 struct[30 3]]]",
		},
		// --- drop the join: passes unqualified ---
		{
			name: "CONTROL_unqualified_structRoot_noJoin_projectedExists",
			q:    "SELECT t1.id, n, " + exists + " FROM t1",
			want: "[[1 struct[50 1] true] [2 struct[40 2] false] [3 struct[30 3] true]]",
		},
		// --- a MULTI-ACCESSOR unqualified reference passes, which is why the
		// failure is specific to a SINGLE accessor and not to unqualified
		// references in general ---
		{
			name: "CONTROL_unqualified_structMember_join_projectedExists",
			q:    "SELECT t1.id, n.sk, " + exists + join,
			want: "[[1 50 true] [2 40 false] [3 30 true]]",
		},
		// Position in the SELECT list is not the discriminator.
		{
			name: "unqualified_structRoot_join_projectedExists_structLast",
			q:    "SELECT t1.id, " + exists + ", n" + join,
			want: "[[1 true struct[50 1]] [2 false struct[40 2]] [3 true struct[30 3]]]",
		},
	}

	// AMBIGUITY IS REJECTED UPSTREAM OF THE BAKE SITE, AND THAT IS WHAT MAKES
	// WIDENING THE BARE GUARD SAFE.
	//
	// resolveQualifiedBaked accepts a child-bearing leg-relative value, and its
	// safety rests on TWO things: the shape predicate, and the fact that
	// resolution ran with an explicit qualifier. Converging the bare arm onto
	// that predicate imports the first without the second, so the question is
	// whether a bare reference that names more than one leg can ever reach the
	// bake site. It cannot: every shape below is refused with 42702 by the SQL
	// layer first, both with and without a join, and with and without a
	// projected EXISTS.
	//
	// This is a NEGATIVE result and it is load-bearing rather than decorative.
	// It is the fact that classifies the guard widening as safe, and nothing
	// else in the tree states it. If this rejection is ever relaxed — an
	// ambiguous bare reference allowed to resolve to a first match — then a
	// widened bare guard would bake a real ordinal for it and read ONE leg's
	// slot silently, which is a wrong-column bug of exactly the kind this whole
	// investigation exists to prevent. These arms are what would go red first.
	ambiguous := []struct{ name, q, wantCode string }{
		{
			"ambiguous_bare_column_over_a_join_projectedExists",
			"SELECT id, " + exists + join,
			"42702",
		},
		{
			"ambiguous_bare_column_over_a_join_noExists",
			"SELECT id" + join,
			"42702",
		},
		{
			"selfjoin_bare_column_present_in_both_legs_projectedExists",
			"SELECT a.id, t1_id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = a.id) AS h " +
				"FROM t3 AS a JOIN t3 AS b ON a.id = b.id",
			"42702",
		},
		{
			"selfjoin_bare_column_present_in_both_legs_noExists",
			"SELECT a.id, t1_id FROM t3 AS a JOIN t3 AS b ON a.id = b.id",
			"42702",
		},
		{
			// The DUPLICATE PLAIN ALIAS shape resolveQualifiedBaked's own doc
			// comment names as declining. It is refused earlier still.
			"duplicate_plain_alias_qualified_projectedExists",
			"SELECT a.id, a.t1_id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = a.id) AS h " +
				"FROM t3 AS a JOIN t3 AS a ON true",
			"42702",
		},
		{
			"duplicate_plain_alias_bare_projectedExists",
			"SELECT t1_id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = 1) AS h " +
				"FROM t3 AS a JOIN t3 AS a ON true",
			"42702",
		},
		// MULTI-ACCESSOR ambiguity. Every arm above is a SINGLE-accessor bare
		// column, but the population a widened bare guard newly admits is the
		// multi-accessor one — RootIsLegRelativeUnpinned carries no
		// single-accessor requirement, unlike SourceRelativeBaked. An ambiguity
		// arm that only covers single accessors would therefore leave the
		// newly-admitted population unguarded while reading as complete.
		//
		// A self-join of t1 puts a struct `n` in BOTH legs, so bare `n.sk` names
		// two legs through a descent rather than at the root.
		{
			"ambiguous_bare_MULTI_ACCESSOR_selfjoin_projectedExists",
			"SELECT a.id, n.sk, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = a.id) AS h " +
				"FROM t1 AS a JOIN t1 AS b ON a.id = b.id",
			"42702",
		},
		{
			"ambiguous_bare_MULTI_ACCESSOR_selfjoin_noExists",
			"SELECT a.id, n.sk FROM t1 AS a JOIN t1 AS b ON a.id = b.id",
			"42702",
		},
		{
			"ambiguous_bare_MULTI_ACCESSOR_structRoot_selfjoin",
			"SELECT a.id, n FROM t1 AS a JOIN t1 AS b ON a.id = b.id",
			"42702",
		},
	}
	for _, tc := range ambiguous {
		t.Run("AMBIGUITY_"+tc.name, func(t *testing.T) {
			t.Parallel()
			rows, err := db.QueryContext(ctx, tc.q)
			if err == nil {
				rows.Close()
				t.Fatalf("%q now RESOLVES. An ambiguous bare reference reaching the "+
					"bake site is what makes a widened bare guard unsafe: it would "+
					"bake one leg's ordinal and read that slot silently. Either "+
					"restore the %s rejection or narrow the bare guard back to the "+
					"childless shape", tc.q, tc.wantCode)
			}
			if !strings.Contains(err.Error(), tc.wantCode) {
				t.Fatalf("expected %s for %q, got: %v — a different refusal means "+
					"the upstream rejection this guard's safety rests on has moved",
					tc.wantCode, tc.q, err)
			}
		})
	}

	// TWO PRE-EXISTING FAILURES, FOUND WHILE ASKING A DIFFERENT QUESTION, AND
	// NEITHER IS RFC-223's DEFECT.
	//
	// The question was whether a CTE or a derived table routes projection
	// binding through `logical_predicate.go`'s copies of the bare-projection
	// predicate rather than through the PlanVisitor site RFC-223 converted — if
	// so, the same bug would still be live behind a different front door.
	//
	// The routes cannot answer it: BOTH forms already fail when a projected
	// EXISTS is added, for two DIFFERENT reasons, and both fail identically with
	// RFC-223's production diff reverted. So they are not regressions, and the
	// twins' latency could not be established this way. That is why the copies
	// were FOLDED into resolveBaked rather than left with a "why they stay"
	// note: a latency claim nothing could measure is not a claim to ship.
	//
	// Both are LOUD, so neither is a silent wrong answer.
	//
	// ONE IS PINNED; THE OTHER IS BLOCKED, AND THE BLOCK IS NAMED.
	//
	// The CTE failure and BOTH controls live in
	// `pkg/relational/conformance/yamsql/testdata/projected_exists_over_a_derived_source.yaml`
	// — it fails with a SQLSTATE (42703), which that runner can match.
	//
	// The DERIVED-TABLE failure is NOT pinned anywhere, and not for want of
	// trying. Two independent blocks, each measured rather than assumed:
	//
	//   - yamsql cannot express it. It surfaces a *values.UnboundEvalContextError
	//     and the runner asserts errors only through errors.As(err, &apiErr)
	//     against an *api.Error carrying a SQLSTATE. There is no code to match.
	//   - This package cannot host it. Adding the query makes the leg-local bake
	//     census report `UnderivableLegs = 2, want 0` — a CQ-63 acceptance gate
	//     asserted as a HARD ZERO in a fixed list, not a tunable expectation.
	//     Control: with the query absent the census emits no NO-LAYOUT witness at
	//     all; with it present, two. The gate is telling the truth — a leg with
	//     no derivable row layout is exactly why that read falls through to a
	//     qualified name — so pinning here would mean relaxing a live acceptance
	//     gate to admit a defect, which is the wrong direction.
	//
	// So it is reported rather than pinned, which is a STOP with a named
	// blocker and not a quiet deferral. RFC-223 §7 carries the reproducer, the
	// passing control, and the root cause. An earlier revision of this comment
	// claimed the arm "costs no census here"; that was measured on a narrowed
	// run and is false on the full suite.
	// THE DERIVED-TABLE ARM LIVES HERE, NOT IN THE YAML, and the reason is
	// mechanical: it fails with a *values.UnboundEvalContextError, and the
	// yamsql runner asserts errors only as an *api.Error carrying a SQLSTATE
	// (runner.go's `errors.As(err, &apiErr)`). There is no SQLSTATE to pin, so
	// the yaml can hold its CONTROL but not the failure itself.
	//
	// THIS ARM IS ALSO THE PROOF THAT THE STRUCT TYPE IS NOT INVOLVED. The
	// fixture it runs against has a struct column, but the query never touches
	// it: the derived table selects `id` alone, and the failure is identical.
	// That sentence appears in RFC-223 as a measured claim, and this is the
	// measurement — without it the claim would be true and unpinned.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rows, err := db.QueryContext(ctx, tc.q)
			if err != nil {
				t.Fatalf("CONTROL %q failed: %v — a failing control makes the "+
					"paired failure above uninterpretable", tc.q, err)
			}
			defer rows.Close()
			cols, err := rows.Columns()
			if err != nil {
				t.Fatalf("columns %q: %v", tc.q, err)
			}
			var out []string
			for rows.Next() {
				vals := make([]any, len(cols))
				ptrs := make([]any, len(cols))
				for i := range vals {
					ptrs[i] = &vals[i]
				}
				if err := rows.Scan(ptrs...); err != nil {
					t.Fatalf("scan %q: %v", tc.q, err)
				}
				r := make([]string, len(vals))
				for i, v := range vals {
					if s, ok := v.(interface{ Attributes() []any }); ok {
						r[i] = fmt.Sprintf("struct%v", s.Attributes())
						continue
					}
					r[i] = fmt.Sprintf("%v", v)
				}
				out = append(out, fmt.Sprint(r))
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("rows %q: %v", tc.q, err)
			}
			if len(out) == 0 {
				t.Fatalf("CONTROL %q returned ZERO rows; the value assertion "+
					"below would hold vacuously", tc.q)
			}
			if fmt.Sprint(out) != tc.want {
				t.Fatalf("query %q =\n  %v\nwant\n  %v", tc.q, fmt.Sprint(out), tc.want)
			}
		})
	}
}
