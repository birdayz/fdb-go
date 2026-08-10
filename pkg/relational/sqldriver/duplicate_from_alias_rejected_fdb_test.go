package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// TestFDB_DuplicateFromAliasRejected closes an UNVERIFIED gap in the safety
// premise that several by-name planner lookups rest on.
//
// `legWindowSlot` and its siblings resolve a leg window by matching a
// reference's QUALIFIER, first-match. That is safe only if two FROM sources
// cannot share an alias. The bare/derived-table ambiguity rejections pinned in
// ambiguous_column_ref_rejected_fdb_test.go are 42702 `ambiguous_column` and say
// nothing about a repeated ALIAS, and every 42712 `duplicate_alias` producer in
// pkg/ concerns CTE names — so on inspection the qualifier half of the premise
// appeared to rest on nothing. Worse, the same channel's other reader poisons a
// duplicate qualifier and refuses to bake, so two readers of one channel held
// opposite dispositions with no upstream reconciliation visible.
//
// Measured, the premise holds — by a different route than expected. Go permits
// the duplicate alias to be DECLARED and rejects any reference through it as
// ambiguous, which is the same disposition it takes for a bare name two sources
// carry. So the qualifier first-match is covered after all: a reference that
// would reach it with an ambiguous qualifier never gets that far.
//
// This diverges from Postgres in WHEN, not whether. Postgres rejects the FROM
// clause itself with 42712 `duplicate_alias` even when nothing references the
// alias; Go accepts the declaration and rejects the read. That difference is
// recorded rather than fixed: it is not a safety hole, because no reference can
// resolve through the ambiguous alias either way.
//
// If this ever stops rejecting, the qualifier first-match in the leg-window
// readers is armed, and its failure mode is a silent wrong-column read — the
// loser of a first match is a real column of the same type.
func TestFDB_DuplicateFromAliasRejected(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/dupalias"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE dupalias_tmpl"+
		" CREATE TABLE zn (id BIGINT, k BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE zp (pid BIGINT, k BIGINT, PRIMARY KEY (pid))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE dupalias_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, s := range []string{
		"INSERT INTO zn VALUES (1, 100), (2, 200)",
		"INSERT INTO zp VALUES (1, 999)",
	} {
		if _, e := db.ExecContext(ctx, s); e != nil {
			t.Fatalf("seed %q: %v", s, e)
		}
	}

	for _, tc := range []struct{ name, query string }{
		{"comma join, two tables", "SELECT a.k FROM zn AS a, zp AS a"},
		{"explicit join", "SELECT a.k FROM zn AS a JOIN zp AS a ON a.k = a.k"},
		{"self-join under one alias", "SELECT a.k FROM zn AS a, zn AS a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := db.QueryContext(ctx, tc.query)
			if err == nil {
				defer rows.Close()
				if rows.Next() {
					t.Fatalf("A REFERENCE THROUGH A DUPLICATED FROM ALIAS RESOLVED.\n"+
						"  query: %s\n"+
						"  Two sources answer to that alias, so a qualifier match has no "+
						"honest answer. The leg-window readers match a qualifier "+
						"first-match and are safe only because this cannot reach them; "+
						"the failure mode once it does is a silent wrong-column read.",
						tc.query)
				}
				if err = rows.Err(); err == nil {
					t.Fatalf("a duplicated FROM alias was accepted (no error, no rows): %s", tc.query)
				}
			}
			// 42702 rather than Postgres's 42712 is deliberate and measured — see
			// the file comment. The assertion is on the code so that a refusal for
			// an unrelated reason cannot pass for this guard.
			if !strings.Contains(err.Error(), "42702") {
				t.Fatalf("duplicated FROM alias refused with the WRONG error.\n"+
					"  query: %s\n  got:   %v\n  want:  42702 (ambiguous_column).\n"+
					"  Go rejects the READ rather than the declaration; if this became "+
					"42712 the guard moved to the FROM clause, which is stricter and "+
					"still safe — update this test deliberately rather than loosening it.",
					tc.query, err)
			}
		})
	}
}
