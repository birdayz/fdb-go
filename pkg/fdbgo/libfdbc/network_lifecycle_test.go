//go:build cgo && libfdbc

package libfdbc

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
)

func TestCNetworkUsesSharedOpenOwnership(t *testing.T) {
	// Serial: initialize the one-shot mapped network before parallel differential tests.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	container, err := foundationdbtc.Run(ctx, "", foundationdbtc.WithDirectIP())
	if err != nil {
		t.Fatalf("start FoundationDB: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer stopCancel()
		if err := container.Terminate(stopCtx); err != nil {
			t.Errorf("stop FoundationDB: %v", err)
		}
	})
	contents, err := container.ClusterFile(ctx)
	if err != nil {
		t.Fatalf("cluster file: %v", err)
	}
	clusterFile := filepath.Join(t.TempDir(), "fdb.cluster")
	if err := os.WriteFile(clusterFile, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	backend, err := Open(clusterFile)
	if err != nil {
		t.Fatalf("open backend: %v", err)
	}
	defer backend.Close()
	shared := requireOpenRefs(t, clusterFile, 1)

	raw, err := COpenDatabase(clusterFile)
	if err != nil {
		t.Fatalf("open raw C database: %v", err)
	}
	defer raw.Close()
	if got := requireOpenRefs(t, clusterFile, 2); got != shared {
		t.Fatal("mapped network did not retain the backend's shared handle")
	}

	backend.Close()
	backend.Close()
	if got := requireOpenRefs(t, clusterFile, 1); got != shared {
		t.Fatal("backend close replaced the mapped network's retained handle")
	}

	reopened, err := Open(clusterFile)
	if err != nil {
		t.Fatalf("reopen backend: %v", err)
	}
	defer reopened.Close()
	if got := requireOpenRefs(t, clusterFile, 2); got != shared {
		t.Fatal("reopen did not share the mapped network's retained handle")
	}
	if _, err := reopened.(fdb.CtxReadTransactor).ReadTransactCtx(ctx, func(tx fdb.ReadTransaction) (any, error) {
		return tx.GetReadVersion().Get()
	}); err != nil {
		t.Fatalf("read through retained handle after backend close: %v", err)
	}
	reopened.Close()
	requireOpenRefs(t, clusterFile, 1)
}

func requireOpenRefs(t *testing.T, clusterFile string, want int) *sharedDB {
	t.Helper()
	openMu.Lock()
	defer openMu.Unlock()
	shared := openRefs[clusterFile]
	if shared == nil {
		t.Fatal("database handle bypassed shared open ownership")
	}
	if shared.refs != want {
		t.Fatalf("shared handle references = %d, want %d; mapped network must use the serialized, refcounted open path", shared.refs, want)
	}
	return shared
}
