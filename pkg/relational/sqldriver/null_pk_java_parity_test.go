package sqldriver_test

// NULL in a PRIMARY KEY column — Java-parity pins.
//
// Java raises NO error here. The relational layer never marks a scalar
// column non-nullable (DdlVisitor.java:156-161 rejects NOT NULL on
// scalars outright; PRIMARY KEY membership implies nothing), so an
// explicit NULL or an omitted PK column flows through
// parseRecordFieldsUnderReorderings (ExpressionVisitor.java:1053-1075)
// as a NullValue, and the record store saves it with a REAL tuple null
// (0x00) in the primary key: Key.Expressions.field defaults to
// NullStandin.NULL (Key.java:70-76,97), FieldKeyExpression.getNullResult
// returns the standin (FieldKeyExpression.java:228-243), and
// TupleTypeUtil.toTupleAppropriateValue maps it to Tuple null
// (TupleTypeUtil.java:148-151). FDBRecordStore.saveRecord
// (FDBRecordStore.java:552-558) never validates the PK for null. Proven
// live on Java by yaml-tests/functions.yamsql:24,34,99-104 (composite PK
// with all-null rows inserted and read back) and
// inserts-updates-deletes.yamsql:133-135 (PK omitted from the column
// list, insert succeeds).
//
// A long-standing watch-list entry claimed Go "silently stores 0" for
// this shape, pinned only by a test that logged-and-returned on both
// paths (it asserted nothing, so it could not distinguish 0 from null
// from an error — that fake pin is deleted in favour of this file).
// Measurement refuted the claim: Go stores the same tuple null Java
// does. The tests below pin every observable that distinguishes the
// three candidate behaviours:
//
//   - stored-as-0 would make the id=0 insert collide (23505) and the
//     read-back return id=0 — both asserted to NOT happen;
//   - rejected-with-23502 would make the NULL insert error — asserted
//     to succeed;
//   - stored-as-tuple-null (Java's behaviour) makes the row readable
//     via IS NULL, scan back as SQL NULL, and a SECOND null-PK insert
//     collide as a duplicate — all asserted to happen.
//
// The wire-level half (absent field → tuple null 0x00, byte-identical
// to Java) is pinned separately at
// pkg/recordlayer/key_expression_test.go (getNullResult standin pins).

