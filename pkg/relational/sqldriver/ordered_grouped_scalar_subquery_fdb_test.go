package sqldriver_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// ogsDB sets up customers + orders for the ordered-grouped correlated scalar
// subquery tests (RFC-085). Customer 1's orders: status 'a' → {10,5} (SUM 15),
// status 'b' → {20} (SUM 20). So GROUP BY status ORDER BY status picks group 'a'
// (ASC, SUM 15) or 'b' (DESC, SUM 20) deterministically.
func ogsDB(t *testing.T, tag string) (*sql.DB, context.Context) {
	t.Helper()
	ctx := context.Background()
	dbPath := "/ogs_" + tag
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	tmpl := "ogs_tmpl_" + tag
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE "+tmpl+
		" CREATE TABLE customers (id BIGINT, name STRING, PRIMARY KEY (id))"+
		" CREATE TABLE orders (id BIGINT, customer_id BIGINT, status STRING, amount BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE ga (id BIGINT, customer_id BIGINT, k BIGINT, amount BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE gb (id BIGINT, a_id BIGINT, k BIGINT, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE "+tmpl); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.ExecContext(ctx, "INSERT INTO customers VALUES (1, 'c1')"); err != nil {
		t.Fatalf("seed c: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO orders VALUES (1,1,'a',10),(2,1,'a',5),(3,1,'b',20)"); err != nil {
		t.Fatalf("seed o: %v", err)
	}
	// Joined grouping keys deliberately share the bare name K but have
	// divergent order. Native groups are (a.k,b.k,sum)=(1,20,300),(2,10,100),
	// so ordering by either qualified leg proves which exact key was read.
	if _, err := db.ExecContext(ctx, "INSERT INTO ga VALUES (101,1,1,300),(102,1,2,100)"); err != nil {
		t.Fatalf("seed ga: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO gb VALUES (201,101,20),(202,102,10)"); err != nil {
		t.Fatalf("seed gb: %v", err)
	}
	return db, ctx
}

func ogsScalar(t *testing.T, ctx context.Context, db *sql.DB, q string) (int64, bool) {
	t.Helper()
	var n sql.NullInt64
	if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return n.Int64, n.Valid
}

func ogsScalarStr(t *testing.T, ctx context.Context, db *sql.DB, q string) (string, bool) {
	t.Helper()
	var s sql.NullString
	if err := db.QueryRowContext(ctx, q).Scan(&s); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return s.String, s.Valid
}

// TestFDB_OrderedGroupedScalarSubquery pins RFC-085: ORDER BY over the grouped
// output of a correlated scalar subquery gives the explicit user LIMIT 1 an
// exact, deterministic page (the shape was previously rejected outright).
func TestFDB_OrderedGroupedScalarSubquery(t *testing.T) {
	t.Parallel()
	db, ctx := ogsDB(t, "core")

	// ORDER BY the group key ASC + written LIMIT 1 → group 'a' → SUM(amount)=15.
	asc := "SELECT (SELECT SUM(o.amount) FROM orders o WHERE o.customer_id = c.id GROUP BY o.status ORDER BY o.status LIMIT 1) FROM customers c WHERE c.id = 1"
	if got, ok := ogsScalar(t, ctx, db, asc); !ok || got != 15 {
		t.Fatalf("ORDER BY status ASC: got %d (valid=%v), want 15 (group 'a')", got, ok)
	}

	// ORDER BY the group key DESC + written LIMIT 1 → group 'b' → SUM(amount)=20.
	desc := "SELECT (SELECT SUM(o.amount) FROM orders o WHERE o.customer_id = c.id GROUP BY o.status ORDER BY o.status DESC LIMIT 1) FROM customers c WHERE c.id = 1"
	if got, ok := ogsScalar(t, ctx, db, desc); !ok || got != 20 {
		t.Fatalf("ORDER BY status DESC: got %d (valid=%v), want 20 (group 'b')", got, ok)
	}
}

// TestFDB_OrderedGroupedScalarSubquery_ByAggregate pins ORDER BY the SELECTed
// aggregate itself (the central aggDatumKey path): ASC picks the min-SUM group,
// DESC the max-SUM group. Covers the aggregate spelled identically to SELECT,
// and spelled with a DIFFERING operand qualifier (the parse-tree FN(BAREARG)
// recovery in groupedScalarSortKeys).
func TestFDB_OrderedGroupedScalarSubquery_ByAggregate(t *testing.T) {
	t.Parallel()
	db, ctx := ogsDB(t, "byagg")
	// Customer 1 groups: 'a'→SUM 15, 'b'→SUM 20.
	cases := []struct {
		name string
		q    string
		want int64
	}{
		{"qualified_asc", "SELECT (SELECT SUM(o.amount) FROM orders o WHERE o.customer_id = c.id GROUP BY o.status ORDER BY SUM(o.amount) LIMIT 1) FROM customers c WHERE c.id = 1", 15},
		{"qualified_desc", "SELECT (SELECT SUM(o.amount) FROM orders o WHERE o.customer_id = c.id GROUP BY o.status ORDER BY SUM(o.amount) DESC LIMIT 1) FROM customers c WHERE c.id = 1", 20},
		// SELECT qualifies the operand, ORDER BY does not (and vice-versa) — both
		// must resolve to the same selected aggregate via the FN(BAREARG) key.
		{"select_qual_order_bare_asc", "SELECT (SELECT SUM(o.amount) FROM orders o WHERE o.customer_id = c.id GROUP BY o.status ORDER BY SUM(amount) LIMIT 1) FROM customers c WHERE c.id = 1", 15},
		{"select_bare_order_qual_desc", "SELECT (SELECT SUM(amount) FROM orders o WHERE o.customer_id = c.id GROUP BY o.status ORDER BY SUM(o.amount) DESC LIMIT 1) FROM customers c WHERE c.id = 1", 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, ok := ogsScalar(t, ctx, db, tc.q); !ok || got != tc.want {
				t.Fatalf("%s: got %d (valid=%v), want %d", tc.name, got, ok, tc.want)
			}
		})
	}
}

