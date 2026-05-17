package stress_test

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	purefdb "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/client"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
)

func TestFDB_RawIngestBench(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}

	ctx0, cancel0 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel0()
	db, err := purefdb.OpenDatabase(ctx0, clusterFilePath)
	if err != nil {
		t.Fatalf("OpenDatabase: %v", err)
	}
	defer db.Close()

	type config struct {
		workers   int
		batchSize int
	}
	configs := []config{
		{1, 500},
		{1, 2000},
		{4, 500},
		{4, 2000},
		{8, 2000},
		{16, 2000},
		{32, 2000},
	}

	for _, cfg := range configs {
		cfg := cfg
		t.Run(fmt.Sprintf("w%d_b%d", cfg.workers, cfg.batchSize), func(t *testing.T) {
			n := 1_000_000
			prefix := []byte(fmt.Sprintf("bench_w%d_b%d_", cfg.workers, cfg.batchSize))

			start := time.Now()
			var totalWritten atomic.Int64
			chunkSize := (n + cfg.workers - 1) / cfg.workers

			var wg sync.WaitGroup
			var firstErr atomic.Value

			for w := range cfg.workers {
				wStart := w * chunkSize
				wEnd := wStart + chunkSize
				if wEnd > n {
					wEnd = n
				}
				if wStart >= n {
					break
				}
				wg.Add(1)
				go func(from, to int) {
					defer wg.Done()
					for offset := from; offset < to; offset += cfg.batchSize {
						end := offset + cfg.batchSize
						if end > to {
							end = to
						}
						ctx := context.Background()
						tx := db.CreateTransaction()
						for i := offset; i < end; i++ {
							key := append(append([]byte{}, prefix...), tuple.Tuple{int64(i)}.Pack()...)
							val := make([]byte, 8)
							binary.LittleEndian.PutUint64(val, uint64(i*7))
							tx.Set(key, val)
						}
						if commitErr := tx.Commit(ctx); commitErr != nil {
							firstErr.CompareAndSwap(nil, commitErr)
							return
						}
						totalWritten.Add(int64(end - offset))
					}
				}(wStart, wEnd)
			}
			wg.Wait()

			if v := firstErr.Load(); v != nil {
				t.Fatalf("error: %v (wrote %d/%d)", v, totalWritten.Load(), n)
			}

			elapsed := time.Since(start)
			t.Logf("RAW FDB w=%d b=%d: %d rows in %v (%.0f rows/s)",
				cfg.workers, cfg.batchSize, n, elapsed, float64(n)/elapsed.Seconds())
		})
	}
}
