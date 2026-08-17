package sqldriver_test

// A CTE whose body is `SELECT *` over a multi-table FROM publishes its columns
// under their SQL LABELS, not under the executor's datum keys.
//
// The exact row of a join qualifies every leg column with its source alias
// (`A.AID`, `B.K`) so the executor's row map can keep two same-named columns
// apart. Those are keys, not labels: `SELECT * FROM A, B` returns columns named
// AID, K, ARR, BID, K. The semantic scope registered the qualified spelling for
// a CTE over such a body, so `D."AID"` resolved against a source whose only
// AID-ish column was called `A.AID` and the reference failed 42703 — on a
// column the parser had seen perfectly well.
//
// It stayed hidden because the exact scope source was DECLINING for an
// unrelated reason: semanticColumnFromExactType refuses a nullable array
// element, the logical scan row was built by the DML-target mapper (which types
// array elements nullable), and every one of these bodies has an array column.
// Correcting the scan row to the stored-row mapper made the source admissible
// and the latent naming defect fired immediately. Two bugs, one visible symptom
// — which is why this pins the LABELS directly rather than only that the query
// runs.
//
// Planning-only on purpose: the failure is at name resolution, so it needs no
// FDB and stays fast enough to keep every arm.

import (
	"strings"
	"testing"

	"fdb.dev/pkg/relational/core/embedded"
)

// THE TWO REJECTION ARMS ABOVE PIN A GAP, NOT THE DESIRED BEHAVIOUR.
//
// Both queries are invalid and both are rejected, so nothing wrong executes —
// but the SQLSTATE is wrong: SQL says 42702 for the ambiguous reference and a
// source-not-found for the qualified one, and the engine says 0AF00
// "projection slot 0 has no resolved Value" for both.
//
// The cause is upstream of anything this file tests, and is measured rather
// than guessed. In PlanVisitor.visitSimpleTableBody the CTE scope map reaching
// buildSelectScope is EMPTY for a main query whose FROM is a CTE, so
// addSource falls through to the catalog, ResolveTable("D") misses, the whole
// resolver comes back nil, and the block guarded by `resolver != nil` — which
// is where every 42702/42703 projection gate lives — is skipped. The reference
// then survives to translation and dies there without a column name.
//
// This is not fallout from the label work: it is the same on master, where
// these queries also fail without reaching a gate. It is booked in TODO.md
// under "CTE main queries skip the 42702/42703 projection gates". When that is
// fixed, these two arms must be RE-ARMED to the real codes — a 0AF00 here
// stops being correct the moment the resolver is populated.
func TestCTEStarBodyPublishesSQLLabels(t *testing.T) {
	t.Parallel()
	md := existsGatherSchemaMetadata(t)

	for _, tc := range []struct {
		name string
		sql  string
		// wantErr is a substring; empty means the query must plan.
		wantErr string
	}{
		{
			// The shape that regressed: a two-table star plus a lateral unnest,
			// referenced by a bare column name through the CTE.
			name: "star_join_unnest_bare_reference",
			sql: `WITH D AS (SELECT * FROM A, B, A."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")) ` +
				`SELECT D."AID", COUNT(*) FROM D GROUP BY D."AID"`,
		},
		{
			// The unnest element alias is already unqualified in the exact row,
			// so this arm passes even with the datum keys published — it is the
			// control that says the fixture reaches resolution at all.
			name: "unnest_alias_reference_control",
			sql: `WITH D AS (SELECT * FROM A, B, A."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")) ` +
				`SELECT D."X", COUNT(*) FROM D GROUP BY D."X"`,
		},
		{
			// A column from the SECOND leg: publishing datum keys broke this one
			// too, and under a different alias, so the fix cannot be a special
			// case for the first source.
			name: "star_join_second_leg_reference",
			sql: `WITH D AS (SELECT * FROM A, B, A."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")) ` +
				`SELECT D."BID", COUNT(*) FROM D GROUP BY D."BID"`,
		},
		{
			// A PLAIN projection, no GROUP BY: this is the arm that reaches the
			// CTE projection binder rather than the aggregate path.
			name: "star_join_plain_projection",
			sql: `WITH D AS (SELECT * FROM A, B, A."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")) ` +
				`SELECT D."AID" FROM D`,
		},
		{
			// K is declared by BOTH legs, so a bare reference through the CTE is
			// genuinely ambiguous and SQL says 42702. It is REJECTED — but by
			// the translator, as an opaque 0AF00 that names neither the column
			// nor the CTE, because the 42702/42703 gates never run for this
			// shape at all. See the gap note below the table.
			name: "duplicated_bare_column_is_rejected",
			sql: `WITH D AS (SELECT * FROM A, B, A."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")) ` +
				`SELECT D."K" FROM D`,
			wantErr: "0AF00",
		},
		{
			// Likewise a qualified reference to a source that does not exist at
			// this level: inside D there is no A any more, which SQL answers
			// with "cannot be resolved".
			name: "inner_leg_alias_is_rejected",
			sql: `WITH D AS (SELECT * FROM A, B, A."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")) ` +
				`SELECT A."AID" FROM D`,
			wantErr: "0AF00",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := embedded.PlanRecordQueryWithMetadata(tc.sql, md, nil)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("plan failed: %v\n  sql: %s", err, tc.sql)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("planned, want an error containing %q\n  sql: %s", tc.wantErr, tc.sql)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("error = %v, want one containing %q\n  sql: %s", err, tc.wantErr, tc.sql)
			}
		})
	}
}
