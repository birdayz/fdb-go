package otel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"fdb.dev/layers/otel/otelsql"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// Ports of the message tests that need the exporter's lifecycle (§2.3):
// the group-commit queue's backing release, an evicted marker replay,
// Shutdown joining a canceled call's message worker, and the raw budget
// accounting of an empty cross-part query.

func TestRelMessageLifecycleRealFDB(t *testing.T) {
	t.Parallel()
	h := newQRHarness(t)
	for name, test := range map[string]func(*testing.T, *qrHarness){
		"queue_backing_release":   testRelQueueBackingRelease,
		"evicted_marker_replay":   testRelEvictedMarkerReplay,
		"shutdown_joins_worker":   testRelShutdownJoinsWorker,
		"multipart_empty_all_raw": testRelMultipartEmptyAllRaw,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			test(t, h)
		})
	}
}

// Port of TestLiveMessageQueueReleasesBackingOnPartialFailureAndCancel: a
// partial take keeps no consumed slot and loses no tail, and the drained
// queue keeps no plan or submission, whether the call succeeds, fails or is
// canceled; the exporter stays usable.
func testRelQueueBackingRelease(t *testing.T, h *qrHarness) {
	for _, outcome := range []string{"success", "failure", "cancel"} {
		t.Run(outcome, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
			defer cancel()
			e := h.msgEnv(t, ctx, messagesDeltas, nil)
			q := &e.m.queue
			backing := make([]queuedMessage, 2)
			q.pending = backing[:0]
			callCtx, stop := context.WithCancel(ctx)
			defer stop()
			sentinel := errors.New("queue drain terminal failure")
			partial := false
			e.m.hook = func(stage string, _ *recordlayer.FDBRecordStore) error {
				if stage != "message-before-commit" {
					return nil
				}
				q.mu.Lock()
				if len(q.pending) == 1 {
					partial = true
					if backing[0].plan != nil || backing[0].sub != nil || backing[1].plan == nil || backing[1].sub == nil {
						t.Error("live partial take retained consumed slot or lost tail")
					}
				}
				q.mu.Unlock()
				switch outcome {
				case "cancel":
					stop()
					return context.Canceled
				case "failure":
					return sentinel
				}
				return nil
			}
			// Two 900 KB messages: one group commit holds at most 1 MiB of raw bytes.
			input := deltaSpan(1, deltaKV("gen_ai.input.messages", deltaArray(deltaStr(strings.Repeat("a", 900000)), deltaStr(strings.Repeat("b", 900000)))))
			st, err := e.export(callCtx, []*tracepb.ResourceSpans{input})
			switch {
			case outcome == "success" && (err != nil || st.Messages.Written != 2):
				t.Fatalf("live queue success: %+v / %v", st.Messages, err)
			case outcome == "failure" && !errors.Is(err, sentinel):
				t.Fatalf("live queue failure: %v", err)
			case outcome == "cancel" && !errors.Is(err, context.Canceled):
				t.Fatalf("live queue cancellation: %v", err)
			}
			// Join the queue loops by their protected count, not by time.
			for {
				q.mu.Lock()
				drained := q.loops == 0
				if drained {
					if q.pending != nil || !partial {
						t.Error("live partial queue did not drain to nil")
					}
					for i, slot := range backing {
						if slot.plan != nil || slot.sub != nil {
							t.Errorf("live drained backing slot%d retained plan/submission", i)
						}
					}
				}
				q.mu.Unlock()
				if drained {
					break
				}
				if ctx.Err() != nil {
					t.Fatal(ctx.Err())
				}
				runtime.Gosched()
			}
			e.m.hook = nil
			if _, err := e.export(ctx, []*tracepb.ResourceSpans{deltaSpan(2, deltaKV("gen_ai.input.messages", deltaArray(deltaStr("live neighbor"))))}); err != nil {
				t.Fatalf("exporter must remain alive after drain: %v", err)
			}
		})
	}
}

// relMarkerProbe counts the snapshot range reads under prefix and those
// that found a row: the marker presence reads of persistTraceMarkers.
type relMarkerProbe struct {
	fdb.Database
	prefix         []byte
	mu             sync.Mutex
	reads, present int
}

type (
	relMarkerProbeTxn struct {
		fdb.WritableTransaction
		probe *relMarkerProbe
	}
	relMarkerProbeSnapshot struct {
		fdb.ReadTransaction
		probe *relMarkerProbe
	}
	relMarkerProbeRange struct {
		fdb.RangeResult
		probe *relMarkerProbe
	}
)

