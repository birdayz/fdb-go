package client

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/transport"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
)

func TestParseClusterString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		wantErr bool
		desc    string
		id      string
		addrs   []string
	}{
		{
			name:  "single coordinator",
			input: "fdb_test:abcd1234@127.0.0.1:4500",
			desc:  "fdb_test", id: "abcd1234",
			addrs: []string{"127.0.0.1:4500"},
		},
		{
			name:  "three coordinators",
			input: "test:id@10.0.0.1:4500,10.0.0.2:4500,10.0.0.3:4500",
			desc:  "test", id: "id",
			addrs: []string{"10.0.0.1:4500", "10.0.0.2:4500", "10.0.0.3:4500"},
		},
		{
			name:    "missing @",
			input:   "invalid_string",
			wantErr: true,
		},
		{
			name:    "missing colon in prefix",
			input:   "nocolon@127.0.0.1:4500",
			wantErr: true,
		},
		{
			name:    "no coordinators",
			input:   "desc:id@",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cf, err := ParseClusterString(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cf.Description != tt.desc {
				t.Errorf("description: got %q, want %q", cf.Description, tt.desc)
			}
			if cf.ID != tt.id {
				t.Errorf("id: got %q, want %q", cf.ID, tt.id)
			}
			if len(cf.Coordinators) != len(tt.addrs) {
				t.Fatalf("coordinators: got %d, want %d", len(cf.Coordinators), len(tt.addrs))
			}
			for i, addr := range cf.Coordinators {
				if addr != tt.addrs[i] {
					t.Errorf("coordinator %d: got %q, want %q", i, addr, tt.addrs[i])
				}
			}
		})
	}
}

func TestParseClusterFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "fdb.cluster")
	os.WriteFile(path, []byte("# comment\ntest:abc@127.0.0.1:4500\n"), 0o644)

	cf, err := ParseClusterFile(path)
	if err != nil {
		t.Fatalf("ParseClusterFile: %v", err)
	}
	if cf.Description != "test" {
		t.Errorf("description: got %q, want %q", cf.Description, "test")
	}
	if len(cf.Coordinators) != 1 || cf.Coordinators[0] != "127.0.0.1:4500" {
		t.Errorf("coordinators: got %v", cf.Coordinators)
	}
}

func TestTransactionSet(t *testing.T) {
	t.Parallel()

	tx := &Transaction{}
	tx.Set([]byte("key1"), []byte("val1"))
	tx.Set([]byte("key2"), []byte("val2"))

	if len(tx.mutations) != 2 {
		t.Fatalf("mutations: got %d, want 2", len(tx.mutations))
	}
	if tx.mutations[0].Type != MutSetValue {
		t.Errorf("mutation 0 type: got %d, want %d", tx.mutations[0].Type, MutSetValue)
	}
	if string(tx.mutations[0].Key) != "key1" {
		t.Errorf("mutation 0 key: got %q, want %q", tx.mutations[0].Key, "key1")
	}
	if len(tx.writeConflicts) != 2 {
		t.Errorf("write conflicts: got %d, want 2", len(tx.writeConflicts))
	}
}

func TestTransactionClear(t *testing.T) {
	t.Parallel()

	tx := &Transaction{}
	tx.Clear([]byte("key1"))

	if len(tx.mutations) != 1 {
		t.Fatalf("mutations: got %d, want 1", len(tx.mutations))
	}
	if tx.mutations[0].Type != MutClearRange {
		t.Errorf("type: got %d, want %d", tx.mutations[0].Type, MutClearRange)
	}
}

func TestTransactionReset(t *testing.T) {
	t.Parallel()

	tx := &Transaction{}
	tx.Set([]byte("key"), []byte("val"))
	tx.SetReadVersion(100)

	tx.reset()

	if len(tx.mutations) != 0 {
		t.Errorf("mutations not cleared: %d", len(tx.mutations))
	}
	if tx.hasReadVersion {
		t.Error("readVersion not cleared")
	}
	if txState(tx.state.Load()) != txStateActive {
		t.Errorf("state: got %d, want %d", txState(tx.state.Load()), txStateActive)
	}
}

