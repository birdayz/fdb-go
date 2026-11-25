package client

import (
	"bytes"
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// BenchmarkTransactionAtomic prices Transaction.Atomic alone: BunchedMap's 24-byte appends to
// 256 bunches (half Set first) or a counter's 8-byte ADD; one op is one transaction.
func BenchmarkTransactionAtomic(b *testing.B) {
	const (
		keys   = 256
		rounds = 32
	)
	prefix := benchLongPrefix
	ks := make([][]byte, keys)
	for i := range ks {
		ks[i] = fmt.Appendf(nil, "%st/%07d/s", prefix, i*4)
	}
	entry := bytes.Repeat([]byte("e"), 24)
	one := []byte{1, 0, 0, 0, 0, 0, 0, 0}
	for _, tc := range []struct {
		name    string
		op      MutationType
		operand []byte
	}{{"append", MutAppendIfFits, entry}, {"add", MutAddValue, one}} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				tx := &Transaction{}
				for i := 0; i < keys; i += 2 {
					tx.Set(ks[i], entry)
				}
				for r := 0; r < rounds; r++ {
					for _, k := range ks {
						tx.Atomic(tc.op, k, tc.operand)
					}
				}
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*keys*rounds), "ns/atomic")
		})
	}
}

// BenchmarkQueueModelParallel is a load-balanced read's QueueModel bookkeeping from many
// goroutines, over one team of three storage servers or four teams of twelve.
func BenchmarkQueueModelParallel(b *testing.B) {
	for _, teams := range []int{1, 4} {
		b.Run(fmt.Sprintf("servers=%d", 3*teams), func(b *testing.B) {
			q := newQueueModel()
			shards := make([][]ServerInfo, teams)
			for t := range shards {
				for r := 0; r < 3; r++ {
					shards[t] = append(shards[t], ServerInfo{Address: fmt.Sprintf("10.0.0.%d:4500", 3*t+r)})
				}
			}
			var sink, next atomic.Int64
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				var delays time.Duration
				shard := int(next.Add(1))
				for pb.Next() {
					shard++
					servers := shards[shard%teams]
					best, second := q.chooseTopTwo(servers)
					addr := servers[best].Address
					delta := q.startRequest(addr)
					if second >= 0 {
						delays += q.secondDelay(addr, servers[second].Address)
					}
					q.firstReplied()
					q.endRequestFull(addr, delta, 300*time.Microsecond, true, false, 1.0)
				}
				sink.Add(int64(delays))
			})
		})
	}
}

// BenchmarkReadOperationContext prices a read's admission under a shared cancellable
// caller context, with and without the merged context a wait on Done derives.
func BenchmarkReadOperationContext(b *testing.B) {
	for _, wait := range []bool{false, true} {
		b.Run(fmt.Sprintf("done=%t", wait), func(b *testing.B) {
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			tx := &Transaction{}
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					ctx, release, _ := tx.opContextWithLease(parent, false)
					if wait {
						select {
						case <-ctx.Done():
							b.Error("cancelled")
						default:
						}
					}
					release.release()
				}
			})
		})
	}
}
