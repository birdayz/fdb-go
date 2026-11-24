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

	"fdb.dev/pkg/relational/sqldriver"
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
			dbPath := fmt.Sprintf("/FRL/sqlpar_w%d", cfg.workers)

			// Setup: create database + schema using a setup engine.
			setup := func() {
				sysEng, err := sqldriver.NewEngine(sqldriver.EngineConfig{
					ClusterFile: clusterFilePath,
					Domains:     []string{"FRL"},
					Limits:      sqldriver.UnboundedJavaCompatLimits(),
				})
				if err != nil {
					t.Fatalf("setup NewEngine: %v", err)
				}
				defer sysEng.Close()
				sysDB, err := sysEng.OpenDB("/__SYS")
				if err != nil {
					t.Fatalf("setup OpenDB: %v", err)
				}
				defer sysDB.Close()
				sysDB.ExecContext(context.Background(), "CREATE DATABASE "+dbPath)
				tmpl := fmt.Sprintf("sqlpar_tmpl_w%d", cfg.workers)
				sysDB.ExecContext(context.Background(),
					fmt.Sprintf("CREATE SCHEMA TEMPLATE %s CREATE TABLE items (id BIGINT, val BIGINT, PRIMARY KEY (id))", tmpl))
				sysDB.ExecContext(context.Background(),
					fmt.Sprintf("CREATE SCHEMA %s/main WITH TEMPLATE %s", dbPath, tmpl))
			}
			setup()

			type workerSlot struct {
				db       *sql.DB
				from, to int
			}
			chunkSize := (n + cfg.workers - 1) / cfg.workers
			var workers []workerSlot
			for w := range cfg.workers {
				wStart := w * chunkSize
				wEnd := wStart + chunkSize
				if wEnd > n {
					wEnd = n
				}
				if wStart >= n {
					break
				}
				wEng, engErr := sqldriver.NewEngine(sqldriver.EngineConfig{
					ClusterFile: clusterFilePath,
					Domains:     []string{"FRL"},
					Limits:      sqldriver.UnboundedJavaCompatLimits(),
				})
				if engErr != nil {
					t.Fatalf("worker %d NewEngine: %v", w, engErr)
				}
				defer func(e *sqldriver.Engine) { _ = e.Close() }(wEng)
				workerDB, openErr := wEng.OpenDB(strings.ToUpper(dbPath), sqldriver.WithSchema("MAIN"))
				if openErr != nil {
					t.Fatalf("worker %d OpenDB: %v", w, openErr)
				}
				defer func(db *sql.DB) { _ = db.Close() }(workerDB)
				workerDB.SetMaxOpenConns(1)
				workers = append(workers, workerSlot{db: workerDB, from: wStart, to: wEnd})
			}

			start := time.Now()
			var totalWritten atomic.Int64
			var wg sync.WaitGroup
			var firstErr firstStressError

			for _, ws := range workers {
				wg.Add(1)
				go func(ws workerSlot) {
					defer wg.Done()
					for offset := ws.from; offset < ws.to; offset += batchSize {
						end := offset + batchSize
						if end > ws.to {
							end = ws.to
						}
						var rows []string
						for i := offset; i < end; i++ {
							rows = append(rows, fmt.Sprintf("(%d, %d)", i, i*7))
						}
						stmt := fmt.Sprintf("INSERT INTO items VALUES %s", strings.Join(rows, ", "))
						if _, execErr := ws.db.ExecContext(context.Background(), stmt); execErr != nil {
							firstErr.Record(execErr)
							return
						}
						totalWritten.Add(int64(end - offset))
					}
				}(ws)
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
