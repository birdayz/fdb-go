package client

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"testing"
	"time"
	"weak"
)

// Each mode runs with a caller context that can be cancelled and with one that
// cannot (context.Background, the context-free API), whose native GRV runs in an
// incarnation that is the native context itself.
func TestCommitGRV_TransactionAndCallerLifetimes(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"cancel", "timeout", "caller", "reset", "db-close", "cancel-reset", "cancel-db-close"} {
		for _, background := range []bool{false, true} {
			if background && mode == "caller" {
				continue // a Background caller cannot be cancelled
			}
			name := mode
			if background {
				name += "/background"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				testCommitGRVLifetime(t, mode, background)
			})
		}
	}
}

func testCommitGRVLifetime(t *testing.T, mode string, background bool) {
	ctx, cancel := context.WithTimeout(context.Background(), commitLifetimeTimeout)
	defer cancel()
	db, gate, _ := newGRVReplyGateDB(t, ctx)
	tx := db.CreateTransaction()
	state := tx.readVersionState
	key := []byte(t.Name())
	tx.Set(key, []byte("native-grv"))
	caller, stopCaller := context.WithCancel(ctx)
	defer stopCaller()
	if background {
		caller = context.Background()
	}
	gate.arm(1, false)
	done := startGatedCommit(tx, caller)
	waitCommitLifetimeGate(t, ctx, gate.heldCh, "native commit GRV reply withheld")
	native := currentNativeCommit(t, tx)
	want := ErrTransactionCancelled
	switch mode {
	case "cancel", "cancel-reset", "cancel-db-close":
		tx.Cancel()
	case "timeout":
		tx.SetTimeout(1)
		want = ErrTransactionTimedOut
	case "caller":
		stopCaller()
	case "reset":
		tx.Reset()
	case "db-close":
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
	err := awaitGatedCommit(t, ctx, done)
	if mode == "caller" || mode == "db-close" {
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("%s during native GRV = %v", mode, err)
		}
	} else {
		requireCommitLifetimeCode(t, err, want, "public wait during native GRV")
	}
	if mode == "cancel-reset" || mode == "cancel-db-close" {
		select {
		case <-native.finished:
			t.Fatal("Cancel destroyed native GRV before owner retirement")
		default:
		}
		if mode == "cancel-reset" {
			tx.Reset()
		} else if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
	retained := mode == "cancel" || mode == "timeout"
	if retained {
		select {
		case <-native.finished:
			t.Fatal("transaction interruption destroyed native commit while its GRV was pending")
		default:
		}
		gate.release()
		waitCommitLifetimeGate(t, ctx, native.finished, "retained native GRV and commit")
	} else {
		waitCommitLifetimeGate(t, ctx, native.finished, "native GRV aborted before its reply")
		gate.release()
	}
	state.readVersionMu.Lock()
	published := state.hasReadVersion && state.readVersion > 0
	state.readVersionMu.Unlock()
	if published != retained {
		t.Fatalf("old shared GRV state published = %v, retained = %v", published, retained)
	}
	if requests, held, err := gate.counts(); requests != 1 || held != 1 || err != nil {
		t.Fatalf("native GRV requests/held/error = %d/%d/%v, want 1/1/nil", requests, held, err)
	}
	verify := openTestDB(t, ctx)
	got := readCommitLifetimeValue(t, ctx, verify, key)
	if retained && string(got) != "native-grv" {
		t.Fatalf("retained native commit missing: %q", got)
	}
	if !retained && got != nil {
		t.Fatalf("dropped native GRV sent a commit: %q", got)
	}
	if retained {
		if _, err := tx.Get(ctx, key); err == nil {
			t.Fatal("late native GRV completion revived interrupted public handle")
		} else {
			requireCommitLifetimeCode(t, err, want, "read after retained native completion")
		}
	}
	runtime.KeepAlive(tx)
}

func TestCommitGRV_LatencyExcludesReadVersionWait(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), commitLifetimeTimeout)
	defer cancel()
	db, gate, _ := newGRVReplyGateDB(t, ctx)
	before := db.db.metrics.Snapshot()
	tx := db.CreateTransaction()
	tx.Set([]byte(t.Name()), []byte("late-native-success"))
	gate.arm(1, false)
	done := startGatedCommit(tx, ctx)
	waitCommitLifetimeGate(t, ctx, gate.heldCh, "native GRV before latency sample")
	native := currentNativeCommit(t, tx)
	heldAt := time.Now()
	tx.Cancel()
	awaitCommitLifetimeCode(t, ctx, done, 1025, "public wait before native latency sample")
	time.Sleep(20 * time.Millisecond)
	held := time.Since(heldAt).Seconds()
	gate.release()
	waitCommitLifetimeGate(t, ctx, native.finished, "late native latency sample")
	after := db.db.metrics.Snapshot()
	commit := after.CommitLatency.Sum - before.CommitLatency.Sum
	total := after.TransactionLatency.Sum - before.TransactionLatency.Sum
	grv := after.GRVLatency.Sum - before.GRVLatency.Sum
	if after.CommitLatency.Count-before.CommitLatency.Count != 1 || after.TransactionLatency.Count-before.TransactionLatency.Count != 1 {
		t.Fatal("late native success did not publish exactly one commit/transaction sample")
	}
	if total-commit < held || grv < held {
		t.Fatalf("GRV hold %.6fs must be excluded from commit %.6fs, included in total %.6fs and GRV %.6fs", held, commit, total, grv)
	}
	runtime.KeepAlive(tx)
}

