package otel

import (
	"context"
	"encoding/json"

	"fdb.dev/pkg/fdbgo/client"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

type Size struct {
	Keys          int64 `json:"keys"`
	KeyBytes      int64 `json:"key_bytes"`
	ValueBytes    int64 `json:"value_bytes"`
	PhysicalBytes int64 `json:"physical_bytes,omitempty"`
}
type Statistics struct {
	Rows                int64              `json:"rows"`
	Attributes          int64              `json:"attributes"`
	LogicalPayloadBytes int64              `json:"logical_payload_bytes"`
	Structures          map[string]Size    `json:"structures"`
	Details             map[string]string  `json:"details,omitempty"`
	Diagnostics         *IngestDiagnostics `json:"diagnostics,omitempty"`
}
type Backend interface {
	Ingest(context.Context) (Statistics, error)
	Query(context.Context, Attribute) (QueryResult, error)
	Trace(context.Context, string) ([]SpanHit, error)
	Audit(context.Context) (Statistics, error)
	Drop(context.Context) error
	Close()
}

type FDBBackend struct {
	cfg           Config
	raw           fdb.Database
	db            *recordlayer.FDBDatabase
	root, store   subspace.Subspace
	md            *recordlayer.RecordMetaData
	idx           *recordlayer.Index
	lastWork      QueryResult
	auditPageHook func(context.Context, *recordlayer.FDBRecordContext, int) error
	ingestProbe   func(ingestEvent)
}

func OpenFDB(c Config, cluster string) (*FDBBackend, error) {
	return OpenFDBContext(context.Background(), c, cluster)
}

func OpenFDBContext(ctx context.Context, c Config, cluster string) (*FDBBackend, error) {
	if err := ctx.Err(); err != nil {
		return nil, wrap("canceled", "open FDB", err)
	}
	c.Backend = "fdb"
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if cluster == "" {
		return nil, fail("config", "open FDB", "explicit owned FDB cluster file required")
	}
	if err := fdb.APIVersion(730); err != nil {
		return nil, err
	}
	cf, err := client.ParseClusterFile(cluster)
	if err != nil {
		return nil, wrap("config", "cluster file", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, wrap("canceled", "open FDB", err)
	}
	raw, err := fdb.OpenDatabaseFromConfig(ctx, cf)
	if err != nil {
		return nil, err
	}
	md, idx, err := metadata(c)
	if err != nil {
		raw.Close()
		return nil, err
	}
	root := subspace.Sub("otel_otelbench_v2", c.Run)
	return &FDBBackend{cfg: c, raw: raw, db: recordlayer.NewFDBDatabase(raw), root: root, store: root.Sub("store"), md: md, idx: idx}, nil
}
func (b *FDBBackend) Close()                     { b.raw.Close() }
func (b *FDBBackend) LastQueryWork() QueryResult { return b.lastWork }
func (b *FDBBackend) open(rc *recordlayer.FDBRecordContext) (*recordlayer.FDBRecordStore, error) {
	if err := rc.Transaction().Options().SetTimeout(1500); err != nil {
		return nil, err
	}
	if err := rc.Transaction().Options().SetRetryLimit(10); err != nil {
		return nil, err
	}
	return recordlayer.NewStoreBuilder().SetContext(rc).SetMetaDataProvider(b.md).SetSubspace(b.store).CreateOrOpen()
}

func (b *FDBBackend) readManifest(rc *recordlayer.FDBRecordContext, ready bool) error {
	wire, err := rc.Transaction().Get(b.root.Sub("owner").FDBKey()).Get()
	if err != nil {
		return err
	}
	if len(wire) == 0 {
		return fail("ownership", "manifest", "run has no ownership manifest")
	}
	var m Manifest
	if err := json.Unmarshal(wire, &m); err != nil {
		return err
	}
	return checkManifest(b.cfg, m, ready)
}

func (b *FDBBackend) Query(ctx context.Context, a Attribute) (QueryResult, error) {
	b.lastWork = QueryResult{}
	out := QueryResult{Hits: []Hit{}}
	_, err := b.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		out = QueryResult{Hits: []Hit{}}
		b.lastWork = QueryResult{}
		if err := b.readManifest(rc, true); err != nil {
			return nil, err
		}
		store, err := b.open(rc)
		if err != nil {
			return nil, err
		}
		indexState, err := store.ReadIndexState(b.idx.Name)
		if err != nil {
			return nil, err
		}
		if !indexState.IsScannable() {
			return nil, fail("ownership", "query", "index is not readable")
		}
		m, err := store.GetIndexMaintainer(b.idx)
		if err != nil {
			return nil, err
		}
		scanner, ok := recordlayer.IndexMaintainerAs[interface {
			Query(Config, Attribute) (QueryResult, error)
		}](m)
		if !ok {
			return nil, fail("config", "query", "custom scanner not linked")
		}
		out, err = scanner.Query(b.cfg, a)
		b.lastWork = out
		return nil, err
	})
	return out, err
}

