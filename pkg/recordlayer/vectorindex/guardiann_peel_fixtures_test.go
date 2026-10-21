package vectorindex

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/recordlayer"

	. "github.com/onsi/ginkgo/v2"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/vectorcodec"
	. "github.com/onsi/gomega"
)

type peelAttemptKey struct {
	callID    uint64
	execution int
}

type peelAttemptTrace struct {
	call             recordlayer.AttemptCall
	phase            string
	callbackDuration time.Duration
	commitDuration   time.Duration
	commitCalled     bool
	callbackErr      error
	commitErr        error
	onErrorErr       error
	err              error
}

// The callback includes record-layer setup and precommit; commit is timed only
// around the explicit Commit().Get(), never inferred from the callback's return.
func measurePeelAttempt(ctx context.Context, now func() time.Time, callback func() (any, error), commit func() error) (any, peelAttemptTrace) {
	var trace peelAttemptTrace
	if trace.err = ctx.Err(); trace.err != nil {
		return nil, trace
	}
	start := now()
	result, err := func() (result any, err error) {
		defer func() {
			if value := recover(); value != nil {
				if failure, ok := value.(error); ok {
					err = failure
				} else {
					panic(value)
				}
			}
		}()
		return callback()
	}()
	trace.callbackDuration = now().Sub(start)
	trace.callbackErr = err
	if err != nil {
		trace.err = err
		return nil, trace
	}
	if trace.err = ctx.Err(); trace.err != nil {
		return nil, trace
	}
	trace.commitCalled = true
	start = now()
	trace.commitErr = commit()
	trace.commitDuration = now().Sub(start)
	trace.err = trace.commitErr
	if trace.err != nil {
		return nil, trace
	}
	return result, trace
}

// Database.TransactCtx commits below the facade, so a transaction wrapper cannot
// time that commit. These Background-context fixtures instead own one attempt.
type peelAttemptTransactor struct {
	fdb.Database
	phase  string
	traces map[peelAttemptKey]peelAttemptTrace
}

var _ recordlayer.AttemptTransactor = (*peelAttemptTransactor)(nil)

func (t *peelAttemptTransactor) TransactAttempt(ctx context.Context, call recordlayer.AttemptCall, callback func(fdb.WritableTransaction) (any, error)) (any, error) {
	Expect(ctx.Done()).To(BeNil(), "manual fixture transactions require a non-cancellable context")
	tx, err := t.Database.CreateTransaction()
	if err != nil {
		t.traces[peelAttemptKey{call.CallID, call.Execution}] = peelAttemptTrace{call: call, phase: t.phase, err: err}
		return nil, err
	}
	defer tx.Cancel()
	result, trace := measurePeelAttempt(ctx, time.Now, func() (any, error) { return callback(tx) }, func() error { return tx.Commit().Get() })
	trace.err, trace.onErrorErr = peelTerminalError(trace.err, func(code fdb.Error) error { return tx.OnError(code).Get() })
	trace.call, trace.phase = call, t.phase
	t.traces[peelAttemptKey{call.CallID, call.Execution}] = trace
	return result, trace.err
}

func (t *peelAttemptTransactor) ReadTransactAttempt(context.Context, recordlayer.AttemptCall, func(fdb.ReadTransaction) (any, error)) (any, error) {
	panic("peel fixtures use writable Run, not read transactions")
}

// Match the facade's unconvertError and keepBodyError: preserve the body's
// chain only when terminal OnError returns its FDB code unchanged.
func peelTerminalError(raw error, onError func(fdb.Error) error) (error, error) {
	var code fdb.Error
	if !errors.As(raw, &code) {
		return raw, nil
	}
	terminal := onError(code)
	if terminal == nil {
		panic("peel fixture OnError(0) unexpectedly permitted a retry")
	}
	var returned fdb.Error
	if errors.As(terminal, &returned) && returned.Code == code.Code {
		return raw, terminal
	}
	return terminal, terminal
}