// TestFDB_OrderedGroupedScalarSubquery_ByAlias pins ORDER BY a SELECT alias of
// the aggregate (`SELECT SUM(o.amount) AS total … ORDER BY total`).
func TestFDB_OrderedGroupedScalarSubquery_ByAlias(t *testing.T) {
	t.Parallel()
	db, ctx := ogsDB(t, "byalias")
	asc := "SELECT (SELECT SUM(o.amount) AS total FROM orders o WHERE o.customer_id = c.id GROUP BY o.status ORDER BY total LIMIT 1) FROM customers c WHERE c.id = 1"
	if got, ok := ogsScalar(t, ctx, db, asc); !ok || got != 15 {
		t.Fatalf("ORDER BY alias ASC: got %d (valid=%v), want 15", got, ok)
	}
	desc := "SELECT (SELECT SUM(o.amount) AS total FROM orders o WHERE o.customer_id = c.id GROUP BY o.status ORDER BY total DESC LIMIT 1) FROM customers c WHERE c.id = 1"
	if got, ok := ogsScalar(t, ctx, db, desc); !ok || got != 20 {
		t.Fatalf("ORDER BY alias DESC: got %d (valid=%v), want 20", got, ok)
	}
}

// TestFDB_OrderedGroupedScalarSubquery_NullsOrdering pins NULLS FIRST/LAST over
// the grouped output: a group whose SUM is NULL (its only order has a NULL
// amount) sorts first or last per the clause.
func TestFDB_OrderedGroupedScalarSubquery_NullsOrdering(t *testing.T) {
	t.Parallel()
	db, ctx := ogsDB(t, "nulls")
	// Add status 'c' for customer 1 with a single NULL-amount order → SUM=NULL.
	if _, err := db.ExecContext(ctx, "INSERT INTO orders VALUES (4,1,'c',NULL)"); err != nil {
		t.Fatalf("seed null: %v", err)
	}
	// ASC NULLS FIRST: the NULL-SUM group 'c' sorts first → scalar NULL.
	nf := "SELECT (SELECT SUM(o.amount) FROM orders o WHERE o.customer_id = c.id GROUP BY o.status ORDER BY SUM(o.amount) ASC NULLS FIRST LIMIT 1) FROM customers c WHERE c.id = 1"
	if _, ok := ogsScalar(t, ctx, db, nf); ok {
		t.Fatalf("ASC NULLS FIRST: want NULL (group 'c'), got a value")
	}
	// ASC NULLS LAST: NULL sorts last → min non-null group 'a' (SUM 15) first.
	nl := "SELECT (SELECT SUM(o.amount) FROM orders o WHERE o.customer_id = c.id GROUP BY o.status ORDER BY SUM(o.amount) ASC NULLS LAST LIMIT 1) FROM customers c WHERE c.id = 1"
	if got, ok := ogsScalar(t, ctx, db, nl); !ok || got != 15 {
		t.Fatalf("ASC NULLS LAST: got %d (valid=%v), want 15 (group 'a')", got, ok)
	}
}

