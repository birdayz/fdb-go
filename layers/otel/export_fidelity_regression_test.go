package otel

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func TestMessageLayoutValidationNamesVersionedIdentity(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	cfg.Run, cfg.Backend = "layout_validation", "fdb"
	cfg.Messages = "fulltext"
	err := cfg.Validate()
	requireOtelKind(t, err, "config")
	if !strings.Contains(err.Error(), messagesFullText) {
		t.Fatalf("validation advertises the retired layout: %v", err)
	}
	cfg.Messages = messagesFullText
	if err := cfg.Validate(); err != nil {
		t.Fatalf("versioned layout refused: %v", err)
	}
}

func TestExporterPreservesProducerFidelityTag(t *testing.T) {
	t.Parallel()
	cluster := startExporterCluster(t)
	for _, normalize := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "normalized"}[normalize], func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			e := openTestExporter(t, ctx, cluster, nil)
			rs := deltaSpan(1)
			if normalize {
				rs.ScopeSpans[0].Spans[0].Attributes = []*commonpb.KeyValue{exKV("duplicate", exInt(1)), exKV("duplicate", exInt(2))}
			}
			var unknown []byte
			for _, payload := range [][]byte{[]byte("producer-owned"), {0xff}, nil} {
				unknown = protowire.AppendTag(unknown, fidelityField, protowire.BytesType)
				unknown = protowire.AppendBytes(unknown, payload)
			}
			rs.ProtoReflect().SetUnknown(unknown)
			before := proto.Clone(rs).(*tracepb.ResourceSpans)
			if _, err := e.ExportResourceSpans(ctx, []*tracepb.ResourceSpans{rs}); err != nil {
				t.Fatal(err)
			}
			sp := rs.ScopeSpans[0].Spans[0]
			got, err := e.LoadSpanOTLP(ctx, hex.EncodeToString(sp.TraceId), hex.EncodeToString(sp.SpanId))
			if err != nil {
				t.Fatal(err)
			}
			requireProtoEqual(t, "producer unknown tag", got, before)
			requireProtoEqual(t, "input remains unchanged", rs, before)
		})
	}
}

func TestMessageFreeLastDuplicateClearsReferences(t *testing.T) {
	t.Parallel()
	cluster := startExporterCluster(t)
	for _, crossWrapper := range []bool{false, true} {
		for _, event := range []bool{false, true} {
			t.Run(map[bool]string{false: "same", true: "cross"}[crossWrapper]+map[bool]string{false: "_span", true: "_event"}[event], func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
				defer cancel()
				e := openFTSExporter(t, ctx, cluster, nil)
				first := deltaSpan(2, deltaKV("gen_ai.input.messages", deltaArray(deltaStr("zebra"))))
				sp := first.ScopeSpans[0].Spans[0]
				if event {
					sp.Events = []*tracepb.Span_Event{{Attributes: sp.Attributes}}
					sp.Attributes = nil
				}
				last := proto.Clone(first).(*tracepb.ResourceSpans)
				winner := last.ScopeSpans[0].Spans[0]
				winner.Attributes, winner.Events = nil, nil
				input := []*tracepb.ResourceSpans{first, last}
				if !crossWrapper {
					first.ScopeSpans[0].Spans = append(first.ScopeSpans[0].Spans, winner)
					input = input[:1]
				}
				if _, err := e.ExportResourceSpans(ctx, input); err != nil {
					t.Fatal(err)
				}
				if n := ftsCount(t, ctx, e, MessageFullTextQuery{Query: "zebra", Mode: MessageMatchAll}); n != 0 {
					t.Fatalf("stale references returned %d hits", n)
				}
				got, err := e.LoadSpanOTLP(ctx, hex.EncodeToString(winner.TraceId), hex.EncodeToString(winner.SpanId))
				if err != nil {
					t.Fatal(err)
				}
				if len(got.ScopeSpans[0].Spans[0].Attributes) != 0 || len(got.ScopeSpans[0].Spans[0].Events) != 0 {
					t.Fatal("last duplicate did not win")
				}
			})
		}
	}
}
