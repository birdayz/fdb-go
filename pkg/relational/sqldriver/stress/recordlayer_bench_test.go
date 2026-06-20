//go:build stress

package stress_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/birdayz/fdb-record-layer-go/pkg/relational/sqldriver"
)

func TestFDB_SQLParallelConnections(t *testing.T) {
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}

	// Each worker gets its OWN sql.DB (own connection pool, own FDB database handle).
	// This eliminates any shared-state contention in database/sql.
	type config struct {
		workers int
	}
	configs := []config{{1}, {4}, {8}}

	for _, cfg := range configs {
		cfg := cfg
		t.Run(fmt.Sprintf("w%d", cfg.workers), func(t *testing.T) {
			n := 500_000
			batchSize := 2000
			dbPath := fmt.Sprintf("/sqlpar_w%d", cfg.workers)

			// Setup: create database + schema using a setup connection.
			setup := func() {
				sysDSN := fmt.Sprintf("fdbsql:///__SYS?cluster_file=%s", clusterFilePath)
				sysDB, _ := sql.Open("fdbsql", sysDSN)
				defer sysDB.Close()
				sysDB.ExecContext(context.Background(), "CREATE DATABASE "+dbPath)
				tmpl := fmt.Sprintf("sqlpar_tmpl_w%d", cfg.workers)
				sysDB.ExecContext(context.Background(),
					fmt.Sprintf("CREATE SCHEMA TEMPLATE %s CREATE TABLE items (id BIGINT NOT NULL, val BIGINT NOT NULL, PRIMARY KEY (id))", tmpl))
				sysDB.ExecContext(context.Background(),
					fmt.Sprintf("CREATE SCHEMA %s/main WITH TEMPLATE %s", dbPath, tmpl))
			}
			setup()

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
					dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=main", dbPath, clusterFilePath)
					workerDB, openErr := sql.Open("fdbsql", dsn)
					if openErr != nil {
						firstErr.CompareAndSwap(nil, openErr)
						return
					}
					defer workerDB.Close()
					workerDB.SetMaxOpenConns(1)

					for offset := from; offset < to; offset += batchSize {
						end := offset + batchSize
						if end > to {
							end = to
						}
						var rows []string
						for i := offset; i < end; i++ {
							rows = append(rows, fmt.Sprintf("(%d, %d)", i, i*7))
						}
						stmt := fmt.Sprintf("INSERT INTO items VALUES %s", strings.Join(rows, ", "))
						if _, execErr := workerDB.ExecContext(context.Background(), stmt); execErr != nil {
							firstErr.CompareAndSwap(nil, execErr)
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
			t.Logf("SQL PARALLEL CONNS w=%d: %d rows in %v (%.0f rows/s)",
				cfg.workers, n, elapsed, float64(n)/elapsed.Seconds())
		})
	}
}
