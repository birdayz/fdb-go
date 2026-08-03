package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// Nested field access resolves exactly ONE level deep today
// (`struct.field`); a deeper path (`struct.inner.field`) and a
// source-qualified nested path (`t.struct.field`) both fail 42703.
//
// This is the KNOWN Phase-3 representation gap, not a local resolver bug.
// The SQL layer splits a dotted reference at the LAST dot into
// (qualifier, column) — one joined string plus one name — so a three-segment
// path arrives as qualifier="HOME.POS", column="LAT", and the scope has
// nothing to decompose: `semantic.Identifier` carries a single name, with no
// qualifier segment list. RFC-204 §4.4 (rfcs/204-struct-types-relational-layer.md:446-455)
// names the fix precisely — "Replace the parseColRef last-dot split with
// Java's Identifier model: the full segment list flows to the semantic scope,
// and ResolveColumn / ResolveQualifiedColumn grow the lookupNestedField
// descent" (Java: SemanticAnalyzer.lookupNestedField, SemanticAnalyzer.java:538)
// — and that representation change is scoped to its own RFC.
//
// This test exists so the gap cannot drift silently in EITHER direction: the
// one-level case must keep working, and the deeper cases must keep failing
// CLEANLY (a typed 42703, never a crash, never a wrong answer). When the
// Identifier model lands, the deep arms start answering and this test fails —
// at which point the assertions flip to value checks.
func TestFDB_NestedPathDepthGate(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/nestdepth")
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE /nestdepth"); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE nd_tmpl "+
			"CREATE TYPE AS STRUCT GEO (lat BIGINT, lon BIGINT) "+
			"CREATE TYPE AS STRUCT ADDR (city STRING, pos GEO) "+
			"CREATE TABLE T (id BIGINT, home ADDR, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA /nestdepth/s WITH TEMPLATE nd_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///nestdepth?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.ExecContext(ctx, "INSERT INTO T VALUES (1, ('sf', (37, -122)))"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	t.Run("one_level_resolves", func(t *testing.T) {
		var city string
		if err := db.QueryRowContext(ctx, "SELECT home.city FROM T WHERE id = 1").Scan(&city); err != nil {
			t.Fatalf("one-level nested access must resolve: %v", err)
		}
		if city != "sf" {
			t.Fatalf("home.city = %q, want \"sf\"", city)
		}
	})

	mustCleanly42703 := func(t *testing.T, q string) {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
			}
			err = rows.Err()
		}
		if err == nil {
			t.Fatalf(`%s now ANSWERS.
If the Identifier model (RFC-204 §4.4) landed, that is the intended outcome —
replace this arm with a value assertion. If it did not, a deep path is
resolving by accident and the answer needs checking.`, q)
		}
		if !strings.Contains(err.Error(), "42703") {
			t.Fatalf("%s failed with %v, want a clean 42703 undefined-column rejection.\n"+
				"The depth gap must stay a typed rejection — never a crash or an internal error.", q, err)
		}
	}

	t.Run("two_level_rejects_cleanly", func(t *testing.T) {
		mustCleanly42703(t, "SELECT home.pos.lat FROM T")
	})

	t.Run("source_qualified_nested_rejects_cleanly", func(t *testing.T) {
		mustCleanly42703(t, "SELECT t.home.city FROM T AS t")
	})
}
