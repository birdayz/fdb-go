package sqlbackend

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"

	"fdb.dev/layers/otel"
	"fdb.dev/layers/otel/otelsql"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// The direct query path reads the tenant schema's store as SQL would, through
// the same OTEL_V1 indexes its plans use (EXPLAIN: a covering reverse scan of
// SPANS_STR_ATTR / SPANS_INT_ATTR / SPANS_TIME, a covering SPANS_SPAN_ID scan,
// a primary-key scan for a trace), but in one Record Layer transaction per
// query: no catalog read, no planning and no per-page store reopen.

// directStore resolves the query tenant's store once.
func (b *Backend) directStore(ctx context.Context) (*otelsql.TenantStore, error) {
	if b.qstore != nil {
		return b.qstore, nil
	}
	ts, err := otelsql.OpenTenantStore(ctx, b.db, b.dbPath, tenantSchema(b.cfg.Tenant), b.readSer)
	if err != nil {
		return nil, err
	}
	b.qstore = ts
	return ts, nil
}

// directRun opens the query tenant's store in one transaction and runs fn.
func (b *Backend) directRun(ctx context.Context, fn func(*recordlayer.FDBRecordStore) error) error {
	ts, err := b.directStore(ctx)
	if err != nil {
		return err
	}
	_, err = b.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		store, err := ts.Builder(rc).Open()
		if err != nil {
			return nil, err
		}
		return nil, fn(store)
	})
	return err
}

// directScan visits the entries of a readable index in rng, within the
// candidate and byte budgets, until visit reports done.
func (b *Backend) directScan(ctx context.Context, store *recordlayer.FDBRecordStore, name string, rng recordlayer.TupleRange, reverse bool, work *otel.QueryResult, visit func(*recordlayer.IndexEntry) (bool, error)) error {
	idx := store.GetRecordMetaData().GetIndex(name)
	if idx == nil {
		return fmt.Errorf("sqlbackend: index %s missing from the schema template", name)
	}
	state, err := store.ReadIndexState(name)
	if err != nil {
		return err
	}
	if !state.IsScannable() {
		return fmt.Errorf("sqlbackend: index %s is not readable", name)
	}
	props := recordlayer.ForwardScan()
	if reverse {
		props = recordlayer.ReverseScan()
	}
	limiter := recordlayer.NewScanLimiterState()
	props.ExecuteProperties.ScanState = limiter
	props.ExecuteProperties.ReturnedRowLimit = b.cfg.MaxCandidates + 1
	cur := store.ScanIndex(idx, rng, nil, props)
	defer cur.Close()
	for {
		r, err := cur.OnNext(ctx)
		if err != nil {
			return err
		}
		if !r.HasNext() {
			if r.GetNoNextReason() != recordlayer.SourceExhausted {
				return fmt.Errorf("sqlbackend: %s scan stopped: %v", name, r.GetNoNextReason())
			}
			return nil
		}
		work.Candidates++
		work.Bytes = limiter.BytesScanned()
		if work.Candidates > b.cfg.MaxCandidates || work.Bytes > b.cfg.MaxBytes {
			return fmt.Errorf("sqlbackend: %s scan exceeds the candidate or byte budget", name)
		}
		done, err := visit(r.GetValue())
		if err != nil || done {
			return err
		}
	}
}

// entryInt64 and entryBytes read an index key column.
func entryInt64(e *recordlayer.IndexEntry, i int) (int64, error) {
	if i < len(e.Key) {
		if v, ok := e.Key[i].(int64); ok {
			return v, nil
		}
	}
	return 0, fmt.Errorf("sqlbackend: %s entry column %d is not an integer", e.Index.Name, i)
}

func entryBytes(e *recordlayer.IndexEntry, i int) ([]byte, error) {
	if i < len(e.Key) {
		if v, ok := e.Key[i].([]byte); ok {
			return v, nil
		}
	}
	return nil, fmt.Errorf("sqlbackend: %s entry column %d is not bytes", e.Index.Name, i)
}

