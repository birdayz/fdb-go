package sqldriver

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/internal/simgate"
	"fdb.dev/pkg/simfdb"
)

func simBackend(seed uint64) (*recordlayer.FDBDatabase, *simgate.Backend) {
	env := dst.NewSim(seed)
	env.Buggify = dst.DisabledBuggifier()
	gate := simgate.New(simfdb.New(env))
	return recordlayer.NewFDBDatabaseWithBackend(gate).SetEnv(env), gate
}

func connectorFor(t *testing.T, clusterFile string) *Connector {
	t.Helper()
	c, err := (&Driver{}).OpenConnector("fdbsql:///FRL/RETRYDB?cluster_file=" + clusterFile)
	if err != nil {
		t.Fatal(err)
	}
	return c.(*Connector)
}

// A failed initialization is not kept: the Connect after a failed FDB open
// tries again and succeeds once the cluster is reachable.
func TestConnector_RetriesFailedInitialization(t *testing.T) {
	t.Parallel()
	clusterFile := filepath.Join(t.TempDir(), "absent.cluster")
	c := connectorFor(t, clusterFile)
	if _, err := c.Connect(context.Background()); err == nil {
		t.Fatal("Connect with no cluster file succeeded")
	}
	db, _ := simBackend(601)
	t.Cleanup(RegisterBackend(clusterFile, db))
	conn, err := c.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect after the cluster became reachable: %v (the first failure was kept)", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
}

// A Connect whose context ends while the connector initializes returns its
// context error; the initialization goes on, and the next Connect uses it.
func TestConnector_CallerContextDoesNotCutInitializationShort(t *testing.T) {
	t.Parallel()
	clusterFile := filepath.Join(t.TempDir(), "gated.cluster")
	db, gate := simBackend(603)
	t.Cleanup(RegisterBackend(clusterFile, db))
	c := connectorFor(t, clusterFile)

	ctx, cancel := context.WithCancel(context.Background())
	read := gate.BlockNextRead()
	done := make(chan error, 1)
	go func() {
		_, err := c.Connect(ctx)
		done <- err
	}()
	<-read.Entered()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Connect whose context ended during initialization = %v, want context.Canceled", err)
	}
	read.Release()
	conn, err := c.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect after the first caller left: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if c.warmUp.Load() == nil {
		t.Fatal("the initialization published no warm-up")
	}
}

// A panic while the connector initializes is the internal error, not a crash,
// and the next Connect initializes again.
func TestConnector_PanickingInitializationIsRetried(t *testing.T) {
	t.Parallel()
	clusterFile := filepath.Join(t.TempDir(), "panicking.cluster")
	db, gate := simBackend(605)
	t.Cleanup(RegisterBackend(clusterFile, db))
	c := connectorFor(t, clusterFile)
	gate.PanicOnce(simgate.Read)
	_, err := c.Connect(context.Background())
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Code != api.ErrCodeInternalError {
		t.Fatalf("Connect whose initialization panicked = %v, want XX000", err)
	}
	conn, err := c.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect after the panicking initialization: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
}

// waitingCtx reports when initFlight.do evaluates its Done channel, which it
// does only in the select that waits for a running attempt.
type waitingCtx struct {
	context.Context
	once    sync.Once
	waiting chan struct{}
}

func newWaitingCtx() *waitingCtx {
	return &waitingCtx{Context: context.Background(), waiting: make(chan struct{})}
}

func (c *waitingCtx) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

// One attempt serves every concurrent caller; success is kept.
func TestInitFlight_ConcurrentCallersShareOneAttempt(t *testing.T) {
	t.Parallel()
	var f initFlight
	var calls atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	init := func(context.Context) error {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return nil
	}
	errs := make(chan error, 8)
	go func() { errs <- f.do(context.Background(), init) }()
	<-entered
	for i := 0; i < 7; i++ {
		ctx := newWaitingCtx()
		go func() { errs <- f.do(ctx, init) }()
		select {
		case <-ctx.waiting: // this caller is blocked on the running attempt
		case err := <-errs:
			t.Fatalf("a caller returned (%v) without waiting for the running attempt", err)
		}
	}
	close(release)
	for i := 0; i < 8; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	if err := f.do(context.Background(), init); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("initialization ran %d times, want once", n)
	}
}

