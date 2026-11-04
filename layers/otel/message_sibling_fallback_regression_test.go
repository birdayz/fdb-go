package otel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func TestMessageSiblingFallbackRetainsCommittedHeaderOutcomes(t *testing.T) {
	t.Parallel()
	cluster := startExporterCluster(t)
	for _, terminal := range []bool{true, false} {
		t.Run(fmt.Sprintf("terminal=%t", terminal), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
			defer cancel()
			e := openFTSExporter(t, ctx, cluster, nil)
			var texts []string
			for _, prefix := range []string{"pword", "qword"} {
				words := make([]string, 600)
				for i := range words {
					words[i] = fmt.Sprintf("%s%04d", prefix, i)
				}
				texts = append(texts, strings.Join(words, " "))
			}
			input := deltaSpan(1, deltaKV("gen_ai.input.messages", deltaArray(deltaStr(texts[0]), deltaStr(texts[1]))))
			d, err := decomposeMessages(ctx, []*tracepb.ResourceSpans{input}, e.cfg.Tenant)
			if err != nil || len(d.elements) != 2 {
				t.Fatalf("fixture: %v / %v", d, err)
			}
			p, err := planMessage(e.cfg.Tenant, d.elements[0], true, 256)
			if err != nil {
				t.Fatal(err)
			}
			q, err := planMessage(e.cfg.Tenant, d.elements[1], true, 64)
			if err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int64
			var committedP atomic.Bool
			sentinel := errors.New("Q terminal failure after P committed")
			e.backend.messageTxnHook = func(stage string, _ *recordlayer.FDBRecordStore) error {
				if stage != "message-before-commit" {
					return nil
				}
				n := calls.Add(1)
				if n == 1 {
					return fdb.Error{Code: 2101}
				}
				if n != 4 {
					return nil
				}
				found, err := e.backend.readMessagePresence(ctx, e.cfg.Tenant, d.elements)
				if err != nil {
					return err
				}
				if !found[d.elements[0].digest].valid() || found[d.elements[1].digest].valid() {
					return errors.New("P committed/Q absent control failed")
				}
				committedP.Store(true)
				if terminal {
					return sentinel
				}
				return fdb.Error{Code: 2101}
			}
			stats, err := e.ExportResourceSpans(ctx, []*tracepb.ResourceSpans{input})
			if !committedP.Load() {
				t.Fatalf("did not reach committed-P/Q-failure boundary: calls=%d err=%v", calls.Load(), err)
			}
			wantWritten, wantParts, wantPostings := int64(1), int64(len(p.records)), p.postings
			if terminal {
				if !errors.Is(err, sentinel) {
					t.Fatalf("terminal cause lost: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				wantWritten = 2
				wantParts += int64(len(q.records))
				wantPostings += q.postings
				got, err := e.LoadSpanOTLP(ctx, ftsTrace(1), ftsSpan(1))
				if err != nil {
					t.Fatal(err)
				}
				requireProtoEqual(t, "sibling fallback", got, input)
			}
			if s := stats.Messages; s.Written != wantWritten || s.Present != 0 || s.Parts != wantParts || s.Postings != wantPostings {
				t.Fatalf("committed outcomes %+v want Written=%d Present=0 Parts=%d Postings=%d", s, wantWritten, wantParts, wantPostings)
			}
			e.backend.messageTxnHook = nil
			found, err := e.backend.readMessagePresence(ctx, e.cfg.Tenant, d.elements)
			if err != nil || !found[d.elements[0].digest].valid() || found[d.elements[1].digest].valid() == terminal {
				t.Fatalf("durable header outcomes: %v / %v", found, err)
			}
		})
	}
}