func peelErrorChain(err error) string {
	if err == nil {
		return "<nil>"
	}
	var chain []string
	var visit func(error)
	visit = func(err error) {
		if err == nil {
			return
		}
		code := ""
		var value fdb.Error
		var pointer *fdb.Error
		if errors.As(err, &value) {
			code = fmt.Sprintf(" [fdb_code=%d]", value.Code)
		} else if errors.As(err, &pointer) && pointer != nil {
			code = fmt.Sprintf(" [fdb_code=%d]", pointer.Code)
		}
		chain = append(chain, fmt.Sprintf("%T: %v%s", err, err, code))
		switch wrapped := err.(type) {
		case interface{ Unwrap() []error }:
			for _, child := range wrapped.Unwrap() {
				visit(child)
			}
		case interface{ Unwrap() error }:
			visit(wrapped.Unwrap())
		}
	}
	visit(err)
	return strings.Join(chain, " -> ")
}

func (t peelAttemptTrace) String() string {
	return fmt.Sprintf("call=%d execution=%d attempt=%d owner=%s route=%d phase=%s callback_including_precommit=%v commit_called=%t commit=%v callback_error={%s} commit_error={%s} terminal_on_error={%s} observed_error={%s}",
		t.call.CallID, t.call.Execution, t.call.Attempt, t.call.Owner, t.call.Route, t.phase,
		t.callbackDuration, t.commitCalled, t.commitDuration, peelErrorChain(t.callbackErr), peelErrorChain(t.commitErr), peelErrorChain(t.onErrorErr), peelErrorChain(t.err))
}

