package client

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/wire"
)

func requireSignalValue(t *testing.T, value []byte, err error, code int) {
	t.Helper()
	if code != 0 {
		if actual, ok := fdbErrorCode(err); !ok || actual != code || value != nil {
			t.Fatalf("stamp=%x, err=%v; want code %d", value, err, code)
		}
	} else if err != nil || !bytes.Equal(value, []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}) {
		t.Fatalf("stamp=%x, err=%v; want 0102030405060708090a", value, err)
	}
}

func TestVersionstampSignalUnobservedCommit(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	defer tx.Cancel()
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	c := tx.lastVersionstamp
	if c == nil || !c.ready || c.done != nil || c.retired || tx.committedVersion != -1 || !tx.hasCommitted {
		t.Fatal("unobserved no-write Commit lost its selected identity or allocated a signal")
	}
	if tx.readLife != nil || tx.readVersionState != nil {
		t.Fatal("Commit eagerly admitted a successor")
	}
	value, err := tx.GetVersionstamp()
	requireSignalValue(t, value, err, 2021)
	value, pending, err := tx.GetVersionstampPending(context.Background())
	if pending != nil {
		t.Fatal("selected no-write result returned a pending handle")
	}
	requireSignalValue(t, value, err, 2021)
	if c.done != nil || tx.lastVersionstamp != c {
		t.Fatal("late getters allocated a signal or replaced the selected identity")
	}
}

func TestVersionstampSignalSelection(t *testing.T) {
	t.Parallel()
	for _, observed := range []bool{false, true} {
		for _, tc := range []struct {
			name string
			code int
		}{
			{"success", 0},
			{"native-error", 2020},
			{"no-write", 2021},
			{"cancel", 1025},
			{"timeout-cause", 1031},
		} {
			name := "selected-first/" + tc.name
			if observed {
				name = "observed-first/" + tc.name
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				tx := &Transaction{}
				defer tx.Cancel()
				c := registryProducer(t, tx).completion
				if c.done != nil {
					t.Fatal("unobserved producer allocated a signal")
				}
				var first, sibling *PendingVersionstamp
				var signal chan struct{}
				if observed {
					first, sibling = registryStamp(t, tx), registryStamp(t, tx)
					signal = c.done
					if first.completion != c || sibling.completion != c || signal == nil || signal == closedDone || first.done != signal || sibling.done != signal {
						t.Fatal("pending observers did not register a private shared signal")
					}
					requireRegistryPending(t, first)
					requireRegistryPending(t, sibling)
				}
				switch tc.code {
				case 0:
					c.finishNative(commitOutcome{version: 0x0102030405060708, batchID: 0x090a}, nil)
				case 2020:
					c.finishNative(commitOutcome{}, &wire.FDBError{Code: 2101})
				case 2021:
					c.finishNoWrite()
				case 1025:
					tx.Cancel()
				case 1031:
					tx.failCapturedIncarnation(c.inc, &wire.FDBError{Code: 1031})
				}
				if !c.ready || c.done != signal {
					t.Fatal("selection lost readiness or replaced/allocated the signal")
				}
				if observed {
					want := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
					requireRegistryReady(t, first, want, tc.code)
					requireRegistryReady(t, sibling, want, tc.code)
					if tc.code == 0 {
						first.value[0] ^= 0xff
						if !bytes.Equal(sibling.value, want) {
							t.Fatal("sibling versionstamp aliases another handle's result")
						}
					}
				}
				value, pending, err := tx.GetVersionstampPending(context.Background())
				if pending != nil {
					t.Fatal("late getter returned pending after selection")
				}
				requireSignalValue(t, value, err, tc.code)
				value, err = tx.GetVersionstamp()
				requireSignalValue(t, value, err, tc.code)
				if c.done != signal {
					t.Fatal("late getter changed the registered signal")
				}
			})
		}
	}
}

