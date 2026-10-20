package sendqueue

import (
	"sync"
	"testing"
)

func requireReady(t *testing.T, p *Permit, want bool) {
	t.Helper()
	select {
	case <-p.Ready():
		if !want {
			t.Fatal("permit became ready before its predecessors released")
		}
	default:
		if want {
			t.Fatal("head permit is not ready")
		}
	}
}

func TestQueueZeroValueAndReuse(t *testing.T) {
	t.Parallel()
	var q Queue
	var nilPermit *Permit
	var zeroPermit Permit
	nilPermit.Release()
	zeroPermit.Release()
	if nilPermit.Ready() != nil || zeroPermit.Ready() != nil || q.Pending() {
		t.Fatal("zero queue or permits are not empty")
	}
	for range 2 {
		p, first := q.Reserve()
		if !first || !q.Pending() {
			t.Fatalf("first reservation: first=%t pending=%t", first, q.Pending())
		}
		requireReady(t, p, true)
		p.Release()
		p.Release()
		if q.Pending() {
			t.Fatal("single released permit left the queue pending")
		}
		requireReady(t, p, true)
	}
}

func TestQueueRemoveHeadMiddleTail(t *testing.T) {
	t.Parallel()
	for removed, name := range []string{"head", "middle", "tail"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var q Queue
			var permits []*Permit
			for i := 0; i < 3; i++ {
				p, first := q.Reserve()
				if first != (i == 0) {
					t.Fatalf("reservation %d first=%t", i, first)
				}
				permits = append(permits, p)
				requireReady(t, p, i == 0)
			}
			permits[removed].Release()
			permits[removed].Release()
			if !q.Pending() {
				t.Fatal("removing one permit cleared a nonempty queue")
			}
			// Appending after unlink catches a stale tail or broken middle links.
			last, first := q.Reserve()
			if first {
				t.Fatal("append after removal started a new queue")
			}
			permits = append(permits, last)
			var order []int
			for i := range permits {
				if i != removed {
					order = append(order, i)
				}
			}
			for i, head := range order {
				requireReady(t, permits[head], true)
				for _, later := range order[i+1:] {
					requireReady(t, permits[later], false)
				}
				permits[head].Release()
				permits[head].Release()
				if q.Pending() != (i+1 < len(order)) {
					t.Fatalf("pending after releasing %d = %t", head, q.Pending())
				}
			}
			if removed != 0 {
				requireReady(t, permits[removed], false)
			}
		})
	}
}

func TestQueueConcurrentReleaseAndGrant(t *testing.T) {
	t.Parallel()
	for i := 0; i < 200; i++ {
		var q Queue
		head, _ := q.Reserve()
		middle, _ := q.Reserve()
		tail, _ := q.Reserve()
		start := make(chan struct{})
		var wg sync.WaitGroup
		for _, p := range []*Permit{head, middle, head, middle} {
			wg.Go(func() { <-start; p.Release() })
		}
		close(start)
		wg.Wait()
		if !q.Pending() {
			t.Fatalf("iteration %d: releasing predecessors removed the tail", i)
		}
		requireReady(t, tail, true)
		tail.Release()
		if q.Pending() {
			t.Fatalf("iteration %d: released tail left queue pending", i)
		}
	}
}

func TestQueueConcurrentReserveAndLastRelease(t *testing.T) {
	t.Parallel()
	for i := 0; i < 200; i++ {
		var q Queue
		old, _ := q.Reserve()
		start := make(chan struct{})
		var next *Permit
		var wg sync.WaitGroup
		wg.Go(func() { <-start; old.Release() })
		wg.Go(func() { <-start; next, _ = q.Reserve() })
		close(start)
		wg.Wait()
		if !q.Pending() {
			t.Fatalf("iteration %d: new reservation was lost", i)
		}
		requireReady(t, next, true)
		old.Release()
		next.Release()
		if q.Pending() {
			t.Fatalf("iteration %d: queue did not empty", i)
		}
	}
}

