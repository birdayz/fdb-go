package client

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/wire"
)

func pendingStamp(t *testing.T, ctx context.Context, tx *Transaction) *PendingVersionstamp {
	t.Helper()
	value, pending, err := tx.GetVersionstampPending(ctx)
	if err != nil || value != nil || pending == nil {
		t.Fatalf("pending versionstamp: value=%x pending=%v err=%v", value, pending != nil, err)
	}
	t.Cleanup(pending.cleanup)
	return pending
}

func requireStampPending(t *testing.T, p *PendingVersionstamp) {
	t.Helper()
	select {
	case <-p.completion.done:
		t.Fatal("versionstamp selected before native completion or retirement")
	default:
	}
}

func TestVersionstampNativeCompletionSelection(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		err     error
		pending bool
	}{
		{"success", nil, false},
		{"native-failure", &wire.FDBError{Code: 2101}, false},
		{"literal-future-not-set", &wire.FDBError{Code: 2015}, false},
		{"actor-cancelled", &wire.FDBError{Code: 1101}, true},
		{"caller-cancelled", context.Canceled, true},
		{"caller-deadline", context.DeadlineExceeded, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			tx := newTestTx()
			defer tx.Cancel()
			p := pendingStamp(t, ctx, tx)
			p.completion.finishNative(commitOutcome{version: 0x0102030405060708, batchID: 0x090a}, tc.err)
			if tc.pending {
				requireStampPending(t, p)
				tx.Cancel()
				_, err := p.Resolve()
				requireCommitLifetimeCode(t, err, 1025, "retirement after excluded completion")
				return
			}
			// Selected native results survive all later retirement paths.
			tx.Cancel()
			tx.Reset()
			value, err := p.Resolve()
			if tc.err != nil {
				requireCommitLifetimeCode(t, err, 2020, "native failure is not Commit's error")
			} else if err != nil || !bytes.Equal(value, []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}) {
				t.Fatalf("immutable CommitID: %x, %v", value, err)
			}
			tx.readErrMu.Lock()
			n := len(p.completion.inc.versionstamps)
			tx.readErrMu.Unlock()
			if n != 0 {
				t.Fatalf("selected records retained: %d", n)
			}
		})
	}
}

func TestVersionstampCallerIsolationAndReadyResult(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx := newTestTx()
	defer tx.Cancel()
	caller, interrupt := context.WithCancel(ctx)
	defer interrupt()
	local := pendingStamp(t, caller, tx)
	sibling := pendingStamp(t, ctx, tx)
	ready := pendingStamp(t, caller, tx)
	interrupt()
	if _, err := local.Resolve(); !errors.Is(err, context.Canceled) {
		t.Fatalf("caller-local interruption: %v", err)
	}
	requireStampPending(t, sibling)
	sibling.completion.finishNative(commitOutcome{version: 42, batchID: 7}, nil)
	tx.Cancel()
	for _, p := range []*PendingVersionstamp{sibling, ready} {
		value, err := p.Resolve()
		if err != nil || !bytes.Equal(value, []byte{0, 0, 0, 0, 0, 0, 0, 42, 0, 7}) {
			t.Fatalf("selected result must outrank caller/retirement: %x, %v", value, err)
		}
	}
	if _, err := local.Resolve(); !errors.Is(err, context.Canceled) {
		t.Fatalf("already-returned caller interruption changed: %v", err)
	}
}

func TestVersionstampRetirementFirstCause(t *testing.T) {
	t.Parallel()
	for _, timeoutFirst := range []bool{false, true} {
		name := "cancel-first"
		if timeoutFirst {
			name = "timeout-first"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			tx := newTestTx()
			tx.SetTimeout(60000)
			first := pendingStamp(t, ctx, tx)
			producer := tx.PrepareCommit(ctx)
			defer producer.cleanup()
			secondProducer := tx.PrepareCommit(ctx)
			defer secondProducer.cleanup()
			second := pendingStamp(t, ctx, tx)
			tx.readErrMu.Lock()
			inc, gen := tx.readLife, tx.readLife.timerGen
			tx.readErrMu.Unlock()
			want := 1025
			if timeoutFirst {
				want = 1031
				tx.fireReadTimeout(inc, gen)
			}
			tx.Cancel()
			tx.fireReadTimeout(inc, gen)
			tx.Reset()
			defer tx.Cancel()
			for _, p := range []*PendingVersionstamp{first, second} {
				_, err := p.Resolve()
				requireCommitLifetimeCode(t, err, want, "first terminal cause after Reset")
			}
			if first.completion == second.completion {
				t.Fatal("overlapping producers share a completion")
			}
			if err := producer.Resolve(); fdbCodeOf(err) != want {
				t.Fatalf("old admitted producer: %v", err)
			}
			if err := secondProducer.Resolve(); fdbCodeOf(err) != want {
				t.Fatalf("second old admitted producer: %v", err)
			}
		})
	}
}

func TestVersionstampRejectedAdmissionDoesNotClaim(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx := newTestTx()
	p := pendingStamp(t, ctx, tx)
	tx.Atomic(MutationType(1), []byte("key"), []byte("value"))
	rejected := tx.PrepareCommit(ctx)
	if err := rejected.Resolve(); fdbCodeOf(err) != 2018 {
		t.Fatalf("admission: %v", err)
	}
	requireStampPending(t, p)
	if p.completion.claimed {
		t.Fatal("rejected Commit claimed the existing promise")
	}
	if _, fresh, err := tx.GetVersionstampPending(ctx); fresh != nil || fdbCodeOf(err) != 2018 {
		t.Fatalf("new poisoned stamp entry: pending=%v err=%v", fresh != nil, err)
	}
	tx.Cancel()
	_, err := p.Resolve()
	requireCommitLifetimeCode(t, err, 1025, "healthy old admission must not read later poison")
}

