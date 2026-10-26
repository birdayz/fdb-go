//go:build !libfdbc

package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"fdb.dev/pkg/fdbgo/client"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/tools/clientbench/internal/bench"
)

type backend struct {
	db  fdb.Database
	raw *client.Database
}
type transaction struct {
	read  fdb.ReadTransaction
	write fdb.WritableTransaction
}

func openBackend(cfg bench.Config) (bench.LifecycleDatabase, map[string]any, error) {
	if err := fdb.APIVersion(730); err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	opts := []client.Option{client.WithAPIVersion(730)}
	if cfg.AdaptiveLinger > 0 {
		opts = append(opts, client.WithAdaptiveLinger(cfg.AdaptiveLinger))
	}
	if cfg.CoalesceDelay != "" {
		if after, ok := strings.CutPrefix(cfg.CoalesceDelay, "adaptive:"); ok {
			d, err := time.ParseDuration(after)
			if err != nil || d < 0 {
				return nil, nil, fmt.Errorf("coalesce-delay %q: want adaptive:<duration> or min,max durations", cfg.CoalesceDelay)
			}
			opts = append(opts, client.WithAdaptiveLinger(d))
		} else {
			lo, hi, ok := strings.Cut(cfg.CoalesceDelay, ",")
			minDelay, err1 := time.ParseDuration(lo)
			maxDelay, err2 := time.ParseDuration(hi)
			if !ok || err1 != nil || err2 != nil {
				return nil, nil, fmt.Errorf("coalesce-delay %q: want min,max durations", cfg.CoalesceDelay)
			}
			opts = append(opts, client.WithCoalesceDelay(minDelay, maxDelay))
		}
	}
	raw, err := client.OpenDatabase(ctx, cfg.ClusterFile, opts...)
	if err != nil {
		return nil, nil, err
	}
	db := fdb.WrapDatabase(raw)
	if err = db.Options().SetTransactionTimeout(cfg.TimeoutMS); err != nil {
		db.Close()
		return nil, nil, err
	}
	if err = db.Options().SetTransactionRetryLimit(cfg.RetryLimit); err != nil {
		db.Close()
		return nil, nil, err
	}
	meta := map[string]any{"backend": "go", "api_version": 730, "native_library": false, "coalesce_delay": cfg.CoalesceDelay}
	if cfg.AdaptiveLinger > 0 {
		meta["adaptive_linger_ns"] = int64(cfg.AdaptiveLinger)
	}
	return &backend{db: db, raw: raw}, meta, nil
}

func (b *backend) Close()         { b.db.Close() }
func (b *backend) Barrier() error { return nil }
func (b *backend) Shutdown() (bool, error) {
	// Database.Close waits for background workers and all transport loops.
	return false, b.raw.Close()
}

func (b *backend) Counters() bench.ClientCounters {
	m := b.raw.Metrics()
	return bench.ClientCounters{Observation: "observed", Values: map[string]int64{
		"grv_cache_hits":               m.GRVCacheHits,
		"read_versions_completed":      m.TransactionReadVersionsCompleted,
		"grv_successful_batch_replies": m.GRVLatency.Count,
		"grv_in_band_maybe_delivered":  m.GRVInBandMaybeDelivered,
		"transaction_retries":          m.TransactionRetries,
		"commit_started":               m.TransactionsCommitStarted,
		"commit_completed":             m.TransactionsCommitCompleted,
		"connection_failures":          m.ClientConnectionFailures,
	}}
}

func (b *backend) Transact(readOnly bool, fn func(bench.Transaction) error) error {
	if readOnly {
		_, err := b.db.ReadTransact(func(tx fdb.ReadTransaction) (any, error) { return nil, fn(transaction{read: tx}) })
		return err
	}
	_, err := b.db.Transact(func(tx fdb.WritableTransaction) (any, error) { return nil, fn(transaction{tx, tx}) })
	return err
}

func (b *backend) Local(fn func(bench.Transaction) error) error {
	tx, err := b.db.CreateTransaction()
	if err != nil {
		return err
	}
	defer tx.Cancel()
	return fn(transaction{tx, tx})
}
func (t transaction) Get(key []byte) ([]byte, error) { return t.read.Get(fdb.Key(key)).Get() }
func (t transaction) Set(key, value []byte)          { t.write.Set(fdb.Key(key), value) }
func (t transaction) ReadVersion() (int64, error)    { return t.read.GetReadVersion().Get() }
func (t transaction) Range(begin, end []byte, limit int) ([]bench.KV, error) {
	rows, err := t.read.GetRange(fdb.KeyRange{Begin: fdb.Key(begin), End: fdb.Key(end)}, fdb.RangeOptions{Limit: limit, Mode: fdb.StreamingModeWantAll}).GetSliceWithError()
	if err != nil {
		return nil, err
	}
	out := make([]bench.KV, len(rows))
	for i, row := range rows {
		out[i] = bench.KV{Key: []byte(row.Key), Value: row.Value}
	}
	return out, nil
}
