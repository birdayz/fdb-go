package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"fdb.dev/pkg/fdbgo/wire"
)

func TestRYWStartGetKeyOwnsSelector(t *testing.T) {
	t.Parallel()
	for _, snapshot := range []bool{false, true} {
		for _, afterReply := range []bool{false, true} {
			t.Run(fmt.Sprintf("snapshot=%t/afterReply=%t", snapshot, afterReply), func(t *testing.T) {
				t.Parallel()
				tx := &Transaction{}
				m := newMemStorage("a", "c")
				tx.nativeRange = m.native
				entered, release := make(chan struct{}), make(chan struct{})
				hook := func() { close(entered); <-release }
				if afterReply {
					tx.afterRangeReply = hook
				} else {
					tx.beforeRangeGoroutine = hook
				}
				key := []byte("a")
				sel := KeySelector{Key: key, OrEqual: afterReply, Offset: 1}
				var p *PendingKey
				if snapshot {
					p = tx.Snapshot().StartGetKey(context.Background(), sel)
				} else {
					p = tx.StartGetKey(context.Background(), sel)
				}
				<-entered
				key[0] = 'z'
				close(release)
				joined(t, "owned selector read", func() {
					got, err := p.Get()
					want := "a"
					if afterReply {
						want = "c"
					}
					if err != nil || string(got) != want {
						t.Errorf("Get = %q, %v; want %q", got, err, want)
					}
				})
				wantBegin := "a"
				if afterReply {
					wantBegin = "a\x00"
				}
				if len(m.requests) != 1 || string(m.requests[0].begin.Key) != wantBegin {
					t.Fatalf("requests = %+v, want begin %q", m.requests, wantBegin)
				}
				if snapshot {
					if len(tx.readConflicts) != 0 {
						t.Fatalf("snapshot added conflicts: %+v", tx.readConflicts)
					}
				} else {
					wantEnd := "a\x00"
					if afterReply {
						wantEnd = "c\x00"
					}
					if len(tx.readConflicts) != 1 || string(tx.readConflicts[0].Begin) != wantBegin || string(tx.readConflicts[0].End) != wantEnd {
						t.Fatalf("conflicts = %+v, want [%q,%q)", tx.readConflicts, wantBegin, wantEnd)
					}
				}
			})
		}
	}
}

// onRequest executes under c.mu, both at issue and when resuming storage. A
// recovered panic must unwind that lock before complete releases the pin.
func TestRYWLockedPanicReleasesRead(t *testing.T) {
	t.Parallel()
	for _, panicAt := range []int32{1, 2} {
		t.Run(fmt.Sprintf("request=%d", panicAt), func(t *testing.T) {
			t.Parallel()
			tx := &Transaction{}
			var requests atomic.Int32
			tx.walkOnRequest = func(walkRequest) {
				if requests.Add(1) == panicAt {
					panic("inside locked walk")
				}
			}
			tx.nativeRange = func(context.Context, KeySelector, KeySelector, rangeLimits, bool) (nativeRangeResult, error) {
				return nativeRangeResult{more: true}, nil // resume builds request 2 under c.mu
			}
			tr := trackPins(&tx.ryw)
			joined(t, "panicking read", func() {
				_, err := tx.StartGetKey(context.Background(), fgtSel("a")).Get()
				if code, _ := fdbErrorCode(err); code != ErrInternalError {
					t.Errorf("read error = %v, want internal_error (4100)", err)
				}
			})
			tr.quiesced(t)
			joined(t, "Commit after panic", func() {
				if code, _ := fdbErrorCode(tx.Commit(context.Background())); code != ErrInternalError {
					t.Errorf("Commit code = %d, want 4100", code)
				}
			})
			joined(t, "Reset after panic", tx.Reset)
			tx.walkOnRequest = nil
			tx.Set([]byte("k"), []byte("v"))
			k, err := tx.StartGetKey(context.Background(), firstGreaterOrEqual([]byte("k"))).Get()
			if err != nil || string(k) != "k" {
				t.Fatalf("read after Reset = %q, %v", k, err)
			}
		})
	}
}

