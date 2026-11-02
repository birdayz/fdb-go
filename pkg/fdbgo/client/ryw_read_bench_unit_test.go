package client

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
)

// benchStorage is a read-only memStorage that concurrent reads may share: it
// records no requests.
type benchStorage struct{ m memStorage }

func newBenchStorage(n int) *benchStorage {
	s := &benchStorage{}
	for i := 0; i < n; i++ {
		k := fmt.Appendf(nil, "t/%07d/s", i*4)
		s.m.kvs = append(s.m.kvs, KeyValue{Key: k, Value: []byte("signpost")})
	}
	return s
}

func (s *benchStorage) native(_ context.Context, begin, end KeySelector, limits rangeLimits, reverse bool) (nativeRangeResult, error) {
	return s.m.answer(walkRequest{begin: begin, end: end, limits: limits}, reverse), nil
}

// exact is the storage range read under an exact RYW range read.
func (s *benchStorage) exact(_ context.Context, begin, end []byte, limit, _ int, reverse bool) ([]KeyValue, bool, []byte, error) {
	kvs := s.m.kvs
	lo := sort.Search(len(kvs), func(i int) bool { return bytes.Compare(kvs[i].Key, begin) >= 0 })
	hi := sort.Search(len(kvs), func(i int) bool { return bytes.Compare(kvs[i].Key, end) >= 0 })
	rows := kvs[lo:hi]
	more := limit > 0 && len(rows) > limit
	if reverse {
		out := make([]KeyValue, 0, len(rows))
		for i := len(rows) - 1; i >= 0 && (limit <= 0 || len(out) < limit); i-- {
			out = append(out, rows[i])
		}
		return out, more, nil, nil
	}
	if more {
		rows = rows[:limit]
	}
	return rows, more, nil, nil
}

// BenchmarkRYWConcurrentReads is Record Layer TEXT index maintenance in one
// transaction: many pending writes, then 2048 reads from 128 concurrent readers
// (or one, to price a read without contention), each issuing BunchedMap's
// selector reads (lastLessOrEqual(k) .. firstGreaterThan(k)+1, as a snapshot and
// as a serializable read), getKeys and exact range reads, and writing the entry
// it read for. One op is one transaction; ns/read divides by the reads it issues.
func BenchmarkRYWConcurrentReads(b *testing.B) {
	const (
		storageKeys = 50000
		reads       = 2048
	)
	st := newBenchStorage(storageKeys)
	for _, tc := range []struct{ writes, readers int }{{1000, 128}, {20000, 128}, {20000, 1}} {
		writes, readers, perReader := tc.writes, tc.readers, reads/tc.readers
		b.Run(fmt.Sprintf("writes=%d/readers=%d", writes, readers), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				tx := &Transaction{}
				tx.nativeRange = st.native
				for i := 0; i < writes; i++ {
					j := (i * 7919) % storageKeys
					tx.Set(fmt.Appendf(nil, "t/%07d/w", j*4+1), []byte("entry"))
					if i%64 == 0 {
						_ = tx.ClearRange(fmt.Appendf(nil, "t/%07d/x", j*4+2), fmt.Appendf(nil, "t/%07d/y", j*4+2))
					}
				}
				ctx := context.Background()
				var wg sync.WaitGroup
				for r := 0; r < readers; r++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						for q := 0; q < perReader; q++ {
							j := ((r*perReader+q)*104729 + 13) % storageKeys
							key := fmt.Appendf(nil, "t/%07d/q", j*4)
							end := KeySelector{Key: key, OrEqual: true, Offset: 2}   // firstGreaterThan(k).add(1)
							begin := KeySelector{Key: key, OrEqual: true, Offset: 0} // lastLessOrEqual(k)
							var err error
							switch q % 4 {
							case 0:
								_, _, err = tx.Snapshot().GetRangeSelectors(ctx, begin, end, 0, 0, false)
							case 1:
								_, _, err = tx.GetRangeSelectors(ctx, begin, KeySelector{Key: key, OrEqual: true, Offset: 1}, 0, 0, false)
							case 2:
								_, err = tx.GetKey(ctx, key, true, 0)
							case 3:
								var rr rywRangeRead
								tx.ryw.issueRangeInto(&rr, key, fmt.Appendf(nil, "t/%07d/z", j*4+8), 0, 0, false, true)
								_, _, err = rr.run(ctx, st.exact)
							}
							if err != nil {
								b.Error(err)
								return
							}
							tx.Set(fmt.Appendf(key[:len(key):len(key)], "%d", q), []byte("entry"))
						}
					}()
				}
				wg.Wait()
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*readers*perReader), "ns/read")
		})
	}
}

