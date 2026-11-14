package client

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/wire"
)

func admissionErrorCode(err error) int {
	var fe *wire.FDBError
	if errors.As(err, &fe) {
		return fe.Code
	}
	return 0
}

func TestReadVersionAdmissionResamplesRejectedSetter(t *testing.T) {
	t.Parallel()
	for _, cached := range []bool{false, true} {
		name := "empty"
		if cached {
			name = "cached"
		}
		t.Run(name, func(t *testing.T) {
			db, observer, ctx := explicitGetterNoGRVFixture(t)
			tx := &Transaction{db: db}
			defer tx.Cancel()
			want := 2011
			if cached {
				tx.SetReadVersion(123)
				want = 2010
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			tx.beforeReadVersionLock = func() { close(entered); <-release }
			done := make(chan error, 1)
			go func() { _, err := tx.GetReadVersion(ctx); done <- err }()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			tx.SetReadVersion(0)
			unblock()
			select {
			case err := <-done:
				if admissionErrorCode(err) != want {
					t.Fatalf("getter=%v want%d", err, want)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if observer.admissions != 0 || tx.pendingReadVersion != nil || tx.hasReadVersion != cached || (cached && tx.readVersion != 123) {
				t.Fatalf("rejected setter admitted GRV or changed pin: admissions=%d pending=%v version=%d cached=%v", observer.admissions, tx.pendingReadVersion, tx.readVersion, tx.hasReadVersion)
			}
		})
	}
}

func TestReadVersionRejectionPublishesUnderAdmissionLock(t *testing.T) {
	t.Parallel()
	for _, cached := range []bool{false, true} {
		tx := &Transaction{}
		if cached {
			tx.SetReadVersion(123)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		locked := make(chan bool, 1)
		release := make(chan struct{})
		tx.beforeReadVersionReject = func() {
			unlocked := tx.readVersionMu.TryLock()
			if unlocked {
				tx.readVersionMu.Unlock()
			}
			locked <- !unlocked
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		done := make(chan struct{})
		go func() { tx.SetReadVersion(0); close(done) }()
		select {
		case held := <-locked:
			close(release)
			if !held {
				cancel()
				t.Fatal("setter released GRV admission lock before publishing rejection")
			}
		case <-ctx.Done():
			cancel()
			t.Fatal(ctx.Err())
		}
		select {
		case <-done:
		case <-ctx.Done():
			cancel()
			t.Fatal(ctx.Err())
		}
		cancel()
		want := 2011
		if cached {
			want = 2010
		}
		requireReadVersionCode(t, tx, want)
		tx.Cancel()
	}
}

type heldReadVersionWindow struct{ armed chan struct{} }

func (w *heldReadVersionWindow) arm(time.Duration) bool { close(w.armed); return true }
func (*heldReadVersionWindow) stop()                    {}

func TestPendingReadVersionWinsConcurrentInvalidSetter(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	window := &heldReadVersionWindow{armed: make(chan struct{})}
	batcher := &grvBatcher{priority: grvPriorityDefault, inFlight: 1, batchTime: time.Hour, window: window}
	db := &database{}
	db.grvBatchers[grvBatcherDefault] = batcher
	tx := &Transaction{db: db}
	defer tx.Cancel()
	done := make(chan grvResult, 1)
	go func() { rv, err := tx.GetReadVersion(ctx); done <- grvResult{version: rv, err: err} }()
	select {
	case <-window.armed:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	batcher.mu.Lock()
	queued := append([]grvRequest(nil), batcher.pending...)
	batcher.mu.Unlock()
	if len(queued) != 1 {
		t.Fatalf("queued requests=%d", len(queued))
	}
	tx.readVersionMu.Lock()
	pending := tx.pendingReadVersion
	tx.readVersionMu.Unlock()
	if pending == nil {
		t.Fatal("getter did not publish pending GRV")
	}
	tx.SetReadVersion(0)
	requireReadVersionCode(t, tx, 2010)
	tx.readVersionMu.Lock()
	preserved := tx.pendingReadVersion == pending && !tx.hasReadVersion && !tx.userSetReadVersion
	tx.readVersionMu.Unlock()
	if !preserved {
		t.Fatal("setter replaced pending GRV")
	}
	queued[0].reply <- grvResult{version: 777, instant: time.Unix(1000, 0)}
	select {
	case result := <-done:
		if result.err != nil || result.version != 777 {
			t.Fatalf("admitted getter=%d,%v", result.version, result.err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if tx.pendingReadVersion != nil || !tx.hasReadVersion || tx.userSetReadVersion || tx.readVersion != 777 || pending.err != nil || pending.version != 777 {
		t.Fatal("getter did not preserve its successful pending result")
	}
	requireReadVersionCode(t, tx, 2010)
}

func TestReadVersionConcurrentRejectedSetterLoop(t *testing.T) {
	t.Parallel()
	for i := 0; i < 200; i++ {
		tx := &Transaction{}
		tx.SetReadVersion(123)
		start, setter := make(chan struct{}), make(chan struct{})
		go func() { <-start; tx.SetReadVersion(int64(i % 2)); close(setter) }()
		close(start)
		rv, err := tx.GetReadVersion(context.Background())
		<-setter
		if !((err == nil && rv == 123) || admissionErrorCode(err) == 2010) {
			t.Fatalf("nonserial getter=%d,%v", rv, err)
		}
		requireReadVersionCode(t, tx, 2010)
		if tx.readVersion != 123 || !tx.userSetReadVersion || tx.pendingReadVersion != nil {
			t.Fatal("rejection changed accepted pin")
		}
		tx.Cancel()
	}
}

func TestReadVersionAdmissionKeepsNestedLeaseDeferredEntry(t *testing.T) {
	t.Parallel()
	for _, reacquire := range []bool{false, true} {
		name := "borrow"
		if reacquire {
			name = "reacquire"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tx := &Transaction{}
			defer tx.Cancel()
			tx.SetReadVersion(42)
			ctx, release := tx.opContext(context.Background())
			defer release()
			if reacquire {
				tx.readOperation(ctx).lease.release()
			}
			// The admitted operation was clean. A later void-operation error
			// gates the next operation, not nested work of the admitted one.
			tx.Atomic(MutationType(1), []byte(t.Name()), []byte("value"))
			version, err := tx.GetReadVersion(ctx)
			if err != nil || version != 42 {
				t.Errorf("late poison rewrote admitted read: version=%d error=%v, want 42/nil", version, err)
			}
			if _, err := tx.GetReadVersion(context.Background()); admissionErrorCode(err) != 2018 {
				t.Fatalf("new operation did not observe deferred error: %v, want 2018", err)
			}
		})
	}
}