func TestQueueSealPreventsLateReservation(t *testing.T) {
	t.Parallel()
	for _, hadOwner := range []bool{false, true} {
		var q Queue
		stale := &q
		if hadOwner {
			owner, _ := q.Reserve()
			if !stale.Pending() {
				t.Fatal("positive control: old generation was not pending")
			}
			owner.Release()
		}
		if !q.SealIfEmpty() || q.Pending() {
			t.Fatal("empty queue did not seal")
		}
		if !q.SealIfEmpty() {
			t.Fatal("sealing was not idempotent")
		}
		if p, first, ok := stale.TryReserve(); p != nil || first || ok || q.Pending() {
			t.Fatalf("stale reservation resurrected sealed queue: permit=%p first=%t ok=%t pending=%t", p, first, ok, q.Pending())
		}
		func() {
			defer func() {
				if got := recover(); got != "sendqueue.Queue.Reserve: sealed queue" {
					t.Errorf("Reserve on sealed queue panic = %v", got)
				}
			}()
			q.Reserve()
		}()
		if q.Pending() || !q.SealIfEmpty() {
			t.Fatal("failed Reserve changed the sealed generation")
		}
	}
}

func TestQueueSealRefusesPending(t *testing.T) {
	t.Parallel()
	var q Queue
	head, first, ok := q.TryReserve()
	if head == nil || !first || !ok || !q.Pending() {
		t.Fatal("positive control: first TryReserve did not establish a live generation")
	}
	if q.SealIfEmpty() {
		t.Fatal("sealed a pending head")
	}
	tail, first, ok := q.TryReserve()
	if tail == nil || first || !ok {
		t.Fatal("refused seal prevented a follower reservation")
	}
	requireReady(t, tail, false)
	head.Release()
	requireReady(t, tail, true)
	if q.SealIfEmpty() {
		t.Fatal("sealed while the granted tail still owned its permit")
	}
	tail.Release()
	if !q.SealIfEmpty() || q.Pending() {
		t.Fatal("last release did not allow sealing")
	}
}

func TestQueueConcurrentReleaseReserveAndSeal(t *testing.T) {
	t.Parallel()
	var accepted, rejected, sealed, refused int
	for _, order := range []string{"reserve-before-seal", "seal-before-reserve", "race"} {
		for i := 0; i < 100; i++ {
			var q Queue
			old, _ := q.Reserve()
			start, released, reserved, sealDone := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
			var next *Permit
			var first, ok, didSeal bool
			var wg sync.WaitGroup
			wg.Go(func() { <-start; old.Release(); close(released) })
			wg.Go(func() {
				<-start
				if order == "seal-before-reserve" {
					<-sealDone
				}
				next, first, ok = q.TryReserve()
				close(reserved)
			})
			wg.Go(func() {
				<-start
				if order == "seal-before-reserve" {
					<-released
				} else if order == "reserve-before-seal" {
					<-reserved
				}
				didSeal = q.SealIfEmpty()
				close(sealDone)
			})
			close(start)
			wg.Wait()
			if didSeal {
				sealed++
			} else {
				refused++
			}
			if ok {
				accepted++
				if next == nil || didSeal || !q.Pending() || order == "seal-before-reserve" {
					t.Fatalf("%s iteration %d: accepted=%t sealed=%t pending=%t permit=%p", order, i, ok, didSeal, q.Pending(), next)
				}
				requireReady(t, next, true)
				next.Release()
			} else {
				rejected++
				if next != nil || first || !didSeal || q.Pending() || order == "reserve-before-seal" {
					t.Fatalf("%s iteration %d: refused permit=%p first=%t sealed=%t pending=%t", order, i, next, first, didSeal, q.Pending())
				}
			}
			if !q.SealIfEmpty() {
				t.Fatalf("%s iteration %d: drained generation could not seal", order, i)
			}
			if p, first, ok := q.TryReserve(); p != nil || first || ok || q.Pending() {
				t.Fatalf("%s iteration %d: sealed generation resurrected", order, i)
			}
		}
	}
	if accepted == 0 || rejected == 0 || sealed == 0 || refused == 0 {
		t.Fatalf("vacuous seal race: %d accepted %d rejected %d seals %d refusals", accepted, rejected, sealed, refused)
	}
}

func TestQueueSealedTryReserveDoesNotAllocate(t *testing.T) {
	var q Queue
	q.SealIfEmpty()
	if got := testing.AllocsPerRun(100, func() { q.TryReserve() }); got != 0 {
		t.Fatalf("sealed TryReserve allocated %g objects, want zero", got)
	}
}

func TestQueueWarmOperationsDoNotAllocate(t *testing.T) {
	var q Queue
	p, _ := q.Reserve()
	p.Release()
	if got := testing.AllocsPerRun(100, func() { q.Pending(); p.Release() }); got != 0 {
		t.Fatalf("warm operations allocated %g objects, want zero", got)
	}
}