func TestRYWObsoleteBaseReleased(t *testing.T) {
	t.Parallel()
	for _, reset := range []bool{false, true} {
		t.Run(fmt.Sprintf("reset=%t", reset), func(t *testing.T) {
			t.Parallel()
			c := &rywCache{}
			c.set([]byte("old"), bytes.Repeat([]byte("x"), 1<<20))
			c.mu.Lock()
			old := c.pinWritesLocked()
			if old != &c.base {
				t.Fatal("first version is not embedded")
			}
			before := old.fingerprint()
			c.mu.Unlock()
			if reset {
				c.reset()
			} else {
				c.set([]byte("new"), []byte("v"))
			}
			c.mu.Lock()
			defer c.mu.Unlock()
			if old.fingerprint() != before || c.w == old {
				t.Fatal("detaching changed the pinned version or did not detach")
			}
			observed := false
			c.onPin = func(w *rywWrites, pin bool) {
				if !pin {
					observed = w == old && w.fingerprint() == before
				}
			}
			c.unpinWritesLocked(old)
			if !observed {
				t.Fatal("last-unpin observer did not see the intact version")
			}
			if !c.base.empty() {
				t.Fatal("obsolete embedded version retains its graph after last unpin")
			}
			if !reset && string(c.w.writes["new"].value) != "v" {
				t.Fatal("releasing obsolete base damaged the live clone")
			}
		})
	}
}

func TestRYWResetBeatsLateStorageError(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	started, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	tx.nativeRange = func(ctx context.Context, _, _ KeySelector, _ rangeLimits, _ bool) (nativeRangeResult, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		<-release
		return nativeRangeResult{}, &wire.FDBError{Code: ErrWrongShardServer}
	}
	p := tx.StartGetKey(context.Background(), fgtSel("a"))
	<-started
	resetDone := make(chan struct{})
	go func() { tx.Reset(); close(resetDone) }()
	<-cancelled // the retired incarnation's cancellation precedes the late error
	close(release)
	joined(t, "reset and late storage error", func() {
		_, err := p.Get()
		if code, _ := fdbErrorCode(err); code != ErrTransactionCancelled {
			t.Errorf("read error = %v, want transaction_cancelled", err)
		}
		<-resetDone
	})
}

func TestGetKeyReadThroughResetBeatsLateOutcome(t *testing.T) {
	t.Parallel()
	for _, snapshot := range []bool{false, true} {
		for _, success := range []bool{false, true} {
			t.Run(fmt.Sprintf("snapshot=%t/success=%t", snapshot, success), func(t *testing.T) {
				t.Parallel()
				tx := &Transaction{txOptions: txOptions{rywDisabled: true}}
				tx.SetReadVersion(1)
				started, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
				tx.resolveKeyFn = func(ctx context.Context, _ KeySelector, _ int64) ([]byte, error) {
					close(started)
					<-ctx.Done()
					close(cancelled)
					<-release
					if success {
						return []byte("c"), nil
					}
					return nil, &wire.FDBError{Code: ErrWrongShardServer}
				}
				var p *PendingKey
				if snapshot {
					p = tx.Snapshot().StartGetKey(context.Background(), fgtSel("a"))
				} else {
					p = tx.StartGetKey(context.Background(), fgtSel("a"))
				}
				<-started
				resetDone := make(chan struct{})
				go func() { tx.Reset(); close(resetDone) }()
				<-cancelled
				close(release)
				joined(t, "read-through reset and late outcome", func() {
					_, err := p.Get()
					if code, _ := fdbErrorCode(err); code != ErrTransactionCancelled {
						t.Errorf("late outcome error = %v, want transaction_cancelled", err)
					}
					<-resetDone
				})
				if len(tx.readConflicts) != 0 {
					t.Fatal("retired read-through outcome added conflicts")
				}
			})
		}
	}
}

