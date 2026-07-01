package bench

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	gofdb "fdb.dev/pkg/fdbgo/fdb"
	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
)

// Watch differential vs libfdb_c — the watch axis was under-probed (no go-vs-cgo coverage).
//
// FDB watch semantics (NativeAPI watchValue): a watch registered by a transaction fires when
// the storage server observes the key's value DIFFER from the value as of the transaction's
// read version. It must NOT fire while the value is unchanged, and it MUST fire after any
// change (set to a different value, or clear). The Go facade pins the watched value + read
// version synchronously in Watch() and long-polls in the returned FutureNil; libfdb_c does the
// same in C. This differential drives identical scenarios through both clients against ONE real
// FDB and asserts identical observable behavior.
//
// Determinism: the hard assertions are on the GUARANTEED behaviors — fires-after-change and
// fires-after-delete (FDB guarantees a watch fires once the value changes). The pre-change
// window proves the watch actually blocks (isn't a no-op that resolves instantly); a quiet
// single-process test container does not move shards or recover storage, so the documented
// rare spurious fire does not occur here. Every watch established is eventually triggered, so
// the long-poll goroutine always completes (no leak).

const (
	watchBlockWindow = 300 * time.Millisecond // value unchanged → watch must still be pending
	watchFireTimeout = 15 * time.Second       // after a change → watch must fire well within this
)

// watchClient abstracts the per-client operations the scenarios need, so one scenario body runs
// against both the pure-Go and the libfdb_c client.
type watchClient struct {
	name string
	// set commits key=val (used for both the seed and the triggering change).
	set func(t *testing.T, key, val []byte)
	// clear commits a delete of key.
	clear func(t *testing.T, key []byte)
	// watch establishes a watch on key inside a committed transaction and returns a channel
	// that receives the watch result (nil = fired) when the long-poll resolves.
	watch func(t *testing.T, key []byte) <-chan error
	// watchSelfWrite Sets key=val AND watches key in the SAME committed transaction (finding #8) —
	// the watch must register at the COMMITTED version so it stays pending until the next EXTERNAL change.
	watchSelfWrite func(t *testing.T, key, val []byte) <-chan error
}

func goWatchClient() watchClient {
	return watchClient{
		name: "go",
		set: func(t *testing.T, key, val []byte) {
			t.Helper()
			if _, err := goClient.Transact(func(txw gofdb.WritableTransaction) (any, error) {
				txw.Set(gofdb.Key(key), val)
				return nil, nil
			}); err != nil {
				t.Fatalf("go set: %v", err)
			}
		},
		clear: func(t *testing.T, key []byte) {
			t.Helper()
			if _, err := goClient.Transact(func(txw gofdb.WritableTransaction) (any, error) {
				txw.Clear(gofdb.Key(key))
				return nil, nil
			}); err != nil {
				t.Fatalf("go clear: %v", err)
			}
		},
		watch: func(t *testing.T, key []byte) <-chan error {
			t.Helper()
			var w gofdb.FutureNil
			if _, err := goClient.Transact(func(txw gofdb.WritableTransaction) (any, error) {
				w = txw.(gofdb.Transaction).Watch(gofdb.Key(key))
				return nil, nil
			}); err != nil {
				t.Fatalf("go establish watch: %v", err)
			}
			ch := make(chan error, 1)
			go func() { ch <- w.Get() }()
			return ch
		},
		watchSelfWrite: func(t *testing.T, key, val []byte) <-chan error {
			t.Helper()
			var w gofdb.FutureNil
			if _, err := goClient.Transact(func(txw gofdb.WritableTransaction) (any, error) {
				tx := txw.(gofdb.Transaction)
				tx.Set(gofdb.Key(key), val) // self-write, THEN watch the same key in this txn
				w = tx.Watch(gofdb.Key(key))
				return nil, nil
			}); err != nil {
				t.Fatalf("go self-write watch: %v", err)
			}
			ch := make(chan error, 1)
			go func() { ch <- w.Get() }()
			return ch
		},
	}
}