func TestOnError_AllRetryableCodes(t *testing.T) {
	t.Parallel()

	retryable := []struct {
		name string
		code int
	}{
		{"not_committed", ErrNotCommitted},
		{"commit_unknown_result", ErrCommitUnknownResult},
		{"cluster_version_changed", ErrClusterVersionChanged},
		{"transaction_too_old", ErrTransactionTooOld},
		{"future_version", ErrFutureVersion},
		{"database_locked", ErrDatabaseLocked},
		{"proxy_memory_limit_exceeded", ErrProxyMemoryLimitExceeded},
		{"grv_proxy_memory_limit", ErrGrvProxyMemoryLimit},
		{"process_behind", ErrProcessBehind},
		{"batch_transaction_throttled", ErrBatchTransactionThrottled},
		{"tag_throttled", ErrTagThrottled},
		{"proxy_tag_throttled", ErrProxyTagThrottled},
		{"throttled_hot_shard", ErrThrottledHotShard},
		{"range_locked", ErrRangeLocked},
		{"all_proxies_unreachable", ErrAllProxiesUnreachable},
		// all_alternatives_failed (1006) is NOT retryable at OnError level.
		// It's retried at Layer 2 (read path wrong-shard loop). If it
		// escapes to OnError, it means the read path exhausted retries
		// and the transaction should fail, not retry forever.
	}
	for _, tc := range retryable {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tx := &Transaction{}
			tx.Set([]byte("key"), []byte("val"))

			// Wrap like real code does: fmt.Errorf("context: %w", fdbErr)
			err := fmt.Errorf("commit: %w", &wire.FDBError{Code: tc.code})
			result := tx.OnError(context.Background(), err)
			if result != nil {
				t.Errorf("code %d should be retryable, got: %v", tc.code, result)
			}
			if tx.retryCount != 1 {
				t.Errorf("retryCount: got %d, want 1", tx.retryCount)
			}
			if len(tx.mutations) != 0 {
				t.Errorf("mutations not cleared: %d", len(tx.mutations))
			}
		})
	}
}

func TestOnError_NonRetryable(t *testing.T) {
	t.Parallel()

	t.Run("non_retryable_fdb_error", func(t *testing.T) {
		t.Parallel()
		tx := &Transaction{}
		err := fmt.Errorf("something: %w", &wire.FDBError{Code: 9999})
		if tx.OnError(context.Background(), err) == nil {
			t.Error("expected non-retryable")
		}
		if txState(tx.state.Load()) != txStateErrored {
			t.Errorf("state: got %d, want %d", txState(tx.state.Load()), txStateErrored)
		}
	})

	t.Run("non_fdb_error", func(t *testing.T) {
		t.Parallel()
		tx := &Transaction{}
		if tx.OnError(context.Background(), fmt.Errorf("network timeout")) == nil {
			t.Error("non-FDB error should be non-retryable")
		}
	})

	t.Run("wrong_shard_server", func(t *testing.T) {
		t.Parallel()
		// 1062 is handled at the read path level, NOT by Transact.
		// OnError should treat it as non-retryable.
		tx := &Transaction{}
		err := fmt.Errorf("getValue: %w", &wire.FDBError{Code: 1062})
		if tx.OnError(context.Background(), err) == nil {
			t.Error("wrong_shard_server should not be retryable at Transact level")
		}
	})

	t.Run("transaction_timed_out", func(t *testing.T) {
		t.Parallel()
		// 1031 = transaction_timed_out — NEVER retryable.
		// Matches C++ where OnError(1031) returns 1031.
		tx := &Transaction{}
		err := fmt.Errorf("timed out: %w", &wire.FDBError{Code: ErrTransactionTimedOut})
		if tx.OnError(context.Background(), err) == nil {
			t.Error("transaction_timed_out should not be retryable")
		}
		if txState(tx.state.Load()) != txStateErrored {
			t.Errorf("state: got %d, want %d", txState(tx.state.Load()), txStateErrored)
		}
	})
}

func TestCommitUnknownResult_SelfConflicting(t *testing.T) {
	t.Parallel()

	tx := &Transaction{}
	tx.Set([]byte("key_a"), []byte("val"))
	tx.Set([]byte("key_b"), []byte("val"))
	tx.ClearRange([]byte("range_begin"), []byte("range_end"))

	// Capture write conflicts before OnError resets them.
	originalWriteConflicts := make([]KeyRange, len(tx.writeConflicts))
	copy(originalWriteConflicts, tx.writeConflicts)
	if len(originalWriteConflicts) != 3 {
		t.Fatalf("expected 3 write conflicts, got %d", len(originalWriteConflicts))
	}

	// Simulate commit_unknown_result.
	err := fmt.Errorf("commit: %w", &wire.FDBError{Code: ErrCommitUnknownResult})
	result := tx.OnError(context.Background(), err)
	if result != nil {
		t.Fatalf("1021 should be retryable, got: %v", result)
	}

	// After reset, mutations and write conflicts should be cleared.
	if len(tx.mutations) != 0 {
		t.Errorf("mutations should be cleared, got %d", len(tx.mutations))
	}
	if len(tx.writeConflicts) != 0 {
		t.Errorf("writeConflicts should be cleared, got %d", len(tx.writeConflicts))
	}

	// But readConflicts should contain the ORIGINAL write conflicts
	// (self-conflicting for double-apply protection).
	if len(tx.readConflicts) != len(originalWriteConflicts) {
		t.Fatalf("readConflicts: got %d, want %d (self-conflicts from writes)",
			len(tx.readConflicts), len(originalWriteConflicts))
	}
	for i, rc := range tx.readConflicts {
		if string(rc.Begin) != string(originalWriteConflicts[i].Begin) ||
			string(rc.End) != string(originalWriteConflicts[i].End) {
			t.Errorf("readConflict[%d]: got [%q,%q), want [%q,%q)",
				i, rc.Begin, rc.End,
				originalWriteConflicts[i].Begin, originalWriteConflicts[i].End)
		}
	}

	// Verify that a normal retryable error (1020) does NOT inject self-conflicts.
	tx2 := &Transaction{}
	tx2.Set([]byte("key"), []byte("val"))
	err2 := fmt.Errorf("commit: %w", &wire.FDBError{Code: ErrNotCommitted})
	tx2.OnError(context.Background(), err2)
	if len(tx2.readConflicts) != 0 {
		t.Errorf("1020 should NOT inject self-conflicts, got %d readConflicts", len(tx2.readConflicts))
	}
}