func (b *FDBBackend) Trace(ctx context.Context, trace string) ([]SpanHit, error) {
	b.lastWork = QueryResult{}
	out := []SpanHit{}
	_, err := b.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		out = []SpanHit{}
		b.lastWork = QueryResult{}
		if err := b.readManifest(rc, true); err != nil {
			return nil, err
		}
		store, err := b.open(rc)
		if err != nil {
			return nil, err
		}
		props := recordlayer.ForwardScan()
		props.ExecuteProperties.ReturnedRowLimit = b.cfg.MaxCandidates + 1
		props.ExecuteProperties.ScannedBytesLimit = b.cfg.MaxBytes
		prefix := tuple.Tuple{int64(b.cfg.Tenant), trace}
		cur := store.ScanRecordsInRange(prefix, prefix, recordlayer.EndpointTypeRangeInclusive, recordlayer.EndpointTypeRangeInclusive, nil, props)
		defer cur.Close()
		for {
			r, err := cur.OnNext(ctx)
			if err != nil {
				return nil, err
			}
			if !r.HasNext() {
				if r.GetNoNextReason() != recordlayer.SourceExhausted {
					return nil, failf("budget", "trace", "trace scan stopped: %v", r.GetNoNextReason())
				}
				break
			}
			record := r.GetValue()
			b.lastWork.Candidates++
			b.lastWork.Bytes += int64(record.KeySize + record.ValueSize)
			if b.lastWork.Bytes > b.cfg.MaxBytes {
				return nil, failf("budget", "trace", "consumed logical bytes %d exceed %d", b.lastWork.Bytes, b.cfg.MaxBytes)
			}
			s, err := decodeRecord(record.Record)
			if err != nil {
				return nil, err
			}
			out = append(out, SpanHit{s.ID, s.Start})
			if len(out) > b.cfg.MaxCandidates {
				return nil, fail("budget", "trace", "trace row limit exceeded")
			}
		}
		sortSpanHits(out)
		return nil, nil
	})
	return out, err
}

func (b *FDBBackend) Drop(ctx context.Context) error {
	_, err := b.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		if err := b.readManifest(rc, false); err != nil {
			return nil, err
		}
		r, err := fdb.PrefixRange(b.root.Bytes())
		if err != nil {
			return nil, err
		}
		rc.Transaction().ClearRange(r)
		return nil, nil
	})
	return err
}

func (b *FDBBackend) size(ctx context.Context, ss subspace.Subspace) (Size, error) {
	var total Size
	begin, end := ss.FDBRangeKeys()
	lo := begin.FDBKey()
	high := end.FDBKey()
	for {
		var rows []fdb.KeyValue
		_, err := b.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
			var err error
			rows, err = rc.ReadTransaction(true).GetRange(fdb.KeyRange{Begin: lo, End: high}, fdb.RangeOptions{Limit: 256}).GetSliceWithError()
			return nil, err
		})
		if err != nil {
			return total, err
		}
		for _, kv := range rows {
			total.Keys++
			total.KeyBytes += int64(len(kv.Key))
			total.ValueBytes += int64(len(kv.Value))
		}
		if len(rows) < 256 {
			break
		}
		lo = append(append(fdb.Key(nil), rows[len(rows)-1].Key...), 0)
	}
	return total, nil
}

