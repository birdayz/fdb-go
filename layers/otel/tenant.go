package otel

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"fdb.dev/layers/otel/otelsql"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"google.golang.org/protobuf/proto"
)

// The relational store kernel (rfcs/relational-exporter-migration.md §3, §5):
// one OTEL_V1 schema store per (run, tenant), every read a direct scan of a
// standard OTEL_V1 index in one transaction, charged against the query budget.

// relDomain is the relational domain of the Exporter's run databases:
// /OTEL/R_<RUN>, one schema T<tenant> per tenant.
const relDomain = "OTEL"

// relTenantPaths names the OTEL_V1 schema store of (run, tenant).
func relTenantPaths(run string, tenant int) (databasePath, schema string) {
	return "/" + relDomain + "/R_" + strings.ToUpper(run), "T" + strconv.Itoa(tenant)
}

// tenantStore is a (run, tenant) schema store: its pinned, compiled metadata and
// subspace, resolved once, and the database its transactions run on.
type tenantStore struct {
	db *recordlayer.FDBDatabase
	ts *otelsql.TenantStore
	// databasePath and schema name the store; continuation tokens are bound
	// to them.
	databasePath, schema string
	// fullText sets the message pipeline sizer on every open, as FDBBackend
	// does for the fulltext layout.
	fullText bool
}

// run runs fn on the store in one transaction under the exporter's policy:
// a write slot when ctx carries the limiter, the transaction timeout and retry
// policy, and error classification (runTxnSlot). The store is opened, never
// created: a dropped schema store fails the transaction as ownership.
func (s *tenantStore) run(ctx context.Context, op string, fn func(*recordlayer.FDBRecordStore) error) error {
	return s.runMode(ctx, op, false, fn)
}

// read is run for a read-only transaction, whose read version may come from
// the client's GRV cache: at most 100ms old (MAX_VERSION_CACHE_LAG) and never
// older than this client's last commit, which refreshes the cache, so a read
// still sees every write this process committed before it began. It saves the
// GRV round trip of a point read's three.
func (s *tenantStore) read(ctx context.Context, op string, fn func(*recordlayer.FDBRecordStore) error) error {
	return s.runMode(ctx, op, true, fn)
}

func (s *tenantStore) runMode(ctx context.Context, op string, cachedVersion bool, fn func(*recordlayer.FDBRecordStore) error) error {
	_, err := runTxnSlot(ctx, s.db, op, func(rc *recordlayer.FDBRecordContext) (any, error) {
		if cachedVersion {
			if err := rc.Transaction().Options().SetUseGrvCache(); err != nil {
				return nil, err
			}
		}
		store, err := s.open(rc, op)
		if err != nil {
			return nil, err
		}
		return nil, fn(store)
	})
	return err
}

// open opens the store in rc after setting the transaction's options, which
// must precede its first read.
func (s *tenantStore) open(rc *recordlayer.FDBRecordContext, op string) (*recordlayer.FDBRecordStore, error) {
	if err := configureStoreContext(rc); err != nil {
		return nil, err
	}
	builder := s.ts.Builder(rc)
	if s.fullText {
		builder.SetPipelineSizer(messagePipelineSizer)
	}
	store, err := builder.Open()
	if err != nil {
		var missing *recordlayer.RecordStoreDoesNotExistError
		if errors.As(err, &missing) {
			return nil, wrap("ownership", op, err)
		}
		return nil, err
	}
	return store, nil
}

// queryWork is a read's consumption, charged as it reads: a candidate is one
// index entry, record or probe row read, bytes are the key and value bytes
// read. Exceeding either budget fails the read; a result is never truncated.
type queryWork struct {
	maxCandidates int
	maxBytes      int64
	candidates    int
	bytes         int64
}

func newQueryWork(cfg Config) *queryWork {
	return &queryWork{maxCandidates: cfg.MaxCandidates, maxBytes: cfg.MaxBytes}
}

// charge adds one read of n bytes.
func (w *queryWork) charge(op string, n int64) error {
	w.candidates++
	w.bytes += n
	return w.check(op)
}

