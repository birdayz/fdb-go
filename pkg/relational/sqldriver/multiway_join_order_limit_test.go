package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// TestFDB_MultiwayJoinOrder_Limit pins the HONEST scope of RFC-042's
// FROM-order-independent re-enumeration: it is correct end-to-end for 3-way
// joins and explicitly NOT for ≥4-way joins.
//
// Why the boundary is at 3: the re-enumeration routes a spanning join predicate
// to the upper partition and flows the correlated lower quantifier up as a
// single QuantifiedObjectValue. That correlation threads cleanly through exactly
// ONE level of upper re-partitioning (3-way: lower=2 tables, upper=1 terminal).
// For ≥4-way the correlation must survive TWO+ nested re-partitions and the
// executor's flattened NLJ/FlatMap merge loses the lower alias — the projection
// resolves to NULL → silent wrong rows. The PartitionSelectRule therefore gates
// the new classification on `n == 3` and keeps Java's original split for n > 3,
// so a ≥4-way join that has no other valid decomposition fails to plan LOUDLY,
// exactly as it does on master (4-way joins have never been supported in this
// port). A loud plan-failure is acceptable; silent wrong rows are not.
//
// This test is the regression sentinel for that boundary. If a future change
// makes a 4-way join PLAN, it must also make it return CORRECT rows — this test
// will then fail on the wrong-rows assertion and force the executor's nested
// RecordConstructorValue + TranslationMap resolution (the N-way follow-up) to be
// completed rather than shipping silent wrong answers.
func TestFDB_MultiwayJoinOrder_Limit(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_mwjo_limit")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_mwjo_limit")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE mwjo_limit_tmpl "+
		"CREATE TABLE a (id BIGINT NOT NULL, val STRING, PRIMARY KEY (id)) "+
		"CREATE TABLE b (id BIGINT NOT NULL, a_ref BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE c (id BIGINT NOT NULL, b_ref BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE d (id BIGINT NOT NULL, c_ref BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE hub (id BIGINT NOT NULL, x_ref BIGINT, y_ref BIGINT, label STRING, PRIMARY KEY (id)) "+
		"CREATE TABLE xx (id BIGINT NOT NULL, PRIMARY KEY (id)) "+
		"CREATE TABLE yy (id BIGINT NOT NULL, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_mwjo_limit/s WITH TEMPLATE mwjo_limit_tmpl")

	dsn := fmt.Sprintf("fdbsql:///testdb_mwjo_limit?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	// chain a -> b -> c -> d (one matching row through the whole chain)
	mwjoMustExec(t, db, ctx, "INSERT INTO a VALUES (1, 'alpha')")
	mwjoMustExec(t, db, ctx, "INSERT INTO b VALUES (10, 1)")
	mwjoMustExec(t, db, ctx, "INSERT INTO c VALUES (100, 10)")
	mwjoMustExec(t, db, ctx, "INSERT INTO d VALUES (1000, 100)")
	mwjoMustExec(t, db, ctx, "INSERT INTO d VALUES (2000, 999)")
	// star: hub -> xx, yy (a 3-way join, the supported multi-way shape)
	mwjoMustExec(t, db, ctx, "INSERT INTO xx VALUES (5)")
	mwjoMustExec(t, db, ctx, "INSERT INTO yy VALUES (7)")
	mwjoMustExec(t, db, ctx, "INSERT INTO hub VALUES (1, 5, 7, 'hublabel')")
	mwjoMustExec(t, db, ctx, "INSERT INTO hub VALUES (2, 5, 99, 'nomatch')")

	// query returns (rows, queryError).
	query := func(q string) ([]string, error) {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var v sql.NullString
			if err := rows.Scan(&v); err != nil {
				return nil, err
			}
			got = append(got, v.String)
		}
		return got, rows.Err()
	}

	// (1) 3-way star join — the SUPPORTED multi-way shape — returns the correct
	// projected value regardless of which hub row's y_ref matches.
	got, err := query("SELECT hub.label FROM hub, xx, yy WHERE hub.x_ref = xx.id AND hub.y_ref = yy.id")
	if err != nil {
		t.Fatalf("3-way star join errored, want correct rows: %v", err)
	}
	if len(got) != 1 || got[0] != "hublabel" {
		t.Errorf("3-way star join = %q, want [\"hublabel\"]", got)
	}

	// (2) 4-way chain joins (both FROM-orders) are NOT supported: they must fail
	// to plan LOUDLY, never return a silent wrong row. The bug this pins: before
	// the 3-way gate, the 4-way planned but the a.val projection resolved to NULL
	// (a single [""] row) — silent wrong data. The contract is: either a correct
	// "alpha" row, or a loud plan-failure. NOT an empty/NULL row.
	for _, q := range []string{
		"SELECT a.val FROM a, b, c, d WHERE a.id = b.a_ref AND b.id = c.b_ref AND c.id = d.c_ref",
		"SELECT a.val FROM d, c, b, a WHERE a.id = b.a_ref AND b.id = c.b_ref AND c.id = d.c_ref",
	} {
		got, err := query(q)
		if err != nil {
			// Loud plan-failure is the accepted behavior for the unsupported
			// ≥4-way shape. Confirm it is a plan failure, not some unrelated error.
			if !strings.Contains(err.Error(), "could not plan") {
				t.Errorf("4-way join %q failed with unexpected error (want a plan-failure): %v", q, err)
			}
			continue
		}
		// If it DID plan, it MUST be correct — exactly one "alpha" row. A silent
		// wrong result (e.g. a [""] NULL row) is forbidden.
		if len(got) != 1 || got[0] != "alpha" {
			t.Errorf("4-way join %q PLANNED but returned wrong rows %q, want [\"alpha\"] "+
				"(silent wrong data — the N-way executor resolution must be completed "+
				"before 4-way is allowed to plan)", q, got)
		}
	}
}
