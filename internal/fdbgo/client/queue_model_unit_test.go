package client

import (
	"math"
	"sync"
	"testing"
	"time"
)

// refSecond is C++ loadBalance's second-request pair updated on one thread
// (LoadBalance.actor.h:694-716, flow/Knobs.cpp:278-283).
type refSecond struct{ multiplier, budget float64 }

func (r *refSecond) firstReplied() {
	r.multiplier = math.Max(r.multiplier-secondRequestMultiplierDecay, 1.0)
	r.budget = math.Min(r.budget+secondRequestBudgetGrowth, secondRequestMaxBudget)
}

func (r *refSecond) spendSecond() bool {
	if r.budget < 1.0 {
		return false
	}
	r.multiplier += secondRequestMultiplierGrowth
	r.budget -= 1.0
	return true
}

// TestQueueModelSecondPairMatchesSerial pins the second-request pair, through its idle
// fixed point, bit for bit against the serial C++ updates.
func TestQueueModelSecondPairMatchesSerial(t *testing.T) {
	t.Parallel()
	q, ref := newQueueModel(), &refSecond{multiplier: 1}
	check := func(step string) {
		t.Helper()
		b, m := secondState(q)
		if b != ref.budget || m != ref.multiplier {
			t.Fatalf("%s: budget=%v multiplier=%v, want %v and %v", step, b, m, ref.budget, ref.multiplier)
		}
		if idle := q.secondIdle.Load(); idle != (m == 1 && b == secondRequestMaxBudget) {
			t.Fatalf("%s: idle=%t at budget=%v multiplier=%v", step, idle, b, m)
		}
		want := time.Duration((ref.multiplier*queueDataInitialLatency + baseSecondRequestTime) * float64(time.Second))
		if got := q.secondDelay("a", "b"); got != want {
			t.Fatalf("%s: secondDelay=%v, want %v", step, got, want)
		}
	}
	for i := 0; i < 2100; i++ {
		q.firstReplied()
		ref.firstReplied()
	}
	check("saturated")
	if !q.secondIdle.Load() {
		t.Fatal("2100 lone replies did not reach the idle fixed point")
	}
	for i := 0; i < 3; i++ {
		if got, want := q.spendSecond(), ref.spendSecond(); got != want {
			t.Fatalf("spend %d paid=%t, want %t", i, got, want)
		}
		check("spent")
	}
	for i := 0; i < 200; i++ {
		q.firstReplied()
		ref.firstReplied()
		check("decaying")
	}
	for i := 0; i < 150; i++ {
		got, want := q.spendSecond(), ref.spendSecond()
		if got != want {
			t.Fatalf("spend %d paid=%t, want %t", i, got, want)
		}
		q.firstReplied()
		ref.firstReplied()
	}
	check("drained")
}

// TestQueueModelConcurrentUpdatesAreNotLost: from many goroutines, every delta comes back
// off its server and the lone replies add up as they would one after another.
func TestQueueModelConcurrentUpdatesAreNotLost(t *testing.T) {
	t.Parallel()
	const (
		goroutines = 32
		perG       = 50
	)
	q := newQueueModel()
	servers := []ServerInfo{{Address: "s1:4500"}, {Address: "s2:4500"}, {Address: "s3:4500"}}
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				best, second := q.chooseTopTwo(servers)
				addr := servers[best].Address
				delta := q.startRequest(addr)
				if second >= 0 {
					_ = q.secondDelay(addr, servers[second].Address)
				}
				q.firstReplied()
				q.endRequestFull(addr, delta, time.Duration(i)*time.Microsecond, true, false, 1.0)
			}
		}()
	}
	wg.Wait()
	for _, s := range servers {
		if total := totalOf(q, s.Address); total != 0 {
			t.Fatalf("%s: outstanding total %v after every request ended", s.Address, total)
		}
	}
	ref := &refSecond{multiplier: 1}
	for i := 0; i < goroutines*perG; i++ {
		ref.firstReplied()
	}
	if b, m := secondState(q); b != ref.budget || m != ref.multiplier {
		t.Fatalf("budget=%v multiplier=%v after %d lone replies, want %v and %v", b, m, goroutines*perG, ref.budget, ref.multiplier)
	}
}