func TestVersionstampSignalConcurrentObservers(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	defer tx.Cancel()
	producer := registryProducer(t, tx)
	type result struct {
		pending *PendingVersionstamp
		err     error
	}
	start, results := make(chan struct{}), make(chan result, 8)
	for range cap(results) {
		go func() {
			<-start
			_, pending, err := tx.GetVersionstampPending(context.Background())
			results <- result{pending, err}
		}()
	}
	close(start)
	var pending []*PendingVersionstamp
	for range cap(results) {
		r := <-results
		if r.err != nil || r.pending == nil {
			t.Fatalf("concurrent observer: %v", r.err)
		}
		t.Cleanup(r.pending.cleanup)
		if r.pending.completion != producer.completion {
			t.Fatal("concurrent getter rebound the completion")
		}
		requireRegistryPending(t, r.pending)
		pending = append(pending, r.pending)
	}
	signal := producer.completion.done
	if signal == nil || signal == closedDone {
		t.Fatal("concurrent observers did not register a private signal")
	}
	for _, p := range pending {
		if p.done != signal {
			t.Fatal("concurrent observers captured different signals")
		}
	}
	producer.completion.finishNative(commitOutcome{version: 0x0102030405060708, batchID: 0x090a}, nil)
	for _, p := range pending {
		requireRegistryReady(t, p, []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 0)
	}
	if producer.completion.done != signal {
		t.Fatal("selection changed an observed signal")
	}
}

func TestVersionstampSignalExcludedInterruption(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"caller-cancel", context.Canceled},
		{"caller-deadline", context.DeadlineExceeded},
		{"actor-cancel", &wire.FDBError{Code: 1101}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tx := &Transaction{}
			defer tx.Cancel()
			p := registryStamp(t, tx)
			p.completion.finishNative(commitOutcome{}, tc.err)
			requireRegistryPending(t, p)
			tx.Cancel()
			requireRegistryReady(t, p, nil, 1025)
		})
	}
}

func TestVersionstampSignalCallerIsolation(t *testing.T) {
	t.Parallel()
	for _, retire := range []bool{false, true} {
		name := "native-success"
		if retire {
			name = "retired"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tx := &Transaction{}
			defer tx.Cancel()
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			_, local, err := tx.GetVersionstampPending(parent)
			if err != nil || local == nil {
				t.Fatalf("local pending handle: %v", err)
			}
			t.Cleanup(local.cleanup)
			_, ready, err := tx.GetVersionstampPending(parent)
			if err != nil || ready == nil {
				t.Fatalf("ready pending handle: %v", err)
			}
			t.Cleanup(ready.cleanup)
			sibling := registryStamp(t, tx)
			cancel()
			if _, err := local.Resolve(); !errors.Is(err, context.Canceled) {
				t.Fatalf("caller-local cancellation: %v", err)
			}
			requireRegistryPending(t, sibling)
			if retire {
				tx.failCapturedIncarnation(sibling.completion.inc, &wire.FDBError{Code: 1031})
				if _, err := ready.Resolve(); !errors.Is(err, context.Canceled) {
					t.Fatalf("retired outcome lost caller-first cancellation: %v", err)
				}
				requireRegistryReady(t, sibling, nil, 1031)
			} else {
				sibling.completion.finishNative(commitOutcome{version: 0x0102030405060708, batchID: 0x090a}, nil)
				for _, p := range []*PendingVersionstamp{ready, sibling} {
					requireRegistryReady(t, p, []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 0)
				}
			}
			if _, err := local.Resolve(); !errors.Is(err, context.Canceled) {
				t.Fatalf("memoized caller outcome changed: %v", err)
			}
		})
	}
}

func TestVersionstampSignalCancellationBeforeSelection(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	defer tx.Cancel()
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, local, err := tx.GetVersionstampPending(parent)
	if err != nil || local == nil {
		t.Fatalf("pending handle: %v", err)
	}
	t.Cleanup(local.cleanup)
	sibling := registryStamp(t, tx)
	enteredCleanup := make(chan struct{})
	cleanup := local.cleanup
	local.cleanup = func() { close(enteredCleanup); cleanup() }
	tx.readErrMu.Lock()
	locked := true
	defer func() {
		if locked {
			tx.readErrMu.Unlock()
		}
	}()
	cancel()
	result := make(chan error, 1)
	go func() { _, err := local.Resolve(); result <- err }()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	select {
	case <-enteredCleanup:
	case <-deadline.C:
		t.Fatal("cancelled resolver acquired the selection lock before fixing its outcome")
	}
	local.completion.finishNativeLocked(commitOutcome{version: 0x0102030405060708, batchID: 0x090a}, nil)
	tx.readErrMu.Unlock()
	locked = false
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("later native selection replaced fixed caller cancellation: %v", err)
		}
	case <-deadline.C:
		t.Fatal("cancelled resolver did not finish cleanup")
	}
	requireRegistryReady(t, sibling, []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 0)
}