func cgoWatchClient() watchClient {
	return watchClient{
		name: "cgo",
		set: func(t *testing.T, key, val []byte) {
			t.Helper()
			if _, err := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
				tx.Set(cgofdb.Key(key), val)
				return nil, nil
			}); err != nil {
				t.Fatalf("cgo set: %v", err)
			}
		},
		clear: func(t *testing.T, key []byte) {
			t.Helper()
			if _, err := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
				tx.Clear(cgofdb.Key(key))
				return nil, nil
			}); err != nil {
				t.Fatalf("cgo clear: %v", err)
			}
		},
		watch: func(t *testing.T, key []byte) <-chan error {
			t.Helper()
			var w cgofdb.FutureNil
			if _, err := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
				w = tx.Watch(cgofdb.Key(key))
				return nil, nil
			}); err != nil {
				t.Fatalf("cgo establish watch: %v", err)
			}
			ch := make(chan error, 1)
			go func() { ch <- w.Get() }()
			return ch
		},
		watchSelfWrite: func(t *testing.T, key, val []byte) <-chan error {
			t.Helper()
			var w cgofdb.FutureNil
			if _, err := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
				tx.Set(cgofdb.Key(key), val)
				w = tx.Watch(cgofdb.Key(key))
				return nil, nil
			}); err != nil {
				t.Fatalf("cgo self-write watch: %v", err)
			}
			ch := make(chan error, 1)
			go func() { ch <- w.Get() }()
			return ch
		},
	}
}

// expectPending asserts the watch has NOT fired within d (the value is unchanged).
func expectPending(t *testing.T, label string, ch <-chan error, d time.Duration) {
	t.Helper()
	select {
	case err := <-ch:
		t.Fatalf("%s: watch fired while value unchanged (err=%v) — must stay pending", label, err)
	case <-time.After(d):
	}
}

// expectFired asserts the watch fires (nil error) within watchFireTimeout.
func expectFired(t *testing.T, label string, ch <-chan error) {
	t.Helper()
	select {
	case err := <-ch:
		if err != nil {
			t.Fatalf("%s: watch resolved with error, want nil (fired): %v", label, err)
		}
	case <-time.After(watchFireTimeout):
		t.Fatalf("%s: watch did not fire within %v after the change", label, watchFireTimeout)
	}
}

func watchKeyFor(t *testing.T, wc watchClient) []byte {
	return []byte(fmt.Sprintf("watch_%d_%s_%s", os.Getpid(), strings.ReplaceAll(t.Name(), "/", "_"), wc.name))
}

// TestDifferential_WatchFiresOnChange: for BOTH clients, a watch on a seeded key stays pending
// while the value is unchanged, then fires after the value is set to a different value.
func TestDifferential_WatchFiresOnChange(t *testing.T) {
	t.Parallel()
	for _, wc := range []watchClient{goWatchClient(), cgoWatchClient()} {
		wc := wc
		t.Run(wc.name, func(t *testing.T) {
			t.Parallel()
			key := watchKeyFor(t, wc)
			wc.set(t, key, []byte("v0"))
			ch := wc.watch(t, key)
			expectPending(t, wc.name, ch, watchBlockWindow)
			wc.set(t, key, []byte("v1")) // different value → must fire
			expectFired(t, wc.name, ch)
		})
	}
}

// TestDifferential_WatchFiresOnDelete: for BOTH clients, a watch on a seeded key fires after the
// key is cleared (a delete is a value change: present → absent).
func TestDifferential_WatchFiresOnDelete(t *testing.T) {
	t.Parallel()
	for _, wc := range []watchClient{goWatchClient(), cgoWatchClient()} {
		wc := wc
		t.Run(wc.name, func(t *testing.T) {
			t.Parallel()
			key := watchKeyFor(t, wc)
			wc.set(t, key, []byte("present"))
			ch := wc.watch(t, key)
			expectPending(t, wc.name, ch, watchBlockWindow)
			wc.clear(t, key) // present → absent → must fire
			expectFired(t, wc.name, ch)
		})
	}
}

