package directory

import (
	"bytes"
	"encoding/binary"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
)

func TestTxLockPoolSameKeySerializes(t *testing.T) {
	t.Parallel()

	pool := newTxLockPool[int]()
	const workers = 16
	const iters = 100

	var active atomic.Int32
	var overlaps atomic.Int32
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for range iters {
				g := pool.lockKey(42)
				if active.Add(1) != 1 {
					overlaps.Add(1)
				}
				runtime.Gosched()
				active.Add(-1)
				g.unlock()
			}
		}()
	}
	wg.Wait()

	if got := overlaps.Load(); got != 0 {
		t.Fatalf("same-key critical sections overlapped %d times", got)
	}
	pool.mu.Lock()
	remaining := len(pool.entries)
	pool.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("expected 0 retained entries after unlock, got %d", remaining)
	}
}

func TestTxLockPoolDistinctKeysDoNotBlock(t *testing.T) {
	t.Parallel()

	pool := newTxLockPool[int]()
	entered1 := make(chan struct{})
	entered2 := make(chan struct{})
	release := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		g := pool.lockKey(1)
		close(entered1)
		<-release
		g.unlock()
	}()
	go func() {
		defer wg.Done()
		g := pool.lockKey(2)
		close(entered2)
		<-release
		g.unlock()
	}()

	// Both goroutines must enter their critical sections concurrently; a single
	// process-wide mutex would deadlock here.
	<-entered1
	<-entered2
	close(release)
	wg.Wait()

	pool.mu.Lock()
	remaining := len(pool.entries)
	pool.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("expected 0 retained entries after unlock, got %d", remaining)
	}
}

// memHCATx is a minimal thread-safe in-memory fdb.WritableTransaction sufficient
// to exercise highContentionAllocator.allocate concurrently across distinct
// directoryLayer / HCA instances sharing one transaction.
type memHCATx struct {
	fdb.WritableTransaction

	mu                     sync.Mutex
	kv                     map[string][]byte
	nextWriteNoConflict    bool
	noConflictOptionBleeds atomic.Int64
}

func newMemHCATx() *memHCATx {
	return &memHCATx{kv: make(map[string][]byte)}
}

func (tx *memHCATx) Snapshot() fdb.ReadTransaction { return tx }
func (tx *memHCATx) Options() fdb.TransactionOptions {
	return memHCATxOptions{tx: tx}
}

type memHCATxOptions struct {
	fdb.TransactionOptions
	tx *memHCATx
}

func (o memHCATxOptions) SetNextWriteNoWriteConflictRange() error {
	o.tx.mu.Lock()
	defer o.tx.mu.Unlock()
	if o.tx.nextWriteNoConflict {
		// Another goroutine set the one-shot flag and has not consumed it yet.
		o.tx.noConflictOptionBleeds.Add(1)
	}
	o.tx.nextWriteNoConflict = true
	return nil
}

type readyByteSliceFuture struct {
	fdb.FutureByteSlice
	val []byte
}

func (f readyByteSliceFuture) Get() ([]byte, error) { return f.val, nil }
func (f readyByteSliceFuture) MustGet() []byte      { return f.val }

type readyRangeResult struct {
	fdb.RangeResult
	kvs []fdb.KeyValue
}

func (r readyRangeResult) GetSliceWithError() ([]fdb.KeyValue, error) { return r.kvs, nil }

func (tx *memHCATx) Get(key fdb.KeyConvertible) fdb.FutureByteSlice {
	tx.mu.Lock()
	v, ok := tx.kv[string(key.FDBKey())]
	var cp []byte
	if ok {
		cp = make([]byte, len(v))
		copy(cp, v)
	}
	tx.mu.Unlock()
	return readyByteSliceFuture{val: cp}
}

