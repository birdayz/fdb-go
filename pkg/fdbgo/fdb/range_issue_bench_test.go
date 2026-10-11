package fdb_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
)

// BenchmarkGetRangeFacade measures one small GetRange+GetSliceWithError. local-hit is answered
// from the transaction's RYW cache; remote reads a range this transaction has not read.
func BenchmarkGetRangeFacade(b *testing.B) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	db, err := fdb.OpenDatabaseFromConfig(ctx, sharedClusterFile)
	cancel()
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	const keys = 512
	key := func(i int) fdb.Key { return fdb.Key(fmt.Sprintf("bench_getrange_facade/%04d", i)) }
	if _, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		for i := 0; i < keys; i++ {
			tr.Set(key(i), []byte("value"))
		}
		return nil, nil
	}); err != nil {
		b.Fatal(err)
	}
	newTx := func() fdb.Transaction {
		tr, err := db.CreateTransaction()
		if err != nil {
			b.Fatal(err)
		}
		if _, err := tr.GetReadVersion().Get(); err != nil {
			b.Fatal(err)
		}
		return tr
	}
	b.Run("local-hit", func(b *testing.B) {
		r := fdb.KeyRange{Begin: key(0), End: key(4)}
		warm := func() fdb.Transaction {
			tr := newTx()
			if _, err := tr.GetRange(r, fdb.RangeOptions{}).GetSliceWithError(); err != nil {
				b.Fatal(err)
			}
			return tr
		}
		tr := warm()
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if i%keys == 0 && i > 0 {
				b.StopTimer()
				tr.Cancel()
				tr = warm()
				b.StartTimer()
			}
			if _, err := tr.GetRange(r, fdb.RangeOptions{}).GetSliceWithError(); err != nil {
				b.Fatal(err)
			}
		}
		tr.Cancel()
	})
	b.Run("remote", func(b *testing.B) {
		tr := newTx()
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			j := i % keys
			if j == 0 && i > 0 {
				b.StopTimer()
				tr.Cancel()
				tr = newTx()
				b.StartTimer()
			}
			if _, err := tr.GetRange(fdb.KeyRange{Begin: key(j), End: key(j + 1)}, fdb.RangeOptions{}).GetSliceWithError(); err != nil {
				b.Fatal(err)
			}
		}
		tr.Cancel()
	})
	b.Run("local-hit-parallel", func(b *testing.B) {
		r := fdb.KeyRange{Begin: key(0), End: key(4)}
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			tr := newTx()
			if _, err := tr.GetRange(r, fdb.RangeOptions{}).GetSliceWithError(); err != nil {
				b.Error(err)
				return
			}
			for i := 1; pb.Next(); i++ {
				if i%keys == 0 {
					tr.Cancel()
					tr = newTx()
				}
				if _, err := tr.GetRange(r, fdb.RangeOptions{}).GetSliceWithError(); err != nil {
					b.Error(err)
					return
				}
			}
			tr.Cancel()
		})
	})
	b.Run("remote-parallel", func(b *testing.B) {
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			tr := newTx()
			for i := 1; pb.Next(); i++ {
				j := i % keys
				if j == 0 {
					tr.Cancel()
					tr = newTx()
				}
				if _, err := tr.GetRange(fdb.KeyRange{Begin: key(j), End: key(j + 1)}, fdb.RangeOptions{}).GetSliceWithError(); err != nil {
					b.Error(err)
					return
				}
			}
			tr.Cancel()
		})
	})
}

// BenchmarkGetRangeWriteHeavy mirrors the R-tree's insert pattern: one transaction holding N
// written keys (cleared node ranges refilled slot by slot) reads small node ranges back.
func BenchmarkGetRangeWriteHeavy(b *testing.B) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	db, err := fdb.OpenDatabaseFromConfig(ctx, sharedClusterFile)
	cancel()
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	const slots = 100 // keys per node
	for _, setFirst := range []bool{false, true} {
		for _, n := range []int{1000, 10000, 50000} {
			b.Run(fmt.Sprintf("set-first=%v/writes=%d", setFirst, n), func(b *testing.B) {
				tr, err := db.CreateTransaction()
				if err != nil {
					b.Fatal(err)
				}
				defer tr.Cancel()
				node := func(i int) fdb.KeyRange {
					p := fdb.Key(fmt.Sprintf("bench_write_heavy/%06d/", i))
					return fdb.KeyRange{Begin: p, End: append(append(fdb.Key{}, p...), 0xff)}
				}
				nodes := n / slots
				for i := 0; i < nodes; i++ {
					r := node(i)
					tr.ClearRange(r)
					for s := 0; s < slots; s++ {
						tr.Set(append(append(fdb.Key{}, r.Begin.(fdb.Key)...), fmt.Sprintf("%03d", s)...), []byte("slot"))
					}
				}
				opts := fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					r := node(i % nodes)
					if setFirst { // an insert rewrites a slot, then re-reads its node
						tr.Set(append(append(fdb.Key{}, r.Begin.(fdb.Key)...), "000"...), []byte("slot"))
					}
					kvs, err := tr.GetRange(r, opts).GetSliceWithError()
					if err != nil || len(kvs) != slots {
						b.Fatalf("node read = %d rows, %v", len(kvs), err)
					}
				}
			})
		}
	}
}