// directTop is topTraces over a newest-first index scan whose key holds
// start_ns at startCol and trace_id at traceCol.
func (b *Backend) directTop(ctx context.Context, name string, rng recordlayer.TupleRange, startCol, traceCol int) (otel.QueryResult, error) {
	var out otel.QueryResult
	err := b.directRun(ctx, func(store *recordlayer.FDBRecordStore) error {
		out = otel.QueryResult{Hits: []otel.Hit{}}
		seen := map[string]bool{}
		return b.directScan(ctx, store, name, rng, true, &out, func(e *recordlayer.IndexEntry) (bool, error) {
			start, err := entryInt64(e, startCol)
			if err != nil {
				return false, err
			}
			trace, err := entryBytes(e, traceCol)
			if err != nil {
				return false, err
			}
			t := hex.EncodeToString(trace)
			if !seen[t] {
				seen[t] = true
				out.Hits = append(out.Hits, otel.Hit{Trace: t, Latest: start})
			}
			return len(out.Hits) == b.cfg.K, nil
		})
	})
	b.lastWork = otel.QueryResult{Candidates: out.Candidates, Bytes: out.Bytes}
	return out, err
}

// directSpan reads SPANS_SPAN_ID (span_id, start_ns, trace_id).
func (b *Backend) directSpan(ctx context.Context, span []byte) ([]otel.SpanMatch, error) {
	var out []otel.SpanMatch
	var work otel.QueryResult
	err := b.directRun(ctx, func(store *recordlayer.FDBRecordStore) error {
		out, work = []otel.SpanMatch{}, otel.QueryResult{}
		return b.directScan(ctx, store, "SPANS_SPAN_ID", recordlayer.TupleRangeAllOf(tuple.Tuple{span}), false, &work, func(e *recordlayer.IndexEntry) (bool, error) {
			id, err := entryBytes(e, 0)
			if err != nil {
				return false, err
			}
			if !bytes.Equal(id, span) {
				return false, fmt.Errorf("sqlbackend: SPANS_SPAN_ID returned another span")
			}
			start, err := entryInt64(e, 1)
			if err != nil {
				return false, err
			}
			trace, err := entryBytes(e, 2)
			if err != nil {
				return false, err
			}
			out = append(out, otel.SpanMatch{Trace: hex.EncodeToString(trace), ID: hex.EncodeToString(id), Start: start})
			return false, nil
		})
	})
	b.lastWork = work
	return out, err
}

// directTrace scans the trace's SPANS rows by primary-key prefix.
func (b *Backend) directTrace(ctx context.Context, trace []byte) ([]otel.SpanHit, error) {
	var out []otel.SpanHit
	var work otel.QueryResult
	err := b.directRun(ctx, func(store *recordlayer.FDBRecordStore) error {
		out, work = []otel.SpanHit{}, otel.QueryResult{}
		spans := store.GetRecordMetaData().GetRecordType("SPANS")
		if spans == nil {
			return fmt.Errorf("sqlbackend: SPANS missing from the schema template")
		}
		fields := spans.Descriptor.Fields()
		spanFD, startFD := fields.ByName("SPAN_ID"), fields.ByName("START_NS")
		if spanFD == nil || startFD == nil {
			return fmt.Errorf("sqlbackend: SPANS lacks SPAN_ID or START_NS")
		}
		props := recordlayer.ForwardScan()
		props.ExecuteProperties.ReturnedRowLimit = b.cfg.MaxCandidates + 1
		props.ExecuteProperties.ScannedBytesLimit = b.cfg.MaxBytes
		prefix := tuple.Tuple{trace}
		if spans.PrimaryKeyHasRecordTypePrefix() {
			// A relational table's primary key leads with its record type key.
			prefix = tuple.Tuple{spans.GetRecordTypeKey(), trace}
		}
		cur := store.ScanRecordsInRange(prefix, prefix, recordlayer.EndpointTypeRangeInclusive, recordlayer.EndpointTypeRangeInclusive, nil, props)
		defer cur.Close()
		for {
			r, err := cur.OnNext(ctx)
			if err != nil {
				return err
			}
			if !r.HasNext() {
				if r.GetNoNextReason() != recordlayer.SourceExhausted {
					return fmt.Errorf("sqlbackend: trace scan stopped: %v", r.GetNoNextReason())
				}
				break
			}
			rec := r.GetValue()
			work.Candidates++
			work.Bytes += int64(rec.KeySize + rec.ValueSize)
			if work.Bytes > b.cfg.MaxBytes || work.Candidates > b.cfg.MaxCandidates {
				return fmt.Errorf("sqlbackend: trace scan exceeds the candidate or byte budget")
			}
			m := rec.Record.ProtoReflect()
			out = append(out, otel.SpanHit{ID: hex.EncodeToString(fieldBytes(m, spanFD)), Start: m.Get(startFD).Int()})
		}
		sortSpanHits(out)
		return nil
	})
	b.lastWork = work
	return out, err
}

func fieldBytes(m protoreflect.Message, fd protoreflect.FieldDescriptor) []byte {
	return m.Get(fd).Bytes()
}
