//go:build cgo

package main

/*
#cgo LDFLAGS: -lfdb_c
#define FDB_API_VERSION 730
#include <foundationdb/fdb_c.h>
*/
import "C"

import (
	"sync"

	"fdb.dev/tools/clientbench/internal/bench"
	"github.com/apple/foundationdb/bindings/go/src/fdb"
)

type (
	backend struct {
		db        fdb.Database
		closeOnce sync.Once
	}
	transaction struct {
		read  fdb.ReadTransaction
		write fdb.Transaction
	}
)

func openBackend(cfg bench.Config) (bench.LifecycleDatabase, map[string]any, error) {
	if err := fdb.APIVersion(730); err != nil {
		return nil, nil, err
	}
	db, err := fdb.OpenDatabase(cfg.ClusterFile)
	if err != nil {
		return nil, nil, err
	}
	if err = db.Options().SetTransactionTimeout(cfg.TimeoutMS); err != nil {
		db.Close()
		return nil, nil, err
	}
	if err = db.Options().SetTransactionRetryLimit(cfg.RetryLimit); err != nil {
		db.Close()
		return nil, nil, err
	}
	return &backend{db: db}, map[string]any{"backend": "cgo", "api_version": 730, "native_library": true, "native_client_version": C.GoString(C.fdb_get_client_version())}, nil
}

func (b *backend) Close() { b.closeOnce.Do(b.db.Close) }

func (b *backend) Barrier() error {
	// This non-I/O future follows finalizer-posted destructors on the native FIFO.
	tx, err := b.db.CreateTransaction()
	if err != nil {
		return err
	}
	_, err = tx.GetApproximateSize().Get()
	return err
}

func (b *backend) Shutdown() (bool, error) {
	b.Close()
	err := fdb.StopNetwork()
	return err == nil, err
}

func (b *backend) Counters() bench.ClientCounters {
	return bench.ClientCounters{Observation: "unobserved"}
}

func (b *backend) Transact(readOnly bool, fn func(bench.Transaction) error) error {
	if readOnly {
		_, err := b.db.ReadTransact(func(tx fdb.ReadTransaction) (any, error) { return nil, fn(transaction{read: tx}) })
		return err
	}
	_, err := b.db.Transact(func(tx fdb.Transaction) (any, error) { return nil, fn(transaction{tx, tx}) })
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