func (p *relMarkerProbe) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return p.TransactCtx(context.Background(), fn)
}

func (p *relMarkerProbe) TransactCtx(ctx context.Context, fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return p.Database.TransactCtx(ctx, func(tx fdb.WritableTransaction) (any, error) { return fn(relMarkerProbeTxn{tx, p}) })
}

func (tx relMarkerProbeTxn) Snapshot() fdb.ReadTransaction {
	return relMarkerProbeSnapshot{tx.WritableTransaction.Snapshot(), tx.probe}
}

func (tx relMarkerProbeSnapshot) GetRange(r fdb.Range, opts fdb.RangeOptions) fdb.RangeResult {
	result := tx.ReadTransaction.GetRange(r, opts)
	begin, _ := r.FDBRangeKeySelectors()
	if bytes.HasPrefix(begin.FDBKeySelector().Key.FDBKey(), tx.probe.prefix) {
		return relMarkerProbeRange{result, tx.probe}
	}
	return result
}

func (r relMarkerProbeRange) GetSliceWithError() ([]fdb.KeyValue, error) {
	rows, err := r.RangeResult.GetSliceWithError()
	r.probe.mu.Lock()
	r.probe.reads++
	if err == nil && len(rows) > 0 {
		r.probe.present++
	}
	r.probe.mu.Unlock()
	return rows, err
}

// Port of TestEvictedPersistedMarkerPublicReplayReadsAndReuses: a marker
// evicted from the exporter's cache is read again, found and cached, not
// written twice, and the query answer does not change.
func testRelEvictedMarkerReplay(t *testing.T, h *qrHarness) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	e := h.msgEnv(t, ctx, messagesFullText, nil)
	input := []*tracepb.ResourceSpans{deltaSpan(1, deltaKV("gen_ai.input.messages", deltaArray(deltaStr("quokka"))))}
	if st, err := e.export(ctx, input); err != nil || st.Messages.Written != 1 || st.Messages.TraceMarkers != 1 {
		t.Fatalf("initial persisted marker: %+v / %v", st.Messages, err)
	}
	cache := &e.m.markers
	if len(cache.ring) != 1 {
		t.Fatalf("initial cache=%d", len(cache.ring))
	}
	originalMarker := cache.ring[0]
	var prefix []byte
	e.run(t, ctx, func(store *recordlayer.FDBRecordStore) error {
		pk, err := tableKey(store, "test", messageTracesRecordType)
		prefix = store.RecordsSubspace().Pack(pk)
		return err
	})
	markerRows := func() map[string]string {
		t.Helper()
		found := map[string]string{}
		e.run(t, ctx, func(store *recordlayer.FDBRecordStore) error {
			clear(found)
			r, err := fdb.PrefixRange(prefix)
			if err != nil {
				return err
			}
			rows, err := store.Context().Transaction().GetRange(r, fdb.RangeOptions{}).GetSliceWithError()
			for _, row := range rows {
				found[string(row.Key)] = string(row.Value)
			}
			return err
		})
		return found
	}
	before := markerRows()
	if len(before) != 1 {
		t.Fatalf("persisted marker rows=%d", len(before))
	}
	for i := range markerCacheSize {
		cache.add([]traceMarker{cacheFixtureMarker(i)})
	}
	if cache.has(originalMarker) || len(cache.set) != markerCacheSize {
		t.Fatalf("marker not evicted / map not bounded: %d", len(cache.set))
	}
	probe := &relMarkerProbe{Database: e.x.raw, prefix: prefix}
	original := e.r.store.db
	e.r.store.db = recordlayer.NewFDBDatabaseWithTransactor(probe, e.x.raw)
	defer func() { e.r.store.db = original }()
	st, err := e.export(ctx, input)
	if err != nil || st.Messages.Written != 0 || st.Messages.Present != 1 || st.Messages.TraceMarkers != 0 {
		t.Fatalf("evicted-marker replay: %+v / %v", st.Messages, err)
	}
	probe.mu.Lock()
	reads, present := probe.reads, probe.present
	probe.mu.Unlock()
	if reads == 0 || reads != present || !cache.has(originalMarker) || len(cache.set) != markerCacheSize {
		t.Fatalf("replay did not read/reuse persisted marker: reads=%d present=%d cached=%v size=%d", reads, present, cache.has(originalMarker), len(cache.set))
	}
	if after := markerRows(); !reflect.DeepEqual(before, after) {
		t.Fatalf("replay duplicated/changed persisted marker: before=%v after=%v", before, after)
	}
	if n := relFtsCount(t, ctx, e, MessageFullTextQuery{Query: "quokka"}); n != 1 {
		t.Fatalf("replay duplicated query result: %d", n)
	}
}

