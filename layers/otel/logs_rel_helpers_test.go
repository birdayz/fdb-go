package otel

import (
	"bytes"
	"context"
	"testing"

	"fdb.dev/layers/otel/otelsql"
	"fdb.dev/layers/otel/otelsql/otelv1pb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/core/embedded"
	"google.golang.org/protobuf/proto"
)

// lrConfig is the log store configuration of the relational exporter's
// defaults: DefaultExporterConfig with D2's zlib-1 record compression.
func lrConfig() ExporterConfig {
	cfg := DefaultExporterConfig()
	cfg.RecordCompression = "zlib-1"
	return cfg
}

// lrCtx carries the exporter transaction policy and a write limiter, as the
// relational exporter applies them to a log store call.
func lrCtx(ctx context.Context) context.Context {
	return withTxnLimiter(withExporterTxns(ctx, defaultExporterTxnTimeout), make(chan struct{}, defaultMaxInFlightTransactions))
}

// logStore opens a log store on (run, tenant)'s schema store, with the
// serializer cfg's record compression names.
func (h *qrHarness) logStore(t *testing.T, ctx context.Context, run string, tenant int, mutate func(*ExporterConfig)) *relLogStore {
	t.Helper()
	cfg := lrConfig()
	cfg.Run, cfg.Tenant = run, tenant
	if mutate != nil {
		mutate(&cfg)
	}
	serializer, err := recordSerializer(cfg.RecordCompression)
	if err != nil {
		t.Fatal(err)
	}
	path, schema := relTenantPaths(run, tenant)
	ts, err := otelsql.OpenTenantStoreCompiled(ctx, h.db, path, schema, serializer)
	if err != nil {
		t.Fatal(err)
	}
	s, err := newRelLogStore(&tenantStore{db: h.db, ts: ts, databasePath: path, schema: schema}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// openLogs is logStore on a fresh run's tenant 11.
func (h *qrHarness) openLogs(t *testing.T, ctx context.Context, mutate func(*ExporterConfig)) *relLogStore {
	t.Helper()
	run := freshRun(t)
	h.create(t, ctx, run, 11)
	return h.logStore(t, ctx, run, 11, mutate)
}

// qrLogStore is a log store on r's tenant store, within r's K and budgets.
func qrLogStore(t *testing.T, r *SpanReader) *relLogStore {
	t.Helper()
	cfg := lrConfig()
	cfg.Run, cfg.Tenant, cfg.K, cfg.MaxCandidates, cfg.MaxBytes = r.cfg.Run, r.cfg.Tenant, r.cfg.K, r.cfg.MaxCandidates, r.cfg.MaxBytes
	s, err := newRelLogStore(r.store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// lrUnitStore is a log store over the built OTEL_V1 metadata and a fixed
// subspace, for the write cost model without FDB.
func lrUnitStore(t *testing.T) *relLogStore {
	t.Helper()
	if err := otelsql.MessageTokenizerRegistrationError(); err != nil {
		t.Fatal(err)
	}
	tmpl, err := embedded.BuildSchemaTemplateFromDDL(otelsql.TemplateDDL())
	if err != nil {
		t.Fatal(err)
	}
	p, err := tmpl.Underlying().ToProto()
	if err != nil {
		t.Fatal(err)
	}
	md, err := recordlayer.RecordMetaDataFromProto(p, recordlayer.WithLocalFileDescriptor(otelv1pb.File_OTEL_V1))
	if err != nil {
		t.Fatal(err)
	}
	serializer, err := recordSerializer(lrConfig().RecordCompression)
	if err != nil {
		t.Fatal(err)
	}
	ts := &otelsql.TenantStore{DatabasePath: "/OTEL/R_LR_UNIT", Schema: "T3", MetaData: md, Subspace: subspace.Sub("lr_unit", int64(3)), Serializer: serializer}
	s, err := newRelLogStore(&tenantStore{ts: ts, databasePath: ts.DatabasePath, schema: ts.Schema}, lrConfig())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// lrPrepared is s.prepare at receiver time 777.
func lrPrepared(t *testing.T, s *relLogStore, l LogRecord) relPreparedLog {
	t.Helper()
	p, err := s.prepare(l, 777)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// lrCensus counts the KVs of a LOGS index, or of the LOGS records when name
// is logsRecordType.
func lrCensus(t *testing.T, ctx context.Context, s *relLogStore, name string) int64 {
	t.Helper()
	ss := s.store.ts.Subspace.Sub(recordlayer.RecordKey, s.rt.GetRecordTypeKey())
	if name != logsRecordType {
		idx := s.store.ts.MetaData.GetIndex(name)
		if idx == nil {
			t.Fatalf("no index %s", name)
		}
		ss = s.store.ts.Subspace.Sub(recordlayer.IndexKey, idx.SubspaceTupleKey())
	}
	sz, err := lrSize(ctx, s.store.db, ss)
	if err != nil {
		t.Fatal(err)
	}
	return sz.Keys
}

// lrCensusAll is lrCensus over the records and every LOGS index.
func lrCensusAll(t *testing.T, ctx context.Context, s *relLogStore) map[string]int64 {
	t.Helper()
	out := map[string]int64{logsRecordType: lrCensus(t, ctx, s, logsRecordType)}
	for _, name := range relLogIndexes {
		out[name] = lrCensus(t, ctx, s, name)
	}
	return out
}

// lrSize is the logical KV size of ss, read in pages of 256 rows.
func lrSize(ctx context.Context, db *recordlayer.FDBDatabase, ss subspace.Subspace) (Size, error) {
	var total Size
	begin, end := ss.FDBRangeKeys()
	lo, high := begin.FDBKey(), end.FDBKey()
	var prev []byte
	for {
		var rows []fdb.KeyValue
		if _, err := db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
			var err error
			rows, err = rc.ReadTransaction(true).GetRange(fdb.KeyRange{Begin: lo, End: high}, fdb.RangeOptions{Limit: 256}).GetSliceWithError()
			return nil, err
		}); err != nil {
			return total, err
		}
		for _, kv := range rows {
			total.AddKV(prev, kv.Key, kv.Value)
			prev = kv.Key
		}
		if len(rows) < 256 {
			return total, nil
		}
		lo = append(append(fdb.Key(nil), rows[len(rows)-1].Key...), 0)
	}
}

// lrRow loads s's stored row of eventID, nil when absent.
func lrRow(t *testing.T, ctx context.Context, s *relLogStore, eventID string) *recordlayer.FDBStoredRecord[proto.Message] {
	t.Helper()
	var out *recordlayer.FDBStoredRecord[proto.Message]
	if _, err := s.store.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		store, err := s.store.ts.Builder(rc).Open()
		if err != nil {
			return nil, err
		}
		out, err = store.LoadRecordAsync(s.primaryKey(eventID), nil).Get()
		return nil, err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// logNeighborBarrier observes a real successful neighbor commit, then grows the
// prior before the following transaction enters its callback. Retries do not
// advance the barrier until the underlying TransactCtx actually succeeds.
type logNeighborBarrier struct {
	fdb.Database
	neighbor           fdb.Key
	committed, crossed bool
	grow               func() error
}

type logNeighborTransaction struct {
	fdb.WritableTransaction
	neighbor fdb.Key
	wrote    *bool
}

func (tx logNeighborTransaction) Set(key fdb.KeyConvertible, value []byte) {
	if bytes.Equal(key.FDBKey(), tx.neighbor) {
		*tx.wrote = true
	}
	tx.WritableTransaction.Set(key, value)
}

func (tx logNeighborTransaction) SetBytes(key, value []byte) { tx.Set(fdb.Key(key), value) }

func (b *logNeighborBarrier) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return b.TransactCtx(context.Background(), fn)
}

func (b *logNeighborBarrier) TransactCtx(ctx context.Context, fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	if b.committed && !b.crossed {
		if err := b.grow(); err != nil {
			return nil, err
		}
		b.crossed = true
	}
	wrote := false
	result, err := b.Database.TransactCtx(ctx, func(tx fdb.WritableTransaction) (any, error) {
		wrote = false
		return fn(logNeighborTransaction{tx, b.neighbor, &wrote})
	})
	if err == nil && wrote {
		b.committed = true
	}
	return result, err
}