func (w *queryWork) check(op string) error {
	if w.candidates > w.maxCandidates || w.bytes > w.maxBytes {
		return failf("budget", op, "consumed %d candidates and %d logical bytes, over the budget of %d candidates and %d bytes",
			w.candidates, w.bytes, w.maxCandidates, w.maxBytes)
	}
	return nil
}

// result records the work in r.
func (w *queryWork) result(r *QueryResult) {
	r.Candidates, r.Bytes = w.candidates, w.bytes
}

// scanIndex visits the entries of the readable index name in rng, charging
// each entry's key and value bytes, until visit reports done or the range is
// exhausted. A missing or unreadable index is ownership: the store is not the
// schema this code was built for, or an index build is incomplete.
func scanIndex(ctx context.Context, store *recordlayer.FDBRecordStore, op, name string, rng recordlayer.TupleRange, reverse bool, work *queryWork, visit func(*recordlayer.IndexEntry) (bool, error)) error {
	idx, err := readableIndex(store, op, name)
	if err != nil {
		return err
	}
	props := recordlayer.ForwardScan()
	if reverse {
		props = recordlayer.ReverseScan()
	}
	limiter := recordlayer.NewScanLimiterState()
	props.ExecuteProperties.ScanState = limiter
	// One entry past the budget, so exceeding it is seen, never a short read.
	props.ExecuteProperties.ReturnedRowLimit = work.maxCandidates - work.candidates + 1
	cur := store.ScanIndex(idx, rng, nil, props)
	defer cur.Close()
	before := work.bytes
	for {
		r, err := cur.OnNext(ctx)
		if err != nil {
			return err
		}
		if !r.HasNext() {
			// The row limit is one past the budget, so the entry over it
			// fails above first; any other stop is a limit, not the end.
			if reason := r.GetNoNextReason(); reason != recordlayer.SourceExhausted {
				return failf("budget", op, "index %s scan stopped: %v", name, reason)
			}
			return nil
		}
		work.candidates++
		work.bytes = before + limiter.BytesScanned()
		if err := work.check(op); err != nil {
			return err
		}
		done, err := visit(r.GetValue())
		if err != nil || done {
			return err
		}
	}
}

// readableIndex is the store's index name, which must be readable: a missing
// or unreadable index is ownership.
func readableIndex(store *recordlayer.FDBRecordStore, op, name string) (*recordlayer.Index, error) {
	idx := store.GetRecordMetaData().GetIndex(name)
	if idx == nil {
		return nil, failf("ownership", op, "index %s is not in the store's metadata", name)
	}
	state, err := store.ReadIndexState(name)
	if err != nil {
		return nil, err
	}
	if !state.IsScannable() {
		return nil, failf("ownership", op, "index %s is not readable", name)
	}
	return idx, nil
}

// loadRecordsBatch is how many record loads loadRecords keeps in flight.
const loadRecordsBatch = 64

// loadRecords loads the records of pks, keeping up to loadRecordsBatch loads
// in flight, and visits them in pks order, charging each record's key and
// value bytes. A missing record is passed as nil; the caller decides whether
// that is corruption.
func loadRecords(store *recordlayer.FDBRecordStore, op string, pks []tuple.Tuple, work *queryWork, visit func(i int, rec *recordlayer.FDBStoredRecord[proto.Message]) error) error {
	futures := make([]recordlayer.PendingRead[*recordlayer.FDBStoredRecord[proto.Message]], 0, min(loadRecordsBatch, len(pks)))
	for lo := 0; lo < len(pks); lo += loadRecordsBatch {
		hi := min(lo+loadRecordsBatch, len(pks))
		futures = futures[:0]
		for i := lo; i < hi; i++ {
			futures = append(futures, store.LoadRecordAsync(pks[i], nil))
		}
		for i, f := range futures {
			rec, err := f.Get()
			if err != nil {
				return err
			}
			var n int64
			if rec != nil {
				n = int64(rec.KeySize + rec.ValueSize)
			}
			if err := work.charge(op, n); err != nil {
				return err
			}
			if err := visit(lo+i, rec); err != nil {
				return err
			}
		}
	}
	return nil
}