func abandonCommitGRV(t *testing.T, ctx context.Context) (*Database, *grvReplyGate, *nativeCommit, weak.Pointer[Transaction], []byte) {
	t.Helper()
	db, gate, _ := newGRVReplyGateDB(t, ctx)
	tx := db.CreateTransaction()
	key := []byte(t.Name())
	tx.Set(key, []byte("must-not-escape"))
	gate.arm(1, false)
	done := startGatedCommit(tx, ctx)
	waitCommitLifetimeGate(t, ctx, gate.heldCh, "native commit GRV before owner abandonment")
	native := currentNativeCommit(t, tx)
	tx.Cancel()
	awaitCommitLifetimeCode(t, ctx, done, ErrTransactionCancelled, "canceled public GRV wait")
	select {
	case <-native.finished:
		t.Fatal("Cancel dropped native GRV before owner abandonment")
	default:
	}
	return db, gate, native, weak.Make(tx), key
}

func TestCommitGRV_AbandonedOwnerDoesNotLeak(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), commitLifetimeTimeout)
	defer cancel()
	_, gate, native, owner, key := abandonCommitGRV(t, ctx)
	for {
		runtime.GC()
		if owner.Value() == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("native GRV retains its abandoned transaction")
		case <-time.After(time.Millisecond):
		}
	}
	waitCommitLifetimeGate(t, ctx, native.finished, "owner cleanup drops native GRV without its reply")
	gate.release()
	verify := openTestDB(t, ctx)
	if got := readCommitLifetimeValue(t, ctx, verify, key); got != nil {
		t.Fatalf("abandoned native GRV sent a commit: %q", got)
	}
}

func TestCommitGRV_ResetAfterPublicWaitPreservesSuccessor(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), commitLifetimeTimeout)
	defer cancel()
	db, gate, _ := newGRVReplyGateDB(t, ctx)
	tx := db.CreateTransaction()
	oldState := tx.readVersionState
	oldKey, newKey := []byte(t.Name()+"/old"), []byte(t.Name()+"/new")
	tx.SetPriority(PriorityBatch)
	tx.Set(oldKey, []byte("must-not-escape"))
	gate.arm(1, false)
	oldDone := startGatedCommit(tx, ctx)
	waitCommitLifetimeGate(t, ctx, gate.heldCh, "old native GRV reply withheld")
	native := currentNativeCommit(t, tx)
	tx.Cancel()
	awaitCommitLifetimeCode(t, ctx, oldDone, ErrTransactionCancelled, "old public wait")
	tx.Reset()
	waitCommitLifetimeGate(t, ctx, native.finished, "Reset drops old native GRV before its reply")
	if tx.readVersionState == oldState {
		t.Fatal("Reset reused old native GRV state for its successor")
	}
	tx.SetPriority(PrioritySystemImmediate)
	tx.SetSkipGrvCache()
	tx.Set(newKey, []byte("successor"))
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("successor commit with old GRV withheld: %v", err)
	}
	version, err := tx.GetCommittedVersion()
	if err != nil {
		t.Fatal(err)
	}
	stamp, err := tx.GetVersionstamp()
	if err != nil {
		t.Fatal(err)
	}
	state := tx.readVersionState
	tx.SetReadVersion(version)
	tx.SetPriority(PriorityBatch)
	gate.release()
	if tx.readVersionState != state || tx.priority != PriorityBatch {
		t.Fatal("old GRV completion replaced successor read state or options")
	}
	if got, err := tx.GetCommittedVersion(); err != nil || got != version {
		t.Fatalf("late old GRV overwrote successor committed version: %d, %v; want %d", got, err, version)
	}
	if got, err := tx.GetVersionstamp(); err != nil || !bytes.Equal(got, stamp) {
		t.Fatalf("late old GRV overwrote successor versionstamp: %x, %v; want %x", got, err, stamp)
	}
	state.readVersionMu.Lock()
	readVersion, userSet := state.readVersion, state.userSetReadVersion
	state.readVersionMu.Unlock()
	if readVersion != version || !userSet {
		t.Fatalf("late old GRV overwrote successor explicit read version: %d/%v, want %d/true", readVersion, userSet, version)
	}
	if got := readCommitLifetimeValue(t, ctx, db, oldKey); got != nil {
		t.Fatalf("retired native GRV sent old mutation: %q", got)
	}
	if got := readCommitLifetimeValue(t, ctx, db, newKey); string(got) != "successor" {
		t.Fatalf("successor mutation = %q", got)
	}
	runtime.KeepAlive(tx)
}