// The peel's acceptance fixtures (ws-d-design.md, "Acceptance fixtures"): real
// FDB, default KMeans knobs, HALF precision, no RaBitQ, the tight core with 50
// outliers at the shapes the admission is chosen to cover. FDB's 5 s
// transaction window is a time limit, so each split asserts through the
// attempt observer that it commits in ONE attempt: a 1007 hidden behind a retry
// would otherwise pass. The peel's own duration at these shapes is gated by
// TestGuardiannPeelPerformanceCriterion.
var _ = Describe("GuardiANN peel acceptance fixtures", func() {
	ctx := context.Background()

	outlierCluster := func(n, d int, seed int64) []gVector {
		rnd := rand.New(rand.NewSource(seed))
		out := make([]gVector, n)
		for i := range out {
			v := make([]float64, d)
			for j := range v {
				v[j] = rnd.NormFloat64() * 0.01
			}
			if i >= n-50 {
				v[(i*7)%d] += 50
			}
			half, err := vectorcodec.Deserialize(vectorcodec.SerializeHalf(v))
			Expect(err).NotTo(HaveOccurred())
			out[i] = gVector{data: half, typ: vectorcodec.TypeHalf}
		}
		return out
	}

	type fixture struct {
		db       *recordlayer.FDBDatabase
		g        func() *guardiann
		attempts map[uint64]int // executions per attempt-loop call
		tracer   *peelAttemptTransactor
	}
	setup := func(name string, d, iterations int) *fixture {
		cfg := defaultGuardiannConfig(d)
		cfg.kMeansMaxIterations = iterations
		cfg.deterministicRandomness = true
		Expect(cfg.validate()).To(Succeed())
		ss := specSubspace().Sub(name)
		tracer := &peelAttemptTransactor{Database: sharedRawDB, traces: map[peelAttemptKey]peelAttemptTrace{}}
		f := &fixture{
			db:       recordlayer.NewFDBDatabaseWithTransactor(tracer, sharedRawDB),
			g:        func() *guardiann { return newGuardiann(ss, cfg, nil, nil) },
			attempts: map[uint64]int{},
			tracer:   tracer,
		}
		f.db.SetAttemptObserver(func(call recordlayer.AttemptCall, err error) {
			key := peelAttemptKey{call.CallID, call.Execution}
			trace, ok := tracer.traces[key]
			Expect(ok).To(BeTrue(), "attempt observer has no timing for %+v", call)
			trace.err = err
			tracer.traces[key] = trace
			f.attempts[call.CallID]++
			GinkgoWriter.Printf("peel attempt: %s\n", trace.String())
		})
		return f
	}
	run := func(f *fixture, phase string, body func(g *guardiann, tx fdb.WritableTransaction) error) {
		f.tracer.phase = phase
		_, err := f.db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			return nil, body(f.g(), rtx.Transaction())
		})
		Expect(err).NotTo(HaveOccurred())
	}
	// insertAll inserts the vectors deferred (the split tasks queue), or
	// inline on the last one only, which then splits the full cluster.
	insertAll := func(f *fixture, vectors []gVector, inlineLast bool) {
		const batch = 200
		for start := 0; start < len(vectors); start += batch {
			end := min(start+batch, len(vectors))
			run(f, fmt.Sprintf("insert[%d:%d] inline_last=%t", start, end, inlineLast && end == len(vectors)), func(g *guardiann, tx fdb.WritableTransaction) error {
				for i := start; i < end; i++ {
					inline := inlineLast && i == len(vectors)-1
					if err := g.insert(tx, tuple.Tuple{int64(i)}, vectors[i], nil, inline); err != nil {
						return err
					}
				}
				return nil
			})
		}
	}
	drain := func(f *fixture) {
		for round := 0; round < 200; round++ {
			var n int
			run(f, fmt.Sprintf("deferred-drain[%d]", round), func(g *guardiann, tx fdb.WritableTransaction) error {
				var err error
				n, err = g.executeDeferredTasks(tx, 1, g.env.Now().Add(1<<40))
				return err
			})
			if n == 0 {
				return
			}
		}
		Fail("deferred tasks did not drain")
	}
	snapshot := func(f *fixture) guardiannSnapshot {
		var snap guardiannSnapshot
		run(f, "snapshot", func(g *guardiann, tx fdb.WritableTransaction) error {
			snap = readGuardiann(tx, g)
			return nil
		})
		return snap
	}
	expectOneAttemptEach := func(f *fixture) {
		Expect(f.attempts).NotTo(BeEmpty(), "no transaction attempts were observed")
		for call, n := range f.attempts {
			for execution := 0; execution < n; execution++ {
				trace, ok := f.tracer.traces[peelAttemptKey{call, execution}]
				Expect(ok).To(BeTrue(), "missing trace for call %d execution %d", call, execution)
				Expect(trace.err).NotTo(HaveOccurred(), "an attempt failed: %s", trace.String())
			}
			Expect(n).To(Equal(1), "call %d took %d attempts", call, n)
		}
	}
	expectSplit := func(f *fixture, n int) {
		snap := snapshot(f)
		Expect(snap.tasks).To(Equal(0))
		Expect(len(snap.clusters)).To(BeNumerically(">=", 2), "the peel split the over-max cluster")
		total := 0
		for id, m := range snap.clusters {
			Expect(m.numPrimary()).To(Equal(snap.primaries[id]))
			Expect(m.numPrimary()).To(BeNumerically("<=", defaultGuardiannConfig(1).primaryClusterMax))
			total += m.numPrimary()
		}
		Expect(total).To(Equal(n))
	}

	It("splits the d = 768 tight cluster at the hard cap by a deferred drain, each transaction in one attempt", func() {
		const n, d = 2000, 768
		f := setup("d768-deferred", d, 8)
		insertAll(f, outlierCluster(n, d, 1), false)
		start := time.Now()
		drain(f)
		GinkgoWriter.Printf("d=768 n=2000 deferred drain: %v\n", time.Since(start))
		expectOneAttemptEach(f)
		expectSplit(f, n)
	})

	It("splits the d = 768 tight cluster at the hard cap by an inline insert, in one attempt", func() {
		const n, d = 2000, 768
		f := setup("d768-inline", d, 8)
		insertAll(f, outlierCluster(n, d, 2), true)
		drain(f)
		expectOneAttemptEach(f)
		expectSplit(f, n)
	})

	It("splits the d = 2048 first over-max cluster by a deferred drain, in one attempt", func() {
		const n, d = 1001, 2048
		f := setup("d2048-deferred", d, 8)
		insertAll(f, outlierCluster(n, d, 3), false)
		start := time.Now()
		drain(f)
		GinkgoWriter.Printf("d=2048 n=1001 deferred drain: %v\n", time.Since(start))
		expectOneAttemptEach(f)
		expectSplit(f, n)
	})

	It("refuses the peel at twice the KMeans iterations (the knob factor) and reconciles the cluster at the hard cap in place", func() {
		const n, d = 2000, 768
		f := setup("d768-i16", d, 16)
		insertAll(f, outlierCluster(n, d, 4), false)
		drain(f)
		expectOneAttemptEach(f)
		snap := snapshot(f)
		Expect(snap.tasks).To(Equal(0))
		Expect(snap.clusters).To(HaveLen(1), "admission refused the peel, so the cluster stays whole")
		for id, m := range snap.clusters {
			Expect(m.numPrimary()).To(Equal(n))
			Expect(snap.primaries[id]).To(Equal(n))
		}
	})
})