// A waiter whose own context ends while it waits stops waiting; the running
// attempt goes on.
func TestInitFlight_WaiterLeavesOnItsContext(t *testing.T) {
	t.Parallel()
	var f initFlight
	entered, release := make(chan struct{}), make(chan struct{})
	first := make(chan error, 1)
	go func() {
		first <- f.do(context.Background(), func(context.Context) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	cancellable, cancel := context.WithCancel(context.Background())
	ctx := newWaitingCtx()
	ctx.Context = cancellable
	waiter := make(chan error, 1)
	go func() {
		waiter <- f.do(ctx, func(context.Context) error {
			t.Error("a waiter ran its own initialization while another was running")
			return nil
		})
	}()
	select {
	case <-ctx.waiting:
	case err := <-waiter:
		t.Fatalf("the waiter returned (%v) without waiting for the running attempt", err)
	}
	cancel()
	if err := <-waiter; !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter whose context ended = %v, want context.Canceled", err)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
}

// The caller that started an attempt may leave on its context too: the
// attempt is detached from it, finishes, and its success is kept.
func TestInitFlight_StartingCallerLeavesAttemptRuns(t *testing.T) {
	t.Parallel()
	var f initFlight
	var calls atomic.Int32
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	init := func(ctx context.Context) error {
		calls.Add(1)
		close(entered)
		<-release
		defer close(finished)
		return ctx.Err() // the attempt's context does not end with its caller's
	}
	ctx, cancel := context.WithCancel(context.Background())
	starter := make(chan error, 1)
	go func() { starter <- f.do(ctx, init) }()
	<-entered
	cancel()
	if err := <-starter; !errors.Is(err, context.Canceled) {
		t.Fatalf("starting caller whose context ended = %v, want context.Canceled", err)
	}
	close(release)
	<-finished
	if err := f.do(context.Background(), init); err != nil {
		t.Fatalf("caller after the attempt finished: %v", err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("initialization ran %d times, want once", n)
	}
}

// A failed or panicking attempt fails the callers waiting on it; the next
// caller starts a new attempt.
func TestInitFlight_FailureReachesItsWaitersAndIsRetried(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"error", "panic"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			var f initFlight
			var calls atomic.Int32
			entered, release := make(chan struct{}), make(chan struct{})
			boom := errors.New("transient open failure")
			init := func(context.Context) error {
				if calls.Add(1) == 1 {
					close(entered)
					<-release
					if mode == "panic" {
						panic("initialization")
					}
					return boom
				}
				return nil
			}
			first := make(chan error, 1)
			go func() { first <- f.do(context.Background(), init) }()
			<-entered
			second := make(chan error, 1)
			ctx := newWaitingCtx()
			go func() { second <- f.do(ctx, init) }()
			select {
			case <-ctx.waiting:
			case err := <-second:
				t.Fatalf("the second caller returned (%v) without waiting for the running attempt", err)
			}
			close(release)
			for name, ch := range map[string]chan error{"starting caller": first, "waiter": second} {
				err := <-ch
				var ae *api.Error
				switch {
				case mode == "error" && !errors.Is(err, boom):
					t.Fatalf("%s got %v, want the attempt's failure", name, err)
				case mode == "panic" && (!errors.As(err, &ae) || ae.Code != api.ErrCodeInternalError):
					t.Fatalf("%s got %v, want the recovered panic (XX000)", name, err)
				}
			}
			if err := f.do(context.Background(), init); err != nil {
				t.Fatalf("the next caller got %v, want its own successful attempt", err)
			}
			if n := calls.Load(); n != 2 {
				t.Fatalf("initialization ran %d times, want 2", n)
			}
		})
	}
}