func TestVersionstampCommitPhaseClassification(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                  string
		setup                 func(*Transaction)
		commitCode, stampCode int
	}{
		{"late-poison", func(tx *Transaction) { tx.Atomic(MutationType(1), []byte("key"), nil) }, 2018, 0},
		{"mutation-validation", func(tx *Transaction) { tx.Set([]byte{0xff, 1}, []byte("value")) }, 2004, 0},
		{"native-size", func(tx *Transaction) { tx.SetSizeLimit(32); tx.Set([]byte("key"), make([]byte, 64)) }, 2101, 2020},
		{"no-write", func(*Transaction) {}, 0, 2021},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			tx := newTestTx()
			defer tx.Cancel()
			p := pendingStamp(t, ctx, tx)
			producer := tx.PrepareCommit(ctx)
			tc.setup(tx)
			err := producer.Resolve()
			if tc.commitCode == 0 {
				if err != nil {
					t.Fatalf("Commit: %v", err)
				}
			} else {
				requireCommitLifetimeCode(t, err, tc.commitCode, "Commit phase result")
			}
			if tc.stampCode == 0 {
				requireStampPending(t, p)
				tx.Cancel()
			}
			_, err = p.Resolve()
			want := tc.stampCode
			if want == 0 {
				want = 1025
			}
			requireCommitLifetimeCode(t, err, want, "versionstamp phase result")
		})
	}
}

func TestVersionstampProducerIdentity(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx := newTestTx()
	defer tx.Cancel()
	first := pendingStamp(t, ctx, tx)
	one := tx.PrepareCommit(ctx)
	defer one.cleanup()
	two := tx.PrepareCommit(ctx)
	defer two.cleanup()
	latest := pendingStamp(t, ctx, tx)
	if one.completion != first.completion || two.completion != latest.completion || one.completion == two.completion {
		t.Fatal("first producer must claim the unclaimed promise; next producer must own a distinct latest promise")
	}
	one.completion.finishNative(commitOutcome{version: 42}, nil)
	requireStampPending(t, latest)
	if _, err := tx.GetVersionstamp(); fdbCodeOf(err) != 2015 {
		t.Fatalf("latest pending synchronous getter: %v", err)
	}
	two.completion.finishNative(commitOutcome{}, &wire.FDBError{Code: 2015})
	_, err := latest.Resolve()
	requireCommitLifetimeCode(t, err, 2020, "2015 is a native error, not a pending discriminator")
	value, err := first.Resolve()
	if err != nil || !bytes.Equal(value, []byte{0, 0, 0, 0, 0, 0, 0, 42, 0, 0}) {
		t.Fatalf("first producer result replaced by second: %x, %v", value, err)
	}
}

// FuzzVersionstampSelection checks the first effective event against a small
// independent lifecycle model. It does not claim wire/cluster fuzz coverage;
// the real-FDB tests exercise dispatch and uncertainty-barrier integration.
func FuzzVersionstampSelection(f *testing.F) {
	for _, seed := range [][]byte{{0, 2}, {2, 0}, {1, 3}, {3, 1}, {4, 5, 0}, {6, 2}, {5, 4}} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, events []byte) {
		if len(events) > 64 {
			events = events[:64]
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		tx := newTestTx()
		defer tx.Cancel()
		p := pendingStamp(t, ctx, tx)
		selected, want := false, 0
		for _, event := range events {
			code, effective := 0, true
			switch event % 7 {
			case 0:
				p.completion.finishNative(commitOutcome{version: 42, batchID: 7}, nil)
			case 1:
				code = 2020
				p.completion.finishNative(commitOutcome{}, &wire.FDBError{Code: 2015})
			case 2:
				code = 1025
				tx.Cancel()
			case 3:
				code = 1031
				tx.failCapturedIncarnation(p.completion.inc, &wire.FDBError{Code: 1031})
			case 4:
				effective = false
				p.completion.finishNative(commitOutcome{}, context.DeadlineExceeded)
			case 5:
				effective = false
				p.completion.finishNative(commitOutcome{}, &wire.FDBError{Code: 1101})
			case 6:
				code = 2021
				p.completion.finishNoWrite()
			}
			if !selected && effective {
				selected, want = true, code
			}
		}
		tx.Cancel()
		if !selected {
			want = 1025
		}
		value, err := p.Resolve()
		if want != 0 {
			requireCommitLifetimeCode(t, err, want, "first effective lifecycle event")
		} else if err != nil || !bytes.Equal(value, []byte{0, 0, 0, 0, 0, 0, 0, 42, 0, 7}) {
			t.Fatalf("first selected CommitID: %x, %v", value, err)
		}
	})
}

func TestVersionstampRetiredCompletionPreservesCallerFirst(t *testing.T) {
	t.Parallel()
	tx := newTestTx()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	caller := pendingStamp(t, ctx, tx)
	sibling := pendingStamp(t, context.Background(), tx)
	tx.Cancel()
	cancel()
	if _, err := caller.Resolve(); !errors.Is(err, context.Canceled) {
		t.Fatalf("caller interruption lost to sealed retirement: %v", err)
	}
	_, err := sibling.Resolve()
	requireCommitLifetimeCode(t, err, 1025, "caller cannot overwrite shared retirement")
}
