package plugin

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
)

type lruProbe struct {
	mu     sync.Mutex
	opens  map[string]int
	closed []string
	fail   map[string]error
	block  chan struct{}
}

func newProbe() *lruProbe { return &lruProbe{opens: map[string]int{}, fail: map[string]error{}} }

func (p *lruProbe) lru(capacity int) *refLRU[string] {
	return newRefLRU(capacity, func(ctx context.Context, k string) (string, error) {
		p.mu.Lock()
		p.opens[k]++
		err := p.fail[k]
		block := p.block
		p.mu.Unlock()
		if block != nil {
			<-block
		}
		return "v" + k, err
	}, func(v string) {
		p.mu.Lock()
		p.closed = append(p.closed, v)
		p.mu.Unlock()
	})
}

func (p *lruProbe) closedList() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.closed)
}

func TestRefLRUEvictsLeastRecentlyUsed(t *testing.T) {
	t.Parallel()
	p := newProbe()
	c := p.lru(2)
	use := func(k string) {
		v, release, err := c.acquire(context.Background(), k)
		if err != nil || v != "v"+k {
			t.Fatalf("%s: %v %v", k, v, err)
		}
		release()
	}
	use("a")
	use("b")
	use("a") // b is now least recent
	use("c")
	if got := c.keys(); !slices.Equal(got, []string{"c", "a"}) {
		t.Fatalf("resident %v", got)
	}
	if got := p.closedList(); !slices.Equal(got, []string{"vb"}) {
		t.Fatalf("closed %v", got)
	}
	if p.opens["a"] != 1 {
		t.Fatalf("a opened %d times", p.opens["a"])
	}
}

func TestRefLRUDefersCloseOfValueInUse(t *testing.T) {
	t.Parallel()
	p := newProbe()
	c := p.lru(1)
	_, releaseA, err := c.acquire(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	_, releaseB, _ := c.acquire(context.Background(), "b")
	if len(p.closedList()) != 0 {
		t.Fatal("closed a value still in use")
	}
	releaseA()
	if got := p.closedList(); !slices.Equal(got, []string{"va"}) {
		t.Fatalf("closed %v", got)
	}
	releaseB()
	if len(p.closedList()) != 1 {
		t.Fatal("closed a resident value on release")
	}
}

func TestRefLRUSharesOneOpen(t *testing.T) {
	t.Parallel()
	p := newProbe()
	p.block = make(chan struct{})
	c := p.lru(4)
	var wg sync.WaitGroup
	var ok atomic.Int32
	for range 8 {
		wg.Go(func() {
			if _, release, err := c.acquire(context.Background(), "a"); err == nil {
				ok.Add(1)
				release()
			}
		})
	}
	close(p.block)
	wg.Wait()
	if ok.Load() != 8 || p.opens["a"] != 1 {
		t.Fatalf("%d acquired, %d opens", ok.Load(), p.opens["a"])
	}
}

func TestRefLRUDoesNotCacheFailures(t *testing.T) {
	t.Parallel()
	p := newProbe()
	boom := errors.New("boom")
	p.fail["a"] = boom
	c := p.lru(2)
	if _, _, err := c.acquire(context.Background(), "a"); !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	if len(c.keys()) != 0 || len(p.closedList()) != 0 {
		t.Fatalf("failed open left %v, closed %v", c.keys(), p.closedList())
	}
	p.mu.Lock()
	delete(p.fail, "a")
	p.mu.Unlock()
	if _, release, err := c.acquire(context.Background(), "a"); err != nil {
		t.Fatal(err)
	} else {
		release()
	}
	if p.opens["a"] != 2 {
		t.Fatalf("%d opens", p.opens["a"])
	}
}

func TestRefLRUShutdown(t *testing.T) {
	t.Parallel()
	p := newProbe()
	c := p.lru(4)
	_, release, _ := c.acquire(context.Background(), "busy")
	for i := range 2 {
		_, r, _ := c.acquire(context.Background(), fmt.Sprint(i))
		r()
	}
	c.shutdown()
	if got := p.closedList(); len(got) != 2 {
		t.Fatalf("closed %v; idle values close at once", got)
	}
	release()
	if got := p.closedList(); len(got) != 3 {
		t.Fatalf("closed %v; the busy value closes on release", got)
	}
	if _, _, err := c.acquire(context.Background(), "x"); !errors.Is(err, errDisposed) {
		t.Fatalf("after shutdown: %v", err)
	}
}

func TestRefLRUWaiterHonorsContext(t *testing.T) {
	t.Parallel()
	p := newProbe()
	p.block = make(chan struct{})
	c := p.lru(2)
	go func() {
		if _, r, err := c.acquire(context.Background(), "a"); err == nil {
			r()
		}
	}()
	for {
		p.mu.Lock()
		n := p.opens["a"]
		p.mu.Unlock()
		if n == 1 {
			break
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := c.acquire(ctx, "a"); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	close(p.block)
}
