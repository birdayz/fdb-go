package fdb

import (
	"context"
	"fmt"
)

// Transact executes fn inside a retried read-write transaction and returns its
// typed result directly. When db implements [CtxTransactor] (such as [Database]
// or [Tenant]), ctx bounds the retry loop, backoff, and reads; otherwise
// ctx.Err() is checked before falling back to [Transactor.Transact].
func Transact[T any](ctx context.Context, db Transactor, fn func(WritableTransaction) (T, error)) (T, error) {
	var zero T
	if db == nil || fn == nil {
		return zero, Error{Code: 2000}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	wrapped := func(tx WritableTransaction) (any, error) {
		return fn(tx)
	}

	var (
		raw any
		err error
	)
	if ct, ok := db.(CtxTransactor); ok {
		raw, err = ct.TransactCtx(ctx, wrapped)
	} else {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		raw, err = db.Transact(wrapped)
	}
	if err != nil {
		return zero, err
	}
	if raw == nil {
		return zero, nil
	}
	val, ok := raw.(T)
	if !ok {
		return zero, fmt.Errorf("fdb: unexpected transaction result type %T", raw)
	}
	return val, nil
}

// ReadTransact executes fn inside a retried read-only transaction and returns its
// typed result directly. When db implements [CtxReadTransactor] (such as
// [Database] or [Tenant]), ctx bounds the retry loop, backoff, and reads;
// otherwise ctx.Err() is checked before falling back to
// [ReadTransactor.ReadTransact].
func ReadTransact[T any](ctx context.Context, db ReadTransactor, fn func(ReadTransaction) (T, error)) (T, error) {
	var zero T
	if db == nil || fn == nil {
		return zero, Error{Code: 2000}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	wrapped := func(tx ReadTransaction) (any, error) {
		return fn(tx)
	}

	var (
		raw any
		err error
	)
	if ct, ok := db.(CtxReadTransactor); ok {
		raw, err = ct.ReadTransactCtx(ctx, wrapped)
	} else {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		raw, err = db.ReadTransact(wrapped)
	}
	if err != nil {
		return zero, err
	}
	if raw == nil {
		return zero, nil
	}
	val, ok := raw.(T)
	if !ok {
		return zero, fmt.Errorf("fdb: unexpected transaction result type %T", raw)
	}
	return val, nil
}
