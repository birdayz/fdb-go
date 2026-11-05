package otel

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestMultipartEmptyAllQueryChargesRawLargeIndexBytes(t *testing.T) {
	t.Parallel()
	cluster := startExporterCluster(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	e := openFTSExporter(t, ctx, cluster, nil)
	const messages = 3
	input := make([]*tracepb.ResourceSpans, messages)
	for i := range input {
		// Unindexed words force multiple searchable segments without any TEXT postings.
		text := strings.Repeat(strings.Repeat("x", maxIndexedTokenBytes+1)+" ", 80+i)
		input[i] = deltaSpan(i+1, deltaKV("gen_ai.input.messages", deltaArray(deltaStr(text))))
	}
	stats, err := e.ExportResourceSpans(ctx, input)
	if err != nil || stats.Messages.Written != messages || stats.Messages.Parts <= messages || stats.Messages.Postings != 0 || stats.Messages.UnindexedTokens == 0 {
		t.Fatalf("multipart fixture: %+v / %v", stats.Messages, err)
	}
	var rawBytes, omittedPrefixes int64
	if err := deltaRun(t, ctx, e.backend.db, func(rc *recordlayer.FDBRecordContext) error {
		rawBytes, omittedPrefixes = 0, 0
		store, err := e.backend.open(rc)
		if err != nil {
			return err
		}
		space := store.IndexSubspace(e.backend.md.GetIndex(messageLargeIndexName))
		rows, err := rc.Transaction().GetRange(space, fdb.RangeOptions{}).GetSliceWithError()
		if err != nil {
			return err
		}
		if len(rows) != messages || len(space.Bytes()) == 0 {
			return fmt.Errorf("need %d prefixed LARGE entries, got %d", messages, len(rows))
		}
		for _, row := range rows {
			rawBytes += int64(len(row.Key) + len(row.Value))
			omittedPrefixes += int64(len(space.Bytes()))
		}
		text, err := rc.Transaction().GetRange(store.IndexSubspace(e.backend.md.GetIndex(messageTextIndexName)), fdb.RangeOptions{}).GetSliceWithError()
		if err != nil {
			return err
		}
		if len(text) != 0 {
			return fmt.Errorf("unindexed fixture has %d TEXT rows", len(text))
		}
		// Each message-bounded absent-term probe consumes the same foreign predecessor.
		textPrefix := store.IndexSubspace(e.backend.md.GetIndex(messageTextIndexName)).Bytes()
		predecessor, err := rc.Transaction().GetRange(fdb.SelectorRange{
			Begin: fdb.LastLessOrEqual(fdb.Key(textPrefix)), End: fdb.FirstGreaterOrEqual(fdb.Key(textPrefix)),
		}, fdb.RangeOptions{}).GetSliceWithError()
		if err != nil {
			return err
		}
		if len(predecessor) != 1 {
			return fmt.Errorf("need one foreign TEXT predecessor, got %d", len(predecessor))
		}
		rawBytes += messages * int64(len(predecessor[0].Key)+len(predecessor[0].Value))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Logf("raw multipart empty-query work: %d bytes, including %d LARGE-prefix bytes", rawBytes, omittedPrefixes)
	query := MessageFullTextQuery{Query: "quokka zebra", Mode: MessageMatchAll, Role: "missing-role"}
	baseline, err := e.QueryMessageFullText(ctx, query)
	if err != nil || baseline.Bytes != rawBytes || baseline.Candidates != messages+1 || len(baseline.Hits) != 0 || baseline.Continuation != nil {
		t.Fatalf("empty result raw work: %+v / %v, want %d bytes including %d prefix bytes", baseline, err, rawBytes, omittedPrefixes)
	}
	for _, limit := range []int64{rawBytes, rawBytes - 1} {
		cfg := e.Config()
		cfg.MaxBytes = limit
		bounded, err := NewExporter(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = bounded.Shutdown(context.Background()) })
		page, err := bounded.QueryMessageFullText(ctx, query)
		if limit == rawBytes {
			if err != nil || page.Candidates != baseline.Candidates {
				t.Fatalf("exact boundary: %+v / %v", page, err)
			}
		} else {
			requireTyped[*QueryBudgetError](t, "below raw byte boundary", err)
		}
		if page.Bytes != rawBytes || bounded.backend.LastQueryWork().Bytes != rawBytes || len(page.Hits) != 0 || page.Continuation != nil {
			t.Fatalf("limit %d: consumed work %+v, want %d raw bytes", limit, page, rawBytes)
		}
	}
}

func TestMessageTokenFreeByteBoundary(t *testing.T) {
	t.Parallel()
	cluster := startExporterCluster(t)
	for _, n := range []int{messageSegmentBytes - 1, messageSegmentBytes, messageSegmentBytes + 1} {
		for _, char := range []string{" ", "!"} {
			t.Run(fmt.Sprintf("%x/%d", char, n), func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
				defer cancel()
				e := openFTSExporter(t, ctx, cluster, nil)
				input := deltaSpan(1, deltaKV("gen_ai.input.messages", deltaArray(deltaStr(strings.Repeat(char, n)))))
				stats, err := e.ExportResourceSpans(ctx, []*tracepb.ResourceSpans{input})
				if err != nil {
					t.Fatal(err)
				}
				if stats.Messages.Written != 1 || stats.Messages.Postings != 0 {
					t.Fatalf("unexpected token-free counters: %+v", stats.Messages)
				}
				sp := input.ScopeSpans[0].Spans[0]
				got, err := e.LoadSpanOTLP(ctx, hex.EncodeToString(sp.TraceId), hex.EncodeToString(sp.SpanId))
				if err != nil {
					t.Fatal(err)
				}
				requireProtoEqual(t, "token-free message", got, input)
			})
		}
	}
}

func TestMessageWritesCannotRepopulateDroppedRun(t *testing.T) {
	t.Parallel()
	cluster := startExporterCluster(t)
	for _, stage := range []string{"writer-before-write", "message-before-write", "marker-before-write"} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			e := openFTSExporter(t, ctx, cluster, nil)
			dropper, err := NewExporter(ctx, e.Config())
			if err != nil {
				t.Fatal(err)
			}
			defer dropper.Shutdown(ctx)
			var once sync.Once
			var dropped atomic.Bool
			e.backend.messageTxnHook = func(at string, _ *recordlayer.FDBRecordStore) error {
				if at == stage {
					once.Do(func() {
						if err := dropper.backend.Drop(ctx); err != nil {
							t.Errorf("drop: %v", err)
						} else {
							dropped.Store(true)
						}
					})
				}
				return nil
			}
			_, err = e.ExportResourceSpans(ctx, []*tracepb.ResourceSpans{deltaSpan(1, deltaKV("gen_ai.input.messages", deltaArray(deltaStr("zebra"))))})
			if !dropped.Load() {
				t.Fatal("write boundary was not reached")
			}
			requireOtelKind(t, err, "ownership")
			_, err = e.backend.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
				for _, ss := range []interface{ Bytes() []byte }{e.backend.keys.data, e.backend.keys.legacy} {
					r, err := fdb.PrefixRange(ss.Bytes())
					if err != nil {
						return nil, err
					}
					rows, err := rc.Transaction().GetRange(r, fdb.RangeOptions{Limit: 1}).GetSliceWithError()
					if err != nil {
						return nil, err
					}
					if len(rows) != 0 {
						t.Errorf("dropped prefix repopulated: %x", rows[0].Key)
					}
				}
				return nil, nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestQueuedMessageSizeFallbackCountersAndExhaustion(t *testing.T) {
	t.Parallel()
	cluster := startExporterCluster(t)
	for _, exhaust := range []bool{false, true} {
		t.Run(map[bool]string{false: "recovery", true: "exhaustion"}[exhaust], func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			e := openFTSExporter(t, ctx, cluster, nil)
			words := make([]string, 600)
			for i := range words {
				words[i] = fmt.Sprintf("term%04d", i)
			}
			input := deltaSpan(1, deltaKV("gen_ai.input.messages", deltaArray(deltaStr(strings.Join(words, " ")))))
			var calls atomic.Int64
			e.backend.messageTxnHook = func(stage string, store *recordlayer.FDBRecordStore) error {
				if stage == "message-before-commit" {
					n := calls.Add(1)
					if n == 1 || exhaust {
						return fdb.Error{Code: 2101}
					}
				}
				return nil
			}
			stats, err := e.ExportResourceSpans(ctx, []*tracepb.ResourceSpans{input})
			if exhaust {
				if code, ok := fdbErrorCode(err); !ok || code != 2101 {
					t.Fatalf("expected terminal 2101: %v", err)
				}
				if calls.Load() != 4 {
					t.Fatalf("fallback attempts %d, want initial plus 256/64/16", calls.Load())
				}
				_, loadErr := e.LoadSpanOTLP(ctx, ftsTrace(1), ftsSpan(1))
				var missing *SpanNotFoundError
				if !errors.As(loadErr, &missing) {
					t.Fatalf("failed export published a span: %v", loadErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if calls.Load() < 3 {
				t.Fatal("multipart fallback was not committed")
			}
			d, err := decomposeMessages(ctx, []*tracepb.ResourceSpans{input}, e.cfg.Tenant)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := planMessage(e.cfg.Tenant, d.elements[0], true, messageSegmentTokens/4)
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.records) <= 1 || stats.Messages.Parts != int64(len(plan.records)) || stats.Messages.Postings != plan.postings {
				t.Fatalf("actual fallback %+v; want parts %d/postings %d", stats.Messages, len(plan.records), plan.postings)
			}
			sp := input.ScopeSpans[0].Spans[0]
			got, err := e.LoadSpanOTLP(ctx, hex.EncodeToString(sp.TraceId), hex.EncodeToString(sp.SpanId))
			if err != nil {
				t.Fatal(err)
			}
			requireProtoEqual(t, "fallback reconstruction", got, input)
			if n := ftsCount(t, ctx, e, MessageFullTextQuery{Query: "term0000 term0599", Mode: MessageMatchAll}); n != 1 {
				t.Fatalf("cross-part search got %d", n)
			}
			again, err := e.ExportResourceSpans(ctx, []*tracepb.ResourceSpans{input})
			if err != nil || again.Messages.Written != 0 || again.Messages.Present != 1 {
				t.Fatalf("fallback dedup %+v/%v", again, err)
			}
		})
	}
}

func TestPersistedMessageCorruptionFailsClosed(t *testing.T) {
	t.Parallel()
	cluster := startExporterCluster(t)
	for _, damage := range []string{"head", "part", "encoding", "parts", "length"} {
		t.Run(damage, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			e := openFTSExporter(t, ctx, cluster, nil)
			rawBytes := messagePartRawBytes + 100
			if damage == "parts" {
				rawBytes = 100 // A complete head isolates the parts<1 guard from raw-length validation.
			}
			input := deltaSpan(1, deltaKV("gen_ai.input.messages", deltaArray(deltaStr(strings.Repeat(" ", rawBytes)))))
			if _, err := e.ExportResourceSpans(ctx, []*tracepb.ResourceSpans{input}); err != nil {
				t.Fatal(err)
			}
			d, err := decomposeMessages(ctx, []*tracepb.ResourceSpans{input}, e.cfg.Tenant)
			if err != nil {
				t.Fatal(err)
			}
			found, err := e.backend.readMessagePresence(ctx, e.cfg.Tenant, d.elements)
			if err != nil {
				t.Fatal(err)
			}
			id := found[d.elements[0].digest]
			if !id.valid() {
				t.Fatal("message not persisted")
			}
			err = deltaRun(t, ctx, e.backend.db, func(rc *recordlayer.FDBRecordContext) error {
				store, err := e.backend.open(rc)
				if err != nil {
					return err
				}
				if damage == "head" || damage == "part" {
					part := 0
					if damage == "part" {
						part = 1
					}
					deleted, err := store.DeleteRecord(messageKey(e.cfg.Tenant, id, part))
					if !deleted && err == nil {
						t.Error("mutation did not remove stored part")
					}
					return err
				}
				r, err := store.LoadRecord(messageKey(e.cfg.Tenant, id, 0))
				if err != nil {
					return err
				}
				m := r.Record.ProtoReflect()
				f := m.Descriptor().Fields()
				switch damage {
				case "encoding":
					m.Set(f.ByName("encoding"), protoreflect.ValueOfString("wrong"))
				case "parts":
					if got := m.Get(f.ByName("parts")).Int(); got != 1 {
						return fmt.Errorf("single-head fixture has %d parts, want 1", got)
					}
					m.Set(f.ByName("parts"), protoreflect.ValueOfInt64(0))
				case "length":
					m.Set(f.ByName("raw_bytes"), protoreflect.ValueOfInt64(messagePartRawBytes+101))
				}
				_, err = store.SaveRecord(r.Record)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			sp := input.ScopeSpans[0].Spans[0]
			got, err := e.LoadSpanOTLP(ctx, hex.EncodeToString(sp.TraceId), hex.EncodeToString(sp.SpanId))
			requireOtelKind(t, err, "corruption")
			if got != nil {
				t.Fatal("corrupt load returned a partial span")
			}
		})
	}
}

func TestShutdownJoinsCanceledMessageWorker(t *testing.T) {
	t.Parallel()
	cluster := startExporterCluster(t)
	for _, mode := range []string{"success", "canceled", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			e := openFTSExporter(t, ctx, cluster, nil)
			entered, release, wrote := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var enterOnce, releaseOnce, wroteOnce sync.Once
			releaseWorker := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(releaseWorker)
			e.backend.messageTxnHook = func(stage string, _ *recordlayer.FDBRecordStore) error {
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
				_, err := e.ExportResourceSpans(callCtx, []*tracepb.ResourceSpans{deltaSpan(1, deltaKV("gen_ai.input.messages", deltaArray(deltaStr("queued zebra"))))})
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
			go func() { shutdown <- e.Shutdown(shutdownCtx) }()
			awaitMessageCondition(t, ctx, func() bool {
				e.backend.queue.mu.Lock()
				closed := e.backend.queue.closed
				e.backend.queue.mu.Unlock()
				select {
				case <-e.done:
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
			if err := deltaRun(t, ctx, e.backend.db, func(rc *recordlayer.FDBRecordContext) error {
				return e.backend.readManifest(rc, true)
			}); err != nil {
				t.Fatalf("database closed while message worker was blocked: %v", err)
			}
			select {
			case <-e.done:
				t.Fatal("database close completed with the message worker blocked")
			default:
			}
			releaseWorker()
			if err := e.Shutdown(ctx); err != nil {
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
			e.backend.queue.mu.Lock()
			drained := e.backend.queue.loops == 0 && e.backend.queue.pending == nil
			e.backend.queue.mu.Unlock()
			if !drained || len(e.backend.writers.slots) != 0 {
				t.Fatal("successful shutdown retained message workers or writers")
			}
			if err := e.Shutdown(ctx); err != nil {
				t.Fatalf("idempotent shutdown: %v", err)
			}
			e.backend.Close()
		})
	}
}

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

func TestCanceledMessageOwnerTwoWaitersShareOneDigest(t *testing.T) {
	t.Parallel()
	cluster := startExporterCluster(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	e := openFTSExporter(t, ctx, cluster, nil)
	b := e.backend
	var held []*messageWriter
	for range messageWriterLimit {
		w, err := b.acquireWriter(ctx)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, w)
	}
	defer func() {
		for _, w := range held {
			b.releaseWriter(w)
		}
	}()
	var awaits atomic.Int64
	b.messageTxnHook = func(stage string, _ *recordlayer.FDBRecordStore) error {
		if stage == "message-await" {
			awaits.Add(1)
		}
		return nil
	}
	payload := func(i int) *tracepb.ResourceSpans {
		return deltaSpan(i, deltaKV("gen_ai.input.messages", deltaArray(deltaStr("shared zebra"))))
	}
	ownerCtx, ownerCancel := context.WithCancel(ctx)
	defer ownerCancel()
	owner := make(chan error, 1)
	go func() { _, err := e.ExportResourceSpans(ownerCtx, []*tracepb.ResourceSpans{payload(1)}); owner <- err }()
	awaitMessageCondition(t, ctx, func() bool { b.queue.mu.Lock(); defer b.queue.mu.Unlock(); return len(b.queue.pending) == 1 })
	type outcome struct {
		stats ExportStats
		err   error
	}
	waiters := make(chan outcome, 2)
	for _, i := range []int{2, 3} {
		go func() {
			stats, err := e.ExportResourceSpans(ctx, []*tracepb.ResourceSpans{payload(i)})
			waiters <- outcome{stats, err}
		}()
	}
	awaitMessageCondition(t, ctx, func() bool { return awaits.Load() == 2 })
	ownerCancel()
	requireOtelKind(t, <-owner, "canceled")
	awaitMessageCondition(t, ctx, func() bool {
		b.queue.mu.Lock()
		defer b.queue.mu.Unlock()
		n := 0
		for _, m := range b.queue.pending {
			if m.sub.err == nil {
				n++
			}
		}
		return n == 2
	})
	b.releaseWriter(held[0])
	held = held[1:]
	var written, present int64
	for range 2 {
		r := <-waiters
		if r.err != nil {
			t.Fatal(r.err)
		}
		written += r.stats.Messages.Written
		present += r.stats.Messages.Present
	}
	if written != 1 || present != 1 {
		t.Fatalf("same digest wrote %d/present %d", written, present)
	}
	if n := ftsCount(t, ctx, e, MessageFullTextQuery{Query: "zebra", Mode: MessageMatchAll}); n != 2 {
		t.Fatalf("waiters searchable spans %d", n)
	}
}
