package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
)

var issueViewSequence atomic.Uint64

// holdIssueViewReply parks the first storage reply with fileID on addr until
// release, so a local write lands while the read waits for storage.
func holdIssueViewReply(t *testing.T, sd *simDialer, addr string, fileID uint32) (<-chan struct{}, func()) {
	t.Helper()
	parked, gate := make(chan struct{}), make(chan struct{})
	var held atomic.Bool
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	sd.setIntercept(func(_ int, _ transport.UID, body []byte) ([]byte, bool) {
		if len(body) >= 8 && binary.LittleEndian.Uint32(body[4:8]) == fileID && held.CompareAndSwap(false, true) {
			close(parked)
			<-gate
		}
		return body, false
	})
	sd.armAddr(addr)
	return parked, release
}

func issueViewTx(db *Database, rv int64) *Transaction {
	tx := db.CreateTransaction()
	tx.SetTimeout(10000)
	tx.SetReadVersion(rv)
	tx.rpcTimeoutOverride = 30 * time.Second
	return tx
}

// A dependent point read returns its issue-time Add over storage even though a
// Set, Clear or further Add lands while the storage reply is held.
func TestFDB_DependentPointReadKeepsIssueTimeStack(t *testing.T) {
	t.Parallel()
	for _, later := range []string{"set", "clear", "add"} {
		t.Run(later, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			sd := newSimDialer()
			db := newTestDatabase(t, ctx, sharedClusterFile, sd.dial)
			t.Cleanup(func() { _ = db.Close() })
			writer := openTestDB(t, ctx)
			prefix := fmt.Sprintf("%s/%d/", t.Name(), issueViewSequence.Add(1))
			key := []byte(prefix + "a")
			rv := seedReadIncarnationValues(t, ctx, db, key, []byte(prefix+"marker"), le64(10), []byte("untouched"))
			parked, release := holdIssueViewReply(t, sd, storageAddrFor(t, db, ctx, key), 2<<24|1378929) // ErrorOr<GetValueReply>

			tx := issueViewTx(db, rv)
			defer tx.Cancel()
			tx.Atomic(MutAddValue, key, le64(3))
			type result struct {
				value []byte
				err   error
			}
			done := make(chan result, 1)
			go func() { v, err := tx.Get(ctx, key); done <- result{v, err} }()
			waitReadParked(t, ctx, parked, "dependent point read")

			var wantLocal []byte
			switch later {
			case "set":
				tx.Set(key, le64(99))
				wantLocal = le64(99)
			case "clear":
				tx.Clear(key)
			case "add":
				tx.Atomic(MutAddValue, key, le64(100))
				wantLocal = le64(113)
			}
			release()
			select {
			case got := <-done:
				if got.err != nil || !bytes.Equal(got.value, le64(13)) {
					t.Fatalf("ISSUE_VIEW_POINT: Add(10, 3) read = %x, %v; want 0d00000000000000 despite the later %s", got.value, got.err, later)
				}
			case <-ctx.Done():
				t.Fatal("dependent read did not finish after release:", ctx.Err())
			}
			// The read must not have folded its result into the write map.
			if local, err := tx.Get(ctx, key); err != nil || !bytes.Equal(local, wantLocal) {
				t.Fatalf("later local Get = %x, %v; want %x", local, err, wantLocal)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatalf("commit: %v", err)
			}
			check := writer.CreateTransaction()
			defer check.Cancel()
			if stored, err := check.Get(ctx, key); err != nil || !bytes.Equal(stored, wantLocal) {
				t.Fatalf("stored = %x, %v; want %x", stored, err, wantLocal)
			}
		})
	}
}

// A cold byte-limited range read issued with no writes in range returns storage
// rows only, even though a Set, Clear or versionstamp lands while its reply is held.
func TestFDB_ColdByteTargetRangeIgnoresLaterWrites(t *testing.T) {
	t.Parallel()
	for _, later := range []string{"set", "clear", "versionstamp"} {
		t.Run(later, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			sd := newSimDialer()
			db := newTestDatabase(t, ctx, sharedClusterFile, sd.dial)
			t.Cleanup(func() { _ = db.Close() })
			prefix := fmt.Sprintf("%s/%d/", t.Name(), issueViewSequence.Add(1))
			a, b, c := []byte(prefix+"a"), []byte(prefix+"b"), []byte(prefix+"c")
			if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
				tx.Set(c, []byte("3"))
				return nil, nil
			}); err != nil {
				t.Fatal("seed:", err)
			}
			rv := seedReadIncarnationValues(t, ctx, db, a, b, []byte("1"), []byte("2"))
			parked, release := holdIssueViewReply(t, sd, storageAddrFor(t, db, ctx, a), 2<<24|1783066) // ErrorOr<GetKeyValuesReply>

			tx := issueViewTx(db, rv)
			defer tx.Cancel()
			type result struct {
				rows []KeyValue
				more bool
				err  error
			}
			done := make(chan result, 1)
			end := []byte(prefix + "\xff")
			go func() {
				rows, more, err := tx.GetRangeWithByteTarget(ctx, []byte(prefix), end, 0, 1<<20, false)
				done <- result{rows, more, err}
			}()
			waitReadParked(t, ctx, parked, "cold byte-limited range")

			switch later {
			case "set":
				tx.Set([]byte(prefix+"b5"), []byte("local"))
			case "clear":
				tx.Clear(b)
			case "versionstamp":
				tx.Atomic(MutSetVersionstampedValue, b, append([]byte("stamp-"), make([]byte, 14)...))
			}
			release()
			want := []KeyValue{{Key: a, Value: []byte("1")}, {Key: b, Value: []byte("2")}, {Key: c, Value: []byte("3")}}
			select {
			case got := <-done:
				if got.err != nil || got.more || !issueViewRowsEqual(got.rows, want) {
					t.Fatalf("ISSUE_VIEW_RANGE: range = %s, more=%t, %v; want the storage rows despite the later %s", issueViewRowsString(got.rows), got.more, got.err, later)
				}
			case <-ctx.Done():
				t.Fatal("range did not finish after release:", ctx.Err())
			}
		})
	}
}

func issueViewRowsEqual(got, want []KeyValue) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if !bytes.Equal(got[i].Key, want[i].Key) || !bytes.Equal(got[i].Value, want[i].Value) {
			return false
		}
	}
	return true
}

func issueViewRowsString(kvs []KeyValue) string {
	parts := make([]string, len(kvs))
	for i, kv := range kvs {
		parts[i] = fmt.Sprintf("%q=%q", kv.Key, kv.Value)
	}
	return fmt.Sprint(parts)
}
