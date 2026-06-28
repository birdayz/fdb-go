package recordlayer

import (
	"context"
	"errors"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
)

// ctxSpyTransactor implements fdb.Transactor + the optional fdb.CtxTransactor /
// fdb.CtxReadTransactor capabilities, recording the ctx it is handed without running
// fn (so no real FDB transaction is needed). It proves Run/RunRead route through the
// ctx-aware path with the *caller's* ctx (RFC-090) — the behavior the old code lost by
// passing context.Background() into the retry loop.
type ctxSpyTransactor struct {
	gotWriteCtx context.Context
	gotReadCtx  context.Context
}

func (s *ctxSpyTransactor) Transact(func(fdb.WritableTransaction) (any, error)) (any, error) {
	return nil, errors.New("ctxSpyTransactor.Transact called — Run must use TransactCtx")
}

func (s *ctxSpyTransactor) TransactCtx(ctx context.Context, _ func(fdb.WritableTransaction) (any, error)) (any, error) {
	s.gotWriteCtx = ctx
	return nil, ctx.Err()
}

func (s *ctxSpyTransactor) ReadTransact(func(fdb.ReadTransaction) (any, error)) (any, error) {
	return nil, errors.New("ctxSpyTransactor.ReadTransact called — RunRead must use ReadTransactCtx")
}

func (s *ctxSpyTransactor) ReadTransactCtx(ctx context.Context, _ func(fdb.ReadTransaction) (any, error)) (any, error) {
	s.gotReadCtx = ctx
	return nil, ctx.Err()
}

func TestRun_ThreadsCallerCtxIntoTransactCtx(t *testing.T) {
	t.Parallel()
	spy := &ctxSpyTransactor{}
	d := &FDBDatabase{transactor: spy}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before Run

	_, err := d.Run(ctx, func(*FDBRecordContext) (any, error) { return nil, nil })

	if spy.gotWriteCtx != ctx {
		t.Fatalf("Run did not thread the caller's ctx into the retry loop (got %v) — "+
			"a cancelled/expired ctx would not bound retries", spy.gotWriteCtx)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled caller ctx must surface from Run; got %v", err)
	}
}

func TestRunRead_ThreadsCallerCtxIntoReadTransactCtx(t *testing.T) {
	t.Parallel()
	spy := &ctxSpyTransactor{}
	d := &FDBDatabase{transactor: spy}

	// Non-cancelled (RunRead returns early on an already-cancelled ctx, before the
	// transactor is even called — so use a live ctx to observe the threading).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := d.RunRead(ctx, func(fdb.ReadTransaction) (any, error) { return nil, nil }); err != nil {
		t.Fatalf("RunRead: %v", err)
	}
	if spy.gotReadCtx != ctx {
		t.Fatalf("RunRead did not thread the caller's ctx into ReadTransactCtx (got %v)", spy.gotReadCtx)
	}
}
