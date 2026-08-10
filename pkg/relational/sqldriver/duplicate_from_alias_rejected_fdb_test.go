package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// TestFDB_DuplicateFromAliasRejected pins WHAT THE USER SEES for a reference
// through a duplicated FROM alias, and it guards the ONLY gate that stands
// between that SQL and a wrong-column read.
//
// Go permits the duplicate alias to be DECLARED and rejects any reference
// through it as ambiguous — 42702. The producer is
// semantic.Scope.ResolveQualifiedColumn's multi-match arm (semantic/scope.go
// :380-387), NOT ResolveColumn's (:267-274): `a.k` is a QUALIFIED reference and
// takes the qualified path. That distinction was established by mutation, not
// by reading — removing ResolveColumn's arm leaves this test fully GREEN, and
// only removing the qualified one reddens it. Cite the qualified arm when
// reasoning about this shape.
//
// This diverges from Postgres in WHEN, not whether: Postgres rejects the FROM
// clause itself with 42712 duplicate_alias even when nothing references the
// alias. Recorded rather than fixed; no reference resolves through the
// ambiguous alias either way.
//
// WHAT RELAXING THIS COSTS, measured rather than predicted. With the qualified
// arm removed, all three spellings below RETURN ROWS — the reference resolves
// to the FIRST matching source at semantic analysis and planning never sees an
// ambiguity. So this 42702 is not one of two layered defences; for this SQL
// shape it is the whole defence, and its failure mode is a silent wrong-column
// read (the loser of a first match is a real column of the same type).
//
// An earlier revision of this comment claimed a second gate caught it —
// legWindowSlot's ambiguity decline routing to a lazy reference that "fails at
// evaluation". THAT IS FALSE and the mutation above is what showed it:
// legWindowSlot never sees a duplicate qualifier on this path, because the
// semantic layer has already collapsed the reference to one source before a leg
// window is walked. The decline is real and asserted
// (TestDuplicateQualifier_ReadersAgree), but it defends a population this SQL
// does not reach — over the whole corpus that population is empty, measured by
// a probe that panicked on any duplicate-leg match and was never hit outside
// its own unit test.
//
// Keeping the verdict here is also right on layering: an ambiguous identifier
// is a SEMANTIC-ANALYSIS verdict and Java raises it there, not inside a
// plan-time slot walk. A helper that threw for a shape the analyzer should have
// rejected would turn a user error into an internal one.
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
						"honest answer, and this 42702 is the ONLY gate that says so for "+
						"this shape. Rows here mean semantic analysis resolved the "+
						"reference to the FIRST matching source: a silent wrong-column "+
						"read, since the loser of that match is a real column of the "+
						"same type. Nothing downstream catches it — legWindowSlot's "+
						"ambiguity decline never sees a duplicate qualifier on this "+
						"path, because the reference was collapsed to one source "+
						"before any leg window was walked. Measured by removing "+
						"ResolveQualifiedColumn's multi-match arm: all three "+
						"spellings returned rows.",
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