func TestMeasurePeelAttempt(t *testing.T) {
	t.Parallel()
	failure := fmt.Errorf("attempt cause: %w", fdb.Error{Code: 1007})
	for _, tc := range []struct {
		name                   string
		callbackErr, commitErr error
		panicError             bool
		cancelBefore           bool
		cancelInCallback       bool
	}{
		{name: "success"},
		{name: "callback failure", callbackErr: failure},
		{name: "callback error panic", callbackErr: failure, panicError: true},
		{name: "commit failure", commitErr: failure},
		{name: "cancelled before callback", cancelBefore: true},
		{name: "cancelled before commit", cancelInCallback: true},
		{name: "callback error precedes cancellation", callbackErr: failure, cancelInCallback: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelBefore {
				cancel()
			}
			clock := time.Unix(0, 0)
			callbackCalled, commitCalled := false, false
			result, trace := measurePeelAttempt(ctx, func() time.Time { return clock }, func() (any, error) {
				callbackCalled = true
				clock = clock.Add(7 * time.Millisecond)
				if tc.cancelInCallback {
					cancel()
				}
				if tc.panicError {
					panic(tc.callbackErr)
				}
				return "result", tc.callbackErr
			}, func() error {
				commitCalled = true
				clock = clock.Add(13 * time.Millisecond)
				return tc.commitErr
			})
			wantCallback := !tc.cancelBefore
			wantCommit := wantCallback && tc.callbackErr == nil && !tc.cancelInCallback
			if callbackCalled != wantCallback || commitCalled != wantCommit || trace.commitCalled != wantCommit {
				t.Fatalf("callback=%t commit=%t trace.commitCalled=%t, want callback=%t commit=%t", callbackCalled, commitCalled, trace.commitCalled, wantCallback, wantCommit)
			}
			var wantCallbackTime, wantCommitTime time.Duration
			if wantCallback {
				wantCallbackTime = 7 * time.Millisecond
			}
			if wantCommit {
				wantCommitTime = 13 * time.Millisecond
			}
			if trace.callbackDuration != wantCallbackTime || trace.commitDuration != wantCommitTime {
				t.Fatalf("callback=%v commit=%v, want %v and %v", trace.callbackDuration, trace.commitDuration, wantCallbackTime, wantCommitTime)
			}
			wantErr := tc.callbackErr
			if tc.cancelBefore {
				wantErr = context.Canceled
			} else if wantErr == nil {
				if tc.cancelInCallback {
					wantErr = context.Canceled
				} else {
					wantErr = tc.commitErr
				}
			}
			if trace.err != wantErr || (wantCallback && trace.callbackErr != tc.callbackErr) || (wantCommit && trace.commitErr != tc.commitErr) {
				t.Fatalf("error identity changed: trace=%+v want=%v", trace, wantErr)
			}
			if wantErr == nil && result != "result" || wantErr != nil && result != nil {
				t.Fatalf("result=%v error=%v", result, trace.err)
			}
		})
	}
}

