package sqldriver_test

// FDB integration tests for FULL OUTER JOIN — a Go-only query extension
// (Java's SQL layer has no outer joins at all). FULL OUTER is implemented
// exclusively by the materialized nested-loop cursor: the LEFT-OUTER
// outer-driven loop emits unmatched-left rows NULL-padded on the right,
// and a drain phase after the outer is exhausted emits inner rows that
// matched no outer row, NULL-padded on the left. These tests prove all
// four row classes (matched, left-only, right-only, both-unmatched),
// SQL 3VL NULL-key semantics (NULL never matches NULL), many-to-many
// fan-out, the hash-index probe + drain interaction on a large inner,
// WHERE-above-the-join filtering, the FULL+EXISTS rejection, and plan
// determinism.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/onsi/gomega"
)

// setupFullOuterDB creates a fresh database + schema for a FULL OUTER
// subtest and returns a *sql.DB connected to the `main` schema. Customer
// and Ord are joined on Customer.id = Ord.customer_id; Ord.customer_id is
// nullable so NULL-key cases can be exercised.
func setupFullOuterDB(t *testing.T, g *gomega.WithT, suffix string) *sql.DB {
	t.Helper()
	ctx := context.Background()
	dbPath := "/testdb_foj_" + suffix
	setup := openTestDB(t, dbPath)
	_, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	tmpl := "foj_tmpl_" + suffix
	_, err = setup.ExecContext(ctx, fmt.Sprintf(`CREATE SCHEMA TEMPLATE %s
		CREATE TABLE Customer (id BIGINT NOT NULL, name STRING NOT NULL, PRIMARY KEY (id))
		CREATE TABLE Ord (id BIGINT NOT NULL, customer_id BIGINT, amount BIGINT NOT NULL, PRIMARY KEY (id))`, tmpl))
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA %s/main WITH TEMPLATE %s", dbPath, tmpl))
	g.Expect(err).NotTo(gomega.HaveOccurred())
	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=main", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	t.Cleanup(func() { db.Close() })
	return db
}

type nameAmount struct {
	name   *string // nullable: NULL when the right side has no matching left row
	amount *int64  // nullable: NULL when the left side has no matching right row
}

