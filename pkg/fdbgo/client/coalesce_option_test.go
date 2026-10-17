package client

import (
	"bytes"
	"context"
	"runtime"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
)

// Every connection of a database, coordinators and roles alike, uses its write
// delay; the default is libfdb_c's MIN/MAX_COALESCE_DELAY.
func TestWithCoalesceDelayReachesEveryConnection(t *testing.T) {
	t.Parallel()
	if sharedClusterFile == nil {
		t.Fatal("shared FDB container not initialized — TestMain must run first")
	}
	for _, tc := range []struct {
		name     string
		opts     []Option
		min, max time.Duration
	}{
		{"default", nil, transport.DefaultMinCoalesceDelay, transport.DefaultMaxCoalesceDelay},
		{"disabled", []Option{WithCoalesceDelay(0, 0)}, 0, 0},
		{"custom", []Option{WithCoalesceDelay(30*time.Microsecond, 50*time.Microsecond)}, 30 * time.Microsecond, 50 * time.Microsecond},
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
			if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
				tx.Set(key, value)
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			got, err := db.Transact(ctx, func(tx *Transaction) (any, error) { return tx.Get(ctx, key) })
			if err != nil || !bytes.Equal(got.([]byte), value) {
				t.Fatalf("read back %q, %v", got, err)
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
			for _, c := range conns {
				lo, hi, waits := c.CoalesceDelay()
				wantWait := runtime.GOOS == "linux" && tc.max > 0
				if lo != tc.min || hi != tc.max || waits != wantWait {
					t.Fatalf("connection delay %v/%v waits=%t, want %v/%v waits=%t", lo, hi, waits, tc.min, tc.max, wantWait)
				}
			}
		})
	}
}