func (b *FDBBackend) Audit(ctx context.Context) (Statistics, error) {
	stats := Statistics{Structures: map[string]Size{}, Details: map[string]string{"storage": "FDB logical KV bytes; replication/logs/engine compression excluded", "consistency": "immutable corpus only; no concurrent mutation audit"}}
	var continuation []byte
	seen := newOrdinalSet(b.cfg.Count)
	page := 0
	for {
		var next []byte
		done := false
		var rows int64
		var ordinals []int
		_, err := b.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
			rows = 0
			done = false
			ordinals = nil
			if err := b.readManifest(rc, true); err != nil {
				return nil, err
			}
			store, err := b.open(rc)
			if err != nil {
				return nil, err
			}
			props := recordlayer.ForwardScan()
			props.ExecuteProperties.ReturnedRowLimit = 128
			cur := store.ScanRecords(continuation, props)
			defer cur.Close()
			for {
				r, err := cur.OnNext(ctx)
				if err != nil {
					return nil, err
				}
				if !r.HasNext() {
					done = r.GetNoNextReason() == recordlayer.SourceExhausted
					if !done && r.GetNoNextReason() != recordlayer.ReturnLimitReached {
						return nil, fail("audit", "source scan", "audit scan budget exceeded")
					}
					next, err = r.GetContinuation().ToBytes()
					if err == nil && b.auditPageHook != nil {
						err = b.auditPageHook(ctx, rc, page)
					}
					return nil, err
				}
				s, err := decodeRecord(r.GetValue().Record)
				if err != nil {
					return nil, err
				}
				ordinal, ok := fixtureOrdinal(b.cfg, s)
				if !ok {
					return nil, fail("audit", "source scan", "source identity outside deterministic corpus")
				}
				want := Generate(b.cfg, ordinal)
				if !equalSpan(s, want) {
					return nil, failf("audit", "source scan", "source audit mismatch at ordinal %d", ordinal)
				}
				ordinals = append(ordinals, ordinal)
				rows++
			}
		})
		if err != nil {
			return stats, err
		}
		for _, ordinal := range ordinals {
			if err := seen.add(ordinal, b.cfg.Count); err != nil {
				return stats, err
			}
		}
		stats.Rows += rows
		page++
		if done {
			break
		}
		if len(next) == 0 {
			return stats, fail("audit", "continuation", "audit made no continuation progress")
		}
		continuation = next
	}
	if stats.Rows != int64(b.cfg.Count) {
		return stats, failf("audit", "source census", "source rows %d want %d", stats.Rows, b.cfg.Count)
	}
	stats.Attributes = stats.Rows * int64(b.cfg.Attributes)
	structures := map[string]subspace.Subspace{"owned_total": b.root, "records": b.store.Sub(recordlayer.RecordKey), "eq_primary": b.store.Sub(recordlayer.IndexKey, b.idx.SubspaceTupleKey()), "eq_secondary": b.store.Sub(recordlayer.IndexSecondarySpaceKey, b.idx.SubspaceTupleKey())}
	for _, name := range []string{spanIndexName, timeIndexName} {
		idx := b.md.GetIndex(name)
		if idx == nil {
			return stats, fail("config", "intrinsic index census", "missing intrinsic index metadata")
		}
		structures[name] = b.store.Sub(recordlayer.IndexKey, idx.SubspaceTupleKey())
	}
	for name, ss := range structures {
		sz, err := b.size(ctx, ss)
		if err != nil {
			return stats, err
		}
		stats.Structures[name] = sz
	}
	for _, name := range []string{spanIndexName, timeIndexName} {
		if stats.Structures[name].Keys != stats.Rows {
			return stats, failf("audit", "intrinsic index census", "%s keys %d want %d", name, stats.Structures[name].Keys, stats.Rows)
		}
	}
	if stats.Structures["eq_secondary"].Keys != stats.Attributes {
		return stats, failf("audit", "posting census", "posting key census %d want %d", stats.Structures["eq_secondary"].Keys, stats.Attributes)
	}
	return stats, nil
}
