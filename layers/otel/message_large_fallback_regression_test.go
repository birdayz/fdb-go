package otel

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func TestInitiallyMultipartSizeFallbackAfterCommittedTail(t *testing.T) {
	t.Parallel()
	cluster := startExporterCluster(t)
	// A non-indexed, poorly compressible raw word forces multiple PART
	// transactions, while >1024 distinct terms select the large path initially.
	rng := rand.New(rand.NewPCG(7, 11))
	raw := make([]byte, 7<<20)
	for i := range raw {
		raw[i] = byte('a' + rng.IntN(26))
	}
	words := make([]string, 1100)
	for i := range words {
		words[i] = fmt.Sprintf("term%04d", i)
	}
	text := strings.Join(words, " ") + " " + string(raw)
	for _, exhaust := range []bool{false, true} {
		t.Run(fmt.Sprintf("exhaust=%t", exhaust), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
			defer cancel()
			e := openFTSExporter(t, ctx, cluster, nil)
			input := deltaSpan(1, deltaKV("gen_ai.input.messages", deltaArray(deltaStr(text))))
			d, err := decomposeMessages(ctx, []*tracepb.ResourceSpans{input}, e.cfg.Tenant)
			if err != nil {
				t.Fatal(err)
			}
			initial, err := planMessage(e.cfg.Tenant, d.elements[0], true, messageSegmentTokens)
			if err != nil || len(initial.records) < 3 {
				t.Fatalf("initial large plan=%v err=%v", initial, err)
			}
			var calls atomic.Int64
			var abandoned messageID
			var committedParts int64
			e.backend.messageTxnHook = func(stage string, store *recordlayer.FDBRecordStore) error {
				if stage != "message-before-commit" {
					return nil
				}
				n := calls.Add(1)
				if n < 2 {
					return nil
				}
				if n == 2 {
					// The attempted second transaction must still be a PART
					// transaction: its own view contains no message head.
					pending, err := recordlayer.AsList(ctx, store.ScanRecords(nil, recordlayer.ForwardScan()))
					if err != nil {
						return err
					}
					for _, r := range pending {
						m := r.Record.ProtoReflect()
						f := m.Descriptor().Fields()
						if f.ByName("part") != nil && m.Get(f.ByName("part")).Int() == 0 {
							return errors.New("second transaction was HEAD, not PART")
						}
					}
					if err := deltaRun(t, ctx, e.backend.db, func(rc *recordlayer.FDBRecordContext) error {
						s, err := e.backend.open(rc)
						if err != nil {
							return err
						}
						rows, err := recordlayer.AsList(ctx, s.ScanRecords(nil, recordlayer.ForwardScan()))
						if err != nil {
							return err
						}
						committedParts = 0
						for _, r := range rows {
							m := r.Record.ProtoReflect()
							f := m.Descriptor().Fields()
							if f.ByName("part") == nil {
								continue
							}
							if m.Get(f.ByName("part")).Int() == 0 {
								return errors.New("premature committed HEAD")
							}
							abandoned = messageID{m.Get(f.ByName("writer")).Int(), m.Get(f.ByName("seq")).Int()}
							committedParts++
						}
						return nil
					}); err != nil {
						return err
					}
					if committedParts == 0 {
						return errors.New("no independent committed-tail control")
					}
				}
				if n == 2 || exhaust {
					return fdb.Error{Code: 2101}
				}
				return nil
			}
			stats, err := e.ExportResourceSpans(ctx, []*tracepb.ResourceSpans{input})
			if committedParts == 0 || !abandoned.valid() {
				t.Fatalf("missing committed tail evidence: calls=%d err=%v", calls.Load(), err)
			}
			if exhaust {
				if code, ok := fdbErrorCode(err); !ok || code != 2101 {
					t.Fatalf("terminal cause=%v", err)
				}
				if stats.Messages.Written != 0 || stats.Messages.Present != 0 || stats.Messages.Parts != committedParts || stats.Messages.Postings != 0 {
					t.Fatalf("exhausted outcomes=%+v committedParts=%d", stats.Messages, committedParts)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				fallback, err := planMessage(e.cfg.Tenant, d.elements[0], true, messageSegmentTokens/4)
				if err != nil {
					t.Fatal(err)
				}
				if s := stats.Messages; s.Written != 1 || s.Present != 0 || s.Parts != committedParts+int64(len(fallback.records)) || s.Postings != fallback.postings {
					t.Fatalf("fallback outcomes=%+v abandonedParts=%d publishedParts=%d", s, committedParts, len(fallback.records))
				}
				got, err := e.LoadSpanOTLP(ctx, ftsTrace(1), ftsSpan(1))
				if err != nil {
					t.Fatal(err)
				}
				requireProtoEqual(t, "large-path fallback reconstruction", got, input)
			}
			e.backend.messageTxnHook = nil
			if err := deltaRun(t, ctx, e.backend.db, func(rc *recordlayer.FDBRecordContext) error {
				store, err := e.backend.open(rc)
				if err != nil {
					return err
				}
				head, err := store.LoadRecord(messageKey(e.cfg.Tenant, abandoned, 0))
				if err != nil {
					return err
				}
				if head != nil {
					t.Error("abandoned ID became visible through a head")
				}
				rows, err := recordlayer.AsList(ctx, store.ScanRecords(nil, recordlayer.ForwardScan()))
				if err != nil {
					return err
				}
				retained := make(map[int64]bool)
				for _, r := range rows {
					m := r.Record.ProtoReflect()
					f := m.Descriptor().Fields()
					if f.ByName("part") == nil || m.Get(f.ByName("writer")).Int() != abandoned.writer || m.Get(f.ByName("seq")).Int() != abandoned.seq {
						continue
					}
					part := m.Get(f.ByName("part")).Int()
					if part <= 0 || part > committedParts || retained[part] {
						t.Fatalf("unexpected retained abandoned part: id=%v part=%d", abandoned, part)
					}
					retained[part] = true
				}
				if int64(len(retained)) != committedParts {
					t.Fatalf("abandoned tail retention: id=%v rows=%v want parts1..%d", abandoned, retained, committedParts)
				}
				for part := int64(1); part <= committedParts; part++ {
					if !retained[part] {
						t.Fatalf("lost abandoned tail: id=%v part=%d", abandoned, part)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			found, err := e.backend.readMessagePresence(ctx, e.cfg.Tenant, d.elements)
			if err != nil || found[d.elements[0].digest] == abandoned || found[d.elements[0].digest].valid() == exhaust {
				t.Fatalf("dedup published abandoned/missing ID: %v / %v", found, err)
			}
			want := 1
			if exhaust {
				want = 0
			}
			if n := ftsCount(t, ctx, e, MessageFullTextQuery{Query: "term0000 term1099", Mode: MessageMatchAll}); n != want {
				t.Fatalf("abandoned IDs searchable: count=%d want=%d", n, want)
			}
		})
	}
}
