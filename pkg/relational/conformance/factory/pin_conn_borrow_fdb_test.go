package factory_test

import (
	"context"
	"testing"

	"fdb.dev/pkg/relational/conformance/factory"
)

// PinConn's disabled planner rules last its borrow: the pooled connection's
// next borrower plans with the connector's options again.
func TestFDB_PinConnRulesLastOneBorrow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openFactorySchema(t, ctx, "pinborrow",
		"CREATE TABLE t (id BIGINT, a BIGINT, PRIMARY KEY (id)) CREATE INDEX idx_a ON t (a)")
	db.SetMaxOpenConns(1)
	const query = "SELECT id, a FROM t WHERE a = 5"
	explainOnce := func(rules []string) string {
		conn, err := factory.PinConn(ctx, db, rules)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close() //nolint:errcheck
		return explainVia(t, ctx, conn, query)
	}
	base := explainOnce(nil)
	if alt := explainOnce(factory.SecondPlanRules()); alt == base {
		t.Fatalf("the disabled rules did not change the plan (%s); the check below would prove nothing", base)
	}
	if next := explainOnce(nil); next != base {
		t.Fatalf("the next borrower plans %s, want %s: the disabled rules outlived their borrow", next, base)
	}
}
