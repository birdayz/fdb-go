/*
 * allocator.go
 *
 * This source file is part of the FoundationDB open source project
 *
 * Copyright 2013-2018 Apple Inc. and the FoundationDB project authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// FoundationDB Go Directory Layer

package directory

import (
	"bytes"
	"encoding/binary"
	"math/rand"
	"sync"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
)

var oneBytes = []byte{0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}

// txLockEntry serializes concurrent HCA operations targeting the same
// transaction. refs is guarded by txLockPool.mu and prevents deleting an entry
// while any goroutine holds or is waiting on mu.
type txLockEntry struct {
	mu   sync.Mutex
	refs int
}

// txLockPool provides per-key mutual exclusion without holding a single
// process-wide lock across unrelated transactions. Because directoryLayer
// instances can be created fresh per partition traversal or per resolver call
// while sharing the same underlying fdb.Transaction, mutual exclusion must be
// keyed on the transaction rather than the highContentionAllocator instance.
type txLockPool[K comparable] struct {
	mu       sync.Mutex
	entries  map[K]*txLockEntry
	fallback sync.Mutex
}

func newTxLockPool[K comparable]() *txLockPool[K] {
	return &txLockPool[K]{
		entries: make(map[K]*txLockEntry),
	}
}

type txLockGuard[K comparable] struct {
	pool     *txLockPool[K]
	key      K
	entry    *txLockEntry
	fallback bool
}

func (p *txLockPool[K]) lockKey(key K) txLockGuard[K] {
	p.mu.Lock()
	e := p.entries[key]
	if e == nil {
		e = &txLockEntry{}
		p.entries[key] = e
	}
	e.refs++
	p.mu.Unlock()

	e.mu.Lock()
	return txLockGuard[K]{pool: p, key: key, entry: e}
}

func (p *txLockPool[K]) lockFallback() txLockGuard[K] {
	p.fallback.Lock()
	return txLockGuard[K]{pool: p, fallback: true}
}

func (g txLockGuard[K]) unlock() {
	if g.fallback {
		g.pool.fallback.Unlock()
		return
	}
	g.entry.mu.Unlock()
	g.pool.mu.Lock()
	g.entry.refs--
	if g.entry.refs == 0 {
		delete(g.pool.entries, g.key)
	}
	g.pool.mu.Unlock()
}

var hcaTxLocks = newTxLockPool[fdb.Transaction]()

func lockHCATx(tr fdb.WritableTransaction) txLockGuard[fdb.Transaction] {
	switch t := tr.(type) {
	case fdb.Transaction:
		return hcaTxLocks.lockKey(t)
	case *fdb.Transaction:
		if t != nil {
			return hcaTxLocks.lockKey(*t)
		}
	}
	return hcaTxLocks.lockFallback()
}

type highContentionAllocator struct {
	counters, recent subspace.Subspace
	// int63n draws a candidate; nil is math/rand.
	int63n func(n int64) int64
}

func newHCA(s subspace.Subspace) highContentionAllocator {
	var hca highContentionAllocator

	hca.counters = s.Sub(0)
	hca.recent = s.Sub(1)

	return hca
}

func windowSize(start int64) int64 {
	// Larger window sizes are better for high contention, smaller sizes for
	// keeping the keys small.  But if there are many allocations, the keys
	// can't be too small.  So start small and scale up.  We don't want this to
	// ever get *too* big because we have to store about window_size/2 recent
	// items.
	if start < 255 {
		return 64
	}
	if start < 65535 {
		return 1024
	}
	return 8192
}

func (hca highContentionAllocator) allocate(tr fdb.WritableTransaction, s subspace.Subspace) (subspace.Subspace, error) {
	for {
		rr := tr.Snapshot().GetRange(hca.counters, fdb.RangeOptions{Limit: 1, Reverse: true})
		kvs, e := rr.GetSliceWithError()
		if e != nil {
			return nil, e
		}

		var start int64
		var window int64

		if len(kvs) == 1 {
			t, e := hca.counters.Unpack(kvs[0].Key)
			if e != nil {
				return nil, e
			}
			start = t[0].(int64)
		}

		windowAdvanced := false
		for {
			guard := lockHCATx(tr)

			if windowAdvanced {
				tr.ClearRange(fdb.KeyRange{Begin: hca.counters, End: hca.counters.Sub(start)})
				tr.Options().SetNextWriteNoWriteConflictRange()
				tr.ClearRange(fdb.KeyRange{Begin: hca.recent, End: hca.recent.Sub(start)})
			}

			// Increment the allocation count for the current window
			tr.Add(hca.counters.Sub(start), oneBytes)
			countFuture := tr.Snapshot().Get(hca.counters.Sub(start))

			guard.unlock()

			countStr, e := countFuture.Get()
			if e != nil {
				return nil, e
			}

			var count int64
			if countStr == nil {
				count = 0
			} else {
				e = binary.Read(bytes.NewBuffer(countStr), binary.LittleEndian, &count)
				if e != nil {
					return nil, e
				}
			}

			window = windowSize(start)
			if count*2 < window {
				break
			}

			start += window
			windowAdvanced = true
		}

		for {
			// As of the snapshot being read from, the window is less than half
			// full, so this should be expected to take 2 tries.  Under high
			// contention (and when the window advances), there is an additional
			// subsequent risk of conflict for this transaction.
			draw := rand.Int63n
			if hca.int63n != nil {
				draw = hca.int63n
			}
			candidate := draw(window) + start
			key := hca.recent.Sub(candidate)

			guard := lockHCATx(tr)

			latestCounter := tr.Snapshot().GetRange(hca.counters, fdb.RangeOptions{Limit: 1, Reverse: true})
			candidateValue := tr.Get(key)
			tr.Options().SetNextWriteNoWriteConflictRange()
			tr.Set(key, []byte(""))

			guard.unlock()

			kvs, e = latestCounter.GetSliceWithError()
			if e != nil {
				return nil, e
			}
			if len(kvs) > 0 {
				t, e := hca.counters.Unpack(kvs[0].Key)
				if e != nil {
					return nil, e
				}
				currentStart := t[0].(int64)
				if currentStart > start {
					break
				}
			}

			v, e := candidateValue.Get()
			if e != nil {
				return nil, e
			}
			if v == nil {
				tr.AddWriteConflictKey(key)
				return s.Sub(candidate), nil
			}
		}
	}
}
