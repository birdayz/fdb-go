package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestNativeCommit_PublicWaitersInterruptDuringRetainedGRV(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"read-version", "read", "second-commit"} {
		for _, mode := range []string{"cancel", "timeout"} {
			t.Run(operation+"/"+mode, func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithTimeout(context.Background(), commitLifetimeTimeout)
				defer cancel()
				db, gate, addrs := newGRVReplyGateDB(t, ctx)
				tx := db.CreateTransaction()
				key := []byte(t.Name())
				tx.Set(key, []byte("retained-native"))
				gate.arm(1, false)
				first := startGatedCommit(tx, ctx)
				waitCommitLifetimeGate(t, ctx, gate.heldCh, "native GRV reply withheld")
				native := currentNativeCommit(t, tx)
				natives := []*nativeCommit{native}
				failures := db.db.metrics.Snapshot().ClientConnectionFailures

				arrived := make(chan struct{})
				var once sync.Once
				if operation != "second-commit" {
					tx.beforeReadVersionLock = func() { once.Do(func() { close(arrived) }) }
				}
				second := make(chan error, 1)
				go func() {
					var err error
					switch operation {
					case "read-version":
						_, err = tx.GetReadVersion(ctx)
					case "read":
						_, err = tx.Get(ctx, []byte(t.Name()+"/absent"))
					case "second-commit":
						err = tx.Commit(ctx)
					}
					second <- err
				}()
				if operation == "second-commit" {
					captureCtx, stopCapture := context.WithTimeout(ctx, 3*time.Second)
					defer stopCapture()
					for {
						tx.readErrMu.Lock()
						count := len(tx.readLife.nativeCommits)
						if count == 2 {
							natives = natives[:0]
							for actor := range tx.readLife.nativeCommits {
								natives = append(natives, actor)
							}
						}
						tx.readErrMu.Unlock()
						if count == 2 {
							break
						}
						select {
						case <-captureCtx.Done():
							t.Fatal("second Commit could not capture while native GRV was held")
						case <-time.After(time.Millisecond):
						}
					}
				} else {
					waitCommitLifetimeGate(t, ctx, arrived, "public waiter reached shared read version")
				}
				want := ErrTransactionCancelled
				if mode == "cancel" {
					tx.Cancel()
				} else {
					tx.SetTimeout(1)
					want = ErrTransactionTimedOut
				}
				waitCtx, stopWait := context.WithTimeout(ctx, 3*time.Second)
				defer stopWait()
				awaitCommitLifetimeCode(t, waitCtx, first, want, "first public commit with GRV still held")
				awaitCommitLifetimeCode(t, waitCtx, second, want, "second public waiter with GRV still held")
				for _, actor := range natives {
					select {
					case <-actor.finished:
						t.Fatal("public interruption destroyed retained native GRV")
					default:
					}
				}
				gate.release()
				for _, actor := range natives {
					waitCommitLifetimeGate(t, ctx, actor.finished, "native commit after GRV release")
				}
				if got := readCommitLifetimeValue(t, ctx, db, key); string(got) != "retained-native" {
					t.Fatalf("retained mutation = %q", got)
				}
				assertGRVProxiesHealthy(t, db, addrs, failures)
				runtime.KeepAlive(tx)
			})
		}
	}
}

func TestNativeCommit_SetReadVersionSupersedesRetainedGRV(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), commitLifetimeTimeout)
	defer cancel()
	db, gate, _ := newGRVReplyGateDB(t, ctx)
	known := db.CreateTransaction()
	version, err := known.GetReadVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	key := []byte(t.Name() + "/native")
	known.Set(key, []byte("advance-version"))
	if err := known.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	tx := db.CreateTransaction()
	// Re-reading the explicit old version into the native input would now conflict.
	if err := tx.AddReadConflictRange(key, append(bytes.Clone(key), 0)); err != nil {
		t.Fatal(err)
	}
	tx.Set(key, []byte("retained"))
	state := tx.readVersionState
	gate.arm(1, false)
	done := startGatedCommit(tx, ctx)
	waitCommitLifetimeGate(t, ctx, gate.heldCh, "native GRV before explicit SetReadVersion")
	native := currentNativeCommit(t, tx)
	state.readVersionMu.Lock()
	pending := state.pendingReadVersion
	state.readVersionMu.Unlock()
	if pending == nil {
		t.Fatal("native GRV has no captured pending completion")
	}
	gate.mu.Lock()
	body := bytes.Clone(gate.held[0].body)
	gate.mu.Unlock()
	heldVersion, _, _, _, _, _, err := parseGetReadVersionReply(body)
	if err != nil || heldVersion <= version {
		t.Fatalf("held GRV = %d/%v, want greater than explicit version %d", heldVersion, err, version)
	}
	set := make(chan error, 1)
	go func() {
		tx.SetReadVersion(version)
		set <- nil
	}()
	waitCtx, stop := context.WithTimeout(ctx, 3*time.Second)
	defer stop()
	awaitCommitLifetimeResult(t, waitCtx, set, "SetReadVersion while native GRV withheld")
	if instant, ok := tx.ReadVersionInstant(); ok || !instant.IsZero() {
		t.Fatalf("explicit version carries a GRV instant: %v/%v", instant, ok)
	}
	if got, err := tx.GetReadVersion(waitCtx); err != nil || got != version {
		t.Fatalf("new operation uses %d/%v, want explicit version %d", got, err, version)
	}
	prefix := []byte(t.Name() + "/svk/")
	svk := append(bytes.Clone(prefix), make([]byte, 14)...)
	binary.LittleEndian.PutUint32(svk[len(svk)-4:], uint32(len(prefix)))
	mutated := make(chan error, 1)
	go func() {
		tx.Atomic(MutSetVersionstampedKey, svk, []byte("not-in-native-snapshot"))
		mutated <- nil
	}()
	awaitCommitLifetimeResult(t, waitCtx, mutated, "SVK mutation while native GRV withheld")
	tx.conflictMu.Lock()
	mutation := tx.mutations[len(tx.mutations)-1]
	stamp := bytes.Clone(mutation.Key[len(prefix) : len(prefix)+10])
	tx.conflictMu.Unlock()
	var wantStamp [10]byte
	binary.BigEndian.PutUint64(wantStamp[:8], uint64(version))
	if !bytes.Equal(stamp, wantStamp[:]) {
		t.Fatalf("SVK cached-version stamp = %x, want %x", stamp, wantStamp)
	}
	tx.Cancel()
	awaitCommitLifetimeCode(t, waitCtx, done, ErrTransactionCancelled, "public commit before old GRV publication")
	gate.release()
	waitCommitLifetimeGate(t, ctx, native.finished, "native commit uses its captured GRV")
	waitCommitLifetimeGate(t, ctx, pending.done, "original GRV completion")
	if pending.err != nil || pending.version != heldVersion {
		t.Fatalf("original captured GRV = %d/%v, want %d/nil", pending.version, pending.err, heldVersion)
	}
	state.readVersionMu.Lock()
	got, hasVersion, userSet, instant := state.readVersion, state.hasReadVersion, state.userSetReadVersion, state.readVersionInstant
	state.readVersionMu.Unlock()
	if got != version || !hasVersion || !userSet || !instant.IsZero() {
		t.Fatalf("old completion replaced explicit state: %d/%v/%v/%v", got, hasVersion, userSet, instant)
	}
	if got := readCommitLifetimeValue(t, ctx, db, key); string(got) != "retained" {
		t.Fatalf("native commit data = %q", got)
	}
	runtime.KeepAlive(tx)
}