// TestFDB_OrderedGroupedScalarSubquery_ExplainSort pins that the inner plan
// actually materialises a Sort over the streaming aggregate — proving the ORDER
// BY fires (not silently dropped, which would "pass" via an arbitrary group).
func TestFDB_OrderedGroupedScalarSubquery_ExplainSort(t *testing.T) {
	t.Parallel()
	db, ctx := ogsDB(t, "explain")
	plan := planExplainVia(t, ctx, db, "SELECT (SELECT SUM(o.amount) FROM orders o WHERE o.customer_id = c.id GROUP BY o.status ORDER BY o.status LIMIT 1) FROM customers c WHERE c.id = 1")
	if !strings.Contains(plan, "Sort") {
		t.Errorf("expected a Sort in the inner plan, got: %s", plan)
	}
	if !strings.Contains(plan, "StreamingAgg") {
		t.Errorf("expected a StreamingAgg in the inner plan, got: %s", plan)
	}
}

// TestFDB_OrderedGroupedScalarSubquery_GroupKeyOnly pins the group-key-only
// (NON-aggregate) GROUP BY + ORDER BY path — the subquery selects a grouping
// column directly (no aggregate function). The zero-call LogicalAggregate has
// the same exact OutputSlots, structural sort-key binding, and one-field public
// Project contract as a real aggregate.
func TestFDB_OrderedGroupedScalarSubquery_GroupKeyOnly(t *testing.T) {
	t.Parallel()
	db, ctx := ogsDB(t, "gkonly")
	// Customer 1 has statuses 'a' and 'b'. ORDER BY o.status DESC LIMIT 1 → 'b'.
	desc := "SELECT (SELECT o.status FROM orders o WHERE o.customer_id = c.id GROUP BY o.status ORDER BY o.status DESC LIMIT 1) FROM customers c WHERE c.id = 1"
	if got, ok := ogsScalarStr(t, ctx, db, desc); !ok || got != "b" {
		t.Fatalf("group-key-only ORDER BY DESC: got %q (valid=%v), want \"b\"", got, ok)
	}
	// ASC → 'a'.
	asc := "SELECT (SELECT o.status FROM orders o WHERE o.customer_id = c.id GROUP BY o.status ORDER BY o.status ASC LIMIT 1) FROM customers c WHERE c.id = 1"
	if got, ok := ogsScalarStr(t, ctx, db, asc); !ok || got != "a" {
		t.Fatalf("group-key-only ORDER BY ASC: got %q (valid=%v), want \"a\"", got, ok)
	}
	// The selected key and GROUP BY key bind semantically, not by matching
	// qualifier spelling. A single surviving group avoids pagination and pins
	// the visible-output mapping itself.
	bareSelect := "SELECT (SELECT status FROM orders o WHERE o.customer_id = c.id AND status = 'a' GROUP BY o.status) FROM customers c WHERE c.id = 1"
	if got, ok := ogsScalarStr(t, ctx, db, bareSelect); !ok || got != "a" {
		t.Fatalf("bare SELECT / qualified GROUP BY: got %q (valid=%v), want \"a\"", got, ok)
	}

	// ORDER BY qualification is symmetric with GROUP BY qualification. These
	// cases bypass bare output-alias binding via AS chosen, so the raw key must
	// resolve structurally to the same native producer slot.
	qualificationCases := []struct {
		name string
		q    string
	}{
		{"qualified_group_bare_order", "SELECT (SELECT o.status AS chosen FROM orders o WHERE o.customer_id = c.id GROUP BY o.status ORDER BY status DESC LIMIT 1) FROM customers c WHERE c.id = 1"},
		{"bare_group_qualified_order", "SELECT (SELECT o.status AS chosen FROM orders o WHERE o.customer_id = c.id GROUP BY status ORDER BY o.status DESC LIMIT 1) FROM customers c WHERE c.id = 1"},
	}
	for _, tc := range qualificationCases {
		t.Run(tc.name, func(t *testing.T) {
			if got, ok := ogsScalarStr(t, ctx, db, tc.q); !ok || got != "b" {
				t.Fatalf("%s: got %q (valid=%v), want \"b\"", tc.name, got, ok)
			}
		})
	}
}

