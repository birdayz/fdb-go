package client

// The cached (version, instant) pair must be one that ACTUALLY OCCURRED.
//
// tryCache reports the cached version together with the instant its MVCC
// window opened, and RFC-198 budgets against that instant. That only holds if
// the two values belong to each other. Publishing them as two independent
// atomics does not give that: a reader can load the version before a writer's
// store and the timestamp after it, observing a pair that never existed — an
// OLD version carrying a NEW time.
//
// That direction is the dangerous one. The reported instant is then LATER than
// the reported version's real window start, so the budget believes the
// transaction is younger than it is, and a page starts against a version the
// cluster is about to refuse with 1007 — the precise failure the anchor exists
// to prevent. (The mirror case, a new version carrying an old time, is safe:
// it only anchors earlier than the truth, which costs a retry at worst. C++
// tolerates exactly that and says so at NativeAPI.actor.cpp:373-376.)
//
// C++ publishes both fields inside one critical section
// (updateCachedReadVersionShared, :348-359, under MutexHolder), so no reader
// there can tear the pair. It needs no more than that because it uses
// lastGrvTime ONLY for freshness and never derives an anchor from it; Go's
// accessor is the extra consumer, and it is what makes coherence load-bearing.

import (
	"sync"
	"testing"
	"time"
)

// coherenceEpoch anchors the test's version→time mapping. Each published pair
// is (v, coherenceEpoch + v·ms), so any observed pair can be checked against
// the mapping with no timing assumptions whatsoever: the pair is coherent iff
// the observed instant is exactly the one that version was published with.
var coherenceEpoch = time.Now()

func coherenceTimeFor(v int64) time.Time {
	return coherenceEpoch.Add(time.Duration(v) * time.Millisecond)
}

// TestGRVCache_PairIsNeverTorn publishes an ascending series of coherent
// (version, time) pairs from several goroutines while readers sample the
// cache, and asserts every observed pair is one that was actually published.
//
// It is deterministic in its ASSERTION (the mapping is exact; no slack, no
// wall-clock comparison) even though the interleaving is concurrent — a
// scheduling accident can only make it miss a defect, never invent one.
func TestGRVCache_PairIsNeverTorn(t *testing.T) {
	t.Parallel()

	const (
		writers        = 4
		versionsEach   = 400
		readerRoutines = 4
	)

	c := &grvCache{}
	// Freshness must never evict during the run: this test is about coherence,
	// and a stale-miss would silently reduce the sample to nothing.
	c.now = func() time.Time { return coherenceTimeFor(0) }

	var stop sync.WaitGroup
	done := make(chan struct{})

	var mu sync.Mutex
	var torn []string
	observed := 0

	for r := 0; r < readerRoutines; r++ {
		stop.Add(1)
		go func() {
			defer stop.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				v, at, ok := c.tryCache(grvPriorityDefault)
				if !ok {
					continue
				}
				mu.Lock()
				observed++
				if want := coherenceTimeFor(v); !at.Equal(want) {
					torn = append(torn, formatTorn(v, at, want))
				}
				mu.Unlock()
			}
		}()
	}

	var pub sync.WaitGroup
	for w := 0; w < writers; w++ {
		pub.Add(1)
		go func(base int64) {
			defer pub.Done()
			for i := int64(1); i <= versionsEach; i++ {
				v := base*versionsEach + i
				c.updateFromGRV(c.generation.Load(), coherenceTimeFor(v), v)
			}
		}(int64(w))
	}
	pub.Wait()
	close(done)
	stop.Wait()

	if observed == 0 {
		t.Fatal("readers never observed a cache hit: the run proves nothing about " +
			"coherence. Check the freshness seam above")
	}
	if len(torn) > 0 {
		shown := torn
		if len(shown) > 5 {
			shown = shown[:5]
		}
		t.Fatalf("%d of %d observed (version, instant) pairs were TORN — a pair that was "+
			"never published:\n  %v\nEach one is a version reported with an instant "+
			"belonging to a different version. When that instant is the NEWER of the "+
			"two, the anchor claims the version's MVCC window opened later than it "+
			"did, the RFC-198 budget under-counts the transaction's age, and the page "+
			"it admits dies on 1007. The pair must be published as ONE value",
			len(torn), observed, shown)
	}
}

func formatTorn(v int64, got, want time.Time) string {
	return "version " + itoa(v) + " reported at " + got.Sub(coherenceEpoch).String() +
		", but that version was published at " + want.Sub(coherenceEpoch).String()
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}
