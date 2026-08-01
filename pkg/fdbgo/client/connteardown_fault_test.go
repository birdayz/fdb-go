package client

import (
	"bytes"
	"context"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
)

// killAddrOnce returns a frameIntercept that, on the FIRST armed reply frame,
// closes every underlying connection to addr — a faithful peer teardown: the
// client's readLoop observes the socket death and failConnection fans out the
// coded request_maybe_delivered (1030) to the in-flight RPC — and swallows that
// reply. Every later frame (e.g. on the re-dialed connection during the retry)
// passes through untouched.
func (d *simDialer) killAddrOnce(addr string, killed *atomic.Bool) frameIntercept {
	return func(idx int, token transport.UID, body []byte) ([]byte, bool) {
		if killed.CompareAndSwap(false, true) {
			d.mu.Lock()
			for _, c := range d.conns[addr] {
				_ = c.Conn.Close()
			}
			d.mu.Unlock()
			return nil, true // swallow the reply; the teardown fails the in-flight RPC
		}
		return body, false
	}
}

// disarmAll clears arming on every connection (and for future dials) so
// sequential subtests sharing one simDialer start from a clean state.
func (d *simDialer) disarmAll() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for addr := range d.armedAddr {
		delete(d.armedAddr, addr)
	}
	for _, cs := range d.conns {
		for _, c := range cs {
			c.armed.Store(false)
		}
	}
}

// TestConnTeardown_MidRead_TransactRetries proves the C++-faithful outcome for
// a connection teardown under an in-flight READ: the transport surfaces the
// coded request_maybe_delivered (1030) teardown, the read loops absorb it like
// C++ loadBalance absorbs maybeDelivered (LoadBalance.actor.h:344 — retry
// another alternative; exhaustion → all_alternatives_failed, absorbed by the
// read's own retry), and Transact completes successfully. Before the fix a
// teardown mid-getRange/getKey escaped as an UNCODED "connection closed" error
// that OnError could not classify → a transient network blip became a terminal
// application error (the conformance-flake shape).
//
// Revert-proofs (two independent directions):
//   - transport revert (deliver a bare errors.New sentinel again): the read
//     loops cannot classify the teardown → it escapes → Transact fails → red.
//   - readpath revert (keep the coded 1030 but drop isMaybeDelivered from the
//     retry arms): 1030 escapes; C++ Transaction::onError does not retry 1030
//     (NativeAPI.actor.cpp:7940-7985) and neither does Go's → Transact fails →
//     red.
//
// The kill is armed INSIDE the transaction, after GetReadVersion, so the first
// armed reply is deterministically the READ's — never this transaction's GRV.
// Subtests share one simDialer/container and therefore run sequentially.
func TestConnTeardown_MidRead_TransactRetries(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db, sd := newSimTestDB(t, ctx)

	prefix := t.Name() + "_"
	k := func(i byte) []byte { return []byte(prefix + string([]byte{'k', '0' + i})) }
	v := func(i byte) []byte { return []byte{'v', '0' + i} }
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		for i := byte(0); i < 3; i++ {
			tx.Set(k(i), v(i))
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Warm the location cache + storage connection.
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, k(0))
	}); err != nil {
		t.Fatalf("warm read: %v", err)
	}
	addr := storageAddrFor(t, db, ctx, k(0))

	run := func(t *testing.T, read func(tx *Transaction) (any, error)) any {
		t.Helper()
		sd.disarmAll()
		var killed atomic.Bool
		sd.setIntercept(sd.killAddrOnce(addr, &killed))
		armOnce := func() {
			if !killed.Load() {
				sd.armAddr(addr)
			}
		}
		res, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
			// Pin the read version FIRST, then arm: the next armed reply is
			// deterministically the READ RPC's, not this transaction's GRV.
			if _, err := tx.GetReadVersion(ctx); err != nil {
				return nil, err
			}
			armOnce()
			return read(tx)
		})
		sd.disarmAll()
		if err != nil {
			t.Fatalf("DIVERGENCE: a mid-read connection teardown must be absorbed by the "+
				"read retry machinery (C++ loadBalance maybeDelivered, LoadBalance.actor.h:344) "+
				"or surface a RETRYABLE code Transact retries — it flattened to a terminal error: %v", err)
		}
		if !killed.Load() {
			t.Fatal("fault never fired — the teardown was not exercised (vacuous run)")
		}
		return res
	}

	// Sequential on purpose: subtests share the simDialer's intercept/arming.
	t.Run("getValue", func(t *testing.T) {
		res := run(t, func(tx *Transaction) (any, error) { return tx.Get(ctx, k(0)) })
		if got := res.([]byte); !bytes.Equal(got, v(0)) {
			t.Fatalf("Get after teardown-retry: got %q, want %q", got, v(0))
		}
	})

	t.Run("getRange", func(t *testing.T) {
		res := run(t, func(tx *Transaction) (any, error) {
			kvs, _, err := tx.GetRange(ctx, []byte(prefix), append([]byte(prefix), 0xff), 10)
			return kvs, err
		})
		kvs := res.([]KeyValue)
		if len(kvs) != 3 {
			t.Fatalf("GetRange after teardown-retry: got %d kvs, want 3: %v", len(kvs), kvs)
		}
		for i := byte(0); i < 3; i++ {
			if !bytes.Equal(kvs[i].Key, k(i)) || !bytes.Equal(kvs[i].Value, v(i)) {
				t.Fatalf("GetRange kv[%d] = %q=%q, want %q=%q", i, kvs[i].Key, kvs[i].Value, k(i), v(i))
			}
		}
	})

	t.Run("getKey", func(t *testing.T) {
		// firstGreaterOrEqual(k0) resolves to k0 itself.
		res := run(t, func(tx *Transaction) (any, error) {
			return tx.GetKey(ctx, k(0), false, 1)
		})
		if got := res.([]byte); !bytes.Equal(got, k(0)) {
			t.Fatalf("GetKey after teardown-retry: got %q, want %q", got, k(0))
		}
	})
}