// TestFDB_OrderedGroupedScalarSubquery_GroupKeyQualificationSymmetry pins the
// same structural ORDER BY identity for real aggregates. DESC must select the
// status-b group (SUM=20) regardless of which spelling carries the redundant
// single-source qualifier.
func TestFDB_OrderedGroupedScalarSubquery_GroupKeyQualificationSymmetry(t *testing.T) {
	t.Parallel()
	db, ctx := ogsDB(t, "gk_qualification")
	cases := []struct {
		name string
		q    string
	}{
		{"qualified_group_bare_order", "SELECT (SELECT SUM(o.amount) FROM orders o WHERE o.customer_id = c.id GROUP BY o.status ORDER BY status DESC LIMIT 1) FROM customers c WHERE c.id = 1"},
		{"bare_group_qualified_order", "SELECT (SELECT SUM(o.amount) FROM orders o WHERE o.customer_id = c.id GROUP BY status ORDER BY o.status DESC LIMIT 1) FROM customers c WHERE c.id = 1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, ok := ogsScalar(t, ctx, db, tc.q); !ok || got != 20 {
				t.Fatalf("%s: got %d (valid=%v), want 20", tc.name, got, ok)
			}
		})
	}
}

// TestFDB_OrderedGroupedScalarSubquery_QualifiedJoinKeyIdentity pins the
// private aggregate ABI as [a.k,b.k,SUM(a.amount)] for both real aggregates and
// zero-call (group-key-only) aggregates. The joined sources intentionally use
// the same bare key name with opposite ordering: any name-based collapse reads
// the wrong native slot and returns a different, valid-looking scalar.
func TestFDB_OrderedGroupedScalarSubquery_QualifiedJoinKeyIdentity(t *testing.T) {
	t.Parallel()
	db, ctx := ogsDB(t, "join_key_identity")
	from := " FROM ga a JOIN gb b ON b.a_id = a.id WHERE a.customer_id = c.id GROUP BY a.k, b.k "
	outer := " FROM customers c WHERE c.id = 1"
	cases := []struct {
		name string
		expr string
		want int64
	}{
		// a.k asc picks (1,20,300); b.k asc picks (2,10,100).
		{"aggregate_order_a_key", "SUM(a.amount)" + from + "ORDER BY a.k LIMIT 1", 300},
		{"aggregate_order_b_key", "SUM(a.amount)" + from + "ORDER BY b.k LIMIT 1", 100},
		// The selected group key is native slot 1, not the scalar seed's
		// assumed slot 0. The exact one-field Project must expose b.k=20.
		{"group_key_only_second_native_slot", "b.k" + from + "ORDER BY a.k LIMIT 1", 20},
		// A qualified source reference must never fall back to the selected
		// aggregate's colliding bare alias K. Qualified a.k picks SUM=300,
		// whereas a mistaken alias-map fallback would order by SUM and pick 100.
		// (Bare alias binding itself is covered by ByAlias above; bare K is
		// intentionally source-ambiguous in this joined fixture.)
		{"qualified_key_beats_colliding_alias", "SUM(a.amount) AS k" + from + "ORDER BY a.k LIMIT 1", 300},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := "SELECT (SELECT " + tc.expr + ")" + outer
			if got, ok := ogsScalar(t, ctx, db, q); !ok || got != tc.want {
				t.Fatalf("%s: got %d (valid=%v), want %d; query=%s", tc.name, got, ok, tc.want, q)
			}
		})
	}

	t.Run("repeated_equivalent_single_source_keys", func(t *testing.T) {
		// Qualified a.k and bare k resolve to the same producer value on this
		// single-source inner. Both native grouping slots are equal, so ORDER BY
		// bare k may deterministically use the first. This must not weaken the
		// joined a.k/b.k distinction proved above.
		q := "SELECT (SELECT SUM(a.amount) FROM ga a WHERE a.customer_id = c.id GROUP BY a.k, k ORDER BY k LIMIT 1)" + outer
		if got, ok := ogsScalar(t, ctx, db, q); !ok || got != 300 {
			t.Fatalf("repeated equivalent keys: got %d (valid=%v), want 300; query=%s", got, ok, q)
		}
	})
}

