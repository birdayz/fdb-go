package client

import (
	"bytes"
	"context"
	"runtime"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
)

// Every connection of a database, coordinators and roles alike, uses its configured
// write delay or adaptive linger; the D-NG default is 0/0 (immediate flush on wakeup).
func TestWithCoalesceDelayReachesEveryConnection(t *testing.T) {
	t.Parallel()
	if sharedClusterFile == nil {
		t.Fatal("shared FDB container not initialized — TestMain must run first")
	}
	for _, tc := range []struct {
		name     string
		opts     []Option
		min, max time.Duration
		adaptive time.Duration
	}{
		{"default", nil, 0, 0, 0},
		{"disabled", []Option{WithCoalesceDelay(0, 0)}, 0, 0, 0},
		{"libfdbc-default", []Option{WithCoalesceDelay(transport.DefaultMinCoalesceDelay, transport.DefaultMaxCoalesceDelay)}, transport.DefaultMinCoalesceDelay, transport.DefaultMaxCoalesceDelay, 0},
		{"custom", []Option{WithCoalesceDelay(30*time.Microsecond, 50*time.Microsecond)}, 30 * time.Microsecond, 50 * time.Microsecond, 0},
		{"adaptive", []Option{WithAdaptiveLinger(20 * time.Microsecond)}, 0, 0, 20 * time.Microsecond},
		{"last-wins-adaptive", []Option{WithCoalesceDelay(30*time.Microsecond, 50*time.Microsecond), WithAdaptiveLinger(15 * time.Microsecond)}, 0, 0, 15 * time.Microsecond},
		{"last-wins-coalesce", []Option{WithAdaptiveLinger(15 * time.Microsecond), WithCoalesceDelay(30*time.Microsecond, 50*time.Microsecond)}, 30 * time.Microsecond, 50 * time.Microsecond, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			db, err := OpenDatabaseFromConfig(ctx, sharedClusterFile, append([]Option{WithAPIVersion(730)}, tc.opts...)...)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			key, value := []byte(t.Name()), []byte("coalesced")
			var escapedWriteTx *Transaction
			var initialSpan [16]byte
			if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
				escapedWriteTx = tx
				tx.SetTimeout(60000)
				tx.Set(key, value)
				initialSpan = tx.currentSpan().TraceID
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			got, err := db.Transact(ctx, func(tx *Transaction) (any, error) { return tx.Get(ctx, key) })
			if err != nil || !bytes.Equal(got.([]byte), value) {
				t.Fatalf("read back %q, %v", got, err)
			}
			if !escapedWriteTx.hasCommitted || escapedWriteTx.committedVersion <= 0 || escapedWriteTx.readLife != nil || escapedWriteTx.readReady != nil || escapedWriteTx.transactFinalCommit.Load() {
				t.Fatalf("escaped write Transact final Commit state: hasCommitted=%t ver=%d readLife=%v", escapedWriteTx.hasCommitted, escapedWriteTx.committedVersion, escapedWriteTx.readLife)
			}
			if span := escapedWriteTx.currentSpan(); span.TraceID == initialSpan || span.SpanID == 0 {
				t.Fatalf("escaped write Transact currentSpan = %+v, want fresh TraceID != %x", span, initialSpan)
			}
			if vs, vsErr := escapedWriteTx.GetVersionstamp(); vsErr != nil || len(vs) != 10 {
				t.Fatalf("escaped write Transact GetVersionstamp = (%x, %v), want 10-byte stamp", vs, vsErr)
			}
			escapedWriteTx.readErrMu.Lock()
			wArm, wPending := escapedWriteTx.timeoutArm, escapedWriteTx.timeoutPendingLocked()
			escapedWriteTx.withdrawTimeoutLocked()
			escapedWriteTx.readErrMu.Unlock()
			if !wPending {
				t.Fatal("escaped write Transact handle did not lazily arm its timeout")
			}
			escapedWriteTx.fireTimeout(wArm)
			wCtx, wRelease := escapedWriteTx.opContext(ctx)
			defer wRelease()
			if code, ok := fdbErrorCode(escapedWriteTx.readEntryError(wCtx)); !ok || code != 1031 {
				t.Fatalf("escaped write Transact timeout delivery = %v, want 1031", escapedWriteTx.readEntryError(wCtx))
			}
			db.db.connMu.Lock()
			conns := make([]*transport.Conn, 0, len(db.db.connPool))
			for _, c := range db.db.connPool {
				conns = append(conns, c)
			}
			db.db.connMu.Unlock()
			if len(conns) == 0 {
				t.Fatal("no pooled connections after a write and a read")
			}
			dbMin, dbMax, dbAdaptive, dbWaits := db.ConnectionLinger()
			wantWait := runtime.GOOS == "linux" && tc.max > 0
			if dbMin != tc.min || dbMax != tc.max || dbWaits != wantWait || dbAdaptive != tc.adaptive {
				t.Fatalf("db.ConnectionLinger() = (%v, %v, %v, %t), want (%v, %v, %v, %t)",
					dbMin, dbMax, dbAdaptive, dbWaits, tc.min, tc.max, tc.adaptive, wantWait)
			}
			for _, c := range conns {
				lo, hi, waits := c.CoalesceDelay()
				wantWait := runtime.GOOS == "linux" && tc.max > 0
				if lo != tc.min || hi != tc.max || waits != wantWait {
					t.Fatalf("connection delay %v/%v waits=%t, want %v/%v waits=%t", lo, hi, waits, tc.min, tc.max, wantWait)
				}
				if got := c.AdaptiveLinger(); got != tc.adaptive {
					t.Fatalf("connection AdaptiveLinger() = %v, want %v", got, tc.adaptive)
				}
			}
		})
	}
}