// BenchmarkRYWBunchedMapPuts is TEXT index maintenance as the Record Layer runs
// it in a message transaction: pending writes, then BunchedMap puts with 128
// snapshot selector reads in flight (lastLessOrEqual(k) .. firstGreaterThan(k)+1
// under a cancellable context, as StartGetRangeSelectors issues them for the
// facade), each completed in issue order and followed by the put's writes: a
// Set of the bunch, a Clear of a split-off key and a write conflict key. One op
// is one transaction's puts; its pending writes are made outside the timer, and
// ns/put divides by the puts.
func BenchmarkRYWBunchedMapPuts(b *testing.B) {
	const (
		storageKeys = 50000
		puts        = 2048
		depth       = 128
	)
	st := newBenchStorage(storageKeys)
	value := bytes.Repeat([]byte("p"), 200)
	for _, writes := range []int{1000, 20000} {
		b.Run(fmt.Sprintf("writes=%d", writes), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				b.StopTimer()
				ctx, cancel := context.WithCancel(context.Background())
				tx := &Transaction{}
				tx.nativeRange = st.native
				for i := 0; i < writes; i++ {
					j := (i * 7919) % storageKeys
					tx.Set(fmt.Appendf(nil, "t/%07d/w", j*4+1), value)
				}
				b.StartTimer()
				type put struct {
					key []byte
					p   *PendingRange
				}
				ring := make([]put, 0, depth)
				finish := func(p put) {
					if _, _, err := p.p.Get(); err != nil {
						b.Fatal(err)
					}
					tx.Set(p.key, value)
					tx.Clear(append(p.key[:len(p.key):len(p.key)], 'x'))
					tx.AddWriteConflictKey(p.key)
				}
				for i := 0; i < puts; i++ {
					if len(ring) == depth {
						finish(ring[0])
						ring = ring[1:]
					}
					// Never before the first storage key: memStorage sets no readToBegin.
					key := fmt.Appendf(nil, "t/%07d/q", (1+(i*104729)%(storageKeys-1))*4)
					end := KeySelector{Key: key, OrEqual: true, Offset: 2}   // firstGreaterThan(k).add(1)
					begin := KeySelector{Key: key, OrEqual: true, Offset: 0} // lastLessOrEqual(k)
					ring = append(ring, put{key: key, p: tx.Snapshot().StartGetRangeSelectors(ctx, begin, end, 0, 0, false)})
				}
				for _, p := range ring {
					finish(p)
				}
				cancel()
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*puts), "ns/put")
		})
	}
}

// BenchmarkRYWCachedReads prices reads the RYW cache answers whole, under a
// cancellable context as the facade passes it: a snapshot selector read over a
// span already read, and a Get of a pending write. C++ answers both without a
// location lookup or a cancellation callback.
func BenchmarkRYWCachedReads(b *testing.B) {
	st := newBenchStorage(1000)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tx := &Transaction{}
	tx.nativeRange = st.native
	key := []byte("t/0000400/q")
	written := []byte("t/0000400/w")
	tx.Set(written, []byte("entry"))
	begin := KeySelector{Key: key, OrEqual: true, Offset: 0} // lastLessOrEqual(k)
	end := KeySelector{Key: key, OrEqual: true, Offset: 2}   // firstGreaterThan(k).add(1)
	if _, _, err := tx.Snapshot().GetRangeSelectors(ctx, begin, end, 0, 0, false); err != nil {
		b.Fatal(err)
	}
	b.Run("selector", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, _, err := tx.Snapshot().GetRangeSelectors(ctx, begin, end, 0, 0, false); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("get", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if v, err := tx.Snapshot().Get(ctx, written); err != nil || v == nil {
				b.Fatal(v, err)
			}
		}
	})
}

// BenchmarkCommitConflictVectors is a message transaction's BunchedMap puts as
// writes and conflicts (Java's per-put calls: a read conflict range, a Set, a
// Clear, a write conflict key and a write conflict range), then the vectors its
// commit sizes and ships. One op is one transaction; ns/put divides by the puts.
func BenchmarkCommitConflictVectors(b *testing.B) {
	const puts = 2048
	value := bytes.Repeat([]byte("p"), 200)
	keys := make([][]byte, puts)
	for i := range keys {
		keys[i] = fmt.Appendf(nil, "t/%07d/q", (1+(i*104729)%49999)*4)
	}
	b.ReportAllocs()
	for b.Loop() {
		tx := &Transaction{}
		for _, key := range keys {
			_ = tx.AddReadConflictRange(key, append(key[:len(key):len(key)], 0xff))
			tx.Set(key, value)
			tx.Clear(append(key[:len(key):len(key)], 'x'))
			tx.AddWriteConflictKey(key)
			_ = tx.AddWriteConflictRange(append(key[:len(key):len(key)], 'a'), append(key[:len(key):len(key)], 'b'))
		}
		tx.conflictMu.Lock()
		muts, writes, reads := tx.mutations, tx.writeConflicts, tx.readConflicts
		tx.conflictMu.Unlock()
		benchCommitVectors(tx, muts, writes, reads)
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*puts), "ns/put")
}

// benchCommitVectors is Commit's vector construction after validation.
func benchCommitVectors(tx *Transaction, muts []Mutation, writes, reads []KeyRange) {
	shipMuts, sized, coalesced := coalesceCommitVectors(muts, writes, false)
	shipReads := shipReadConflicts(reads, false)
	shipReads, sc, added := tx.selfConflictShipReads(sized, shipReads)
	ship := finalizeShipConflicts(sized, sc, added, coalesced)
	if len(shipMuts) == 0 || len(ship) == 0 || len(shipReads) == 0 {
		panic("empty commit vectors")
	}
}
