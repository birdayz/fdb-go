package client

import (
	"bytes"
	"context"
	"testing"

	"fdb.dev/pkg/fdbgo/wire"
)

func registryStamp(t *testing.T, tx *Transaction) *PendingVersionstamp {
	t.Helper()
	value, pending, err := tx.GetVersionstampPending(context.Background())
	if value != nil || pending == nil || err != nil {
		t.Fatalf("pending stamp: value=%x, pending=%v, err=%v", value, pending != nil, err)
	}
	t.Cleanup(pending.cleanup)
	return pending
}

func registryProducer(t *testing.T, tx *Transaction) *PendingCommit {
	t.Helper()
	producer := tx.PrepareCommit(context.Background())
	t.Cleanup(producer.cleanup)
	if producer.err != nil || producer.completion == nil {
		t.Fatalf("producer admission: %v", producer.err)
	}
	return producer
}

func requireRegistry(t *testing.T, tx *Transaction, inc *readIncarnation, latest *versionstampCompletion, pending int, overflow ...*versionstampCompletion) {
	t.Helper()
	tx.readErrMu.Lock()
	defer tx.readErrMu.Unlock()
	if inc.versionstamp != latest || len(inc.versionstamps) != len(overflow) {
		t.Fatalf("registry: latest matches=%v, overflow=%d, want %d", inc.versionstamp == latest, len(inc.versionstamps), len(overflow))
	}
	n := len(inc.versionstamps)
	if latest != nil && !latest.ready {
		n++
	}
	if n != pending {
		t.Fatalf("outstanding completions=%d, want %d", n, pending)
	}
	for c := range inc.versionstamps {
		if c == latest || c.ready || c.inc != inc {
			t.Fatal("overflow contains latest, selected, or foreign completion")
		}
	}
	for _, c := range overflow {
		if _, ok := inc.versionstamps[c]; !ok {
			t.Fatal("displaced producer lost its strong registry reference")
		}
	}
}

func requireRegistryReady(t *testing.T, p *PendingVersionstamp, value []byte, code int) {
	t.Helper()
	select {
	case <-p.done:
	default:
		t.Fatal("versionstamp result was not selected")
	}
	got, err := p.Resolve()
	if code != 0 {
		if actual, ok := fdbErrorCode(err); !ok || actual != code || got != nil {
			t.Fatalf("stamp=%x, err=%v; want code %d", got, err, code)
		}
	} else if err != nil || !bytes.Equal(got, value) {
		t.Fatalf("stamp=%x, err=%v; want %x", got, err, value)
	}
}

func requireRegistryPending(t *testing.T, p *PendingVersionstamp) {
	t.Helper()
	select {
	case <-p.done:
		t.Fatal("pending producer was selected")
	default:
	}
}

func TestVersionstampRegistrySingleProducer(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	defer tx.Cancel()
	first, sibling := registryStamp(t, tx), registryStamp(t, tx)
	inc := first.completion.inc
	if first.completion != sibling.completion || first.completion.claimed {
		t.Fatal("getters must share one unclaimed completion")
	}
	requireRegistry(t, tx, inc, first.completion, 1)
	producer := registryProducer(t, tx)
	if producer.completion != first.completion || !first.completion.claimed || inc.versionstamps != nil {
		t.Fatal("first Commit did not claim the shared completion without overflow")
	}
	if err := producer.Resolve(); err != nil {
		t.Fatal(err)
	}
	requireRegistry(t, tx, inc, first.completion, 0)
	for _, p := range []*PendingVersionstamp{first, sibling} {
		requireRegistryReady(t, p, nil, 2021)
	}
	if version, err := tx.GetCommittedVersion(); err != nil || version != -1 || tx.lastVersionstamp != first.completion {
		t.Fatalf("no-write Commit lost its selected identity: version=%d, err=%v", version, err)
	}
}

func TestVersionstampRegistryOutOfOrder(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	defer tx.Cancel()
	first := registryStamp(t, tx)
	one, two := registryProducer(t, tx), registryProducer(t, tx)
	second := registryStamp(t, tx)
	three := registryProducer(t, tx)
	latest := registryStamp(t, tx)
	inc := first.completion.inc
	if one.completion != first.completion || two.completion != second.completion || three.completion != latest.completion {
		t.Fatal("getter/producer identity changed")
	}
	requireRegistry(t, tx, inc, latest.completion, 3, first.completion, second.completion)
	second.completion.finishNative(commitOutcome{version: 2, batchID: 7}, nil)
	requireRegistry(t, tx, inc, latest.completion, 2, first.completion)
	requireRegistryPending(t, first)
	requireRegistryPending(t, latest)
	requireRegistryReady(t, second, []byte{0, 0, 0, 0, 0, 0, 0, 2, 0, 7}, 0)
	first.completion.finishNative(commitOutcome{}, &wire.FDBError{Code: 2101})
	requireRegistry(t, tx, inc, latest.completion, 1)
	requireRegistryPending(t, latest)
	requireRegistryReady(t, first, nil, 2020)
	latest.completion.finishNative(commitOutcome{version: 3}, nil)
	requireRegistry(t, tx, inc, latest.completion, 0)
	requireRegistryReady(t, latest, []byte{0, 0, 0, 0, 0, 0, 0, 3, 0, 0}, 0)
}

