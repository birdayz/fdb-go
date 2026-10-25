//go:build libfdbc

package main

/*
#cgo LDFLAGS: -lfdb_c
#define FDB_API_VERSION 730
#include <foundationdb/fdb_c.h>
*/
import "C"

import (
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"fdb.dev/tools/clientbench/internal/bench"
	"github.com/apple/foundationdb/bindings/go/src/fdb"
)

type (
	backend struct {
		db fdb.Database
		// dbs holds -databases handles (db is dbs[0]); next picks the one a
		// transaction starts on, so load spreads over libfdb_c network threads.
		dbs       []fdb.Database
		next      atomic.Uint64
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
	if cfg.CoalesceDelay != "" {
		return nil, nil, errors.New("coalesce-delay applies to the Go client only")
	}
	if cfg.CGoExternalClient != "" {
		if err := fdb.Options().SetExternalClientLibrary(cfg.CGoExternalClient); err != nil {
			return nil, nil, err
		}
	}
	if cfg.CGoClientThreads > 0 {
		// MultiVersionApi::setupNetwork disables the local client when threadCount > 1.
		if err := fdb.Options().SetClientThreadsPerVersion(int64(cfg.CGoClientThreads)); err != nil {
			return nil, nil, err
		}
	}
	b := &backend{}
	open := func() (fdb.Database, error) { return fdb.OpenDatabase(cfg.ClusterFile) }
	if cfg.Databases > 1 {
		// fdb.OpenDatabase caches one handle per cluster file. Each connection-string
		// open is its own MultiVersionApi::createDatabase, which assigns network
		// threads round-robin (MultiVersionTransaction.actor.cpp:3302-3305).
		conn, err := os.ReadFile(cfg.ClusterFile)
		if err != nil {
			return nil, nil, err
		}
		connectionString := strings.TrimSpace(string(conn))
		open = func() (fdb.Database, error) { return fdb.OpenWithConnectionString(connectionString) }
	}
	for range max(cfg.Databases, 1) {
		db, err := open()
		if err == nil {
			b.dbs = append(b.dbs, db)
			if err = db.Options().SetTransactionTimeout(cfg.TimeoutMS); err == nil {
				err = db.Options().SetTransactionRetryLimit(cfg.RetryLimit)
			}
		}
		if err != nil {
			b.Close()
			return nil, nil, err
		}
	}
	b.db = b.dbs[0]
	return b, map[string]any{
		"backend": "cgo", "api_version": 730, "native_library": true, "native_client_version": C.GoString(C.fdb_get_client_version()),
		"cgo_client_threads": cfg.CGoClientThreads, "cgo_external_client": cfg.CGoExternalClient, "databases": len(b.dbs),
	}, nil
}

// pick returns the database the next transaction starts on.
func (b *backend) pick() fdb.Database {
	if len(b.dbs) == 1 {
		return b.db
	}
	return b.dbs[(b.next.Add(1)-1)%uint64(len(b.dbs))]
}

func (b *backend) Close() {
	b.closeOnce.Do(func() {
		for _, db := range b.dbs {
			db.Close()
		}
	})
}

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
		_, err := b.pick().ReadTransact(func(tx fdb.ReadTransaction) (any, error) { return nil, fn(transaction{read: tx}) })
		return err
	}
	_, err := b.pick().Transact(func(tx fdb.Transaction) (any, error) { return nil, fn(transaction{tx, tx}) })
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