// Port of TestShutdownJoinsCanceledMessageWorker: a canceled call's queued
// message keeps its shared transaction; Shutdown closes the queue, waits for
// the worker before closing the database, and returns the caller's
// cancellation or deadline when it ends first.
func testRelShutdownJoinsWorker(t *testing.T, h *qrHarness) {
	for _, mode := range []string{"success", "canceled", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			e := h.msgEnv(t, ctx, messagesFullText, nil)
			entered, release, wrote := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var enterOnce, releaseOnce, wroteOnce sync.Once
			releaseWorker := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(releaseWorker)
			e.m.hook = func(stage string, _ *recordlayer.FDBRecordStore) error {
				if stage == "message-before-write" {
					enterOnce.Do(func() { close(entered); <-release })
				}
				if stage == "message-before-commit" {
					wroteOnce.Do(func() { close(wrote) })
				}
				return nil
			}
			callCtx, stop := context.WithCancel(ctx)
			defer stop()
			exported := make(chan error, 1)
			go func() {
				_, err := e.export(callCtx, []*tracepb.ResourceSpans{deltaSpan(1, deltaKV("gen_ai.input.messages", deltaArray(deltaStr("queued zebra"))))})
				exported <- err
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			stop()
			select {
			case err := <-exported:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled export: %v", err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			shutdownCtx, stopShutdown := context.WithCancel(ctx)
			if mode == "deadline" {
				stopShutdown()
				shutdownCtx, stopShutdown = context.WithDeadline(ctx, time.Now().Add(-time.Second))
			}
			defer stopShutdown()
			shutdown := make(chan error, 1)
			go func() { shutdown <- e.x.Shutdown(shutdownCtx) }()
			awaitMessageCondition(t, ctx, func() bool {
				e.m.queue.mu.Lock()
				closed := e.m.queue.closed
				e.m.queue.mu.Unlock()
				select {
				case <-e.x.done:
					t.Fatal("shutdown completed before releasing the message worker")
				default:
				}
				return closed
			})
			if mode == "canceled" {
				stopShutdown()
			}
			if mode != "success" {
				select {
				case err := <-shutdown:
					want := context.Canceled
					if mode == "deadline" {
						want = context.DeadlineExceeded
					}
					if !errors.Is(err, want) {
						t.Fatalf("shutdown %s: %v", mode, err)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			if err := e.r.store.read(e.txn(ctx), "test", func(*recordlayer.FDBRecordStore) error { return nil }); err != nil {
				t.Fatalf("database closed while the message worker was blocked: %v", err)
			}
			select {
			case <-e.x.done:
				t.Fatal("database close completed with the message worker blocked")
			default:
			}
			releaseWorker()
			if err := e.x.Shutdown(ctx); err != nil {
				t.Fatalf("shutdown after worker release: %v", err)
			}
			if mode == "success" {
				if err := <-shutdown; err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-wrote:
			default:
				t.Fatal("worker did not finish its database write before close")
			}
			e.m.queue.mu.Lock()
			drained := e.m.queue.loops == 0 && e.m.queue.pending == nil
			e.m.queue.mu.Unlock()
			if !drained || len(e.m.writers.slots) != 0 {
				t.Fatal("successful shutdown retained message workers or writers")
			}
			if err := e.x.Shutdown(ctx); err != nil {
				t.Fatalf("idempotent shutdown: %v", err)
			}
		})
	}
}

// Port of TestMultipartEmptyAllQueryChargesRawLargeIndexBytes: an "all"
// query with no match reads every MESSAGES_LARGE entry and, per multipart
// message, the TEXT bunch preceding its bounded first-term probe, and is
// charged their raw key and value bytes with the budget edge exact. The
// messages' text is all overlong words, now indexed as the otelsql
// placeholder (one posting per text part, was none); the probe's preceding
// bunch is the last of those, not a key before the TEXT index.
func testRelMultipartEmptyAllRaw(t *testing.T, h *qrHarness) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	e := h.msgEnv(t, ctx, messagesFullText, nil)
	const messages = 3
	input := make([]*tracepb.ResourceSpans, messages)
	for i := range input {
		// Overlong words force several text segments, each one placeholder posting.
		text := strings.Repeat(strings.Repeat("x", maxIndexedTokenBytes+1)+" ", 80+i)
		input[i] = deltaSpan(i+1, deltaKV("gen_ai.input.messages", deltaArray(deltaStr(text))))
	}
	stats, err := e.export(ctx, input)
	if err != nil || stats.Messages.Written != messages || stats.Messages.Parts <= messages || stats.Messages.Postings != stats.Messages.Parts || stats.Messages.UnindexedTokens == 0 {
		t.Fatalf("multipart fixture: %+v / %v", stats.Messages, err)
	}
	const role = "missing-role"
	var rawBytes, largeBytes int64
	e.run(t, ctx, func(store *recordlayer.FDBRecordStore) error {
		rawBytes, largeBytes = 0, 0
		md := store.GetRecordMetaData()
		tx := store.Context().Transaction()
		largeIdx := md.GetIndex(messagesLargeIndex)
		large := store.IndexSubspace(largeIdx)
		rows, err := tx.GetRange(large, fdb.RangeOptions{}).GetSliceWithError()
		if err != nil {
			return err
		}
		if len(rows) != messages {
			return fmt.Errorf("need %d LARGE entries, got %d", messages, len(rows))
		}
		text := store.IndexSubspace(md.GetIndex(messagesTextIndex))
		for _, row := range rows {
			largeBytes += int64(len(row.Key) + len(row.Value))
			key, err := large.Unpack(row.Key)
			if err != nil {
				return err
			}
			id, _, err := messagePK(md, "test", largeIdx.EntryPrimaryKey(key))
			if err != nil {
				return err
			}
			prefix, err := tableKey(store, "test", messagesRecordType, id.writer, id.seq)
			if err != nil {
				return err
			}
			// The bounded probe of the first term starts here; it reads the
			// bunch at or before its start.
			start := fdb.Key(text.Pack(append(tuple.Tuple{role, otelsql.MessageShard(id.writer), "quokka"}, prefix...)))
			pred, err := tx.GetRange(fdb.SelectorRange{Begin: fdb.LastLessOrEqual(start), End: fdb.FirstGreaterOrEqual(start)}, fdb.RangeOptions{}).GetSliceWithError()
			if err != nil {
				return err
			}
			if len(pred) != 1 || !bytes.HasPrefix(pred[0].Key, text.Bytes()) {
				return fmt.Errorf("need one preceding TEXT bunch, got %d", len(pred))
			}
			rawBytes += int64(len(pred[0].Key) + len(pred[0].Value))
		}
		rawBytes += largeBytes
		return nil
	})
	t.Logf("raw multipart empty-query work: %d bytes, %d of them LARGE entries", rawBytes, largeBytes)
	query := MessageFullTextQuery{Query: "quokka zebra", Mode: MessageMatchAll, Role: role}
	baseline, err := e.x.QueryMessageFullText(ctx, query)
	if err != nil || baseline.Bytes != rawBytes || baseline.Candidates != messages+1 || len(baseline.Hits) != 0 || baseline.Continuation != nil {
		t.Fatalf("empty result raw work: %+v / %v, want %d bytes, %d candidates", baseline, err, rawBytes, messages+1)
	}
	for _, limit := range []int64{rawBytes, rawBytes - 1} {
		bounded := h.msgEnvOn(t, ctx, e.x.cfg.Run, messagesFullText, func(c *ExporterConfig) { c.MaxBytes = limit })
		page, err := bounded.x.QueryMessageFullText(ctx, query)
		if limit == rawBytes {
			if err != nil || page.Candidates != baseline.Candidates {
				t.Fatalf("exact boundary: %+v / %v", page, err)
			}
		} else {
			requireTyped[*QueryBudgetError](t, "below raw byte boundary", err)
		}
		if page.Bytes != rawBytes || len(page.Hits) != 0 || page.Continuation != nil {
			t.Fatalf("limit %d: consumed work %+v, want %d raw bytes", limit, page, rawBytes)
		}
	}
}

// awaitMessageCondition polls fn until it holds or ctx ends.
func awaitMessageCondition(t *testing.T, ctx context.Context, fn func() bool) {
	t.Helper()
	for !fn() {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
}