func TestClusterVersionChanged_SelfConflicting(t *testing.T) {
	t.Parallel()

	// cluster_version_changed (1039) is MAYBE_COMMITTED — must inject
	// self-conflicts, same as commit_unknown_result (1021).
	tx := &Transaction{}
	tx.Set([]byte("key_a"), []byte("val"))
	tx.Set([]byte("key_b"), []byte("val"))

	originalWriteConflicts := make([]KeyRange, len(tx.writeConflicts))
	copy(originalWriteConflicts, tx.writeConflicts)

	err := fmt.Errorf("commit: %w", &wire.FDBError{Code: ErrClusterVersionChanged})
	result := tx.OnError(context.Background(), err)
	if result != nil {
		t.Fatalf("1039 should be retryable, got: %v", result)
	}

	// Self-conflicts injected: write conflicts → read conflicts.
	if len(tx.readConflicts) != len(originalWriteConflicts) {
		t.Fatalf("readConflicts: got %d, want %d (self-conflicts)",
			len(tx.readConflicts), len(originalWriteConflicts))
	}
	if len(tx.writeConflicts) != 0 {
		t.Errorf("writeConflicts should be cleared, got %d", len(tx.writeConflicts))
	}
}

func TestReadOnlyCommit(t *testing.T) {
	t.Parallel()

	db := newTestDatabaseStub()
	tx := db.CreateTransaction()

	// No mutations → read-only → commit succeeds immediately.
	// After commit, transaction resets for reuse (matches C client behavior).
	err := tx.Commit(context.Background())
	if err != nil {
		t.Errorf("read-only commit should succeed: %v", err)
	}
	if txState(tx.state.Load()) != txStateActive {
		t.Errorf("state: got %d, want %d (active after postCommitReset)", txState(tx.state.Load()), txStateActive)
	}
}

// TestProxiesChangedBroadcast verifies the close-and-replace broadcast
// pattern used for mid-commit proxy change detection.
func TestProxiesChangedBroadcast(t *testing.T) {
	t.Parallel()
	db := &database{
		proxiesChanged: make(chan struct{}),
	}

	// Capture channel before signal.
	ch1 := db.waitProxiesChanged()

	// Signal proxy change.
	db.proxiesChangedMu.Lock()
	close(db.proxiesChanged)
	db.proxiesChanged = make(chan struct{})
	db.proxiesChangedMu.Unlock()

	// ch1 should be closed (readable without blocking).
	select {
	case <-ch1:
		// good — signal received
	default:
		t.Fatal("proxiesChanged channel should be closed after signal")
	}

	// New channel should NOT be closed.
	ch2 := db.waitProxiesChanged()
	select {
	case <-ch2:
		t.Fatal("new proxiesChanged channel should not be closed")
	default:
		// good — not closed
	}
}

// newTestDatabaseStub creates a minimal Database for unit tests that don't
// need real FDB connectivity (e.g., testing mutation buffering, state transitions).
func newTestDatabaseStub() *Database {
	ctx, cancel := context.WithCancel(context.Background())
	db := &database{
		clusterFile:    &ClusterFile{Coordinators: []string{"127.0.0.1:4500"}},
		connPool:       make(map[string]*transport.Conn),
		topologyKick:   make(chan struct{}, 1),
		proxiesChanged: make(chan struct{}),
		connected:      make(chan struct{}),
		ctx:            ctx,
		cancel:         cancel,
	}
	return &Database{db: db}
}

