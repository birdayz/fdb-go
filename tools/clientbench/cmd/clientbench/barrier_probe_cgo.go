//go:build libfdbc

package main

/*
#cgo LDFLAGS: -lfdb_c -lpthread
#define FDB_API_VERSION 730
#include <foundationdb/fdb_c.h>
#include <pthread.h>
#include <stdatomic.h>
#include <stdint.h>
#include <stdlib.h>
#include <unistd.h>

typedef struct {
	FDBTransaction* gate_tr;
	FDBTransaction* pre_tr;
	FDBFuture* gate_f;
	FDBFuture* pre_f;
	pthread_t caller_tid;
	atomic_int ran_inline;
	atomic_int parked;
	atomic_int release;
	atomic_int seq;
	atomic_int pre_seq;
	atomic_int return_seq;
	atomic_int pre_done;
	atomic_int pre_err;
	atomic_int gate_cb_done;
	atomic_int pre_cb_done;
} barrier_probe_t;

static void on_pre_ready(FDBFuture* f, void* param) {
	barrier_probe_t* p = (barrier_probe_t*)param;
	int s = atomic_fetch_add(&p->seq, 1) + 1;
	atomic_store(&p->pre_seq, s);
	atomic_store(&p->pre_done, 1);
	atomic_store(&p->pre_cb_done, 1);
}

static void on_gate_ready(FDBFuture* f, void* param) {
	barrier_probe_t* p = (barrier_probe_t*)param;
	if (pthread_equal(pthread_self(), p->caller_tid)) {
		atomic_store(&p->ran_inline, 1);
		atomic_store(&p->gate_cb_done, 1);
		return;
	}
	p->pre_f = fdb_transaction_get_approximate_size(p->pre_tr);
	fdb_error_t err = fdb_future_set_callback(p->pre_f, on_pre_ready, p);
	if (err != 0) {
		atomic_store(&p->pre_err, (int)err);
		atomic_store(&p->pre_cb_done, 1);
	}
	atomic_store(&p->parked, 1);
	while (atomic_load(&p->release) == 0) {
		usleep(100);
	}
	atomic_store(&p->gate_cb_done, 1);
}

static void cleanup_probe_attempt(barrier_probe_t* p) {
	atomic_store(&p->release, 1);
	if (p->gate_f != NULL) {
		while (atomic_load(&p->gate_cb_done) == 0) {
			usleep(100);
		}
		fdb_future_block_until_ready(p->gate_f);
		fdb_future_destroy(p->gate_f);
		p->gate_f = NULL;
	}
	if (p->pre_f != NULL) {
		while (atomic_load(&p->pre_cb_done) == 0) {
			usleep(100);
		}
		fdb_future_block_until_ready(p->pre_f);
		fdb_future_destroy(p->pre_f);
		p->pre_f = NULL;
	}
	if (p->pre_tr != NULL) {
		fdb_transaction_destroy(p->pre_tr);
		p->pre_tr = NULL;
	}
	if (p->gate_tr != NULL) {
		fdb_transaction_destroy(p->gate_tr);
		p->gate_tr = NULL;
	}
}

static fdb_error_t start_barrier_probe(FDBDatabase* db, int64_t initial_delay_ms, barrier_probe_t** out_p) {
	barrier_probe_t* p = (barrier_probe_t*)calloc(1, sizeof(barrier_probe_t));
	if (p == NULL) return 4100;
	int64_t delay_ms = initial_delay_ms;
	for (int attempt = 0; attempt < 8; attempt++) {
		atomic_init(&p->ran_inline, 0);
		atomic_init(&p->parked, 0);
		atomic_init(&p->release, 0);
		atomic_init(&p->seq, 0);
		atomic_init(&p->pre_seq, 0);
		atomic_init(&p->return_seq, 0);
		atomic_init(&p->pre_done, 0);
		atomic_init(&p->pre_err, 0);
		atomic_init(&p->gate_cb_done, 0);
		atomic_init(&p->pre_cb_done, 0);
		p->caller_tid = pthread_self();

		fdb_error_t err = fdb_database_create_transaction(db, &p->gate_tr);
		if (err != 0) {
			free(p);
			return err;
		}
		err = fdb_database_create_transaction(db, &p->pre_tr);
		if (err != 0) {
			cleanup_probe_attempt(p);
			free(p);
			return err;
		}
		if (delay_ms > 0) {
			fdb_transaction_set_option(p->gate_tr, FDB_TR_OPTION_MAX_RETRY_DELAY, (const uint8_t*)&delay_ms, sizeof(delay_ms));
			p->gate_f = fdb_transaction_on_error(p->gate_tr, 1020);
		} else {
			p->gate_f = fdb_transaction_get_approximate_size(p->gate_tr);
			fdb_future_block_until_ready(p->gate_f);
		}
		err = fdb_future_set_callback(p->gate_f, on_gate_ready, p);
		if (err != 0) {
			atomic_store(&p->gate_cb_done, 1);
			cleanup_probe_attempt(p);
			free(p);
			return err;
		}
		while (atomic_load(&p->parked) == 0 && atomic_load(&p->ran_inline) == 0) {
			usleep(100);
		}
		if (atomic_load(&p->parked) != 0) {
			*out_p = p;
			return 0;
		}
		cleanup_probe_attempt(p);
		delay_ms = (delay_ms <= 0) ? 5 : delay_ms * 2;
	}
	free(p);
	return 4100;
}

static int probe_pre_done(barrier_probe_t* p) {
	return atomic_load(&p->pre_done) != 0 && atomic_load(&p->pre_err) == 0;
}

static int record_barrier_return(barrier_probe_t* p) {
	int s = atomic_fetch_add(&p->seq, 1) + 1;
	atomic_store(&p->return_seq, s);
	return atomic_load(&p->pre_seq) == 1 && s == 2 && atomic_load(&p->pre_err) == 0;
}

static void release_barrier_probe(barrier_probe_t* p) {
	atomic_store(&p->release, 1);
}

static int finish_barrier_probe(barrier_probe_t* p) {
	cleanup_probe_attempt(p);
	int ok = (atomic_load(&p->pre_err) == 0 &&
		atomic_load(&p->pre_done) == 1 &&
		atomic_load(&p->pre_seq) == 1 &&
		atomic_load(&p->return_seq) == 2);
	free(p);
	return ok;
}
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
)

type nativeProbeDB struct {
	cDB *C.FDBDatabase
}

func openNativeProbeDB(clusterFile string) (*nativeProbeDB, error) {
	var cCluster *C.char
	if clusterFile != "" {
		cCluster = C.CString(clusterFile)
		defer C.free(unsafe.Pointer(cCluster))
	}
	var cDB *C.FDBDatabase
	if cerr := C.fdb_create_database(cCluster, &cDB); cerr != 0 {
		return nil, fmt.Errorf("fdb_create_database: %d", int(cerr))
	}
	return &nativeProbeDB{cDB: cDB}, nil
}

func (p *nativeProbeDB) Close() {
	if p.cDB != nil {
		C.fdb_database_destroy(p.cDB)
		p.cDB = nil
	}
}

var errSynchronousBypass = errors.New("synchronous call returned before queued onMainThread predecessor completed")

// verifySynchronousBypassRejected proves a non-waiting call returns while the
// native network thread is still parked before the predecessor.
func (p *nativeProbeDB) verifySynchronousBypassRejected(fn func() error) error {
	var probe *C.barrier_probe_t
	if cerr := C.start_barrier_probe(p.cDB, 0, &probe); cerr != 0 {
		return fmt.Errorf("start_barrier_probe: %d", int(cerr))
	}
	err := fn()
	ordered := C.record_barrier_return(probe)
	C.finish_barrier_probe(probe)
	if err != nil {
		return err
	}
	if ordered == 0 {
		return errSynchronousBypass
	}
	return nil
}

type barrierProbeFuture struct {
	fdb.FutureInt64
	probe             *C.barrier_probe_t
	queuedWhileParked bool
	waited            bool
	ordered           bool
}

func (f *barrierProbeFuture) Get() (int64, error) {
	if !f.queuedWhileParked || f.FutureInt64.IsReady() || C.probe_pre_done(f.probe) != 0 {
		return f.FutureInt64.Get()
	}
	f.waited = true
	C.release_barrier_probe(f.probe)
	v, err := f.FutureInt64.Get()
	f.ordered = C.record_barrier_return(f.probe) != 0
	return v, err
}

// verifyBarrierWaitsForPredecessor proves fn queues an unready future while the
// network thread is parked and completes only after the predecessor finishes.
func (p *nativeProbeDB) verifyBarrierWaitsForPredecessor(b *backend, fn func() error) error {
	var probe *C.barrier_probe_t
	if cerr := C.start_barrier_probe(p.cDB, 0, &probe); cerr != 0 {
		return fmt.Errorf("start_barrier_probe: %d", int(cerr))
	}
	var wrapped *barrierProbeFuture
	b.onBarrierWait = func(fut fdb.FutureInt64) fdb.FutureInt64 {
		wrapped = &barrierProbeFuture{
			FutureInt64:       fut,
			probe:             probe,
			queuedWhileParked: !fut.IsReady() && C.probe_pre_done(probe) == 0,
		}
		return wrapped
	}
	defer func() { b.onBarrierWait = nil }()
	err := fn()
	if wrapped == nil || !wrapped.waited {
		_ = C.record_barrier_return(probe)
	}
	ok := C.finish_barrier_probe(probe)
	if err != nil {
		return err
	}
	if wrapped == nil || !wrapped.queuedWhileParked || !wrapped.waited || !wrapped.ordered || ok == 0 {
		return errSynchronousBypass
	}
	return nil
}
