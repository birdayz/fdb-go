package otel

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func TestMessageOverlongRawWordWithShortNormalizedToken(t *testing.T) {
	t.Parallel()
	cluster := startExporterCluster(t)
	for _, between := range []bool{false, true} {
		t.Run(fmt.Sprintf("between=%t", between), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
			defer cancel()
			e := openFTSExporter(t, ctx, cluster, nil)
			text := "a" + strings.Repeat("\u0301", 6<<20)
			query := "a"
			if between {
				text = "zebra " + text + " omega"
				query = "zebra a omega"
			}
			segments, unindexed, err := segmentMessageText(text, messageSegmentTokens)
			if err != nil || unindexed != 0 || len(segments) == 0 {
				t.Fatalf("normalized segments=%d unindexed=%d err=%v", len(segments), unindexed, err)
			}
			for _, segment := range segments {
				if len(segment.text) > messageSegmentBytes {
					t.Fatalf("indexed segment bytes=%d", len(segment.text))
				}
			}
			input := deltaSpan(1, deltaKV("gen_ai.input.messages", deltaArray(deltaStr(text))))
			stats, err := e.ExportResourceSpans(ctx, []*tracepb.ResourceSpans{input})
			if err != nil {
				t.Fatal(err)
			}
			if stats.Messages.Written != 1 || stats.Messages.UnindexedTokens != 0 {
				t.Fatalf("message outcomes: %+v", stats.Messages)
			}
			got, err := e.LoadSpanOTLP(ctx, ftsTrace(1), ftsSpan(1))
			if err != nil {
				t.Fatal(err)
			}
			requireProtoEqual(t, "lossless raw combining word", got, input)
			if n := ftsCount(t, ctx, e, MessageFullTextQuery{Query: query, Mode: MessageMatchAll}); n != 1 {
				t.Fatalf("normalized search count=%d", n)
			}
		})
	}
}