// TestFDB_OrderedGroupedScalarSubquery_Determinism runs the ordered subquery
// repeatedly — the written one-row page must select the same group every time.
func TestFDB_OrderedGroupedScalarSubquery_Determinism(t *testing.T) {
	t.Parallel()
	db, ctx := ogsDB(t, "det")
	q := "SELECT (SELECT SUM(o.amount) FROM orders o WHERE o.customer_id = c.id GROUP BY o.status ORDER BY o.status LIMIT 1) FROM customers c WHERE c.id = 1"
	for i := 0; i < 10; i++ {
		if got, ok := ogsScalar(t, ctx, db, q); !ok || got != 15 {
			t.Fatalf("run %d: got %d (valid=%v), want 15", i, got, ok)
		}
	}
}

// TestFDB_OrderedGroupedScalarSubquery_Reject pins that ORDER BY over a column
// that is neither grouped nor a selected aggregate is rejected LOUDLY (not a
// silent-nil sort that would defeat the determinism this feature provides).
func TestFDB_OrderedGroupedScalarSubquery_Reject(t *testing.T) {
	t.Parallel()
	db, ctx := ogsDB(t, "rej")

	// assertGroupingReject confirms the query fails with SQLSTATE 42803 (grouping
	// error) — not a silent-ignored ORDER BY, and not some unrelated error.
	assertGroupingReject := func(why, q string) {
		t.Helper()
		_, err := db.ExecContext(ctx, q)
		if err == nil {
			t.Fatalf("%s should be rejected, not silently ignored", why)
		}
		var apiErr *api.Error
		if !errors.As(err, &apiErr) {
			t.Fatalf("%s: error is not *api.Error: %T %v", why, err, err)
		}
		if apiErr.Code != api.ErrCodeGroupingError {
			t.Fatalf("%s: error code = %s, want %s (42803)", why, apiErr.Code, api.ErrCodeGroupingError)
		}
	}

	// ORDER BY a non-grouped, non-aggregated column (amount) → reject.
	assertGroupingReject("ORDER BY a non-grouped/non-aggregated column",
		"SELECT (SELECT SUM(o.amount) FROM orders o WHERE o.customer_id = c.id GROUP BY o.status ORDER BY o.amount LIMIT 1) FROM customers c WHERE c.id = 1")

	// ORDER BY an aggregate that is NOT selected (the subquery selects SUM, orders
	// by COUNT) → reject (harvesting ORDER-BY-only aggregates is a future extension).
	assertGroupingReject("ORDER BY an unselected aggregate",
		"SELECT (SELECT SUM(o.amount) FROM orders o WHERE o.customer_id = c.id GROUP BY o.status ORDER BY COUNT(*) LIMIT 1) FROM customers c WHERE c.id = 1")
}