func TestGetKeyCompletedOutcomeSurvivesReset(t *testing.T) {
	t.Parallel()
	for _, disabled := range []bool{false, true} {
		for _, success := range []bool{false, true} {
			t.Run(fmt.Sprintf("disabled=%t/success=%t", disabled, success), func(t *testing.T) {
				t.Parallel()
				tx := &Transaction{txOptions: txOptions{rywDisabled: disabled}}
				tx.SetReadVersion(1)
				failure := &wire.FDBError{Code: ErrWrongShardServer}
				tx.resolveKeyFn = func(context.Context, KeySelector, int64) ([]byte, error) {
					if success {
						return []byte("c"), nil
					}
					return nil, failure
				}
				m := newMemStorage("a", "c")
				tx.nativeRange = func(ctx context.Context, b, e KeySelector, l rangeLimits, rev bool) (nativeRangeResult, error) {
					if !success {
						return nativeRangeResult{}, failure
					}
					return m.native(ctx, b, e, l, rev)
				}
				p := tx.StartGetKey(context.Background(), fgtSel("a"))
				before, errBefore := p.Get()
				tx.Reset()
				after, errAfter := p.Get()
				if !bytes.Equal(before, after) || errBefore != errAfter {
					t.Fatalf("completed outcome changed across Reset: (%q,%v) -> (%q,%v)", before, errBefore, after, errAfter)
				}
				if success && (string(after) != "c" || errAfter != nil) {
					t.Fatalf("completed success = %q, %v", after, errAfter)
				}
				if !success && errAfter != failure {
					t.Fatalf("completed protocol error = %v, want %v", errAfter, failure)
				}
			})
		}
	}
}

// A borrowed async read can outlive its admitting frame. Reset drains state
// leases, not that retired frame's borrowed lease, so the last conflict publish
// must recheck its captured incarnation instead of appending to the new handle.
func TestCompletedReadCannotPublishAfterReset(t *testing.T) {
	t.Parallel()
	for _, route := range []string{"getKey", "range", "watch"} {
		t.Run(route, func(t *testing.T) {
			t.Parallel()
			tx := &Transaction{}
			m := newMemStorage("a", "c")
			tx.nativeRange = m.native
			if route == "range" {
				tx.ryw.withLock(func() { tx.ryw.serverCache.insert([]byte("a"), []byte("z"), m.kvs) })
			}
			if route == "watch" {
				tx.db = &database{ctx: context.Background()}
				tx.db.maxWatches.Store(1)
				// Model an acquired GRV, not a user-set version needing DB-floor validation.
				tx.readVersionState = &readVersionState{readVersion: 1, hasReadVersion: true}
				tx.ryw.cachePointResult([]byte("a"), []byte("value"))
			}
			entered, release := make(chan struct{}), make(chan struct{})
			resume := sync.OnceFunc(func() { close(release) })
			defer resume()
			tx.beforeReadConflict = func() { close(entered); <-release }
			parent, leave := tx.opContext(context.Background())
			defer leave()
			done := make(chan error, 1)
			go func() {
				switch route {
				case "getKey":
					_, err := tx.StartGetKey(parent, fgtSel("a")).Get()
					done <- err
				case "range":
					_, _, err := tx.StartGetRange(parent, []byte("a"), []byte("d"), 0, -1, false).Get()
					done <- err
				case "watch":
					_, _, _, watchCtx, watchCancel, err := tx.WatchSetup(parent, []byte("a"))
					if watchCancel != nil {
						watchCancel()
						tx.db.releaseWatch()
					}
					if watchCtx != nil || watchCancel != nil {
						t.Error("retired watch setup returned poll ownership")
					}
					done <- err
				}
			}()
			joined(t, "completed read reaching conflict publication", func() { <-entered })
			leave()
			joined(t, "Reset with a completed borrowed read", tx.Reset)
			resume()
			joined(t, "old read completion", func() {
				if err := <-done; err == nil {
					t.Error("retired read succeeded instead of failing reset")
				} else if code, _ := fdbErrorCode(err); code != ErrTransactionCancelled {
					t.Errorf("retired read error = %v, want transaction_cancelled", err)
				}
			})
			if route == "watch" {
				if n := tx.db.outstandingWatches.Load(); n != 0 {
					t.Fatalf("failed watch setup retained %d watch slots", n)
				}
				tx.watchMu.Lock()
				remaining := len(tx.watchCancels)
				tx.watchMu.Unlock()
				if remaining != 0 {
					t.Fatalf("failed watch setup retained %d watch contexts", remaining)
				}
			}
			tx.conflictMu.Lock()
			defer tx.conflictMu.Unlock()
			if len(tx.readConflicts) != 0 {
				t.Fatalf("new incarnation inherited old conflicts: %+v", tx.readConflicts)
			}
		})
	}
}

