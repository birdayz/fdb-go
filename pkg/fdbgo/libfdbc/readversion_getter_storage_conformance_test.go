//go:build cgo && libfdbc

package libfdbc_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	fdbclient "fdb.dev/pkg/fdbgo/client"
	gofdb "fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/libfdbc"
	"fdb.dev/pkg/fdbgo/wire"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
)

// An explicit read version is a getter result, not a request to validate that
// snapshot against the cluster. A subsequent non-local read validates it.
func TestLibFDBC_ExplicitReadVersionGetterDoesNotValidateStorageSnapshot(t *testing.T) {
	t.Parallel()
	startup, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ctr, err := foundationdbtc.Run(startup, "", foundationdbtc.WithDirectIP())
	if err != nil {
		if foundationdbtc.SkipWithoutDocker(err) {
			t.Skip("FDB not available (no Docker)")
		}
		t.Fatal(err)
	}
	defer foundationdbtc.TeardownT(t, ctr)
	cluster, err := ctr.ClusterFile(startup)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "fdb.cluster")
	if err := os.WriteFile(path, []byte(cluster), 0o600); err != nil {
		t.Fatal(err)
	}

	// libfdbc.Open initializes the real native730 binding/network and returns
	// standalone transactions that directly forward getter and Get to C.
	native, err := libfdbc.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	openCtx, openCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer openCancel()
	goDB, err := fdbclient.OpenDatabase(openCtx, path, fdbclient.WithAPIVersion(730))
	if err != nil {
		t.Fatal(err)
	}
	defer goDB.Close()

	key := gofdb.Key("getter-storage-contract/" + t.Name())
	wantValue := []byte("committed by an independent transaction")
	seed, err := native.CreateWritableTransaction()
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Cancel()
	if err := seed.Options().SetTimeout(10000); err != nil {
		t.Fatal(err)
	}
	seed.Set(key, wantValue)
	if err := seed.Commit().Get(); err != nil {
		t.Fatal(err)
	}

	// Independent positive control: a normal native transaction really reads
	// the seeded storage key. Neither tested pinned transaction writes it.
	control, err := native.CreateWritableTransaction()
	if err != nil {
		t.Fatal(err)
	}
	defer control.Cancel()
	if err := control.Options().SetTimeout(10000); err != nil {
		t.Fatal(err)
	}
	current, err := control.GetReadVersion().Get()
	if err != nil || current <= 123 {
		t.Fatalf("cluster version=%d err=%v must exceed pinned123", current, err)
	}
	value, err := control.Get(key).Get()
	if err != nil || !bytes.Equal(value, wantValue) {
		t.Fatalf("positive storage control=%q err=%v", value, err)
	}

	// Native first: on the pre-fix Go tree this still records the independent
	// native behavior before the Go getter regression fails.
	t.Run("native", func(t *testing.T) {
		tr, err := native.CreateWritableTransaction()
		if err != nil {
			t.Fatal(err)
		}
		defer tr.Cancel()
		if err := tr.Options().SetTimeout(10000); err != nil {
			t.Fatal(err)
		}
		tr.SetReadVersion(123)
		version, err := tr.GetReadVersion().Get()
		if err != nil || version != 123 {
			t.Fatalf("native explicit getter=(%d,%v), want(123,nil)", version, err)
		}
		// Same transaction, no local mutation: this cannot be served by RYW.
		value, err := tr.Get(key).Get()
		var fe gofdb.Error
		if !errors.As(err, &fe) || fe.Code != 1007 || len(value) != 0 {
			t.Fatalf("native non-local storage Get=(%q,%v), want transaction_too_old1007", value, err)
		}
	})

	t.Run("pure_go_client", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		tr := goDB.CreateTransaction()
		defer tr.Cancel()
		tr.SetTimeout(10000)
		tr.SetReadVersion(123)
		version, err := tr.GetReadVersion(ctx)
		if err != nil || version != 123 {
			t.Fatalf("Go explicit getter=(%d,%v), want(123,nil)", version, err)
		}
		value, err := tr.Get(ctx, key)
		var fe *wire.FDBError
		if ctx.Err() != nil || !errors.As(err, &fe) || fe.Code != 1007 || len(value) != 0 {
			t.Fatalf("Go non-local storage Get=(%q,%v), budget=%v, want transaction_too_old1007", value, err, ctx.Err())
		}
	})

	// The public pure-Go facade must expose the same distinction. Its getter
	// calls the core API, whereas Get dispatches an actual storage read path.
	t.Run("pure_go_facade", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		facade := gofdb.WrapDatabaseContext(ctx, goDB)
		tr, err := facade.CreateTransaction()
		if err != nil {
			t.Fatal(err)
		}
		defer tr.Cancel()
		if err := tr.Options().SetTimeout(10000); err != nil {
			t.Fatal(err)
		}
		tr.SetReadVersion(123)
		version, err := tr.GetReadVersion().Get()
		if err != nil || version != 123 {
			t.Fatalf("Go facade explicit getter=(%d,%v), want(123,nil)", version, err)
		}
		value, err := tr.Get(key).Get()
		var fe gofdb.Error
		if ctx.Err() != nil || !errors.As(err, &fe) || fe.Code != 1007 || len(value) != 0 {
			t.Fatalf("Go facade non-local storage Get=(%q,%v), budget=%v, want transaction_too_old1007", value, err, ctx.Err())
		}
	})
}
