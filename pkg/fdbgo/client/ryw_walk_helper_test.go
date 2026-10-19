package client

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
)

// memStorage is an in-memory NativeAPI getRange over one shard at one version
// (A10): it resolves both selectors over sorted keys, then returns rows in scan
// order while the limits are not reached, as getRange's loop does when every
// reply ends at the byte limit or the row limit. It records every request.
type memStorage struct {
	kvs      []KeyValue // ascending
	requests []walkRequest
	// before, when set, runs before a request is answered (an interleaving seam);
	// reply, when set, replaces the answer.
	before func(n int)
	reply  func(n int, req walkRequest) (nativeRangeResult, error)
}

func newMemStorage(keys ...string) *memStorage {
	m := &memStorage{}
	sort.Strings(keys)
	for _, k := range keys {
		m.kvs = append(m.kvs, KeyValue{Key: []byte(k), Value: []byte("v" + k)})
	}
	return m
}

// position resolves a selector to an index into kvs (FDBTypes.h:631-700).
func (m *memStorage) position(sel KeySelector) int {
	sel = sel.removeOrEqual()
	base := sort.Search(len(m.kvs), func(i int) bool { return bytes.Compare(m.kvs[i].Key, sel.Key) >= 0 })
	p := base + int(sel.Offset) - 1
	return max(0, min(p, len(m.kvs)))
}

func (m *memStorage) native(_ context.Context, begin, end KeySelector, limits rangeLimits, reverse bool) (nativeRangeResult, error) {
	n := len(m.requests)
	req := walkRequest{begin: begin, end: end, limits: limits}
	m.requests = append(m.requests, req)
	if m.before != nil {
		m.before(n)
	}
	if m.reply != nil {
		return m.reply(n, req)
	}
	return m.answer(req, reverse), nil
}

func (m *memStorage) answer(req walkRequest, reverse bool) nativeRangeResult {
	var r nativeRangeResult
	b, e := m.position(req.begin), m.position(req.end)
	if b >= e {
		return r
	}
	l := req.limits
	rows := m.kvs[b:e]
	for i := range rows {
		kv := rows[i]
		if reverse {
			kv = rows[len(rows)-1-i]
		}
		if l.isReached() {
			r.more = true
			break
		}
		r.kvs = append(r.kvs, kv)
		size := 0
		if l.hasByteLimit() {
			size = 8 + len(kv.Key) + len(kv.Value)
		}
		l.decrement(1, size)
	}
	// A reached limit reports more whether or not rows remain (NAPI:4765-4808).
	r.more = r.more || l.isReached()
	return r
}

// selString renders a selector as (key, orEqual, offset).
func selString(s KeySelector) string {
	return fmt.Sprintf("(%q,%t,%d)", s.Key, s.OrEqual, s.Offset)
}

// reqString renders a request: selectors, limits and the wire limits.
func reqString(r walkRequest, reverse bool) string {
	limit, limitBytes := transformRangeLimits(r.limits, reverse)
	return fmt.Sprintf("[%s %s) rows=%d minRows=%d bytes=%d wire=%d/%d",
		selString(r.begin), selString(r.end), r.limits.rows, r.limits.minRows, r.limits.bytes, limit, limitBytes)
}

func keysOf(kvs []KeyValue) string {
	var ks []string
	for _, kv := range kvs {
		ks = append(ks, string(kv.Key))
	}
	return strings.Join(ks, ",")
}

// cacheDump renders the snapshot cache entries.
func (c *rywCache) cacheDump() string {
	var b strings.Builder
	for _, e := range c.serverCache.entries {
		fmt.Fprintf(&b, "[%q,%q){%s} ", e.begin, e.end, keysOf(e.kvs))
	}
	return strings.TrimSpace(b.String())
}

// runWalk issues and runs a walk against m; it returns the walk and its error.
func runWalk(c *rywCache, view *rywWrites, begin, end KeySelector, limits rangeLimits, reverse bool, maxKey []byte, m *memStorage) (*rywWalk, error) {
	c.mu.Lock()
	w := c.newRangeWalkLocked(view, begin, end, limits, reverse, maxKey)
	w.startLocked()
	c.mu.Unlock()
	return w, w.run(context.Background(), m.native)
}

// testGetKey resolves sel with a getKey walk over the live version (or the
// snapshot cache alone) against m.
func testGetKey(c *rywCache, sel KeySelector, maxKey []byte, includeWrites bool, m *memStorage) ([]byte, error) {
	c.mu.Lock()
	w := c.newGetKeyWalkLocked(c.viewLocked(includeWrites), sel, maxKey)
	w.startLocked()
	c.mu.Unlock()
	if err := w.run(context.Background(), m.native); err != nil {
		return nil, err
	}
	return w.keyResult(), nil
}