func TestWatchSetupPublishesLiveReadConflict(t *testing.T) {
	t.Parallel()
	db := &database{ctx: context.Background()}
	db.maxWatches.Store(1)
	tx := &Transaction{db: db, readVersionState: &readVersionState{readVersion: 1, hasReadVersion: true}}
	key := []byte("watch-key")
	tx.ryw.cachePointResult(key, []byte("value"))
	value, version, _, watchCtx, watchCancel, err := tx.WatchSetup(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if watchCancel == nil {
		t.Fatal("successful watch setup has no cancellation ownership")
	}
	defer watchCancel()
	defer db.releaseWatch()
	if string(value) != "value" || version != 1 || watchCtx == nil || watchCtx.Err() != nil {
		t.Fatalf("watch setup = %q, version %d, context %v", value, version, watchCtx)
	}
	if n := db.outstandingWatches.Load(); n != 1 {
		t.Fatalf("successful watch setup reserved %d slots, want 1", n)
	}
	if len(tx.readConflicts) != 1 || string(tx.readConflicts[0].Begin) != string(key) || string(tx.readConflicts[0].End) != string(key)+"\x00" {
		t.Fatalf("watch conflicts = %+v, want [%q,%q)", tx.readConflicts, key, keyAfterBytes(key))
	}
}

func TestAddWriteConflictKeyLegalRange(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"user", "read-system", "access-system"} {
		for _, key := range []string{"", "k", "\xff", "\xffx", "\xff\xff", "\xff\xffx"} {
			t.Run(fmt.Sprintf("%s/%x", mode, key), func(t *testing.T) {
				t.Parallel()
				tx := &Transaction{}
				if mode == "read-system" {
					tx.SetReadSystemKeys()
				} else if mode == "access-system" {
					tx.SetAccessSystemKeys()
				}
				tx.Set([]byte("a"), []byte("v"))
				before := liveOf(&tx.ryw).fingerprint()
				tx.AddWriteConflictKey([]byte(key))
				illegal := key >= "\xff" && (mode != "access-system" || key >= "\xff\xff")
				_, err := tx.StartGetKey(context.Background(), firstGreaterOrEqual([]byte("a"))).Get()
				if illegal {
					var fe *wire.FDBError
					if !errors.As(err, &fe) || fe.Code != 2004 {
						t.Fatalf("next GetKey = %v, want deferred key_outside_legal_range", err)
					}
					if after := liveOf(&tx.ryw).fingerprint(); after != before {
						t.Fatal("illegal conflict key inserted structural nodes")
					}
					if code, _ := fdbErrorCode(tx.Commit(context.Background())); code != 2004 {
						t.Fatalf("Commit code %d, want deferred 2004", code)
					}
				} else if err != nil {
					t.Fatalf("legal conflict key poisoned next GetKey: %v", err)
				}
			})
		}
	}
}

func TestAddWriteConflictKeyPreservesDeferredOrder(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	tx.Set(bytes.Repeat([]byte("k"), 10001), []byte("v"))
	tx.AddWriteConflictKey([]byte("\xff\xffx"))
	if code, _ := fdbErrorCode(tx.Commit(context.Background())); code != 2102 {
		t.Fatalf("earlier oversized Set lost to conflict-key error: code %d, want key_too_large (2102)", code)
	}
	before := liveOf(&tx.ryw).fingerprint()
	tx.AddWriteConflictKey([]byte("a"))
	if after := liveOf(&tx.ryw).fingerprint(); after != before {
		t.Fatal("a write-conflict key executed after a deferred error")
	}
}