func TestVersionstampRegistryReadyReplacement(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	defer tx.Cancel()
	old := registryStamp(t, tx)
	registryProducer(t, tx)
	old.completion.finishNative(commitOutcome{version: 42}, nil)
	requireRegistry(t, tx, old.completion.inc, old.completion, 0)
	producer := registryProducer(t, tx)
	latest := registryStamp(t, tx)
	if producer.completion != latest.completion || latest.completion == old.completion || latest.completion.inc.versionstamps != nil {
		t.Fatal("ready replacement retained a completed producer in overflow")
	}
	requireRegistry(t, tx, latest.completion.inc, latest.completion, 1)
	requireRegistryReady(t, old, []byte{0, 0, 0, 0, 0, 0, 0, 42, 0, 0}, 0)
	requireRegistryPending(t, latest)
}

func TestVersionstampRegistryPreNativeFailure(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	defer tx.Cancel()
	first := registryStamp(t, tx)
	producer := registryProducer(t, tx)
	registryProducer(t, tx)
	latest := registryStamp(t, tx)
	tx.Set([]byte{0xff, 1}, []byte("value"))
	if err := producer.Resolve(); err == nil {
		t.Fatal("expected system-key validation failure")
	} else if code, ok := fdbErrorCode(err); !ok || code != 2004 {
		t.Fatalf("pre-native Commit error=%v, want 2004", err)
	}
	requireRegistryPending(t, first)
	requireRegistryPending(t, latest)
	requireRegistry(t, tx, first.completion.inc, latest.completion, 2, first.completion)
	tx.Cancel()
	requireRegistry(t, tx, first.completion.inc, latest.completion, 0)
	requireRegistryReady(t, first, nil, 1025)
	requireRegistryReady(t, latest, nil, 1025)
}

func TestVersionstampRegistryRetirement(t *testing.T) {
	t.Parallel()
	for _, timeoutFirst := range []bool{false, true} {
		for _, selectedError := range []bool{false, true} {
			name := "cancel-first/"
			if timeoutFirst {
				name = "timeout-first/"
			}
			if selectedError {
				name += "selected-error"
			} else {
				name += "selected-success"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				tx := &Transaction{}
				defer tx.Cancel()
				tx.SetTimeout(3600000)
				selected := registryStamp(t, tx)
				registryProducer(t, tx)
				olderProducer := registryProducer(t, tx)
				older := registryStamp(t, tx)
				latestProducer := registryProducer(t, tx)
				latest := registryStamp(t, tx)
				inc := selected.completion.inc
				if selectedError {
					selected.completion.finishNative(commitOutcome{}, &wire.FDBError{Code: 2101})
				} else {
					selected.completion.finishNative(commitOutcome{version: 42}, nil)
				}
				requireRegistry(t, tx, inc, latest.completion, 2, older.completion)
				tx.readErrMu.Lock()
				arm := tx.timeoutArm
				tx.withdrawTimeoutLocked()
				tx.readErrMu.Unlock()
				want := 1025
				if timeoutFirst {
					want = 1031
					tx.fireTimeout(arm)
				}
				tx.Cancel()
				tx.fireTimeout(arm)
				requireRegistry(t, tx, inc, latest.completion, 0)
				tx.Reset()
				fresh := registryStamp(t, tx)
				if fresh.completion.inc == inc {
					t.Fatal("Reset rebound an old incarnation")
				}
				for _, p := range []*PendingVersionstamp{selected, older, latest} {
					p.completion.finishNative(commitOutcome{version: 99}, nil)
				}
				requireRegistryReady(t, older, nil, want)
				requireRegistryReady(t, latest, nil, want)
				if selectedError {
					requireRegistryReady(t, selected, nil, 2020)
				} else {
					requireRegistryReady(t, selected, []byte{0, 0, 0, 0, 0, 0, 0, 42, 0, 0}, 0)
				}
				for _, p := range []*PendingCommit{olderProducer, latestProducer} {
					if err := p.Resolve(); err == nil {
						t.Fatal("retired producer succeeded")
					} else if code, ok := fdbErrorCode(err); !ok || code != want {
						t.Fatalf("old producer error=%v, want %d", err, want)
					}
				}
				requireRegistry(t, tx, fresh.completion.inc, fresh.completion, 1)
				requireRegistryPending(t, fresh)
			})
		}
	}
}

// Not parallel: isolate completion creation from public context/goroutine costs.
func TestVersionstampRegistrySingleProducerAllocations(t *testing.T) {
	tx := &Transaction{}
	tx.readErrMu.Lock()
	defer tx.readErrMu.Unlock()
	inc := tx.readIncarnationLocked()
	allocs := testing.AllocsPerRun(100, func() {
		inc.versionstamp, inc.versionstamps = nil, nil
		tx.newVersionstampLocked(inc)
	})
	if allocs != 1 {
		t.Fatalf("VERSIONSTAMP_REGISTRY_ALLOC: got %v, want unobserved completion only (1)", allocs)
	}
	if inc.versionstamp == nil || inc.versionstamps != nil {
		t.Fatal("first completion did not remain inline")
	}
	t.Logf("VERSIONSTAMP_REGISTRY_ALLOC: %v", allocs)
}
