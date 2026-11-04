package embedded

import (
	"context"
	"database/sql/driver"
	"errors"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
)

func TestExplicitTxLateBeginCancellationAfterCommitHandoff(t *testing.T) {
	t.Parallel()
	g := newGatedSim(t, 1701)
	c := g.connect(t, "S")
	type key struct{}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), key{}, "retained"))
	defer cancel()
	tx, err := c.BeginTx(ctx, driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, c, "INSERT INTO t (id, v) VALUES (1701, 1)")
	rc := c.activeTx.rctx
	checks, posts := 0, 0
	rc.AddCommitCheck(func() error { checks++; cancel(); return nil })
	rc.AddPostCommit(func() { posts++ })
	if err := tx.Commit(); err != nil {
		t.Fatalf("detach won but commit failed: %v", err)
	}
	if checks != 1 || posts != 1 || c.activeTx != nil || g.gate.OpenTransactions() != 0 {
		t.Fatalf("checks=%d posts=%d active=%v handles=%d", checks, posts, c.activeTx, g.gate.OpenTransactions())
	}
	if rc.Context() != ctx || rc.Context().Value(key{}) != "retained" || !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("commit changed original context identity/values or failed to cancel the caller")
	}
	if n := countRows(t, c, 1701); n != 1 {
		t.Fatalf("durable rows=%d", n)
	}
	if err := tx.Commit(); err == nil {
		t.Fatal("committed twice")
	}
}

func TestExplicitTxCommitConflictClosesHandle(t *testing.T) {
	t.Parallel()
	g := newGatedSim(t, 1703)
	c := g.connect(t, "S")
	other := g.connect(t, "S")
	mustExec(t, other, "INSERT INTO t (id, v) VALUES (1703, 1)")
	tx, err := c.BeginTx(context.Background(), driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, c, 1703); n != 1 {
		t.Fatalf("transaction did not read initial row: %d", n)
	}
	mustExec(t, c, "INSERT INTO t (id, v) VALUES (1704, 2)")
	mustExec(t, other, "UPDATE t SET v = 3 WHERE id = 1703")
	posts := 0
	c.activeTx.rctx.AddPostCommit(func() { posts++ })
	err = tx.Commit()
	var conflict fdb.Error
	if !errors.As(err, &conflict) || conflict.Code != 1020 {
		t.Fatalf("commit=%v, want real not_committed (1020)", err)
	}
	if posts != 0 || c.activeTx != nil || g.gate.OpenTransactions() != 0 {
		t.Fatalf("posts=%d active=%v handles=%d", posts, c.activeTx, g.gate.OpenTransactions())
	}
	if n := countRows(t, other, 1704); n != 0 {
		t.Fatalf("conflicted transaction committed %d rows", n)
	}
}

func TestExplicitTxHandoffPreservesExplicitCheckErrorsAndClosesHandle(t *testing.T) {
	t.Parallel()
	for _, explicitContext := range []bool{false, true} {
		t.Run(map[bool]string{false: "sentinel", true: "explicit_context_error"}[explicitContext], func(t *testing.T) {
			g := newGatedSim(t, 1702)
			c := g.connect(t, "S")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			tx, err := c.BeginTx(ctx, driver.TxOptions{})
			if err != nil {
				t.Fatal(err)
			}
			mustExec(t, c, "INSERT INTO t (id, v) VALUES (1702, 1)")
			sentinel := errors.New("explicit commit check failure")
			want := error(sentinel)
			if explicitContext {
				want = context.Canceled
			}
			posts := 0
			c.activeTx.rctx.AddPostCommit(func() { posts++ })
			c.activeTx.rctx.AddCommitCheck(func() error { cancel(); return want })
			if err := tx.Commit(); !errors.Is(err, want) {
				t.Fatalf("commit=%v want %v", err, want)
			}
			if posts != 0 || c.activeTx != nil || g.gate.OpenTransactions() != 0 {
				t.Fatalf("posts=%d handles=%d", posts, g.gate.OpenTransactions())
			}
			if n := countRows(t, c, 1702); n != 0 {
				t.Fatalf("failed check committed %d rows", n)
			}
		})
	}
}