func (tx *memHCATx) GetRange(r fdb.Range, opts fdb.RangeOptions) fdb.RangeResult {
	b, e := r.(fdb.ExactRange).FDBRangeKeys()
	begin, end := b.FDBKey(), e.FDBKey()

	tx.mu.Lock()
	var matched []fdb.KeyValue
	for k, v := range tx.kv {
		kb := []byte(k)
		if bytes.Compare(kb, begin) >= 0 && bytes.Compare(kb, end) < 0 {
			matched = append(matched, fdb.KeyValue{
				Key:   fdb.Key(kb),
				Value: append([]byte(nil), v...),
			})
		}
	}
	tx.mu.Unlock()

	sort.Slice(matched, func(i, j int) bool {
		return bytes.Compare(matched[i].Key, matched[j].Key) < 0
	})
	if opts.Reverse {
		for i, j := 0, len(matched)-1; i < j; i, j = i+1, j-1 {
			matched[i], matched[j] = matched[j], matched[i]
		}
	}
	if opts.Limit > 0 && len(matched) > opts.Limit {
		matched = matched[:opts.Limit]
	}
	return readyRangeResult{kvs: matched}
}

func (tx *memHCATx) Set(key fdb.KeyConvertible, value []byte) {
	k := string(key.FDBKey())
	tx.mu.Lock()
	tx.nextWriteNoConflict = false
	tx.kv[k] = append([]byte(nil), value...)
	tx.mu.Unlock()
}

func (tx *memHCATx) ClearRange(er fdb.ExactRange) {
	b, e := er.FDBRangeKeys()
	begin, end := b.FDBKey(), e.FDBKey()
	tx.mu.Lock()
	tx.nextWriteNoConflict = false
	for k := range tx.kv {
		kb := []byte(k)
		if bytes.Compare(kb, begin) >= 0 && bytes.Compare(kb, end) < 0 {
			delete(tx.kv, k)
		}
	}
	tx.mu.Unlock()
}

func (tx *memHCATx) Add(key fdb.KeyConvertible, param []byte) {
	k := string(key.FDBKey())
	var delta int64
	_ = binary.Read(bytes.NewReader(param), binary.LittleEndian, &delta)
	tx.mu.Lock()
	tx.nextWriteNoConflict = false
	var cur int64
	if existing, ok := tx.kv[k]; ok && len(existing) >= 8 {
		_ = binary.Read(bytes.NewReader(existing), binary.LittleEndian, &cur)
	}
	cur += delta
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, cur)
	tx.kv[k] = buf.Bytes()
	tx.mu.Unlock()
}

func (tx *memHCATx) AddWriteConflictKey(fdb.KeyConvertible) error { return nil }

func TestHCAAllocateConcurrentDistinctInstancesSameTx(t *testing.T) {
	t.Parallel()

	tx := newMemHCATx()
	hcaSubspace := subspace.Sub("hca")
	contentSubspace := subspace.Sub("content")

	const goroutines = 24
	results := make(chan string, goroutines)
	errs := make(chan error, goroutines)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := range goroutines {
		go func(id int) {
			defer wg.Done()
			// Construct a fresh HCA per goroutine (matching partition traversal
			// and per-call directory layer construction on a shared transaction)
			// and draw deterministically starting at 0 to maximize collision pressure.
			hca := newHCA(hcaSubspace)
			var attempt atomic.Int64
			hca.int63n = func(window int64) int64 {
				// Collide aggressively on candidate 0 first, then sweep window.
				return attempt.Add(1) % window
			}
			ss, err := hca.allocate(tx, contentSubspace)
			if err != nil {
				errs <- err
				return
			}
			results <- string(ss.Bytes())
		}(i)
	}
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Fatalf("allocate failed: %v", err)
	}
	if bleeds := tx.noConflictOptionBleeds.Load(); bleeds != 0 {
		t.Fatalf("SetNextWriteNoWriteConflictRange overlapped across goroutines %d times", bleeds)
	}
	seen := make(map[string]struct{}, goroutines)
	for prefix := range results {
		if _, dup := seen[prefix]; dup {
			t.Fatalf("duplicate directory prefix allocated concurrently on same transaction: %x", prefix)
		}
		seen[prefix] = struct{}{}
	}
	if len(seen) != goroutines {
		t.Fatalf("expected %d unique prefixes, got %d", goroutines, len(seen))
	}
}
