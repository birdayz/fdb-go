//go:build libfdbc

package main

/*
#cgo LDFLAGS: -lfdb_c
#define FDB_API_VERSION 730
#include <foundationdb/fdb_c.h>
#include <stdatomic.h>
#include <stdint.h>
#include <stdlib.h>
#include <unistd.h>

typedef struct {
	FDBTransaction* gate_tr;
	FDBTransaction* pre_tr;
	FDBFuture* gate_f;
	FDBFuture* pre_f;
	atomic_int parked;
	atomic_int release;
	atomic_int pre_done;
} barrier_probe_t;

static void on_pre_ready(FDBFuture* f, void* param) {
	barrier_probe_t* p = (barrier_probe_t*)param;
	atomic_store(&p->pre_done, 1);
}

static void on_gate_ready(FDBFuture* f, void* param) {
	barrier_probe_t* p = (barrier_probe_t*)param;
	p->pre_f = fdb_transaction_get_approximate_size(p->pre_tr);
	fdb_future_set_callback(p->pre_f, on_pre_ready, p);
	atomic_store(&p->parked, 1);
	while (atomic_load(&p->release) == 0) {
		usleep(100);
	}
}

static fdb_error_t start_barrier_probe(FDBDatabase* db, barrier_probe_t* p) {
	p->gate_tr = NULL;
	p->pre_tr = NULL;
	p->gate_f = NULL;
	p->pre_f = NULL;
	atomic_init(&p->parked, 0);
	atomic_init(&p->release, 0);
	atomic_init(&p->pre_done, 0);
	fdb_error_t err = fdb_database_create_transaction(db, &p->gate_tr);
	if (err != 0) return err;
	err = fdb_database_create_transaction(db, &p->pre_tr);
	if (err != 0) {
		fdb_transaction_destroy(p->gate_tr);
		return err;
	}
	int64_t delay_ms = 1;
	fdb_transaction_set_option(p->gate_tr, FDB_TR_OPTION_MAX_RETRY_DELAY, (const uint8_t*)&delay_ms, sizeof(delay_ms));
	p->gate_f = fdb_transaction_on_error(p->gate_tr, 1020);
	err = fdb_future_set_callback(p->gate_f, on_gate_ready, p);
	if (err != 0) {
		fdb_future_destroy(p->gate_f);
		fdb_transaction_destroy(p->pre_tr);
		fdb_transaction_destroy(p->gate_tr);
		return err;
	}
	while (atomic_load(&p->parked) == 0) {
		usleep(100);
	}
	return 0;
}

static int probe_pre_done(barrier_probe_t* p) {
	return atomic_load(&p->pre_done);
}

static void release_barrier_probe(barrier_probe_t* p) {
	atomic_store(&p->release, 1);
}

static int finish_barrier_probe(barrier_probe_t* p) {
	int done = atomic_load(&p->pre_done);
	atomic_store(&p->release, 1);
	if (p->gate_f != NULL) {
		fdb_future_block_until_ready(p->gate_f);
		fdb_future_destroy(p->gate_f);
	}
	if (p->pre_f != NULL) {
		fdb_future_block_until_ready(p->pre_f);
		fdb_future_destroy(p->pre_f);
	}
	if (p->pre_tr != NULL) fdb_transaction_destroy(p->pre_tr);
	if (p->gate_tr != NULL) fdb_transaction_destroy(p->gate_tr);
	return done;
}
*/
import "C"

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"

	"fdb.dev/tools/clientbench/internal/bench"
	"github.com/apple/foundationdb/bindings/go/src/fdb"
)

type (
	backend struct {
		db        fdb.Database
		cDB       *C.FDBDatabase
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
	if cfg.CoalesceDelay != "" || cfg.AdaptiveLinger != 0 {
		return nil, nil, errors.New("coalesce-delay and adaptive-linger apply to the Go client only")
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
	var cCluster *C.char
	if cfg.ClusterFile != "" {
		cCluster = C.CString(cfg.ClusterFile)
		defer C.free(unsafe.Pointer(cCluster))
	}
	var cDB *C.FDBDatabase
	if cerr := C.fdb_create_database(cCluster, &cDB); cerr != 0 {
		db.Close()
		return nil, nil, fmt.Errorf("fdb_create_database: %d", int(cerr))
	}
	return &backend{db: db, cDB: cDB}, map[string]any{"backend": "cgo", "api_version": 730, "native_library": true, "native_client_version": C.GoString(C.fdb_get_client_version())}, nil
}

func (b *backend) Close() {
	b.closeOnce.Do(func() {
		if b.cDB != nil {
			C.fdb_database_destroy(b.cDB)
			b.cDB = nil
		}
		b.db.Close()
	})
}

func (b *backend) Barrier() error {
	// Queue a predecessor on the parked native network thread, verify synchronous
	// GetCommittedVersion does not drain it, then queue GetApproximateSize behind it.
	var probe C.barrier_probe_t
	if cerr := C.start_barrier_probe(b.cDB, &probe); cerr != 0 {
		return fmt.Errorf("start_barrier_probe: %d", int(cerr))
	}
	tx, err := b.db.CreateTransaction()
	if err != nil {
		C.finish_barrier_probe(&probe)
		return err
	}
	defer tx.Cancel()
	if _, err = tx.GetCommittedVersion(); err != nil {
		C.finish_barrier_probe(&probe)
		return err
	}
	fut := tx.GetApproximateSize()
	if C.probe_pre_done(&probe) != 0 || fut.IsReady() {
		C.finish_barrier_probe(&probe)
		return errors.New("native FIFO predecessor completed before barrier future was queued")
	}
	C.release_barrier_probe(&probe)
	_, err = fut.Get()
	preDone := C.finish_barrier_probe(&probe)
	if err != nil {
		return err
	}
	if preDone == 0 {
		return errors.New("native barrier returned before queued onMainThread predecessor completed")
	}
	return nil
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
