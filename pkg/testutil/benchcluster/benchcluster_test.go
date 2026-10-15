package benchcluster

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExternalClusterFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	good := filepath.Join(dir, "fdb.cluster")
	if err := os.WriteFile(good, []byte("bench:bench@127.0.0.1:4500\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty.cluster")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := externalClusterFile(good); err != nil || got != good {
		t.Fatalf("externalClusterFile(good) = %q, %v; want %q, nil", got, err, good)
	}
	for _, bad := range []string{empty, dir, filepath.Join(dir, "missing.cluster")} {
		if got, err := externalClusterFile(bad); err == nil {
			t.Fatalf("externalClusterFile(%q) = %q, nil; want an error", bad, got)
		}
	}
}

func TestShutdownWithoutClusterIsANoOp(t *testing.T) {
	t.Parallel()
	if err := Shutdown(); err != nil {
		t.Fatalf("Shutdown with nothing started: %v", err)
	}
}
