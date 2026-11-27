package sqldriver

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"fdb.dev/internal/dst"
	"fdb.dev/internal/relational/relapi"
	"fdb.dev/internal/relational/simgate"
	"fdb.dev/internal/simfdb"
	"fdb.dev/pkg/fdbgo/fdb"
)

func simGateBackend(seed uint64) *simgate.Backend {
	env := dst.NewSim(seed)
	env.Buggify = dst.DisabledBuggifier()
	return simgate.New(simfdb.New(env))
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
	t.Cleanup(func() { _ = c.Close() })
	if _, err := c.Connect(context.Background()); err == nil {
		t.Fatal("Connect with no cluster file succeeded")
	}
	gate := simGateBackend(601)
	c.openBackend = func(string) (fdb.BackendDatabase, error) { return gate, nil }
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
	gate := simGateBackend(603)
	c := connectorFor(t, clusterFile)
	c.openBackend = func(string) (fdb.BackendDatabase, error) { return gate, nil }
	t.Cleanup(func() { _ = c.Close() })

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
// closes any backend opened by the failed attempt, and the next Connect
// initializes again with a fresh backend.
func TestConnector_PanickingInitializationIsRetried(t *testing.T) {
	t.Parallel()
	clusterFile := filepath.Join(t.TempDir(), "panicking.cluster")
	gate1 := simGateBackend(605)
	gate1.PanicOnce(simgate.Read)
	b1 := &closeCountingBackend{BackendDatabase: gate1}
	b2 := &closeCountingBackend{BackendDatabase: simGateBackend(606)}
	var opens atomic.Int32
	c := connectorFor(t, clusterFile)
	c.openBackend = func(string) (fdb.BackendDatabase, error) {
		if opens.Add(1) == 1 {
			return b1, nil
		}
		return b2, nil
	}
	_, err := c.Connect(context.Background())
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Code != api.ErrCodeInternalError {
		t.Fatalf("Connect whose initialization panicked = %v, want XX000", err)
	}
	if got := b1.closes.Load(); got != 1 {
		t.Fatalf("panicking attempt backend closed %d times, want 1 (leaked)", got)
	}
	conn, err := c.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect after the panicking initialization: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if got := b2.closes.Load(); got != 0 {
		t.Fatalf("retried backend closed %d times before Connector.Close, want 0", got)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Connector.Close: %v", err)
	}
	if got := b2.closes.Load(); got != 1 {
		t.Fatalf("retried backend closed %d times after Connector.Close, want 1", got)
	}
	if got := b1.closes.Load(); got != 1 {
		t.Fatalf("first backend closed %d times total, want 1", got)
	}
}

// Driver.Open closes the owned connector and its opened backend if Connect fails
// after opening the backend (e.g. a panic during stored-query warm-up).
func TestDriverOpen_ConnectFailureClosesOwnedConnector(t *testing.T) {
	t.Parallel()
	gate := simGateBackend(607)
	gate.PanicOnce(simgate.Read)
	b := &closeCountingBackend{BackendDatabase: gate}
	d := &Driver{openBackend: func(string) (fdb.BackendDatabase, error) { return b, nil }}
	if _, err := d.Open("fdbsql:///FRL/OPENFAIL"); err == nil {
		t.Fatal("Driver.Open succeeded when initialization panicked, want error")
	}
	if got := b.closes.Load(); got != 1 {
		t.Fatalf("backend closed %d times after Driver.Open Connect failure, want 1", got)
	}
}

// Concurrent Connect (including a cancelled caller leaving a detached init
// goroutine running) and Close must not race on planCache or ownedBackend, and
// must close the opened backend exactly once.
func TestConnector_ConcurrentConnectAndCloseWithDetachedInit(t *testing.T) {
	t.Parallel()
	for i := 0; i < 8; i++ {
		clusterFile := filepath.Join(t.TempDir(), "race.cluster")
		gate := simGateBackend(uint64(700 + i))
		b := &closeCountingBackend{BackendDatabase: gate}
		c := connectorFor(t, clusterFile)
		c.openBackend = func(string) (fdb.BackendDatabase, error) { return b, nil }

		read := gate.BlockNextRead()
		ctx, cancel := context.WithCancel(context.Background())
		firstDone := make(chan error, 1)
		go func() {
			_, err := c.Connect(ctx)
			firstDone <- err
		}()
		<-read.Entered()

		// Capture the active initAttempt so we can wait for the detached goroutine
		// after releasing the gate.
		c.init.mu.Lock()
		attempt := c.init.running
		c.init.mu.Unlock()
		if attempt == nil {
			t.Fatal("expected active initAttempt while gate read is blocked")
		}

		cancel()
		if err := <-firstDone; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled Connect = %v, want context.Canceled", err)
		}

		var wg sync.WaitGroup
		wg.Add(3)
		go func() {
			defer wg.Done()
			_ = c.Close()
		}()
		go func() {
			defer wg.Done()
			if conn, err := c.Connect(context.Background()); err == nil {
				_ = conn.Close()
			}
		}()
		go func() {
			defer wg.Done()
			read.Release()
		}()
		wg.Wait()
		<-attempt.finished

		if got := b.closes.Load(); got != 1 {
			t.Fatalf("iteration %d: backend closed %d times, want 1", i, got)
		}
		var ae *api.Error
		if _, err := c.Connect(context.Background()); !errors.As(err, &ae) || ae.Code != api.ErrCodeConnectionDoesNotExist {
			t.Fatalf("iteration %d: Connect after Close = %v, want ErrCodeConnectionDoesNotExist", i, err)
		}
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
