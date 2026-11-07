package otel

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// BenchmarkMessageWriteCostRealFDB is the R1/R4 measurement of
// rfcs/relational-exporter-migration.md §7: client CPU and stored bytes of
// GenAI span ingest with messages, the custom store (Exporter, FDBBackend)
// against the relational exporter (relExporter, relMessageStore, SPANS rows
// with ref_chunks) in one binary, one FDB container, sequential arms. Run
// it alone:
//
//	bazelisk test //layers/otel:otel_test --test_arg=-test.run='^$' \
//	  --test_arg=-test.bench=BenchmarkMessageWriteCostRealFDB \
//	  --test_arg=-test.benchtime=1x --test_arg=-test.count=2 --test_output=streamed
//
// Arms: fdb_* the old Exporter, rel_* the relational exporter, both with
// record_compression none so record bytes compare; *_fulltext with full text,
// *_deltas messages without text, rel_fulltext_norefs full text without
// ref_chunks, isolating SPANS_REF_CHUNKS. CPU is the process's user+sys
// over the export calls; FDB runs in its container, outside it.
func BenchmarkMessageWriteCostRealFDB(b *testing.B) {
	cluster := startCostCluster(b)
	for _, w := range []struct{ convs, turns int }{{40, 25}, {5, 200}} {
		calls := costWorkload(w.convs, w.turns)
		spans := float64(w.convs * w.turns)
		for _, arm := range []string{"fdb_fulltext", "rel_fulltext", "rel_fulltext_norefs", "fdb_deltas", "rel_deltas"} {
			b.Run(fmt.Sprintf("%dx%d/%s", w.convs, w.turns, arm), func(b *testing.B) {
				for range b.N {
					var r costResult
					if arm[:3] == "fdb" {
						r = costOld(b, cluster, arm == "fdb_fulltext", calls)
					} else {
						r = costRel(b, cluster, arm != "rel_deltas", arm == "rel_fulltext_norefs", calls)
					}
					b.ReportMetric(float64(r.cpu.Microseconds())/spans, "cpu_us/span")
					b.ReportMetric(float64(r.storeBytes)/spans, "store_B/span")
					b.ReportMetric(float64(r.refBytes)/spans, "refs_B/span")
					b.ReportMetric(float64(r.textBytes)/spans, "text_B/span")
					b.ReportMetric(float64(r.messages.Retries), "msg_retries")
					b.Logf("%s %dx%d: cpu %v wall %v; bytes store %d refs %d (%d entries, max key %d) text %d (prefix %d); messages %+v",
						arm, w.convs, w.turns, r.cpu, r.wall, r.storeBytes, r.refBytes, r.refEntries, r.refMaxKey, r.textBytes, r.textPrefix, r.messages)
				}
			})
		}
	}
}

type costResult struct {
	cpu, wall                       time.Duration
	storeBytes, refBytes, textBytes int64
	refEntries, refMaxKey           int
	textPrefix                      int // bytes of the TEXT index subspace prefix
	messages                        MessageStoreStats
}

// costWorkload is convs conversations of turns turns, one trace each, as
// deltaConversation builds them but with distinct text per conversation
// (system instructions and tools are shared), exported 25 spans per call.
func costWorkload(convs, turns int) [][]*tracepb.ResourceSpans {
	var calls [][]*tracepb.ResourceSpans
	for c := range convs {
		var history []deltaMsg
		var call []*tracepb.ResourceSpans
		trace := make([]byte, 16)
		binary.BigEndian.PutUint64(trace[8:], uint64(c+1))
		for k := 1; k <= turns; k++ {
			history = append(history, deltaMsg{"user", fmt.Sprintf("conversation %d %s", c, deltaText("user", k, 800))})
			reply := deltaMsg{"assistant", fmt.Sprintf("conversation %d %s", c, deltaText("assistant", k, 800))}
			sys := deltaArray(deltaKvlist(deltaKV("type", deltaStr("text")), deltaKV("content", deltaStr(deltaSystem))))
			rs := deltaSpan(k,
				deltaKV("gen_ai.operation.name", deltaStr("chat")),
				deltaKV("gen_ai.conversation.id", deltaStr(fmt.Sprintf("conv-%d", c))),
				deltaKV("gen_ai.system_instructions", sys),
				deltaKV("gen_ai.tool.definitions", deltaToolDefinitions("array")),
				deltaKV("gen_ai.input.messages", deltaMessages("array", history)),
				deltaKV("gen_ai.output.messages", deltaMessages("array", []deltaMsg{reply})),
			)
			sp := rs.ScopeSpans[0].Spans[0]
			sp.TraceId = trace
			sp.SpanId = binary.BigEndian.AppendUint64(nil, uint64(c<<16|k))
			sp.StartTimeUnixNano = uint64(ftsBase) + uint64(c*100000+k)
			sp.EndTimeUnixNano = sp.StartTimeUnixNano + 1000
			history = append(history, reply)
			call = append(call, rs)
			if len(call) == 25 || k == turns {
				calls = append(calls, call)
				call = nil
			}
		}
	}
	return calls
}

func processCPU(b *testing.B) time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		b.Fatal(err)
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

func costRun(b *testing.B) string {
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		b.Fatal(err)
	}
	return "cost_" + hex.EncodeToString(nonce[:])
}