// TestFDB_OrderedScalarSubquery_NoGroupByUnchanged guards that ORDER BY WITHOUT
// GROUP BY (ordering rows before LIMIT 1) still works (it sorts the raw rows),
// with the ORDER BY key written either bare or qualified.
func TestFDB_OrderedScalarSubquery_NoGroupByUnchanged(t *testing.T) {
	t.Parallel()
	db, ctx := ogsDB(t, "nogb")
	// Pick the lowest-amount order's amount for c.id=1: ORDER BY amount ASC LIMIT 1 → 5.
	bare := "SELECT (SELECT amount FROM orders o WHERE o.customer_id = c.id ORDER BY amount LIMIT 1) FROM customers c WHERE c.id = 1"
	if got, ok := ogsScalar(t, ctx, db, bare); !ok || got != 5 {
		t.Fatalf("non-GROUP-BY ORDER BY (bare key): got %d (valid=%v), want 5", got, ok)
	}
	// A QUALIFIED ORDER BY key (`ORDER BY o.amount`) must resolve to the same
	// bare datum key the single-table scan row carries — otherwise it misses,
	// sorts every row equal, and silently returns an arbitrary row. Regression
	// for the qualifier-not-stripped bug surfaced by RFC-085.
	qualKey := "SELECT (SELECT amount FROM orders o WHERE o.customer_id = c.id ORDER BY o.amount LIMIT 1) FROM customers c WHERE c.id = 1"
	if got, ok := ogsScalar(t, ctx, db, qualKey); !ok || got != 5 {
		t.Fatalf("non-GROUP-BY ORDER BY (qualified key): got %d (valid=%v), want 5", got, ok)
	}
}

// TestFDB_QualifiedNonAggScalarSubquery pins that a QUALIFIED projection in a
// non-aggregate correlated scalar subquery (`SELECT o.amount`) resolves to the
// bare datum key instead of silently returning NULL. The scalarCol used to keep
// the `o.` qualifier (`O.AMOUNT`), which replaceScalarSubqueryRef double-prefixed
// to `O.O.AMOUNT` at read time → NULL. Surfaced while testing RFC-085. The
// no-ORDER-BY case proves the bug is in column resolution, not the sort.
func TestFDB_QualifiedNonAggScalarSubquery(t *testing.T) {
	t.Parallel()
	db, ctx := ogsDB(t, "qproj")

	// Qualified projection, no ORDER BY → first scan row's amount (id=1 → 10).
	noOrder := "SELECT (SELECT o.amount FROM orders o WHERE o.customer_id = c.id LIMIT 1) FROM customers c WHERE c.id = 1"
	if got, ok := ogsScalar(t, ctx, db, noOrder); !ok || got != 10 {
		t.Fatalf("qualified projection (no ORDER BY): got %d (valid=%v), want 10 (was NULL)", got, ok)
	}

	// Qualified projection + qualified ORDER BY → lowest amount (5).
	ordered := "SELECT (SELECT o.amount FROM orders o WHERE o.customer_id = c.id ORDER BY o.amount LIMIT 1) FROM customers c WHERE c.id = 1"
	if got, ok := ogsScalar(t, ctx, db, ordered); !ok || got != 5 {
		t.Fatalf("qualified projection + ORDER BY: got %d (valid=%v), want 5 (was NULL)", got, ok)
	}
}
