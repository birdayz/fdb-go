package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// CAN TWO COLUMN NAMES THAT COLLIDE UNDER ESCAPING RETURN EACH OTHER'S VALUES?
//
// Descriptor field names are ESCAPED (protoname.ToProtoBufCompliantName), and
// the escaping is not injective: `a$b` is stored as `a__1b`, while a column
// whose SQL name IS `a__1b` is stored as `a__01b`. Field resolution tries the
// name as given before it tries the escaped form, so a reference to `a__1b`
// could match the field STORED as `a__1b` — which belongs to `a$b`.
//
// If that happens it is a WRONG-ROWS bug, strictly worse than the analogous
// collision in collected statistics (which only mis-prices a plan). DDL accepts
// both columns, so the shape is reachable; this establishes what actually comes
// back, because "reachable" and "wrong" are different claims and only the second
// one matters.
//
// This is a MEASUREMENT that reports. It asserts only the thing that must hold
// however resolution behaves: the two columns must not report each other's
// values.
func TestFDB_FieldNameCollisionAcrossEscaping(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	dbPath := "/fieldcollide"
	setup := openTestDB(t, dbPath)
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE "+dbPath)
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE fieldcollide"+
			` CREATE TABLE t (id BIGINT, "a$b" BIGINT, "a__1b" BIGINT, PRIMARY KEY (id))`)
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE fieldcollide")

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Distinct sentinel values, so whichever comes back names its own column.
	const dollarVal, underVal = 111, 222
	if _, e := db.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO t (id, "a$b", "a__1b") VALUES (1, %d, %d)`, dollarVal, underVal)); e != nil {
		t.Fatalf("insert: %v", e)
	}

	read := func(col string) (int64, error) {
		var v sql.NullInt64
		row := db.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s FROM t WHERE id = 1`, col))
		if e := row.Scan(&v); e != nil {
			return 0, e
		}
		if !v.Valid {
			return -1, nil // NULL, reported distinctly from any real value
		}
		return v.Int64, nil
	}

	gotDollar, errDollar := read(`"a$b"`)
	gotUnder, errUnder := read(`"a__1b"`)

	t.Logf("")
	t.Logf(`FIELD-NAME COLLISION ACROSS ESCAPING (-1 means NULL):`)
	t.Logf(`  "a$b"    stored as a__1b   -> %d (want %d)   err=%v`, gotDollar, dollarVal, errDollar)
	t.Logf(`  "a__1b"  stored as a__01b  -> %d (want %d)   err=%v`, gotUnder, underVal, errUnder)

	if errDollar != nil || errUnder != nil {
		t.Fatalf("a column the DDL accepted could not be read back: %v / %v", errDollar, errUnder)
	}
	// THE ASSERTION THAT MATTERS. Whatever resolution does, one column must
	// never report the other's value — that is a wrong row, not a wrong plan.
	if gotDollar == underVal || gotUnder == dollarVal {
		t.Fatalf(`the two columns returned each other's values: "a$b"=%d "a__1b"=%d — `+
			`field resolution matched the unescaped name against another column's `+
			`ESCAPED storage name, so a valid query reads the wrong column`,
			gotDollar, gotUnder)
	}
	if gotDollar != dollarVal || gotUnder != underVal {
		t.Fatalf(`columns did not round-trip: "a$b"=%d (want %d), "a__1b"=%d (want %d)`,
			gotDollar, dollarVal, gotUnder, underVal)
	}
}
