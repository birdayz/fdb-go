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
