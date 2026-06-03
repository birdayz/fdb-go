package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// Regression sentinel for the RFC-069 join-ordering regression (the stress
// join_10_outer catastrophe). The query
//
//	SELECT o.id, c.name FROM orders o, customers c WHERE o.customer_id = c.id AND o.id < 10 ORDER BY o.id
//
// must drive off the SELECTIVE predicate o.id < 10 (a 10-row PK range scan on ORDERS),
// then PK-lookup customers. The regressed cost model instead planned
//
//	FlatMap(outer=Scan(CUSTOMERS), inner=PredicatesFilter(Scan(ORDERS)))
//
// — a nested loop re-scanning all ORDERS per customer (catastrophic at scale). This
// pins the corrected plan shape at small scale; TestFDB_Stress_1M/join_10_outer covers 1M.
func TestFDB_JoinSelPred_Repro(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_jsp")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_jsp")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE jsp_tmpl "+
			"CREATE TABLE orders (id BIGINT NOT NULL, customer_id BIGINT NOT NULL, PRIMARY KEY (id)) "+
			"CREATE TABLE customers (id BIGINT NOT NULL, name STRING, PRIMARY KEY (id)) "+
			"CREATE INDEX idx_customer ON orders (customer_id)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_jsp/s WITH TEMPLATE jsp_tmpl")

	dsn := fmt.Sprintf("fdbsql:///testdb_jsp?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	const nCust, nOrd = 100, 2000
	for i := 1; i <= nCust; i++ {
		mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO customers VALUES (%d, 'cust%d')", i, i))
	}
	for i := 1; i <= nOrd; i++ {
		mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO orders VALUES (%d, %d)", i, (i%nCust)+1))
	}

	plan := mwjoExplainer(t, db, ctx)(
		"SELECT o.id, c.name FROM orders o, customers c WHERE o.customer_id = c.id AND o.id < 10 ORDER BY o.id")
	t.Logf("PLAN[id<10 range]: %s", plan)
	planEq := mwjoExplainer(t, db, ctx)(
		"SELECT o.id, c.name FROM orders o, customers c WHERE o.customer_id = c.id AND o.id = 5")
	t.Logf("PLAN[id=5 point]: %s", planEq)

	up := strings.ToUpper(plan)
	// The bad plan: CUSTOMERS as the nested-loop OUTER (re-scans ORDERS per customer).
	if strings.Contains(up, "OUTER=SCAN(CUSTOMERS)") {
		t.Errorf("REGRESSION: nested loop drives off full Scan(CUSTOMERS), re-scanning ORDERS per row: %s", plan)
	}
}