import (
	"context"
	"database/sql"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// TestFDB_NullPK_ExplicitNull_SingleColumn: INSERT of an explicit NULL
// into a single-column BIGINT PK succeeds and stores tuple null.
func TestFDB_NullPK_ExplicitNull_SingleColumn(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/testdb_nullpk_single", "nullpk_single",
		"CREATE TABLE Item (id BIGINT, name STRING, PRIMARY KEY (id))")
	ctx := context.Background()

	// Control: a valid insert works.
	if _, err := db.ExecContext(ctx, "INSERT INTO Item (id, name) VALUES (7, 'seven')"); err != nil {
		t.Fatalf("control INSERT: %v", err)
	}
	// Java-parity: explicit NULL PK succeeds (no 23502, no rejection).
	if _, err := db.ExecContext(ctx, "INSERT INTO Item (id, name) VALUES (NULL, 'nullrow')"); err != nil {
		t.Fatalf("explicit-NULL-PK INSERT must succeed (Java stores tuple null; ExpressionVisitor.java:1053-1075 raises nothing): %v", err)
	}

	// The stored PK is NOT 0: id=0 must be a distinct, free key.
	if _, err := db.ExecContext(ctx, "INSERT INTO Item (id, name) VALUES (0, 'zero')"); err != nil {
		t.Fatalf("id=0 INSERT collided — the NULL PK was stored as 0, not as tuple null: %v", err)
	}

	// The stored PK IS a real key: a second NULL-PK insert collides as a
	// duplicate, exactly like equal non-null keys (Java: two identical
	// null PK tuples are the same record key).
	_, err := db.ExecContext(ctx, "INSERT INTO Item (id, name) VALUES (NULL, 'dup')")
	if err == nil {
		t.Fatal("second NULL-PK INSERT did not collide — the first NULL was not stored under the null key")
	}
	if got := asAPIError(err); got == nil || got.Code != api.ErrCodeUniqueConstraintViolation {
		t.Fatalf("second NULL-PK INSERT error = %v, want SQLSTATE %s", err, api.ErrCodeUniqueConstraintViolation)
	}

	// Read-back: exactly one row with SQL NULL id, visible via IS NULL.
	var name string
	if err := db.QueryRowContext(ctx, "SELECT name FROM Item WHERE id IS NULL").Scan(&name); err != nil {
		t.Fatalf("WHERE id IS NULL: %v", err)
	}
	if name != "nullrow" {
		t.Fatalf("IS NULL row name = %q, want %q", name, "nullrow")
	}
	// Full scan: the null-PK row scans back with id NULL (not 0); the
	// zero row is separate.
	rows, err := db.QueryContext(ctx, "SELECT id, name FROM Item")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	defer rows.Close()
	byName := map[string]sql.NullInt64{}
	for rows.Next() {
		var id sql.NullInt64
		var n string
		if err := rows.Scan(&id, &n); err != nil {
			t.Fatalf("row scan: %v", err)
		}
		byName[n] = id
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(byName) != 3 {
		t.Fatalf("stored rows = %d (%v), want 3 (seven, nullrow, zero)", len(byName), byName)
	}
	if id := byName["nullrow"]; id.Valid {
		t.Fatalf("nullrow id read back as %d, want SQL NULL", id.Int64)
	}
	if id := byName["zero"]; !id.Valid || id.Int64 != 0 {
		t.Fatalf("zero id read back as %+v, want 0", id)
	}
	if id := byName["seven"]; !id.Valid || id.Int64 != 7 {
		t.Fatalf("seven id read back as %+v, want 7", id)
	}
}

// TestFDB_NullPK_CompositePartialNull: NULL in one (or every) component
// of a composite PK — the exact shape Java's functions.yamsql:34 proves
// live: `insert into B values (1, 2), (3, null), (null, 4), (null, null)`.
func TestFDB_NullPK_CompositePartialNull(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/testdb_nullpk_comp", "nullpk_comp",
		"CREATE TABLE B (b1 BIGINT, b2 DOUBLE, PRIMARY KEY (b1, b2))")
	ctx := context.Background()

	if _, err := db.ExecContext(ctx,
		"INSERT INTO B VALUES (1, 2), (3, NULL), (NULL, 4), (NULL, NULL)"); err != nil {
		t.Fatalf("composite-PK INSERT with null components must succeed (Java functions.yamsql:34): %v", err)
	}

	// The null component is stored as tuple null in the KEY, not as the
	// column's zero value: (3, 0.0) must be a distinct, free key from the
	// stored (3, NULL).
	if _, err := db.ExecContext(ctx, "INSERT INTO B VALUES (3, 0)"); err != nil {
		t.Fatalf("(3, 0.0) INSERT collided — the NULL composite component was stored as 0, not as tuple null: %v", err)
	}

	rows, err := db.QueryContext(ctx, "SELECT b1, b2 FROM B")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	defer rows.Close()
	type pair struct{ b1, b2 bool } // Valid flags
	got := map[pair]int{}
	n := 0
	for rows.Next() {
		var b1 sql.NullInt64
		var b2 sql.NullFloat64
		if err := rows.Scan(&b1, &b2); err != nil {
			t.Fatalf("row scan: %v", err)
		}
		got[pair{b1.Valid, b2.Valid}]++
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if n != 5 {
		t.Fatalf("stored rows = %d, want 5 (Java persists all four null-pattern rows — functions.yamsql:99-104 — plus the (3, 0.0) probe)", n)
	}
	want := map[pair]int{
		{true, true}: 2, {true, false}: 1, {false, true}: 1, {false, false}: 1,
	}
	for k, c := range want {
		if got[k] != c {
			t.Fatalf("null-pattern census = %v, want %v", got, want)
		}
	}
}

// TestFDB_NullPK_OmittedColumn: the PK column absent from an explicit
// column list — Java substitutes NullValue and succeeds
// (ExpressionVisitor.java:1067-1073, nullable passes; proven live by
// inserts-updates-deletes.yamsql:133-135).
func TestFDB_NullPK_OmittedColumn(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/testdb_nullpk_omit", "nullpk_omit",
		"CREATE TABLE Item (id BIGINT, name STRING, PRIMARY KEY (id))")
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, "INSERT INTO Item (name) VALUES ('omitted')"); err != nil {
		t.Fatalf("omitted-PK INSERT must succeed (Java inserts-updates-deletes.yamsql:133-135): %v", err)
	}
	var name string
	if err := db.QueryRowContext(ctx, "SELECT name FROM Item WHERE id IS NULL").Scan(&name); err != nil {
		t.Fatalf("WHERE id IS NULL after omitted-PK insert: %v", err)
	}
	if name != "omitted" {
		t.Fatalf("IS NULL row name = %q, want %q", name, "omitted")
	}
	// Not stored as 0: id=0 is still free.
	if _, err := db.ExecContext(ctx, "INSERT INTO Item (id, name) VALUES (0, 'zero')"); err != nil {
		t.Fatalf("id=0 INSERT collided — the omitted PK was stored as 0, not as tuple null: %v", err)
	}
}
