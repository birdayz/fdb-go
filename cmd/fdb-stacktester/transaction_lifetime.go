package main

import (
	"context"
	"sync"

	"fdb.dev/pkg/fdbgo/client"
	gofdb "fdb.dev/pkg/fdbgo/fdb"
)

type transactionWaits struct {
	mu       sync.Mutex
	next     uint64
	stopping bool
	cause    error
	active   map[uint64]func()
}

func (w *transactionWaits) track(cancel func()) func() {
	w.mu.Lock()
	if w.stopping {
		w.mu.Unlock()
		cancel()
		return func() {}
	}
	if w.active == nil {
		w.active = make(map[uint64]func())
	}
	w.next++
	id := w.next
	w.active[id] = cancel
	w.mu.Unlock()
	return func() { w.mu.Lock(); delete(w.active, id); w.mu.Unlock() }
}

func (w *transactionWaits) stop(cause error) {
	w.mu.Lock()
	if w.cause == nil {
		w.cause = cause
	}
	w.stopping = true
	active := w.active
	w.active = nil
	w.mu.Unlock()
	go func() {
		for _, cancel := range active {
			cancel()
		}
	}()
}

func (w *transactionWaits) failure() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cause
}

type namedTransaction struct {
	tx      *client.Transaction
	refs    int
	untrack func()
}

// Registry slots and dispatched COMMIT calls each own a reference.
// Callers of replaceTransaction and releaseTransaction hold trMu.
func (sm *StackMachine) replaceTransaction(name string) {
	tx := sm.db.CreateTransaction()
	next := &namedTransaction{tx: tx, refs: 1, untrack: sm.waits.track(tx.Cancel)}
	old := sm.trMap[name]
	sm.trMap[name] = next
	if old != nil {
		sm.releaseTransaction(old)
	}
}

func (sm *StackMachine) releaseTransaction(handle *namedTransaction) {
	handle.refs--
	if handle.refs == 0 {
		handle.untrack()
	}
}

func (sm *StackMachine) acquireCommitTransaction() (*client.Transaction, func()) {
	sm.trMu.Lock()
	handle := sm.trMap[sm.trName]
	handle.refs++
	sm.trMu.Unlock()
	return handle.tx, func() {
		sm.trMu.Lock()
		sm.releaseTransaction(handle)
		sm.trMu.Unlock()
	}
}

func (sm *StackMachine) transact(ctx context.Context, body func(*client.Transaction) (any, error)) (any, error) {
	var untrack []func()
	defer func() {
		for _, release := range untrack {
			release()
		}
	}()
	return sm.db.Transact(ctx, func(tx *client.Transaction) (any, error) {
		untrack = append(untrack, sm.waits.track(tx.Cancel))
		return body(tx)
	})
}

type directoryDatabaseTransactor struct {
	db    gofdb.Database
	ctx   context.Context
	waits *transactionWaits
}

func (d directoryDatabaseTransactor) Transact(body func(gofdb.WritableTransaction) (any, error)) (any, error) {
	var untrack []func()
	defer func() {
		for _, release := range untrack {
			release()
		}
	}()
	return d.db.TransactCtx(d.ctx, func(tx gofdb.WritableTransaction) (any, error) {
		untrack = append(untrack, d.waits.track(tx.Cancel))
		return body(tx)
	})
}

func (d directoryDatabaseTransactor) ReadTransact(body func(gofdb.ReadTransaction) (any, error)) (any, error) {
	var untrack []func()
	defer func() {
		for _, release := range untrack {
			release()
		}
	}()
	return d.db.ReadTransactCtx(d.ctx, func(tx gofdb.ReadTransaction) (any, error) {
		if cancellable, ok := tx.(interface{ Cancel() }); ok {
			untrack = append(untrack, d.waits.track(cancellable.Cancel))
		}
		return body(tx)
	})
}