func TestParseClusterFile_Errors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		setup      func(t *testing.T) string // returns path
		wantSubstr string
	}{
		{
			name: "file not found",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "nonexistent.cluster")
			},
		},
		{
			name: "empty file with only comments",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				p := filepath.Join(dir, "empty.cluster")
				os.WriteFile(p, []byte("# just a comment\n\n# another\n"), 0o644)
				return p
			},
			wantSubstr: "empty cluster file",
		},
		{
			name: "invalid coordinator address",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				p := filepath.Join(dir, "bad.cluster")
				os.WriteFile(p, []byte("test:id@not-a-host-port\n"), 0o644)
				return p
			},
			wantSubstr: "invalid coordinator address",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := tt.setup(t)
			_, err := ParseClusterFile(path)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if tt.wantSubstr != "" {
				if !strings.Contains(err.Error(), tt.wantSubstr) {
					t.Errorf("error %q does not contain %q", err.Error(), tt.wantSubstr)
				}
			}
		})
	}
}

func TestGrvCache_TryCache(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		setup    func(c *grvCache)
		priority uint32
		wantOK   bool
		wantVer  int64
	}{
		{
			name: "batch priority returns cached version",
			setup: func(c *grvCache) {
				c.version.Store(1000000)
				c.lastTime.Store(time.Now().UnixNano())
			},
			priority: grvPriorityBatch,
			wantOK:   true,
			wantVer:  1000000,
		},
		{
			name: "batch ratekeeper throttle",
			setup: func(c *grvCache) {
				c.version.Store(1000000)
				c.lastTime.Store(time.Now().UnixNano())
				c.lastRkBatch.Store(time.Now().UnixNano())
			},
			priority: grvPriorityBatch,
			wantOK:   false,
		},
		{
			name: "default ratekeeper throttle",
			setup: func(c *grvCache) {
				c.version.Store(1000000)
				c.lastTime.Store(time.Now().UnixNano())
				c.lastRkDefault.Store(time.Now().UnixNano())
			},
			priority: grvPriorityDefault,
			wantOK:   false,
		},
		{
			name: "system immediate always bypasses cache",
			setup: func(c *grvCache) {
				c.version.Store(1000000)
				c.lastTime.Store(time.Now().UnixNano())
			},
			priority: grvPrioritySystemImmediate,
			wantOK:   false,
		},
		{
			name: "stale cache expired",
			setup: func(c *grvCache) {
				c.version.Store(1000000)
				// Set lastTime to 1 second ago — well beyond 100ms maxVersionCacheLag.
				c.lastTime.Store(time.Now().Add(-1 * time.Second).UnixNano())
			},
			priority: grvPriorityDefault,
			wantOK:   false,
		},
		{
			name: "zero version returns false",
			setup: func(c *grvCache) {
				// version defaults to 0
			},
			priority: grvPriorityDefault,
			wantOK:   false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var c grvCache
			tt.setup(&c)
			v, ok := c.tryCache(tt.priority)
			if ok != tt.wantOK {
				t.Fatalf("tryCache ok: got %v, want %v", ok, tt.wantOK)
			}
			if ok && v != tt.wantVer {
				t.Errorf("tryCache version: got %d, want %d", v, tt.wantVer)
			}
		})
	}
}

func TestGrvCache_UpdateMonotonic(t *testing.T) {
	t.Parallel()

	var c grvCache

	// First update: version advances to 200.
	c.update(time.Now(), 200)
	if v := c.version.Load(); v != 200 {
		t.Fatalf("after update(200): got %d, want 200", v)
	}

	// Backwards update: version must stay at 200.
	c.update(time.Now(), 100)
	if v := c.version.Load(); v != 200 {
		t.Fatalf("after update(100): got %d, want 200 (should not go backwards)", v)
	}

	// Forward update: version advances to 300.
	c.update(time.Now(), 300)
	if v := c.version.Load(); v != 300 {
		t.Fatalf("after update(300): got %d, want 300", v)
	}
}

func TestGrvPriorityToPriority(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		flags uint32
		want  TransactionPriority
	}{
		{"batch", grvPriorityBatch, PriorityBatch},
		{"system_immediate", grvPrioritySystemImmediate, PrioritySystemImmediate},
		{"default", grvPriorityDefault, PriorityDefault},
		{"unknown falls to default", 0x03000000, PriorityDefault},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := grvPriorityToPriority(tt.flags)
			if got != tt.want {
				t.Errorf("grvPriorityToPriority(%#x): got %d, want %d", tt.flags, got, tt.want)
			}
		})
	}
}

func TestReadTransact_ContextCancellation(t *testing.T) {
	t.Parallel()

	db := newTestDatabaseStub()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before calling ReadTransact

	_, err := db.ReadTransact(ctx, func(tx *Transaction) (any, error) {
		t.Fatal("function should not be called with cancelled context")
		return nil, nil
	})
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
}
