package otel

import (
	"context"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func TestLargeNonpostingMessagesRetainRawAndPositions(t *testing.T) {
	t.Parallel()
	cluster := startExporterCluster(t)
	for _, name := range []string{"whitespace", "word", "mixed-whitespace", "mixed-word"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
			defer cancel()
			e := openFTSExporter(t, ctx, cluster, nil)
			middle := strings.Repeat(" ", 12<<20)
			wantUnindexed := int64(0)
			if strings.Contains(name, "word") {
				middle = strings.Repeat("x", 12<<20)
				wantUnindexed = 1
			}
			text := middle
			mixed := strings.HasPrefix(name, "mixed-")
			if mixed {
				text = "alpha " + middle + " omega"
			}
			input := deltaSpan(1, deltaKV("gen_ai.input.messages", deltaArray(deltaStr(text))))
			stats, err := e.ExportResourceSpans(ctx, []*tracepb.ResourceSpans{input})
			if err != nil {
				t.Fatal(err)
			}
			if stats.Messages.Written != 1 || stats.Messages.UnindexedTokens != wantUnindexed {
				t.Fatalf("raw-token counters: %+v, unindexed want %d", stats.Messages, wantUnindexed)
			}
			if !mixed && stats.Messages.Postings != 0 {
				t.Fatalf("nonposting message indexed: %+v", stats.Messages)
			}
			got, err := e.LoadSpanOTLP(ctx, ftsTrace(1), ftsSpan(1))
			if err != nil {
				t.Fatal(err)
			}
			requireProtoEqual(t, "large raw reconstruction", got, input)
			if mixed {
				if n := ftsCount(t, ctx, e, MessageFullTextQuery{Query: "alpha omega", Mode: MessageMatchAll}); n != 1 {
					t.Fatalf("all search: %d", n)
				}
				wantPhrase := 1
				if wantUnindexed != 0 {
					wantPhrase = 0
				}
				if n := ftsCount(t, ctx, e, MessageFullTextQuery{Query: "alpha omega", Mode: MessageMatchPhrase}); n != wantPhrase {
					t.Fatalf("phrase search %d want %d", n, wantPhrase)
				}
				if n := ftsCount(t, ctx, e, MessageFullTextQuery{Query: "alphaomega", Mode: MessageMatchAll}); n != 0 {
					t.Fatalf("false concatenated token: %d", n)
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
				if err := deltaRun(t, ctx, e.backend.db, func(rc *recordlayer.FDBRecordContext) error {
					store, err := e.backend.open(rc)
					if err != nil {
						return err
					}
					cmp := recordlayer.NewTextWithMaxDistanceComparisonTokens([]string{"alpha", "omega"}, 1, "", "")
					group := tuple.Tuple{int64(e.cfg.Tenant), "", messageShard(id.writer)}
					budget := queryBudget{maxCandidates: e.cfg.MaxCandidates, maxBytes: e.cfg.MaxBytes}
					ids, err := textScanIDs(ctx, store, recordlayer.NewTextScan(e.backend.md.GetIndex(messageTextIndexName), group, cmp, nil), 0, &budget)
					if err == nil && len(ids) != wantPhrase {
						t.Errorf("ALL_WITHIN distance 1=%v want %d", ids, wantPhrase)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			again, err := e.ExportResourceSpans(ctx, []*tracepb.ResourceSpans{input})
			if err != nil || again.Messages.Written != 0 || again.Messages.Present != 1 {
				t.Fatalf("large dedup: %+v / %v", again, err)
			}
		})
	}
}
