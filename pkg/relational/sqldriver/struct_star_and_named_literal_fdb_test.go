package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// `SELECT structcol.*` expands the struct's fields, and `STRUCT name (…)`
// carries its declared name into the result type.
//
// Both are RFC-204 P3 surface (rfcs/204-struct-types-relational-layer.md
// :456-462) and both were silently wrong:
//
//   - the star qualifier was only ever looked up as a RELATION, so a struct
//     column died 42F01 "table HOME does not exist". Java's expandStar tries
//     the relation first and falls through to "qualifying a column inside a
//     table" (SemanticAnalyzer.java:361-367), expanding the struct's fields in
//     ORDINAL order via expandStructExpression (:746-763).
//   - the OfTypeClause was parsed and dropped, so `STRUCT GEO (…)` produced an
//     ANONYMOUS record. Java resolves it through
//     RecordConstructorValue.ofColumnsAndName → Type.Record.withName
//     (RecordConstructorValue.java:485-487, Type.java:2221-2223).
func TestFDB_StructStarAndNamedLiteral(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/structstar")
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE /structstar"); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE ss_tmpl "+
			"CREATE TYPE AS STRUCT GEO (lat BIGINT, lon BIGINT) "+
			"CREATE TYPE AS STRUCT ADDR (city STRING, zip BIGINT) "+
			"CREATE TABLE T (id BIGINT, home ADDR, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA /structstar/s WITH TEMPLATE ss_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///structstar?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.ExecContext(ctx, "INSERT INTO T VALUES (1, ('sf', 94107))"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Star over a struct column expands to its fields, IN DECLARED ORDER.
	// Order is the assertion that matters: Java expands by ordinal, so a
	// name-keyed expansion that happened to produce the right SET of columns
	// in the wrong ORDER would still be wrong.
	t.Run("struct_star_expands_fields_in_ordinal_order", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT home.* FROM T WHERE id = 1")
		if err != nil {
			t.Fatalf("SELECT home.* must expand the struct's fields, got: %v", err)
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			t.Fatalf("columns: %v", err)
		}
		if len(cols) != 2 {
			t.Fatalf("home.* produced %d columns %v, want 2 (CITY, ZIP)", len(cols), cols)
		}
		if !strings.EqualFold(cols[0], "CITY") || !strings.EqualFold(cols[1], "ZIP") {
			t.Fatalf("home.* produced columns %v, want [CITY ZIP] in DECLARED order — "+
				"expansion must be by ordinal, as expandStructExpression is", cols)
		}
		if !rows.Next() {
			t.Fatalf("no row: %v", rows.Err())
		}
		var city string
		var zip int64
		if err := rows.Scan(&city, &zip); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if city != "sf" || zip != 94107 {
			t.Fatalf("home.* = (%q,%d), want (\"sf\",94107)", city, zip)
		}
	})

	// The struct star must compose with other select-list items, which is the
	// mixed projStarQualifiers path rather than the whole-projection one.
	t.Run("struct_star_mixed_with_other_columns", func(t *testing.T) {
		var id int64
		var city string
		var zip int64
		if err := db.QueryRowContext(ctx,
			"SELECT id, home.* FROM T WHERE id = 1").Scan(&id, &city, &zip); err != nil {
			t.Fatalf("mixed struct star: %v", err)
		}
		if id != 1 || city != "sf" || zip != 94107 {
			t.Fatalf("got (%d,%q,%d), want (1,\"sf\",94107)", id, city, zip)
		}
	})

	// A star over a NON-struct column must still be rejected — the
	// fall-through must not turn every unknown qualifier into an expansion.
	// Java asserts isRecord and raises INVALID_COLUMN_REFERENCE "attempt to
	// expand non-struct column" (SemanticAnalyzer.java:339-341, :364-365).
	t.Run("star_over_scalar_column_rejected", func(t *testing.T) {
		_, err := db.QueryContext(ctx, "SELECT id.* FROM T")
		if err == nil {
			t.Fatal("SELECT id.* over a BIGINT column was ACCEPTED; a non-struct qualifier must be rejected")
		}
	})

	t.Run("star_over_unknown_qualifier_rejected", func(t *testing.T) {
		_, err := db.QueryContext(ctx, "SELECT nosuch.* FROM T")
		if err == nil {
			t.Fatal("SELECT nosuch.* was ACCEPTED; an unknown qualifier must be rejected")
		}
	})

	// A named struct literal carries its DECLARED TYPE NAME into result
	// metadata. Asserting only that the literal answers is not coverage: the
	// declared name was dropped and the anonymous record still answered
	// correctly, so the type name is the only thing that distinguishes the
	// fixed behaviour from the broken one.
	//
	// The name becomes observable because the plan-time descriptor is minted
	// FROM the record type: defineRecordLocked escapes RecordName through
	// ToProtoBufCompliantName (Java's Type.Record.withName does the same,
	// Type.java:2221-2223) and names the synthesised message accordingly,
	// instead of falling back to an anonymous __type__N.
	t.Run("named_struct_literal_carries_declared_type_name", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT STRUCT GEO (1 AS lat, 2 AS lon) FROM T WHERE id = 1")
		if err != nil {
			t.Fatalf("named struct literal: %v", err)
		}
		defer rows.Close()
		if !rows.Next() {
			t.Fatalf("no row: %v", rows.Err())
		}
		var got any
		if err := rows.Scan(&got); err != nil {
			t.Fatalf("scan: %v", err)
		}
		s, isStruct := got.(api.Struct)
		if !isStruct {
			t.Fatalf("named struct literal returned %T, want an api.Struct", got)
		}
		md := s.MetaData()
		if md == nil {
			t.Fatal("named struct literal produced no struct metadata")
		}
		if !strings.EqualFold(md.TypeName(), "GEO") {
			t.Fatalf(`STRUCT GEO (…) has type name %q, want "GEO".
The OfTypeClause is being parsed and dropped, so the literal produces an
ANONYMOUS record type and the declared name never reaches metadata.`, md.TypeName())
		}
	})

	// The anonymous literal must STAY anonymous — consuming the clause must
	// not start inventing names for records that declared none.
	t.Run("anonymous_struct_literal_has_no_declared_name", func(t *testing.T) {
		var got any
		if err := db.QueryRowContext(ctx,
			"SELECT (1 AS lat, 2 AS lon) FROM T WHERE id = 1").Scan(&got); err != nil {
			t.Fatalf("anonymous struct literal: %v", err)
		}
		s, isStruct := got.(api.Struct)
		if !isStruct {
			t.Fatalf("anonymous literal returned %T, want an api.Struct", got)
		}
		if name := s.MetaData().TypeName(); strings.EqualFold(name, "GEO") {
			t.Fatalf("anonymous struct literal picked up the name %q", name)
		}
	})
}
