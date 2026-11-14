//go:build cgo && libfdbc

package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/wire"
	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
)

func awaitFailedGRVNativeFuture(t *testing.T, future cgofdb.Future) {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !future.IsReady() {
		select {
		case <-deadline.C:
			future.Cancel()
			t.Fatal("native future did not resolve")
		case <-tick.C:
		}
	}
}

func failedGRVNativeCode(err error) int {
	if err == nil {
		return 0
	}
	var fe cgofdb.Error
	if errors.As(err, &fe) {
		return fe.Code
	}
	return -1
}

// The pinned 7.3.77 oracle retains failed GRV futures; no Docker or shared
// database lock is needed because the reserved endpoint never serves FDB.
func TestFailedReadVersionNativeSetterDifferential(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	path := filepath.Join(t.TempDir(), "dead.cluster")
	if err := os.WriteFile(path, []byte("failedgrv:failedgrv@"+listener.Addr().String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cgofdb.APIVersion(730); err != nil {
		t.Fatal(err)
	}
	native, err := cgofdb.OpenDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	for _, mode := range []string{"timeout", "future_cancel", "timeout_reset", "timeout_on_error", "future_cancel_on_error"} {
		for _, pin := range []int64{123, 0} {
			t.Run(fmt.Sprintf("%s/%d", mode, pin), func(t *testing.T) {
				tr, err := native.CreateTransaction()
				if err != nil {
					t.Fatal(err)
				}
				defer tr.Cancel()
				future := tr.GetReadVersion()
				if future.IsReady() {
					t.Fatal("native dead-end GRV was not pending before failure")
				}
				failure := "timeout"
				initial := 1031
				if mode == "future_cancel" || mode == "future_cancel_on_error" {
					future.Cancel()
					failure, initial = "caller_cancel", 1101
				} else if err := tr.Options().SetTimeout(20); err != nil {
					t.Fatal(err)
				}
				awaitFailedGRVNativeFuture(t, future)
				if _, err := future.Get(); failedGRVNativeCode(err) != initial {
					t.Fatalf("native failed GRV=%v want%d", err, initial)
				}
				goTx := failedReadVersionTransaction(t, failure, false)
				want := 2010
				switch mode {
				case "timeout_reset":
					tr.Reset()
					if err := tr.Options().SetTimeout(0); err != nil {
						t.Fatal(err)
					}
					goTx.Reset()
					goTx.SetTimeout(0)
					want = 0
				case "timeout_on_error", "future_cancel_on_error":
					retry := tr.OnError(cgofdb.Error{Code: 1020})
					awaitFailedGRVNativeFuture(t, retry)
					nativeErr := retry.Get()
					goErr := goTx.OnError(context.Background(), &wire.FDBError{Code: 1020})
					expected := 1031
					if mode == "future_cancel_on_error" {
						expected, want = 0, 0
					}
					if failedGRVNativeCode(nativeErr) != expected || admissionErrorCodeForFailedGRV(goErr) != expected {
						t.Fatalf("OnError native=%v Go=%v want%d", nativeErr, goErr, expected)
					}
				}
				if want == 0 && pin == 0 {
					want = 2011
				}
				tr.SetReadVersion(pin)
				getter := tr.GetReadVersion()
				awaitFailedGRVNativeFuture(t, getter)
				nativeVersion, nativeErr := getter.Get()
				goTx.SetReadVersion(pin)
				goVersion, goErr := goTx.GetReadVersion(context.Background())
				if failedGRVNativeCode(nativeErr) != want || admissionErrorCodeForFailedGRV(goErr) != want || (want == 0 && (nativeVersion != 123 || goVersion != 123)) {
					t.Fatalf("setter(%d) native=%d/%v Go=%d/%v want code%d", pin, nativeVersion, nativeErr, goVersion, goErr, want)
				}
			})
		}
	}
}
