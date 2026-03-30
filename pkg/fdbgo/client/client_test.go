package client

import (
	"context"
	"os"
	"path/filepath"
	"testing"
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

	tx := &Transaction{state: txStateActive}
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

	tx := &Transaction{state: txStateActive}
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

	tx := &Transaction{state: txStateActive}
	tx.Set([]byte("key"), []byte("val"))
	tx.SetReadVersion(100)

	tx.reset()

	if len(tx.mutations) != 0 {
		t.Errorf("mutations not cleared: %d", len(tx.mutations))
	}
	if tx.hasReadVersion {
		t.Error("readVersion not cleared")
	}
	if tx.state != txStateActive {
		t.Errorf("state: got %d, want %d", tx.state, txStateActive)
	}
}

func TestOnError_Retryable(t *testing.T) {
	t.Parallel()

	tx := &Transaction{state: txStateActive}
	tx.Set([]byte("key"), []byte("val"))

	err := &FDBError{Code: ErrNotCommitted, Message: "conflict"}
	result := tx.OnError(err)

	if result != nil {
		t.Errorf("expected nil (retryable), got: %v", result)
	}
	if tx.retryCount != 1 {
		t.Errorf("retryCount: got %d, want 1", tx.retryCount)
	}
	if len(tx.mutations) != 0 {
		t.Errorf("mutations not cleared after retry: %d", len(tx.mutations))
	}
}

func TestOnError_NonRetryable(t *testing.T) {
	t.Parallel()

	tx := &Transaction{state: txStateActive}
	err := &FDBError{Code: 9999, Message: "unknown"}
	result := tx.OnError(err)

	if result == nil {
		t.Error("expected non-nil (non-retryable)")
	}
	if tx.state != txStateErrored {
		t.Errorf("state: got %d, want %d", tx.state, txStateErrored)
	}
}

func TestReadOnlyCommit(t *testing.T) {
	t.Parallel()

	db := &Database{cluster: &Cluster{}}
	tx := db.CreateTransaction()

	// No mutations → read-only → commit succeeds immediately.
	err := tx.Commit(context.Background())
	if err != nil {
		t.Errorf("read-only commit should succeed: %v", err)
	}
	if tx.state != txStateCommitted {
		t.Errorf("state: got %d, want %d", tx.state, txStateCommitted)
	}
}
