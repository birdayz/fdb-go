// Package simgate wraps a SimFDB database so a test can hold a read, commit or
// cancel in flight until it lets it go, or make one transaction call (or the
// transaction's creation) panic.
package simgate

import (
	"sync"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/simfdb"
)

// Call selects the call PanicOnce or BlockNextIn arms; only PanicOnce takes
// Begin.
type Call string

const (
	// Read is Get, GetKey or GetRange, on the transaction or its snapshot view.
	Read Call = "read"
	// Commit is the transaction's Commit.
	Commit Call = "commit"
	// Cancel is the transaction's Cancel.
	Cancel Call = "cancel"
	// Begin is the database's CreateWritableTransaction.
	Begin Call = "begin"
)

// Backend is a SimFDB database whose transactions block or panic on demand.
// Transactions from CreateWritableTransaction and Transact are wrapped;
// ReadTransact's are not.
type Backend struct {
	*simfdb.SimDB

	mu     sync.Mutex
	gates  map[Call]*Gate
	panics map[Call]bool
	open   map[*txn]bool // CreateWritableTransaction's, until committed or cancelled
}

// New wraps sim.
func New(sim *simfdb.SimDB) *Backend {
	return &Backend{SimDB: sim, gates: make(map[Call]*Gate), panics: make(map[Call]bool), open: make(map[*txn]bool)}
}

// Gate is one armed blocking call: the next such call blocks on it until
// Release, a Read or Commit also until its transaction is cancelled, then
// runs.
type Gate struct {
	only    *txn // nil: any wrapped transaction
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

// Entered is closed when a call blocks on the gate.
func (g *Gate) Entered() <-chan struct{} { return g.entered }

// Release lets the blocked (or the next) call go.
func (g *Gate) Release() { g.once.Do(func() { close(g.release) }) }

// BlockNextRead arms a gate for the next read through any wrapped
// transaction.
func (b *Backend) BlockNextRead() *Gate {
	return b.arm(Read, nil)
}

// BlockNextIn arms a gate for the next call c (Read, Commit or Cancel) on tx,
// which must be a transaction this backend created.
func (b *Backend) BlockNextIn(c Call, tx fdb.ReadTransaction) *Gate {
	t, ok := tx.(*txn)
	if !ok || t.b != b {
		panic("simgate: BlockNextIn on a transaction this backend did not create")
	}
	return b.arm(c, t)
}

func (b *Backend) arm(c Call, only *txn) *Gate {
	g := &Gate{only: only, entered: make(chan struct{}), release: make(chan struct{})}
	b.mu.Lock()
	b.gates[c] = g
	b.mu.Unlock()
	return g
}

// PanicOnce makes the next call c panic.
func (b *Backend) PanicOnce(c Call) {
	b.mu.Lock()
	b.panics[c] = true
	b.mu.Unlock()
}

func (b *Backend) takePanic(c Call) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	armed := b.panics[c]
	delete(b.panics, c)
	return armed
}

func (b *Backend) takeGate(c Call, t *txn) *Gate {
	b.mu.Lock()
	defer b.mu.Unlock()
	g := b.gates[c]
	if g == nil || (g.only != nil && g.only != t) {
		return nil
	}
	delete(b.gates, c)
	return g
}

// hold blocks on the armed gate for c, if any, until Release or (when
// untilCancel) the transaction's Cancel.
func (t *txn) hold(c Call, untilCancel bool) {
	g := t.b.takeGate(c, t)
	if g == nil {
		return
	}
	close(g.entered)
	if !untilCancel {
		<-g.release
		return
	}
	select {
	case <-g.release:
	case <-t.cancelled:
	}
}

func (b *Backend) wrap(tx fdb.WritableTransaction) *txn {
	return &txn{WritableTransaction: tx, b: b, cancelled: make(chan struct{})}
}

// CreateWritableTransaction is SimFDB's, wrapped.
func (b *Backend) CreateWritableTransaction() (fdb.WritableTransaction, error) {
	if b.takePanic(Begin) {
		panic("simgate: CreateWritableTransaction")
	}
	tx, err := b.SimDB.CreateWritableTransaction()
	if err != nil {
		return nil, err
	}
	t := b.wrap(tx)
	b.mu.Lock()
	b.open[t] = true
	b.mu.Unlock()
	return t, nil
}

// OpenTransactions counts the transactions CreateWritableTransaction made that
// were neither committed nor cancelled.
func (b *Backend) OpenTransactions() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.open)
}

func (b *Backend) closed(t *txn) {
	b.mu.Lock()
	delete(b.open, t)
	b.mu.Unlock()
}

// Transact is SimFDB's, with each attempt's transaction wrapped.
func (b *Backend) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return b.SimDB.Transact(func(tx fdb.WritableTransaction) (any, error) {
		return fn(b.wrap(tx))
	})
}

type txn struct {
	fdb.WritableTransaction
	b          *Backend
	cancelOnce sync.Once
	cancelled  chan struct{}
}

func (t *txn) beforeRead() {
	if t.b.takePanic(Read) {
		panic("simgate: read")
	}
	t.hold(Read, true)
}

func (t *txn) Get(key fdb.KeyConvertible) fdb.FutureByteSlice {
	t.beforeRead()
	return t.WritableTransaction.Get(key)
}

func (t *txn) GetKey(sel fdb.Selectable) fdb.FutureKey {
	t.beforeRead()
	return t.WritableTransaction.GetKey(sel)
}

func (t *txn) GetRange(r fdb.Range, options fdb.RangeOptions) fdb.RangeResult {
	t.beforeRead()
	return t.WritableTransaction.GetRange(r, options)
}

func (t *txn) Snapshot() fdb.ReadTransaction {
	return &snapshot{ReadTransaction: t.WritableTransaction.Snapshot(), t: t}
}

func (t *txn) Commit() fdb.FutureNil {
	if t.b.takePanic(Commit) {
		panic("simgate: Commit")
	}
	t.hold(Commit, true)
	f := t.WritableTransaction.Commit()
	if f.Get() == nil {
		t.b.closed(t)
	}
	return f
}

func (t *txn) Cancel() {
	if t.b.takePanic(Cancel) {
		panic("simgate: Cancel")
	}
	t.hold(Cancel, false)
	t.WritableTransaction.Cancel()
	t.b.closed(t)
	t.cancelOnce.Do(func() { close(t.cancelled) })
}

type snapshot struct {
	fdb.ReadTransaction
	t *txn
}

func (s *snapshot) Get(key fdb.KeyConvertible) fdb.FutureByteSlice {
	s.t.beforeRead()
	return s.ReadTransaction.Get(key)
}

func (s *snapshot) GetKey(sel fdb.Selectable) fdb.FutureKey {
	s.t.beforeRead()
	return s.ReadTransaction.GetKey(sel)
}

func (s *snapshot) GetRange(r fdb.Range, options fdb.RangeOptions) fdb.RangeResult {
	s.t.beforeRead()
	return s.ReadTransaction.GetRange(r, options)
}