// libfdb_c returns success from add_conflict_range and records key_outside_legal_range as the
// deferred error (ThreadSafeTransaction.cpp:659-663; check at ReadYourWrites.actor.cpp:2466-2468).
func TestAddWriteConflictRangeLegalRangeIsDeferred(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"user", "read-system", "access-system"} {
		for _, r := range [][2]string{
			{"a", "b"},
			{"a", "\xff"},
			{"\xff", "\xff"},
			{"a", "\xff\x05"},
			{"\xff", "\xff\x05"},
			{"a", "\xff\xff"},
			{"a", "\xff\xff\x00"},
			{"\xff\xffx", "\xff\xffy"},
		} {
			t.Run(fmt.Sprintf("%s/%x-%x", mode, r[0], r[1]), func(t *testing.T) {
				t.Parallel()
				tx := &Transaction{}
				maxWrite := "\xff"
				if mode == "read-system" {
					tx.SetReadSystemKeys()
				} else if mode == "access-system" {
					tx.SetAccessSystemKeys()
					maxWrite = "\xff\xff"
				}
				tx.Set([]byte("a"), []byte("v"))
				before, conflicts := liveOf(&tx.ryw).fingerprint(), len(tx.writeConflicts)
				if err := tx.AddWriteConflictRange([]byte(r[0]), []byte(r[1])); err != nil {
					t.Fatalf("AddWriteConflictRange returned %v, want nil (the error is deferred)", err)
				}
				_, err := tx.StartGetKey(context.Background(), firstGreaterOrEqual([]byte("a"))).Get()
				if r[0] > maxWrite || r[1] > maxWrite {
					if code, _ := fdbErrorCode(err); code != 2004 {
						t.Fatalf("next GetKey = %v, want deferred key_outside_legal_range", err)
					}
					if liveOf(&tx.ryw).fingerprint() != before || len(tx.writeConflicts) != conflicts {
						t.Fatal("illegal write-conflict range was recorded")
					}
					if code, _ := fdbErrorCode(tx.Commit(context.Background())); code != 2004 {
						t.Fatalf("Commit code %d, want deferred 2004", code)
					}
				} else if err != nil {
					t.Fatalf("legal write-conflict range poisoned next GetKey: %v", err)
				}
			})
		}
	}
}

// inverted_range comes from the KeyRangeRef built on the calling thread (fdb_c.cpp:1223,
// FDBTypes.h:318-323), so it is returned by the call even after a deferred error and never deferred.
func TestAddWriteConflictRangeInvertedIsImmediate(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	tx.Set([]byte("a"), []byte("v"))
	before, conflicts := liveOf(&tx.ryw).fingerprint(), len(tx.writeConflicts)
	if code, _ := fdbErrorCode(tx.AddWriteConflictRange([]byte("b"), []byte("a"))); code != 2005 {
		t.Fatalf("inverted range code %d, want inverted_range (2005) from the call", code)
	}
	if tx.deferredErr.Load() != nil || liveOf(&tx.ryw).fingerprint() != before || len(tx.writeConflicts) != conflicts {
		t.Fatal("inverted range was deferred or recorded")
	}
	if _, err := tx.StartGetKey(context.Background(), firstGreaterOrEqual([]byte("a"))).Get(); err != nil {
		t.Fatalf("inverted range was deferred: next GetKey = %v", err)
	}
	if err := tx.AddWriteConflictRange([]byte("a"), []byte("\xff\xff")); err != nil {
		t.Fatalf("illegal range returned %v, want nil", err)
	}
	if code, _ := fdbErrorCode(tx.AddWriteConflictRange([]byte("\xff\xff\xff"), []byte("\xff\xff"))); code != 2005 {
		t.Fatalf("inverted range after a deferred error: code %d, want 2005 from the call", code)
	}
	if code, _ := fdbErrorCode(tx.Commit(context.Background())); code != 2004 {
		t.Fatalf("Commit code %d, want the first deferred error, key_outside_legal_range (2004)", code)
	}
}