func TestMeasurePeelAttemptNonErrorPanic(t *testing.T) {
	t.Parallel()
	defer func() {
		if got := recover(); got != "non-error panic" {
			t.Fatalf("panic = %v, want the original non-error value", got)
		}
	}()
	_, _ = measurePeelAttempt(context.Background(), time.Now, func() (any, error) {
		panic("non-error panic")
	}, func() error {
		t.Fatal("commit ran after a panic")
		return nil
	})
}

func TestPeelTerminalError(t *testing.T) {
	t.Parallel()
	body := fmt.Errorf("body chain: %w", fdb.Error{Code: 1007})
	plain := errors.New("application failure")
	for _, tc := range []struct {
		name                    string
		raw, terminal, expected error
		called                  bool
	}{
		{name: "success"},
		{name: "application error", raw: plain, expected: plain},
		{name: "same code restores chain", raw: body, terminal: fdb.Error{Code: 1007}, expected: body, called: true},
		{name: "timeout replaces chain", raw: body, terminal: fdb.Error{Code: 1031}, expected: fdb.Error{Code: 1031}, called: true},
		{name: "cancellation replaces chain", raw: body, terminal: fdb.Error{Code: 1025}, expected: fdb.Error{Code: 1025}, called: true},
		{name: "non-FDB verdict replaces chain", raw: body, terminal: plain, expected: plain, called: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			called := false
			got, terminal := peelTerminalError(tc.raw, func(code fdb.Error) error {
				called = true
				if code.Code != 1007 {
					t.Fatalf("OnError code=%d, want 1007", code.Code)
				}
				return tc.terminal
			})
			if called != tc.called || got != tc.expected || terminal != tc.terminal {
				t.Fatalf("called=%t result=%v terminal=%v; want %t, %v, %v", called, got, terminal, tc.called, tc.expected, tc.terminal)
			}
		})
	}
}

func TestPeelTerminalErrorRejectsRetry(t *testing.T) {
	t.Parallel()
	defer func() {
		if got := recover(); got != "peel fixture OnError(0) unexpectedly permitted a retry" {
			t.Fatalf("unexpected retry guard panic: %v", got)
		}
	}()
	_, _ = peelTerminalError(fdb.Error{Code: 1007}, func(fdb.Error) error { return nil })
}

func TestPeelErrorChain(t *testing.T) {
	t.Parallel()
	if got := peelErrorChain(nil); got != "<nil>" {
		t.Fatalf("nil error chain = %q", got)
	}
	cause := errors.New("non-FDB cause")
	if got := peelErrorChain(cause); strings.Contains(got, "fdb_code=") {
		t.Fatalf("non-FDB error acquired a code: %q", got)
	}
	err := fmt.Errorf("outer: %w", errors.Join(
		fmt.Errorf("first branch: %w", fdb.Error{Code: 1020}),
		errors.Join(errors.New("second branch:"), &fdb.Error{Code: 1007}),
		fmt.Errorf("third branch: %w", cause),
	))
	got := peelErrorChain(err)
	for _, want := range []string{"outer:", "first branch:", "second branch:", "third branch:", "fdb_code=1020", "fdb_code=1007", "*fdb.Error:", "fdb.Error:", "non-FDB cause"} {
		if !strings.Contains(got, want) {
			t.Errorf("chain %q does not contain %q", got, want)
		}
	}
	if !errors.Is(err, cause) {
		t.Fatal("diagnostics changed the original error chain")
	}
}