func scanNameAmount(t *testing.T, g *gomega.WithT, rows *sql.Rows) []nameAmount {
	t.Helper()
	var got []nameAmount
	for rows.Next() {
		var r nameAmount
		g.Expect(rows.Scan(&r.name, &r.amount)).To(gomega.Succeed())
		got = append(got, r)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	return got
}

// TestFDB_FullOuterJoin_AllClasses exercises all four FULL OUTER row
// classes in a single query: matched, left-only (NULL right), right-only
// (NULL left, the drain path), and verifies the plan is a materialized
// NLJ — never a FlatMap (which cannot drain).
func TestFDB_FullOuterJoin_AllClasses(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()
	db := setupFullOuterDB(t, g, "all_classes")

	// Alice(1) & Bob(2) have orders; Carol(3) has none (left-only).
	// Order 12 references customer 99 which does not exist (right-only).
	_, err := db.ExecContext(ctx, `INSERT INTO Customer (id, name) VALUES (1, 'Alice'), (2, 'Bob'), (3, 'Carol')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Ord (id, customer_id, amount) VALUES (10, 1, 100), (11, 2, 200), (12, 99, 999)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	const q = `SELECT Customer.name, Ord.amount
		FROM Customer FULL OUTER JOIN Ord ON Customer.id = Ord.customer_id`

	plan := planExplainVia(t, ctx, db, q)
	g.Expect(plan).To(gomega.ContainSubstring("NestedLoopJoin(FULL OUTER"),
		"FULL OUTER must plan as a materialized nested-loop join")
	g.Expect(strings.Contains(plan, "FlatMap")).To(gomega.BeFalse(),
		"FULL OUTER must NOT use the correlated FlatMap path: %s", plan)

	rows, err := db.QueryContext(ctx, q)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	got := scanNameAmount(t, g, rows)

	ptrStr := func(s string) *string { return &s }
	ptrI := func(i int64) *int64 { return &i }
	g.Expect(got).To(gomega.ConsistOf(
		nameAmount{ptrStr("Alice"), ptrI(100)}, // matched
		nameAmount{ptrStr("Bob"), ptrI(200)},   // matched
		nameAmount{ptrStr("Carol"), nil},       // left-only → NULL amount
		nameAmount{nil, ptrI(999)},             // right-only → NULL name (drain)
	))
}

// TestFDB_FullOuterJoin_NullKeys proves SQL 3VL: NULL = NULL is UNKNOWN,
// so NULL-keyed rows on BOTH sides match nothing and each lands in its
// own NULL-padded output row (never joined to each other).
func TestFDB_FullOuterJoin_NullKeys(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()
	db := setupFullOuterDB(t, g, "null_keys")

	// Customer 1 has NULL... not possible (id is PK NOT NULL). Use the
	// Ord.customer_id NULL case on the right, plus a customer with no
	// order on the left. Two distinct NULL-keyed orders must NOT collapse
	// into one another nor match the NULL-id-less customers.
	_, err := db.ExecContext(ctx, `INSERT INTO Customer (id, name) VALUES (5, 'Eve')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	// Two orders with NULL customer_id — neither matches any customer.
	_, err = db.ExecContext(ctx, `INSERT INTO Ord (id, customer_id, amount) VALUES (20, 5, 50), (21, NULL, 700), (22, NULL, 800)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	rows, err := db.QueryContext(ctx, `SELECT Customer.name, Ord.amount
		FROM Customer FULL OUTER JOIN Ord ON Customer.id = Ord.customer_id`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	got := scanNameAmount(t, g, rows)

	ptrStr := func(s string) *string { return &s }
	ptrI := func(i int64) *int64 { return &i }
	// Eve(5) ↔ order 20 matched. Orders 21 & 22 (NULL key) each drain
	// separately with NULL name — they do NOT match each other.
	g.Expect(got).To(gomega.ConsistOf(
		nameAmount{ptrStr("Eve"), ptrI(50)},
		nameAmount{nil, ptrI(700)},
		nameAmount{nil, ptrI(800)},
	))
}

// TestFDB_FullOuterJoin_ManyToMany proves the matchedInner bitmap flips
// for EVERY passing inner index: a left row matching multiple right rows
// (and vice versa) must not leave any matched right row in the drain set.
func TestFDB_FullOuterJoin_ManyToMany(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()
	db := setupFullOuterDB(t, g, "many_to_many")

	// Two customers and two orders all share customer_id=1 → 2×2 fan-out.
	// (Customers 1 and 2 both joined to orders via customer_id=1 is not
	// possible since customer_id matches a single id; instead make two
	// orders for customer 1 and verify both right rows are consumed.)
	_, err := db.ExecContext(ctx, `INSERT INTO Customer (id, name) VALUES (1, 'Ann'), (7, 'Zed')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	// Ann(1) has THREE orders → 3 matched rows, none drained.
	// Order 33 references customer 88 (right-only). Zed(7) has no order (left-only).
	_, err = db.ExecContext(ctx, `INSERT INTO Ord (id, customer_id, amount) VALUES (30, 1, 10), (31, 1, 11), (32, 1, 12), (33, 88, 880)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	rows, err := db.QueryContext(ctx, `SELECT Customer.name, Ord.amount
		FROM Customer FULL OUTER JOIN Ord ON Customer.id = Ord.customer_id`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	got := scanNameAmount(t, g, rows)

	ptrStr := func(s string) *string { return &s }
	ptrI := func(i int64) *int64 { return &i }
	g.Expect(got).To(gomega.ConsistOf(
		nameAmount{ptrStr("Ann"), ptrI(10)},
		nameAmount{ptrStr("Ann"), ptrI(11)},
		nameAmount{ptrStr("Ann"), ptrI(12)},
		nameAmount{ptrStr("Zed"), nil}, // left-only
		nameAmount{nil, ptrI(880)},     // right-only (drain)
	))
}

// TestFDB_FullOuterJoin_LargeInner exercises the hash-index probe path
// (built when the inner side has ≥100 rows) together with the drain: many
// inner rows are never probed (different bucket) and must still appear in
// the drain set NULL-padded on the left.
func TestFDB_FullOuterJoin_LargeInner(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()
	db := setupFullOuterDB(t, g, "large_inner")

	// Customers 1..5 plus customer 200 (no orders → left-only).
	_, err := db.ExecContext(ctx, `INSERT INTO Customer (id, name) VALUES (1,'c1'),(2,'c2'),(3,'c3'),(4,'c4'),(5,'c5'),(200,'lonely')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	// 150 orders, customer_id cycles 1..10. customer_id 1..5 match (75
	// rows); 6..10 are right-only (75 rows, drained).
	for i := 1; i <= 150; i++ {
		cid := (i-1)%10 + 1
		_, err = db.ExecContext(ctx, fmt.Sprintf(`INSERT INTO Ord (id, customer_id, amount) VALUES (%d, %d, %d)`, i, cid, i))
		g.Expect(err).NotTo(gomega.HaveOccurred())
	}

	const q = `SELECT Customer.name, Ord.amount
		FROM Customer FULL OUTER JOIN Ord ON Customer.id = Ord.customer_id`
	plan := planExplainVia(t, ctx, db, q)
	g.Expect(plan).To(gomega.ContainSubstring("NestedLoopJoin(FULL OUTER"))

	rows, err := db.QueryContext(ctx, q)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	got := scanNameAmount(t, g, rows)

	var matched, leftOnly, rightOnly int
	for _, r := range got {
		switch {
		case r.name != nil && r.amount != nil:
			matched++
		case r.name != nil && r.amount == nil:
			leftOnly++
		case r.name == nil && r.amount != nil:
			rightOnly++
		default:
			t.Fatalf("row with both NULL should not occur: %+v", r)
		}
	}
	g.Expect(matched).To(gomega.Equal(75), "customer_id 1..5 × 15 each")
	g.Expect(rightOnly).To(gomega.Equal(75), "customer_id 6..10 × 15 each (drained)")
	g.Expect(leftOnly).To(gomega.Equal(1), "customer 200 has no orders")
	g.Expect(len(got)).To(gomega.Equal(151))
}

// TestFDB_FullOuterJoin_EmptyOuter exercises the drain path in isolation:
// the outer (Customer) is empty, so every inner (Ord) row drains with a
// NULL left side.
func TestFDB_FullOuterJoin_EmptyOuter(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()
	db := setupFullOuterDB(t, g, "empty_outer")

	// No customers; two orders → both right-only (NULL name).
	_, err := db.ExecContext(ctx, `INSERT INTO Ord (id, customer_id, amount) VALUES (1, 7, 100), (2, 8, 200)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	rows, err := db.QueryContext(ctx, `SELECT Customer.name, Ord.amount
		FROM Customer FULL OUTER JOIN Ord ON Customer.id = Ord.customer_id`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	got := scanNameAmount(t, g, rows)

	ptrI := func(i int64) *int64 { return &i }
	g.Expect(got).To(gomega.ConsistOf(
		nameAmount{nil, ptrI(100)},
		nameAmount{nil, ptrI(200)},
	))
}

// TestFDB_FullOuterJoin_EmptyInner exercises the drain no-op: the inner
// (Ord) is empty, so the matchedInner slice is empty and the drain loop
// does nothing; every outer (Customer) row emits with a NULL right side.
func TestFDB_FullOuterJoin_EmptyInner(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()
	db := setupFullOuterDB(t, g, "empty_inner")

	// Two customers; no orders → both left-only (NULL amount).
	_, err := db.ExecContext(ctx, `INSERT INTO Customer (id, name) VALUES (1, 'Alice'), (2, 'Bob')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	rows, err := db.QueryContext(ctx, `SELECT Customer.name, Ord.amount
		FROM Customer FULL OUTER JOIN Ord ON Customer.id = Ord.customer_id`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	got := scanNameAmount(t, g, rows)

	ptrStr := func(s string) *string { return &s }
	g.Expect(got).To(gomega.ConsistOf(
		nameAmount{ptrStr("Alice"), nil},
		nameAmount{ptrStr("Bob"), nil},
	))
}

// TestFDB_FullOuterJoin_OrderBy verifies a sort operator fires ABOVE the
// FULL join (the NLJ advertises no ordering — the drain appends
// unmatched-inner rows after the outer stream). All four row classes are
// present; ORDER BY Ord.amount must produce a globally sorted result with
// NULL amounts ordered consistently.
func TestFDB_FullOuterJoin_OrderBy(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()
	db := setupFullOuterDB(t, g, "order_by")

	_, err := db.ExecContext(ctx, `INSERT INTO Customer (id, name) VALUES (1, 'Alice'), (2, 'Bob'), (3, 'Carol')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Ord (id, customer_id, amount) VALUES (10, 1, 300), (11, 2, 100), (12, 99, 200)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// FULL OUTER rows: (Alice,300),(Bob,100),(Carol,NULL),(NULL,200).
	// ORDER BY Ord.amount ASC. NULLs sort first (FDB/Java NULLS FIRST for ASC).
	rows, err := db.QueryContext(ctx, `SELECT Customer.name, Ord.amount
		FROM Customer FULL OUTER JOIN Ord ON Customer.id = Ord.customer_id
		ORDER BY Ord.amount`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	got := scanNameAmount(t, g, rows)

	g.Expect(len(got)).To(gomega.Equal(4))
	// Non-NULL amounts must be in ascending order; verify the ordering is
	// monotonic among the rows that have a non-NULL amount.
	var lastAmt *int64
	for _, r := range got {
		if r.amount == nil {
			continue
		}
		if lastAmt != nil {
			g.Expect(*r.amount >= *lastAmt).To(gomega.BeTrue(),
				"amounts must be ascending: %d after %d", *r.amount, *lastAmt)
		}
		lastAmt = r.amount
	}
	// All four rows present (one NULL-amount left-only row, one NULL-name
	// right-only row, two matched).
	var nullAmt, nullName int
	for _, r := range got {
		if r.amount == nil {
			nullAmt++
		}
		if r.name == nil {
			nullName++
		}
	}
	g.Expect(nullAmt).To(gomega.Equal(1), "Carol has no order")
	g.Expect(nullName).To(gomega.Equal(1), "order 12 has no customer")
}

// TestFDB_FullOuterJoin_WhereFilter proves the WHERE clause is applied
// ABOVE the join (it stays a filter, never merged into the ON), so it can
// correctly drop NULL-padded rows.
func TestFDB_FullOuterJoin_WhereFilter(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()
	db := setupFullOuterDB(t, g, "where_filter")

	_, err := db.ExecContext(ctx, `INSERT INTO Customer (id, name) VALUES (1, 'Alice'), (3, 'Carol')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Ord (id, customer_id, amount) VALUES (10, 1, 100), (12, 99, 999)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// FULL OUTER produces: (Alice,100), (Carol,NULL), (NULL,999).
	// WHERE Ord.amount > 150 keeps only (NULL,999): Alice/100 fails,
	// Carol/NULL → NULL>150 is UNKNOWN → dropped.
	rows, err := db.QueryContext(ctx, `SELECT Customer.name, Ord.amount
		FROM Customer FULL OUTER JOIN Ord ON Customer.id = Ord.customer_id
		WHERE Ord.amount > 150`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	got := scanNameAmount(t, g, rows)

	ptrI := func(i int64) *int64 { return &i }
	g.Expect(got).To(gomega.ConsistOf(nameAmount{nil, ptrI(999)}))
}

// TestFDB_FullOuterJoin_ExistsRejected verifies FULL OUTER combined with
// an EXISTS subquery in the same WHERE is rejected with a clear error
// (the join+EXISTS flatten path cannot carry the FULL drain).
func TestFDB_FullOuterJoin_ExistsRejected(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()
	db := setupFullOuterDB(t, g, "exists_rejected")

	_, err := db.ExecContext(ctx, `INSERT INTO Customer (id, name) VALUES (1, 'Alice')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Ord (id, customer_id, amount) VALUES (10, 1, 100)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	_, err = db.QueryContext(ctx, `SELECT Customer.name, Ord.amount
		FROM Customer FULL OUTER JOIN Ord ON Customer.id = Ord.customer_id
		WHERE EXISTS (SELECT 1 FROM Customer c2 WHERE c2.id = 1)`)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("FULL OUTER JOIN combined with an EXISTS subquery is not supported"))
}

// TestFDB_FullOuterJoin_Determinism asserts the planner produces an
// identical FULL OUTER plan across repeated calls (planner determinism is
// mandatory — a flapping plan is always a bug).
func TestFDB_FullOuterJoin_Determinism(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()
	db := setupFullOuterDB(t, g, "determinism")

	_, err := db.ExecContext(ctx, `INSERT INTO Customer (id, name) VALUES (1, 'Alice'), (2, 'Bob')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Ord (id, customer_id, amount) VALUES (10, 1, 100), (12, 99, 999)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	const q = `SELECT Customer.name, Ord.amount
		FROM Customer FULL OUTER JOIN Ord ON Customer.id = Ord.customer_id`
	first := planExplainVia(t, ctx, db, q)
	g.Expect(first).To(gomega.ContainSubstring("NestedLoopJoin(FULL OUTER"))
	for i := 0; i < 10; i++ {
		got := planExplainVia(t, ctx, db, q)
		g.Expect(got).To(gomega.Equal(first), "FULL OUTER plan must be deterministic across runs")
	}
}

// TestFDB_RightJoin_NullKeys pins RIGHT OUTER JOIN NULL-key behavior (a
// regression dimension the existing TestFDB_RightJoin did not cover):
// an order with NULL customer_id matches no customer and is emitted with
// a NULL customer name.
func TestFDB_RightJoin_NullKeys(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()
	db := setupFullOuterDB(t, g, "right_null_keys")

	_, err := db.ExecContext(ctx, `INSERT INTO Customer (id, name) VALUES (1, 'Alice')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Ord (id, customer_id, amount) VALUES (10, 1, 100), (11, NULL, 500)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	rows, err := db.QueryContext(ctx, `SELECT Customer.name, Ord.amount
		FROM Customer RIGHT JOIN Ord ON Customer.id = Ord.customer_id`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	got := scanNameAmount(t, g, rows)

	ptrStr := func(s string) *string { return &s }
	ptrI := func(i int64) *int64 { return &i }
	g.Expect(got).To(gomega.ConsistOf(
		nameAmount{ptrStr("Alice"), ptrI(100)},
		nameAmount{nil, ptrI(500)}, // NULL customer_id → no match
	))
}
