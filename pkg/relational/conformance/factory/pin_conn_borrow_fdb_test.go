package factory_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/conformance/factory"
	"fdb.dev/pkg/relational/core/embedded"
)

// PinConn's disabled planner rules last its borrow, over the connector's DSN
// options: the pooled connection's next plain borrower plans without them.
func TestFDB_PinConnRulesLastOneBorrow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	openFactorySchema(t, ctx, "pinborrow", "CREATE TABLE t (id BIGINT, a BIGINT, PRIMARY KEY (id)) CREATE INDEX idx_a ON t (a)")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///FRL/PINBORROW?cluster_file=%s&schema=PINBORROW&max_total_task_count=150000", clusterFilePath))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck
	db.SetMaxOpenConns(1)
	const query = "SELECT id, a FROM t WHERE a = 5"
	options := func(conn *sql.Conn) *api.Options {
		var o *api.Options
		if err := conn.Raw(func(dc any) error {
			o = dc.(*embedded.EmbeddedConnection).Options()
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return o
	}

	plain, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	base := explainVia(t, ctx, plain, query)
	plain.Close() //nolint:errcheck

	pinned, err := factory.PinConn(ctx, db, factory.SecondPlanRules())
	if err != nil {
		t.Fatal(err)
	}
	if got := options(pinned).Get(api.OptMaxTotalTaskCount); fmt.Sprint(got) != "150000" {
		t.Fatalf("pinned borrow's max_total_task_count = %v, want the DSN's 150000", got)
	}
	if alt := explainVia(t, ctx, pinned, query); alt == base {
		t.Fatalf("the disabled rules did not change the plan (%s); the check below would prove nothing", base)
	}
	pinned.Close() //nolint:errcheck

	next, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close() //nolint:errcheck
	if rules, _ := options(next).Get(api.OptDisabledPlannerRules).([]string); len(rules) != 0 {
		t.Fatalf("the next borrower's disabled rules = %v, want none", rules)
	}
	if got := explainVia(t, ctx, next, query); got != base {
		t.Fatalf("the next borrower plans %s, want %s: the disabled rules outlived their borrow", got, base)
	}
}