func TestAddWriteConflictRangePreservesDeferredOrder(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	tx.Set(bytes.Repeat([]byte("k"), 10001), []byte("v"))
	if err := tx.AddWriteConflictRange([]byte("\xff\xffx"), []byte("\xff\xffy")); err != nil {
		t.Fatalf("illegal range returned %v, want nil", err)
	}
	if code, _ := fdbErrorCode(tx.Commit(context.Background())); code != 2102 {
		t.Fatalf("earlier oversized Set lost to conflict-range error: code %d, want key_too_large (2102)", code)
	}
	before, conflicts := liveOf(&tx.ryw).fingerprint(), len(tx.writeConflicts)
	if err := tx.AddWriteConflictRange([]byte("a"), []byte("b")); err != nil {
		t.Fatalf("legal range after a deferred error returned %v, want nil", err)
	}
	if liveOf(&tx.ryw).fingerprint() != before || len(tx.writeConflicts) != conflicts {
		t.Fatal("a write-conflict range executed after a deferred error")
	}
}

// libfdb_c returns success from add_conflict_range and records RYW addReadConflictRange's
// key_outside_legal_range as the deferred error (ThreadSafeTransaction.cpp:589-594; check at
// ReadYourWrites.actor.cpp:1950-1955). The key form is the same call over [k, k\x00)
// (bindings/go/src/fdb/transaction.go:548-554).
func TestAddReadConflictRangeLegalRangeIsDeferred(t *testing.T) {
	t.Parallel()
	mvk, mvkEnd := string(metadataVersionKeyBytes), string(metadataVersionKeyEndBytes)
	type add struct {
		begin, end string
		key        bool
	}
	var adds []add
	for _, r := range [][2]string{
		{"b", "c"},
		{"b", "\xff"},
		{"\xff", "\xff"},
		{"b", "\xff\x05"},
		{"\xff", "\xff\x05"},
		{"b", "\xff\xff"},
		{"b", "\xff\xff\x00"},
		{"\xff\xffx", "\xff\xffy"},
		{mvk, mvkEnd},
		{mvk, mvk + "Z"},
	} {
		adds = append(adds, add{r[0], r[1], false})
	}
	for _, k := range []string{"", "k", "\xfe\xff", "\xff", "\xff\x05", "\xff\xff", "\xff\xffx", mvk} {
		adds = append(adds, add{k, k + "\x00", true})
	}
	for _, mode := range []string{"user", "read-system", "access-system"} {
		for _, a := range adds {
			form := "range"
			if a.key {
				form = "key"
			}
			t.Run(fmt.Sprintf("%s/%s/%x-%x", mode, form, a.begin, a.end), func(t *testing.T) {
				t.Parallel()
				tx := &Transaction{}
				maxRead := "\xff"
				if mode == "read-system" {
					tx.SetReadSystemKeys()
					maxRead = "\xff\xff"
				} else if mode == "access-system" {
					tx.SetAccessSystemKeys()
					maxRead = "\xff\xff"
				}
				tx.Set([]byte("a"), []byte("v"))
				conflicts := len(tx.readConflicts)
				if a.key {
					tx.AddReadConflictKey([]byte(a.begin))
				} else if err := tx.AddReadConflictRange([]byte(a.begin), []byte(a.end)); err != nil {
					t.Fatalf("AddReadConflictRange returned %v, want nil (the error is deferred)", err)
				}
				illegal := (a.begin > maxRead || a.end > maxRead) && !(a.begin == mvk && a.end == mvkEnd)
				_, err := tx.StartGetKey(context.Background(), firstGreaterOrEqual([]byte("a"))).Get()
				if illegal {
					if code, _ := fdbErrorCode(err); code != 2004 {
						t.Fatalf("next GetKey = %v, want deferred key_outside_legal_range", err)
					}
					if len(tx.readConflicts) != conflicts {
						t.Fatal("illegal read-conflict range was recorded")
					}
					if code, _ := fdbErrorCode(tx.Commit(context.Background())); code != 2004 {
						t.Fatalf("Commit code %d, want deferred 2004", code)
					}
				} else {
					if err != nil {
						t.Fatalf("legal read-conflict range poisoned next GetKey: %v", err)
					}
					// The added range does not touch the self-written "a", so the RYW filter keeps
					// it whole; an empty one records nothing (RYW:1973-1975).
					want := []KeyRange{}
					if a.begin < a.end {
						want = []KeyRange{{Begin: []byte(a.begin), End: []byte(a.end)}}
					}
					if got := tx.readConflicts[conflicts:]; fmt.Sprintf("%q", got) != fmt.Sprintf("%q", want) {
						t.Fatalf("legal read conflict recorded %q, want %q", got, want)
					}
				}
			})
		}
	}
}

