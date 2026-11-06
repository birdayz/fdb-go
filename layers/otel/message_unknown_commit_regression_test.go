package otel

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func TestMessageUnknownCommitDoesNotReuseSequence(t *testing.T) {
	t.Parallel()
	cluster := startExporterCluster(t)
	for _, code := range []int{1021, 1039} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			e := openFTSExporter(t, ctx, cluster, nil)
			b := e.backend
			w, err := b.acquireWriter(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer b.releaseWriter(w)
			inputs := []*tracepb.ResourceSpans{
				deltaSpan(1, deltaKV("gen_ai.input.messages", deltaArray(deltaStr("zebra original")))),
				deltaSpan(2, deltaKV("gen_ai.input.messages", deltaArray(deltaStr("platypus distinct")))),
			}
			var ids []messageID
			var mu sync.Mutex
			committed := false
			b.messageTxnHook = func(stage string, store *recordlayer.FDBRecordStore) error {
				if stage != "message-before-commit" || committed {
					return nil
				}
				// Commit the real transaction, then report an ambiguous result to
				// the normal retry loop. Its next attempt must observe the dedup hit.
				if err := store.Context().Transaction().Commit().Get(); err != nil {
					return err
				}
				committed = true
				return fdb.Error{Code: code}
			}
			for i, input := range inputs {
				d, err := decomposeMessages(ctx, []*tracepb.ResourceSpans{input}, e.cfg.Tenant)
				if err != nil {
					t.Fatal(err)
				}
				p, err := planMessage(e.cfg.Tenant, d.elements[0], true, messageSegmentTokens)
				if err != nil {
					t.Fatal(err)
				}
				var stats MessageStoreStats
				if err := b.writeSmallMessages(ctx, e.cfg.Tenant, w, []*messagePlan{p}, &stats, &mu); err != nil {
					t.Fatal(err)
				}
				if i == 0 && (!committed || stats.Retries != 1 || stats.Present != 1) {
					t.Fatalf("ambiguous commit/retry control: committed=%t stats=%+v", committed, stats)
				}
				ids = append(ids, p.element.id)
				b.messageTxnHook = nil
				// Publish the first span's references BEFORE the next distinct
				// message is written by this very same writer.
				if _, err := e.ExportResourceSpans(ctx, []*tracepb.ResourceSpans{input}); err != nil {
					t.Fatal(err)
				}
			}
			if ids[0].writer != ids[1].writer || ids[0].seq >= ids[1].seq {
				t.Fatalf("reused sequence after ambiguous commit: %v", ids)
			}
			for i, input := range inputs {
				got, err := e.LoadSpanOTLP(ctx, ftsTrace(i+1), ftsSpan(i+1))
				if err != nil {
					t.Fatal(err)
				}
				requireProtoEqual(t, "old and new manifest reconstruction", got, input)
				d, err := decomposeMessages(ctx, []*tracepb.ResourceSpans{input}, e.cfg.Tenant)
				if err != nil {
					t.Fatal(err)
				}
				found, err := b.readMessagePresence(ctx, e.cfg.Tenant, d.elements)
				if err != nil || found[d.elements[0].digest] != ids[i] {
					t.Fatalf("dedup identity changed: %v / %v", found, err)
				}
			}
			for _, word := range []string{"zebra original", "platypus distinct"} {
				if n := ftsCount(t, ctx, e, MessageFullTextQuery{Query: word, Mode: MessageMatchAll}); n != 1 {
					t.Fatalf("TEXT references for %q: %d", word, n)
				}
			}
		})
	}
}
