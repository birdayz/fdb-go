package otel

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
)

type delayedBootstrapTransactor struct {
	*retryInjector
	once    sync.Once
	delayed atomic.Bool
}

func (d *delayedBootstrapTransactor) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return d.TransactCtx(context.Background(), fn)
}

func (d *delayedBootstrapTransactor) TransactCtx(ctx context.Context, fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return d.retryInjector.TransactCtx(ctx, func(tx fdb.WritableTransaction) (any, error) {
		d.once.Do(func() {
			timer := time.NewTimer(benchTxnTimeout + 100*time.Millisecond)
			defer timer.Stop()
			select {
			case <-timer.C:
				d.delayed.Store(true)
			case <-ctx.Done():
			}
		})
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return fn(tx)
	})
}

func TestExporterBootstrapCarriesPolicyIntoFreshResolvers(t *testing.T) {
	t.Parallel()
	cluster := startExporterCluster(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	cfg := DefaultExporterConfig()
	cfg.ClusterFile = cluster
	cfg.Run = freshRun(t)
	cfg.TransactionTimeout = time.Minute
	var inject *delayedBootstrapTransactor
	e, err := newExporter(ctx, cfg, func(openCtx context.Context, c Config, cluster string) (*FDBBackend, error) {
		if !isExporterTxn(openCtx) || txnTimeout(openCtx) != cfg.TransactionTimeout {
			t.Fatal("constructor opened backend before attaching exporter policy")
		}
		return openFDBContext(openCtx, c, cluster, func(raw fdb.Database) *recordlayer.FDBDatabase {
			inject = &delayedBootstrapTransactor{retryInjector: &retryInjector{Database: raw, code: 1009, k: 12}}
			return recordlayer.NewFDBDatabaseWithTransactor(inject, raw)
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Shutdown(ctx)
	if inject == nil || !inject.delayed.Load() {
		t.Fatal("first owner read did not cross the benchmark timeout boundary")
	}
	inject.mu.Lock()
	defer inject.mu.Unlock()
	if inject.transactions < 4 || inject.minimum != 13 {
		t.Fatalf("bootstrap transactions=%d, minimum executions=%d; owner, fresh resolver children and live initialization must each absorb 12 retryable failures", inject.transactions, inject.minimum)
	}
}