// inverted_range comes from the KeyRangeRef built on the calling thread (fdb_c.cpp:1223,
// FDBTypes.h:318-323), so it is returned by the call even after a deferred error and never deferred.
func TestAddReadConflictRangeInvertedIsImmediate(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	tx.Set([]byte("a"), []byte("v"))
	conflicts := len(tx.readConflicts)
	if code, _ := fdbErrorCode(tx.AddReadConflictRange([]byte("c"), []byte("b"))); code != 2005 {
		t.Fatalf("inverted range code %d, want inverted_range (2005) from the call", code)
	}
	if tx.deferredErr.Load() != nil || len(tx.readConflicts) != conflicts {
		t.Fatal("inverted range was deferred or recorded")
	}
	if _, err := tx.StartGetKey(context.Background(), firstGreaterOrEqual([]byte("a"))).Get(); err != nil {
		t.Fatalf("inverted range was deferred: next GetKey = %v", err)
	}
	if err := tx.AddReadConflictRange([]byte("b"), []byte("\xff\xff")); err != nil {
		t.Fatalf("illegal range returned %v, want nil", err)
	}
	if code, _ := fdbErrorCode(tx.AddReadConflictRange([]byte("\xff\xff\xff"), []byte("\xff\xff"))); code != 2005 {
		t.Fatalf("inverted range after a deferred error: code %d, want 2005 from the call", code)
	}
	if code, _ := fdbErrorCode(tx.Commit(context.Background())); code != 2004 {
		t.Fatalf("Commit code %d, want the first deferred error, key_outside_legal_range (2004)", code)
	}
}

func TestAddReadConflictRangePreservesDeferredOrder(t *testing.T) {
	t.Parallel()
	for _, key := range []bool{false, true} {
		t.Run(fmt.Sprintf("key=%t", key), func(t *testing.T) {
			t.Parallel()
			tx := &Transaction{}
			tx.Set(bytes.Repeat([]byte("k"), 10001), []byte("v"))
			if key {
				tx.AddReadConflictKey([]byte("\xff\xffx"))
			} else if err := tx.AddReadConflictRange([]byte("\xff\xffx"), []byte("\xff\xffy")); err != nil {
				t.Fatalf("illegal range returned %v, want nil", err)
			}
			conflicts := len(tx.readConflicts)
			if key {
				tx.AddReadConflictKey([]byte("b"))
			} else if err := tx.AddReadConflictRange([]byte("b"), []byte("c")); err != nil {
				t.Fatalf("legal range after a deferred error returned %v, want nil", err)
			}
			if len(tx.readConflicts) != conflicts {
				t.Fatal("a read-conflict range executed after a deferred error")
			}
			if code, _ := fdbErrorCode(tx.Commit(context.Background())); code != 2102 {
				t.Fatalf("earlier oversized Set lost to conflict-range error: code %d, want key_too_large (2102)", code)
			}
		})
	}
}

