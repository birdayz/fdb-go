package plugin

import (
	"container/list"
	"context"
	"errors"
	"sync"
)

// errDisposed is returned for work arriving after the instance was disposed.
var errDisposed = errors.New("datasource instance was replaced by a settings change; retry")

// refLRU keeps at most capacity open values (per-tenant SQL pools or OTel
// readers), least recently used first out. A value in use when evicted is
// closed when its last user releases it, so eviction never breaks a query.
// Concurrent first acquires of one key share a single open.
type refLRU[V any] struct {
	capacity int
	open     func(ctx context.Context, key string) (V, error)
	close    func(V)

	mu     sync.Mutex
	order  *list.List // of *lruEntry[V], front = most recent
	items  map[string]*lruEntry[V]
	closed bool
}

type lruEntry[V any] struct {
	key     string
	val     V
	err     error
	ready   chan struct{} // closed once open finished
	refs    int
	evicted bool
	elem    *list.Element
}

func newRefLRU[V any](capacity int, open func(context.Context, string) (V, error), closeFn func(V)) *refLRU[V] {
	return &refLRU[V]{
		capacity: capacity,
		open:     open,
		close:    closeFn,
		order:    list.New(),
		items:    map[string]*lruEntry[V]{},
	}
}

// acquire returns the value for key, opening it if needed. The caller must
// call release exactly once when done with the value.
func (c *refLRU[V]) acquire(ctx context.Context, key string) (V, func(), error) {
	var zero V
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return zero, nil, errDisposed
	}
	e, ok := c.items[key]
	if ok {
		e.refs++
		c.order.MoveToFront(e.elem)
		c.mu.Unlock()
		select {
		case <-e.ready:
		case <-ctx.Done():
			c.release(e)
			return zero, nil, ctx.Err()
		}
		if e.err != nil {
			c.release(e)
			return zero, nil, e.err
		}
		return e.val, func() { c.release(e) }, nil
	}
	e = &lruEntry[V]{key: key, ready: make(chan struct{}), refs: 1}
	e.elem = c.order.PushFront(e)
	c.items[key] = e
	victims := c.evictLocked()
	c.mu.Unlock()
	c.closeAll(victims)

	e.val, e.err = c.open(ctx, key)
	if e.err != nil {
		c.mu.Lock()
		c.removeLocked(e)
		c.mu.Unlock()
	}
	close(e.ready)
	if e.err != nil {
		c.release(e)
		return zero, nil, e.err
	}
	return e.val, func() { c.release(e) }, nil
}

// evictLocked removes entries past capacity and returns the idle ones to close.
func (c *refLRU[V]) evictLocked() []*lruEntry[V] {
	var victims []*lruEntry[V]
	for c.order.Len() > c.capacity {
		e := c.order.Back().Value.(*lruEntry[V])
		c.removeLocked(e)
		if e.refs == 0 {
			victims = append(victims, e)
		}
	}
	return victims
}

func (c *refLRU[V]) removeLocked(e *lruEntry[V]) {
	if e.evicted {
		return
	}
	e.evicted = true
	c.order.Remove(e.elem)
	delete(c.items, e.key)
}

func (c *refLRU[V]) release(e *lruEntry[V]) {
	c.mu.Lock()
	e.refs--
	last := e.refs == 0 && e.evicted
	c.mu.Unlock()
	if last {
		c.closeAll([]*lruEntry[V]{e})
	}
}

// closeAll closes the values whose open succeeded.
func (c *refLRU[V]) closeAll(es []*lruEntry[V]) {
	for _, e := range es {
		<-e.ready
		if e.err == nil {
			c.close(e.val)
		}
	}
}

// invalidate drops key's entry if it holds a value same accepts, so the next
// acquire opens a fresh one; the dropped value closes once released.
func (c *refLRU[V]) invalidate(key string, same func(V) bool) {
	c.mu.Lock()
	e, ok := c.items[key]
	if !ok {
		c.mu.Unlock()
		return
	}
	select {
	case <-e.ready:
	default:
		c.mu.Unlock()
		return // still opening: it is not the value the caller saw
	}
	if e.err != nil || !same(e.val) {
		c.mu.Unlock()
		return
	}
	c.removeLocked(e)
	idle := e.refs == 0
	c.mu.Unlock()
	if idle {
		c.closeAll([]*lruEntry[V]{e})
	}
}

// keys returns the resident keys, most recent first.
func (c *refLRU[V]) keys() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, c.order.Len())
	for el := c.order.Front(); el != nil; el = el.Next() {
		out = append(out, el.Value.(*lruEntry[V]).key)
	}
	return out
}

// shutdown refuses new acquires and closes every value once it is released.
func (c *refLRU[V]) shutdown() {
	c.mu.Lock()
	c.closed = true
	var idle []*lruEntry[V]
	for c.order.Len() > 0 {
		e := c.order.Back().Value.(*lruEntry[V])
		c.removeLocked(e)
		if e.refs == 0 {
			idle = append(idle, e)
		}
	}
	c.mu.Unlock()
	c.closeAll(idle)
}

// gate counts in-flight requests so Dispose closes the FDB database only
// after the last request that started before it has finished.
type gate struct {
	mu     sync.Mutex
	n      int
	closed bool
	onIdle func()
}

func (g *gate) enter() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return false
	}
	g.n++
	return true
}

func (g *gate) leave() {
	g.mu.Lock()
	g.n--
	fire := g.closed && g.n == 0 && g.onIdle != nil
	f := g.onIdle
	if fire {
		g.onIdle = nil
	}
	g.mu.Unlock()
	if fire {
		f()
	}
}

// close runs onIdle once no request is in flight (immediately if none is).
func (g *gate) close(onIdle func()) {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return
	}
	g.closed = true
	if g.n > 0 {
		g.onIdle = onIdle
		g.mu.Unlock()
		return
	}
	g.mu.Unlock()
	onIdle()
}
