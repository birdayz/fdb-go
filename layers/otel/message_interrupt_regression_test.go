package otel

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/recordlayer"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func TestInterruptedMultipartDoesNotPublishHeadOrSpan(t *testing.T) {
	t.Parallel()
	cluster := startExporterCluster(t)
	for _, failure := range []string{"error", "cancel"} {
		t.Run(failure, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
			defer cancel()
			e := openFTSExporter(t, ctx, cluster, nil)
			input := deltaSpan(1, deltaKV("gen_ai.input.messages", deltaArray(deltaStr("zebra "+strings.Repeat(" ", 3<<20)+" omega"))))
			d, err := decomposeMessages(ctx, []*tracepb.ResourceSpans{input}, e.cfg.Tenant)
			if err != nil {
				t.Fatal(err)
			}
			attempt, stop := context.WithCancel(ctx)
			defer stop()
			sentinel := errors.New("later multipart transaction refused")
			var commits atomic.Int64
			var tailProven atomic.Bool
			var tailID messageID
			e.backend.messageTxnHook = func(stage string, _ *recordlayer.FDBRecordStore) error {
				if stage != "message-before-commit" || commits.Add(1) != 2 {
					return nil
				}
				if err := deltaRun(t, ctx, e.backend.db, func(rc *recordlayer.FDBRecordContext) error {
					store, err := e.backend.open(rc)
					if err != nil {
						return err
					}
					records, err := recordlayer.AsList(ctx, store.ScanRecords(nil, recordlayer.ForwardScan()))
					if err != nil {
						return err
					}
					for _, r := range records {
						m := r.Record.ProtoReflect()
						f := m.Descriptor().Fields()
						if f.ByName("part") == nil || m.Get(f.ByName("part")).Int() == 0 {
							continue
						}
						tailID = messageID{m.Get(f.ByName("writer")).Int(), m.Get(f.ByName("seq")).Int()}
						tailProven.Store(true)
					}
					return nil
				}); err != nil {
					return err
				}
				if !tailProven.Load() {
					return errors.New("no independently committed tail observed")
				}
				if failure == "cancel" {
					stop()
					return context.Canceled
				}
				return sentinel
			}
			stats, err := e.ExportResourceSpans(attempt, []*tracepb.ResourceSpans{input})
			if failure == "error" && !errors.Is(err, sentinel) {
				t.Fatalf("terminal cause: %v", err)
			}
			if failure == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel cause: %v", err)
			}
			if !tailProven.Load() || stats.Messages.Written != 0 || stats.Messages.Parts == 0 {
				t.Fatalf("committed-tail evidence/counters: %v / %+v", tailProven.Load(), stats.Messages)
			}
			e.backend.messageTxnHook = nil
			if err := deltaRun(t, ctx, e.backend.db, func(rc *recordlayer.FDBRecordContext) error {
				store, err := e.backend.open(rc)
				if err != nil {
					return err
				}
				head, err := store.LoadRecord(messageKey(e.cfg.Tenant, tailID, 0))
				if err != nil {
					return err
				}
				if head != nil {
					t.Error("interrupted message published head")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			found, err := e.backend.readMessagePresence(ctx, e.cfg.Tenant, d.elements)
			if err != nil || found[d.elements[0].digest].valid() {
				t.Fatalf("interrupted hash present: %v / %v", found, err)
			}
			_, err = e.LoadSpanOTLP(ctx, ftsTrace(1), ftsSpan(1))
			var missing *SpanNotFoundError
			if !errors.As(err, &missing) {
				t.Fatalf("interrupted span published: %v", err)
			}
			if _, err := e.ExportResourceSpans(ctx, []*tracepb.ResourceSpans{input}); err != nil {
				t.Fatal(err)
			}
			got, err := e.LoadSpanOTLP(ctx, ftsTrace(1), ftsSpan(1))
			if err != nil {
				t.Fatal(err)
			}
			requireProtoEqual(t, "multipart recovery", got, input)
			if n := ftsCount(t, ctx, e, MessageFullTextQuery{Query: "zebra omega", Mode: MessageMatchAll}); n != 1 {
				t.Fatalf("recovered search: %d", n)
			}
		})
	}
}