// The key form is addReadConflictRange over [k, k\x00): it records what the range form records,
// including nothing for a key the size clamp (ReadYourWrites.actor.cpp:1962-1975) empties.
func TestAddReadConflictKeyMatchesRangeForm(t *testing.T) {
	t.Parallel()
	limit := getMaxClearKeySize([]byte("k"))
	sysLimit := getMaxClearKeySize([]byte("\xff"))
	keys := map[string][]byte{
		"empty":           {},
		"user":            []byte("k"),
		"self_written":    []byte("w"),
		"at_limit":        bytes.Repeat([]byte("k"), limit),
		"over_limit":      bytes.Repeat([]byte("k"), limit+1),
		"far_over_limit":  bytes.Repeat([]byte("k"), 2*limit),
		"system":          []byte("\xff\x05"),
		"sys_at_limit":    append([]byte("\xff\x05"), bytes.Repeat([]byte("s"), sysLimit-2)...),
		"sys_over_limit":  append([]byte("\xff\x05"), bytes.Repeat([]byte("s"), sysLimit-1)...),
		"max_read_key":    []byte("\xff\xff"),
		"metadataVersion": metadataVersionKeyBytes,
	}
	for name, k := range keys {
		for _, mode := range []string{"user", "read-system", "access-system"} {
			for _, rywDisabled := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/rywDisabled=%t", name, mode, rywDisabled), func(t *testing.T) {
					t.Parallel()
					var txs [2]*Transaction
					for i := range txs {
						tx := &Transaction{}
						if mode == "read-system" {
							tx.SetReadSystemKeys()
						} else if mode == "access-system" {
							tx.SetAccessSystemKeys()
						}
						tx.rywDisabled = rywDisabled
						tx.Set([]byte("w"), []byte("v"))
						txs[i] = tx
					}
					txs[0].AddReadConflictKey(k)
					if err := txs[1].AddReadConflictRange(k, keyAfterBytes(k)); err != nil {
						t.Fatalf("AddReadConflictRange: %v", err)
					}
					var got [2]string
					for i, tx := range txs {
						code := 0
						if e := tx.deferredErr.Load(); e != nil {
							code = e.Code
						}
						got[i] = fmt.Sprintf("deferred=%d conflicts=%q", code, tx.readConflicts)
					}
					if got[0] != got[1] {
						t.Fatalf("key form: %s\nrange form: %s", got[0], got[1])
					}
					switch name {
					case "over_limit", "far_over_limit", "sys_over_limit":
						if len(txs[0].readConflicts) != 0 {
							t.Fatalf("oversized key recorded %d read conflicts, want none", len(txs[0].readConflicts))
						}
					case "at_limit":
						if len(txs[0].readConflicts) != 1 {
							t.Fatalf("key at the size limit recorded %d read conflicts, want 1", len(txs[0].readConflicts))
						}
					}
				})
			}
		}
	}
}

func TestGetKeyMaxSentinelConflict(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		sel                  KeySelector
		resolved, begin, end string
	}{
		{KeySelector{Key: []byte("\xff\xff"), OrEqual: true, Offset: 0}, "\xffa", "\xffa", "\xff\xff"},
		{KeySelector{Key: []byte("\xff\xff"), OrEqual: true, Offset: 1}, "\xff\xff", "\xff\xff", "\xff\xff"},
		{KeySelector{Key: []byte("a"), Offset: 1}, "\xff\xff", "a", "\xff\xff"},
	} {
		b, e := getKeyConflictSpan(tc.sel, []byte(tc.resolved))
		if string(b) != tc.begin || string(e) != tc.end {
			t.Errorf("%+v resolved %q: [%q,%q), want [%q,%q)", tc.sel, tc.resolved, b, e, tc.begin, tc.end)
		}
	}
	tx := &Transaction{}
	tx.SetReadSystemKeys()
	tx.nativeRange = newMemStorage("a", "\xffa").native
	k, err := tx.StartGetKey(context.Background(), KeySelector{Key: []byte("\xff\xff"), Offset: 0}).Get()
	if err != nil || string(k) != "\xffa" {
		t.Fatalf("LLT(max) = %q, %v", k, err)
	}
	if len(tx.readConflicts) != 1 || string(tx.readConflicts[0].Begin) != "\xffa" || string(tx.readConflicts[0].End) != "\xff\xff" {
		t.Fatalf("LLT(max) conflicts = %+v", tx.readConflicts)
	}
}
