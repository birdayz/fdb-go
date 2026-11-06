package otel

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func TestCrossPartAllReadsEarlierSameWriterSignpost(t *testing.T) {
	t.Parallel()
	cluster := startExporterCluster(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	e := openFTSExporter(t, ctx, cluster, nil)
	small := deltaSpan(1, deltaKV("gen_ai.input.messages", deltaArray(deltaStr("zebra"))))
	if _, err := e.ExportResourceSpans(ctx, []*tracepb.ResourceSpans{small}); err != nil {
		t.Fatal(err)
	}
	words := []string{"zebra"}
	for i := range 1100 {
		words = append(words, fmt.Sprintf("term%04d", i))
	}
	words = append(words, "platypus")
	text := strings.Join(words, " ")
	segments, _, err := segmentMessageText(text, messageSegmentTokens)
	if err != nil || len(segments) < 2 {
		t.Fatalf("multipart text fixture: %d / %v", len(segments), err)
	}
	for _, segment := range segments {
		if strings.Contains(segment.text, "zebra") && strings.Contains(segment.text, "platypus") {
			t.Fatal("single part satisfies all terms")
		}
	}
	large := deltaSpan(2, deltaKV("gen_ai.input.messages", deltaArray(deltaStr(text))))
	if _, err := e.ExportResourceSpans(ctx, []*tracepb.ResourceSpans{large}); err != nil {
		t.Fatal(err)
	}
	ids := make([]messageID, 2)
	for i, input := range []*tracepb.ResourceSpans{small, large} {
		d, err := decomposeMessages(ctx, []*tracepb.ResourceSpans{input}, e.cfg.Tenant)
		if err != nil {
			t.Fatal(err)
		}
		found, err := e.backend.readMessagePresence(ctx, e.cfg.Tenant, d.elements)
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = found[d.elements[0].digest]
	}
	if !ids[0].valid() || ids[0].writer != ids[1].writer || ids[0].seq >= ids[1].seq {
		t.Fatalf("not earlier same writer: %v", ids)
	}
	proven := false
	if err := deltaRun(t, ctx, e.backend.db, func(rc *recordlayer.FDBRecordContext) error {
		store, err := e.backend.open(rc)
		if err != nil {
			return err
		}
		space := store.IndexSubspace(e.backend.md.GetIndex(messageTextIndexName)).Sub(int64(e.cfg.Tenant), "", messageShard(ids[1].writer), "zebra")
		rows, err := rc.Transaction().GetRange(space, fdb.RangeOptions{}).GetSliceWithError()
		if err != nil {
			return err
		}
		logical := space.Pack(tuple.Tuple{ids[1].writer, ids[1].seq})
		serializer := &recordlayer.TextIndexBunchedSerializer{}
		for _, row := range rows {
			boundary, err := space.Unpack(row.Key)
			if err != nil {
				return err
			}
			entries, err := serializer.DeserializeEntries(boundary, row.Value)
			if err != nil {
				return err
			}
			if len(boundary) < 2 || boundary[0] != ids[0].writer || boundary[1] != ids[0].seq || bytes.Compare(row.Key, logical) >= 0 {
				continue
			}
			for _, entry := range entries {
				if len(entry.Key) >= 2 && entry.Key[0] == ids[1].writer && entry.Key[1] == ids[1].seq {
					proven = true
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !proven {
		t.Fatal("new posting not physically proven inside earlier same-writer signpost bunch")
	}
	q := MessageFullTextQuery{Query: "zebra platypus", Mode: MessageMatchAll, Limit: 1}
	hits, pages := ftsDrain(t, ctx, e, q)
	if len(hits) != 1 || hits[0].Span != ftsSpan(2) || pages == 0 {
		t.Fatalf("bounded cross-part match: %+v pages=%d", hits, pages)
	}
	result, err := e.QueryTraceExistential(ctx, TraceExistentialQuery{MessageClauses: []MessageFullTextQuery{{Query: q.Query, Mode: q.Mode}}})
	if err != nil || len(result.Hits) != 1 || result.Hits[0].Trace != ftsTrace(2) {
		t.Fatalf("trace clause: %+v / %v", result, err)
	}
}