// TestDifferential_WatchRYWDisabled pins the watches_disabled (1034) error: a transaction with
// read-your-writes disabled cannot establish a watch. C++ NativeAPI returns watches_disabled
// immediately; the Go client mirrors it in WatchSetup (tx.rywDisabled → 1034). Both clients must
// surface the same code through the watch future.
func TestDifferential_WatchRYWDisabled(t *testing.T) {
	t.Parallel()
	ns := strings.ReplaceAll(t.Name(), "/", "_")

	goCode := func() int {
		var w gofdb.FutureNil
		_, err := goClient.Transact(func(txw gofdb.WritableTransaction) (any, error) {
			tx := txw.(gofdb.Transaction)
			if e := tx.Options().SetReadYourWritesDisable(); e != nil {
				return nil, e
			}
			w = tx.Watch(gofdb.Key(fmt.Sprintf("watchryw_%d_%s_go", os.Getpid(), ns)))
			return nil, nil
		})
		if err != nil {
			return fdbErrorCode(err)
		}
		return fdbErrorCode(w.Get())
	}()

	cgoCode := func() int {
		var w cgofdb.FutureNil
		_, err := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
			if e := tx.Options().SetReadYourWritesDisable(); e != nil {
				return nil, e
			}
			w = tx.Watch(cgofdb.Key(fmt.Sprintf("watchryw_%d_%s_c", os.Getpid(), ns)))
			return nil, nil
		})
		if err != nil {
			return fdbErrorCode(err)
		}
		return fdbErrorCode(w.Get())
	}()

	if goCode != cgoCode {
		t.Fatalf("watch RYW-disabled: error code differs go=%d cgo=%d", goCode, cgoCode)
	}
	if goCode != 1034 {
		t.Fatalf("watch RYW-disabled: want 1034 watches_disabled, both returned %d", goCode)
	}
}

// TestDifferential_WatchSelfWriteStaysPending pins finding #8 / RFC-170: a transaction that WRITES a key
// and then WATCHES the same key must register the watch at the txn's COMMITTED version — so it stays
// PENDING until the next EXTERNAL change, exactly like libfdb_c, instead of firing on the txn's OWN write.
// Before the fix the Go client registered the watch at the READ version (< committed version): the storage
// server read the pre-write value, saw it differ from the watched value B, and fired the watch almost
// immediately — a silent divergence (a watch that fires when it must not). Covers the seeded baseline
// (k=A → Set B) and the absent baseline (k cleared → Set B); a genuine external change fires both.
func TestDifferential_WatchSelfWriteStaysPending(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		seed func(t *testing.T, wc watchClient, key []byte) // establishes the pre-write baseline
	}{
		{"seeded", func(t *testing.T, wc watchClient, key []byte) { wc.set(t, key, []byte("A")) }},
		{"absent", func(t *testing.T, wc watchClient, key []byte) { wc.clear(t, key) }},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, wc := range []watchClient{goWatchClient(), cgoWatchClient()} {
				wc := wc
				t.Run(wc.name, func(t *testing.T) {
					t.Parallel()
					key := watchKeyFor(t, wc)
					tc.seed(t, wc, key)
					ch := wc.watchSelfWrite(t, key, []byte("B")) // {Set(k,B); Watch(k)} in ONE txn
					// No external change: the watch is registered at the committed value B, so both clients
					// must stay pending (pre-fix Go fired here — the bug).
					expectPending(t, wc.name+"/"+tc.name, ch, watchBlockWindow)
					wc.set(t, key, []byte("C")) // a genuine EXTERNAL change → fires
					expectFired(t, wc.name+"/"+tc.name, ch)
				})
			}
		})
	}
}

// TestDifferential_WatchOnAbsentKeyFiresOnCreate: for BOTH clients, a watch on a key that does
// NOT exist (value = absent at the read version) fires when the key is first created — the
// absent→present transition is a value change. Pins the absent-baseline case both clients must
// handle identically (a watch registered against "no value").
func TestDifferential_WatchOnAbsentKeyFiresOnCreate(t *testing.T) {
	t.Parallel()
	for _, wc := range []watchClient{goWatchClient(), cgoWatchClient()} {
		wc := wc
		t.Run(wc.name, func(t *testing.T) {
			t.Parallel()
			key := watchKeyFor(t, wc)
			wc.clear(t, key) // ensure absent baseline
			ch := wc.watch(t, key)
			expectPending(t, wc.name, ch, watchBlockWindow)
			wc.set(t, key, []byte("created")) // absent → present → must fire
			expectFired(t, wc.name, ch)
		})
	}
}