// TestQueueModelConcurrentFromFixedPoint starts at the idle fixed point and mixes
// spends, delays and lone replies on many goroutines, so the lock-free paths race the spends.
func TestQueueModelConcurrentFromFixedPoint(t *testing.T) {
	t.Parallel()
	const (
		goroutines = 32
		perG       = 200
	)
	q := newQueueModel()
	setSecondState(q, 1, secondRequestMaxBudget)
	minDelay := q.secondDelay("a", "b")
	maxDelay := time.Duration(((1+goroutines*secondRequestMultiplierGrowth)*queueDataInitialLatency + baseSecondRequestTime) * float64(time.Second))
	var wg sync.WaitGroup
	var paid, idleSeen sync.Map
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				switch {
				case i == g:
					paid.Store(g, q.spendSecond())
				case i%3 == 0:
					if d := q.secondDelay("a", "b"); d < minDelay || d > maxDelay {
						t.Errorf("secondDelay %v outside [%v, %v]", d, minDelay, maxDelay)
					}
				default:
					if q.secondIdle.Load() {
						idleSeen.Store(g, true)
					}
					q.firstReplied()
				}
			}
		}()
	}
	wg.Wait()
	for g := 0; g < goroutines; g++ {
		if ok, _ := paid.Load(g); ok != true {
			t.Fatalf("goroutine %d: a budget of %v did not pay one of %d second requests", g, secondRequestMaxBudget, goroutines)
		}
	}
	n := 0
	idleSeen.Range(func(_, _ any) bool { n++; return true })
	if n == 0 {
		t.Fatal("no lone reply took the idle fast path")
	}
	b, m := secondState(q)
	if idle := q.secondIdle.Load(); idle != (m == 1 && b == secondRequestMaxBudget) || m < 1 || b > secondRequestMaxBudget {
		t.Fatalf("quiescent idle=%t at budget=%v multiplier=%v", idle, b, m)
	}
	for i := 0; i < 2*int(secondRequestMaxBudget/secondRequestBudgetGrowth); i++ {
		q.firstReplied()
	}
	if b, m := secondState(q); !q.secondIdle.Load() || m != 1 || b != secondRequestMaxBudget {
		t.Fatalf("after draining replies idle=%t budget=%v multiplier=%v, want the fixed point", q.secondIdle.Load(), b, m)
	}
}

// TestFailureMonitorExcludedFastPath checks the lock-free answer with nothing
// failed against the locked one as addresses fail and recover.
func TestFailureMonitorExcludedFastPath(t *testing.T) {
	t.Parallel()
	fm := newFailureMonitor()
	now := nowSeconds()
	if fm.excluded("a", now) || fm.nFailed.Load() != 0 {
		t.Fatal("a fresh monitor excludes an address")
	}
	fm.markFailedAt("a", now)
	fm.markFailedAt("a", now) // a repeat failure is not a second entry
	fm.markFailedAt("b", now)
	if n := fm.nFailed.Load(); n != 2 {
		t.Fatalf("nFailed=%d with two failed addresses", n)
	}
	if !fm.excluded("a", now) || fm.excluded("c", now) {
		t.Fatal("excluded disagrees with the failed set")
	}
	fm.markAlive("a")
	fm.markAlive("a")
	fm.markAlive("c")
	if n := fm.nFailed.Load(); n != 1 || fm.excluded("a", now) || !fm.excluded("b", now) {
		t.Fatalf("after recovery nFailed=%d", n)
	}
	fm.markAlive("b")
	if fm.nFailed.Load() != 0 || fm.excluded("b", now) {
		t.Fatal("recovered monitor still excludes")
	}
}