// costOld exports calls through the old Exporter.
func costOld(b *testing.B, cluster string, fullText bool, calls [][]*tracepb.ResourceSpans) costResult {
	ctx := context.Background()
	cfg := DefaultExporterConfig()
	cfg.ClusterFile, cfg.Run, cfg.Tenant = cluster, costRun(b), 11
	cfg.RecordCompression = "none"
	cfg.EnableMessageDeltas, cfg.EnableMessageFullText = true, fullText
	e, err := NewExporter(ctx, cfg)
	if err != nil {
		b.Fatal(err)
	}
	defer e.Shutdown(ctx)
	var r costResult
	inputs := make([][]*tracepb.ResourceSpans, len(calls))
	for i, c := range calls {
		inputs[i] = cloneDeltaSpans(c)
	}
	started, cpu := time.Now(), processCPU(b)
	for _, in := range inputs {
		st, err := e.ExportResourceSpans(ctx, in)
		if err != nil {
			b.Fatal(err)
		}
		r.messages.Add(st.Messages)
	}
	r.cpu, r.wall = processCPU(b)-cpu, time.Since(started)
	be := e.backend
	if _, err := be.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		store, err := be.open(rc)
		if err != nil {
			return nil, err
		}
		if fullText {
			r.refBytes, r.refEntries, r.refMaxKey = costRange(b, ctx, be.db, store.IndexSubspace(be.md.GetIndex(envelopeRefsIndexName)).Bytes())
			text := store.IndexSubspace(be.md.GetIndex(messageTextIndexName)).Bytes()
			r.textBytes, _, _ = costRange(b, ctx, be.db, text)
			r.textPrefix = len(text)
		}
		return nil, nil
	}); err != nil {
		b.Fatal(err)
	}
	r.storeBytes, _, _ = costRange(b, ctx, be.db, be.keys.data.Bytes())
	return r
}

// noRefHooks is the message store without SPANS ref_chunks (measurement only).
type noRefHooks struct{ *relMessageStore }

func (noRefHooks) refChunks([]messageUse) ([][]byte, error) { return nil, nil }

// costRel exports calls through a relExporter on a fresh tenant store.
func costRel(b *testing.B, cluster string, fullText, noRefs bool, calls [][]*tracepb.ResourceSpans) costResult {
	ctx := context.Background()
	cfg := DefaultExporterConfig()
	cfg.ClusterFile, cfg.Run, cfg.Tenant = cluster, costRun(b), 11
	cfg.RecordCompression = "none"
	cfg.EnableMessageDeltas, cfg.EnableMessageFullText = true, fullText
	var parts relParts
	if noRefs {
		parts.messages = func(s *tenantStore, cfg ExporterConfig) (relMessageHooks, error) {
			return noRefHooks{newRelMessageStore(s, cfg.EnableMessageFullText)}, nil
		}
	}
	e, err := newRelExporter(ctx, cfg, parts)
	if err != nil {
		b.Fatal(err)
	}
	defer e.Shutdown(ctx)
	var r costResult
	inputs := make([][]*tracepb.ResourceSpans, len(calls))
	for i, c := range calls {
		inputs[i] = cloneDeltaSpans(c)
	}
	started, cpu := time.Now(), processCPU(b)
	for _, in := range inputs {
		st, err := e.ExportResourceSpans(ctx, in)
		if err != nil {
			b.Fatal(err)
		}
		r.messages.Add(st.Messages)
	}
	r.cpu, r.wall = processCPU(b)-cpu, time.Since(started)
	db := e.store.db
	if _, err := db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		store, err := e.store.ts.Builder(rc).Open()
		if err != nil {
			return nil, err
		}
		md := store.GetRecordMetaData()
		r.refBytes, r.refEntries, r.refMaxKey = costRange(b, ctx, db, store.IndexSubspace(md.GetIndex(spansRefChunksIndex)).Bytes())
		text := store.IndexSubspace(md.GetIndex(messagesTextIndex)).Bytes()
		r.textBytes, _, _ = costRange(b, ctx, db, text)
		r.textPrefix = len(text)
		return nil, nil
	}); err != nil {
		b.Fatal(err)
	}
	r.storeBytes, _, _ = costRange(b, ctx, db, e.store.ts.Subspace.Bytes())
	return r
}

// costRange is the key and value bytes, entries and longest key under prefix.
func costRange(b *testing.B, ctx context.Context, db *recordlayer.FDBDatabase, prefix []byte) (int64, int, int) {
	var total int64
	var rows, maxKey int
	begin := fdb.Key(prefix)
	pr, err := fdb.PrefixRange(prefix)
	if err != nil {
		b.Fatal(err)
	}
	for {
		var page []fdb.KeyValue
		if _, err := db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
			var err error
			page, err = rc.Transaction().Snapshot().GetRange(fdb.KeyRange{Begin: begin, End: pr.End}, fdb.RangeOptions{Limit: 5000}).GetSliceWithError()
			return nil, err
		}); err != nil {
			b.Fatal(err)
		}
		for _, kv := range page {
			total += int64(len(kv.Key) + len(kv.Value))
			maxKey = max(maxKey, len(kv.Key))
		}
		rows += len(page)
		if len(page) < 5000 {
			return total, rows, maxKey
		}
		begin = append(append(fdb.Key(nil), page[len(page)-1].Key...), 0)
	}
}

func startCostCluster(b *testing.B) string {
	setup, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	container, err := foundationdbtc.Run(setup, "", foundationdbtc.WithAPIVersion(730))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = container.Terminate(ctx)
	})
	wire, err := container.ClusterFile(setup)
	if err != nil {
		b.Fatal(err)
	}
	cluster := filepath.Join(b.TempDir(), "fdb.cluster")
	if err := os.WriteFile(cluster, []byte(wire), 0o600); err != nil {
		b.Fatal(err)
	}
	return cluster
}
