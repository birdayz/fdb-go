package otel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/proto"
)

func TestLogPersistedPostingDisagreement(t *testing.T) {
	t.Parallel()
	cluster := startExporterCluster(t)
	for _, damage := range []string{"trace", "span", "time", "attribute"} {
		t.Run(damage, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			e := openTestExporter(t, ctx, cluster, nil)
			original := LogRecord{EventID: "stale", Timestamp: 7, TraceID: ftsTrace(1), SpanID: ftsSpan(1), Attributes: []*commonpb.KeyValue{exKV("kind", exStr("old"))}}
			if _, err := e.ExportLogs(ctx, []LogRecord{original}); err != nil {
				t.Fatal(err)
			}
			if got, err := e.QueryLogsByTrace(ctx, original.TraceID, original.SpanID); err != nil || len(got) != 1 {
				t.Fatalf("live trace positive control: %v / %v", got, err)
			}
			a := Attribute{LogScopeRecord, "kind", "string", "old"}
			if got, err := e.QueryLogsByAttribute(ctx, a, 0); err != nil || len(got) != 1 {
				t.Fatalf("live equality positive control: %v / %v", got, err)
			}
			changed := original
			switch damage {
			case "trace":
				changed.TraceID = ftsTrace(2)
			case "span":
				changed.SpanID = ftsSpan(2)
			case "time":
				changed.Timestamp++
			case "attribute":
				changed.Attributes = []*commonpb.KeyValue{exKV("kind", exStr("new"))}
			}
			p, err := e.backend.prepareLog(changed, 1)
			if err != nil {
				t.Fatal(err)
			}
			// Keep both old indexes physically intact, replacing only a valid
			// decodable record image. This is not a malformed-row test.
			if err := deltaRun(t, ctx, e.backend.db, func(rc *recordlayer.FDBRecordContext) error {
				tx := rc.Transaction()
				space := logRecordSpace(e.backend.keys, e.cfg.Tenant, original.EventID)
				r, err := fdb.PrefixRange(space.Bytes())
				if err != nil {
					return err
				}
				tx.ClearRange(r)
				tx.Set(space.Pack(tuple.Tuple{int64(0)}), p.header)
				for i, chunk := range p.chunks {
					tx.Set(space.Pack(tuple.Tuple{int64(i + 1)}), chunk)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			got, err := e.QueryLogsByAttribute(ctx, a, 0)
			requireTyped[*CorruptionError](t, "stale equality posting", err)
			if len(got) != 0 {
				t.Fatalf("corruption returned logs: %v", got)
			}
			got, err = e.QueryLogsByTrace(ctx, original.TraceID, original.SpanID)
			if damage == "attribute" {
				if err != nil || len(got) != 1 {
					t.Fatalf("trace-only witness remains valid: %v / %v", got, err)
				}
			} else {
				requireTyped[*CorruptionError](t, "stale trace posting", err)
				if len(got) != 0 {
					t.Fatalf("corruption returned logs: %v", got)
				}
			}
		})
	}
}

func TestLogSmallIntegerPostingNativeCostRejectsWholeCall(t *testing.T) {
	t.Parallel()
	cluster := startExporterCluster(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	e := openTestExporter(t, ctx, cluster, nil)
	// Keep covering values small enough for the old formula to admit this
	// uncorrelated log while native mutation/conflict accounting exceeds 10 MB.
	large := LogRecord{EventID: "0123456789abcdef0123456789abcdef", Timestamp: 7, EffectiveTime: 7}
	for i := range 31000 {
		large.Attributes = append(large.Attributes, exKV(fmt.Sprintf("k%05d", i), &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: int64(i)}}))
	}
	attrs, unindexed := logIndexed(large)
	if len(attrs) != 31000 || unindexed != 0 {
		t.Fatalf("small integer fixture: indexed=%d unindexed=%d", len(attrs), unindexed)
	}
	rl, err := large.OTLP()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := proto.Marshal(rl)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) >= maxLogRawBytes {
		t.Fatalf("not a native-cost-only fixture: raw=%d", len(raw))
	}
	header, chunks, err := encodeLogValue(raw, 7, LogTimeEvent)
	if err != nil {
		t.Fatalf("fixture must fit stored-value admission: %v", err)
	}
	stored := len(header)
	for _, chunk := range chunks {
		stored += len(chunk)
	}
	trace, postings, _ := logIndexKeys(e.backend.keys, e.cfg.Tenant, e.cfg.HashBytes, large)
	// Complete previous admission formula versus a posting-only native Set +
	// point-conflict lower bound, independently of the replacement helpers.
	old := 1024 + 2*stored + 2*len(trace) + (len(chunks)+1)*2*(len(e.backend.keys.logs.Bytes())+len(large.EventID)+64)
	native := 0
	for _, p := range postings {
		old += 2 * (len(p.Key) + len(p.Value))
		native += 3*len(p.Key) + len(p.Value) + 69
	}
	if old >= maxLogTxnWriteBytes || native <= 10_000_000 {
		t.Fatalf("fixture must pass COMPLETE old8M admission but exceed native10M: old=%d postingNative=%d", old, native)
	}
	t.Logf("native-cost fixture: attributes=%d old=%d postingNative=%d", len(attrs), old, native)
	stats, err := e.ExportLogs(ctx, []LogRecord{{EventID: "valid-neighbor", Timestamp: 1}, large})
	requireTyped[*AdmissionError](t, "small integer native costs", err)
	if stats.Logs != 0 || stats.Batches != 0 {
		t.Fatalf("partially admitted request: %+v", stats)
	}
	for _, name := range []string{logRecordSubspace, logTraceSubspace, logEqSubspace} {
		if n := lgCensus(t, ctx, e, name); n != 0 {
			t.Fatalf("rejected request wrote %d %s keys", n, name)
		}
	}
	if stats, err := e.ExportLogs(ctx, []LogRecord{{EventID: "valid-neighbor", Timestamp: 1}}); err != nil || stats.Logs != 1 {
		t.Fatalf("neighbor positive control: %+v / %v", stats, err)
	}
}

func TestLogReplacementAdmissionCountsActualPriorPostings(t *testing.T) {
	t.Parallel()
	cluster := startExporterCluster(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	e := openTestExporter(t, ctx, cluster, nil)
	large := LogRecord{EventID: "replacement", Timestamp: 7, EffectiveTime: 7, TraceID: ftsTrace(1)}
	for i := range 20000 {
		large.Attributes = append(large.Attributes, exKV(fmt.Sprintf("k%05d", i), &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: int64(i)}}))
	}
	p, err := e.backend.prepareLog(large, 1)
	if err != nil {
		t.Fatalf("initial image must fit native admission: %v", err)
	}
	space := logRecordSpace(e.backend.keys, e.cfg.Tenant, large.EventID)
	replacementCost := p.writeCost + e.backend.logReplacementWriteCost(space, large)
	if replacementCost <= maxLogTxnWriteBytes {
		t.Fatalf("fixture does not exercise prior clears: new=%d replacement=%d", p.writeCost, replacementCost)
	}
	if stats, err := e.ExportLogs(ctx, []LogRecord{large}); err != nil || stats.Logs != 1 {
		t.Fatalf("initial image: %+v / %v", stats, err)
	}
	before := make(map[string]int64)
	for _, name := range []string{logRecordSubspace, logTraceSubspace, logEqSubspace} {
		before[name] = int64(lgCensus(t, ctx, e, name))
	}
	stats, err := e.ExportLogs(ctx, []LogRecord{{EventID: "valid-neighbor", Timestamp: 1}, large})
	requireTyped[*AdmissionError](t, "prior image native clears", err)
	if stats.Logs != 0 || stats.Batches != 0 {
		t.Fatalf("replacement admission partially wrote: %+v", stats)
	}
	for name, want := range before {
		if n := int64(lgCensus(t, ctx, e, name)); n != want {
			t.Fatalf("rejected replacement changed %s: %d want%d", name, n, want)
		}
	}
	// A small replacement still counts every OLD posting clear, not merely
	// clears estimated from its own new attributes or stored chunks.
	small, err := e.backend.prepareLog(LogRecord{EventID: large.EventID, Timestamp: 8}, 1)
	if err != nil {
		t.Fatal(err)
	}
	items := []preparedLog{small}
	if err := e.backend.admitLogReplacements(ctx, items); err != nil {
		t.Fatal(err)
	}
	if items[0].cost != small.writeCost+e.backend.logReplacementWriteCost(space, large) {
		t.Fatalf("small replacement ignored actual prior image: %+v", items[0])
	}
	if stats, err := e.ExportLogs(ctx, []LogRecord{{EventID: large.EventID, Timestamp: 8}}); err != nil || stats.Replaced != 1 {
		t.Fatalf("small replacement: %+v / %v", stats, err)
	}
	if n := lgCensus(t, ctx, e, logEqSubspace); n != 0 {
		t.Fatalf("small replacement left %d old postings", n)
	}
}

func TestLogPriorChangeAfterAdmissionIsOperationalWithCommittedNeighbor(t *testing.T) {
	t.Parallel()
	cluster := startExporterCluster(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	e := openTestExporter(t, ctx, cluster, nil)
	initial := LogRecord{EventID: "changing-prior", Timestamp: 1, TraceID: ftsTrace(1), Body: exStr("initial")}
	if _, err := e.ExportLogs(ctx, []LogRecord{initial}); err != nil {
		t.Fatal(err)
	}
	incoming := LogRecord{EventID: initial.EventID, Timestamp: 8, TraceID: initial.TraceID, Body: exStr("must not persist")}
	for i := range 20000 {
		incoming.Attributes = append(incoming.Attributes, exKV(fmt.Sprintf("k%05d", i), &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: int64(i)}}))
	}
	neighbor, err := e.backend.prepareLog(LogRecord{EventID: "committed-neighbor", Timestamp: 2, TraceID: ftsTrace(2)}, 1)
	if err != nil {
		t.Fatal(err)
	}
	next, err := e.backend.prepareLog(incoming, 1)
	if err != nil {
		t.Fatal(err)
	}
	items := []preparedLog{neighbor, next}
	if err := e.backend.admitLogReplacements(ctx, items); err != nil {
		t.Fatalf("whole-call preflight must succeed against small prior: %v", err)
	}
	batches := partitionLogs(items, e.cfg.BatchSize)
	if len(batches) != 2 {
		t.Fatalf("fixture needs independent neighbor/large batches, got%d", len(batches))
	}
	if _, err := e.backend.saveLogBatch(ctx, batches[0]); err != nil {
		t.Fatal(err)
	}
	// Deterministic interleaving: another exporter grows the prior image
	// after whole-call admission and after the independent neighbor commit.
	other, err := NewExporter(ctx, e.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Shutdown(ctx)
	grown := incoming
	grown.Timestamp, grown.Body = 7, exStr("concurrent prior")
	if _, err := other.ExportLogs(ctx, []LogRecord{grown}); err != nil {
		t.Fatalf("concurrent prior must fit alone: %v", err)
	}
	_, err = e.backend.saveLogBatch(ctx, batches[1])
	requireTyped[*OperationError](t, "live prior changed after admission", err)
	var admission *AdmissionError
	if errors.As(err, &admission) {
		t.Fatalf("runtime prior change mislabeled whole-call admission: %v", err)
	}
	got, err := e.QueryLogsByTrace(ctx, ftsTrace(2), "")
	if err != nil || len(got) != 1 || got[0].EventID != neighbor.eventID {
		t.Fatalf("committed neighbor rolled back: %+v / %v", got, err)
	}
	got, err = e.QueryLogsByTrace(ctx, initial.TraceID, "")
	if err != nil || len(got) != 1 || got[0].Timestamp != 7 || got[0].Body.GetStringValue() != "concurrent prior" || len(got[0].Attributes) != 20000 {
		t.Fatalf("failed live batch mutated concurrent prior: logs=%d err=%v", len(got), err)
	}
}

func TestLogNativeMutationCostAccounting(t *testing.T) {
	t.Parallel()
	if got := logSetWriteCost([]byte("abc"), []byte("value")); got != 83 {
		t.Fatalf("Set cost=%d want83", got)
	}
	if got := logClearWriteCost([]byte("abc")); got != 82 {
		t.Fatalf("Clear+conflict cost=%d want82", got)
	}
}

func TestLogExpandedWriteAdmissionRejectsWholeCall(t *testing.T) {
	t.Parallel()
	cluster := startExporterCluster(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	e := openTestExporter(t, ctx, cluster, nil)
	large := LogRecord{EventID: "expanded", Timestamp: 7, TraceID: ftsTrace(1)}
	// Independently generated octal strings compress under the stored-value
	// cap while their covering values plus stored payload exceed one FDB transaction.
	state := uint64(0x91ad4708b3)
	for i := range 120 {
		value := make([]byte, 64000)
		for j := range value {
			state ^= state << 13
			state ^= state >> 7
			state ^= state << 17
			value[j] = '0' + byte(state&7)
		}
		large.Attributes = append(large.Attributes, exKV(fmt.Sprintf("k%03d", i), exStr(string(value))))
	}
	attrs, unindexed := logIndexed(large)
	if len(attrs) != 120 || unindexed != 0 {
		t.Fatalf("fixture not indexed: %d / %d", len(attrs), unindexed)
	}
	rl, err := large.OTLP()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(rl)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > maxLogRawBytes {
		t.Fatalf("raw fixture exceeds old admission: %d", len(raw))
	}
	header, chunks, err := encodeLogValue(raw, 7, LogTimeEvent)
	if err != nil {
		t.Fatalf("fixture must fit stored-value admission: %v", err)
	}
	stored := len(header)
	for _, chunk := range chunks {
		stored += len(chunk)
	}
	if stored > maxLogStoredBytes {
		t.Fatalf("stored fixture exceeds old admission: %d", stored)
	}
	_, err = e.backend.prepareLog(large, 1)
	requireTyped[*AdmissionError](t, "expanded singleton", err)
	if err == nil || !strings.Contains(err.Error(), "expanded transaction writes") {
		t.Fatalf("wrong admission boundary: %v", err)
	}
	stats, err := e.ExportLogs(ctx, []LogRecord{{EventID: "valid-neighbor", Timestamp: 1}, large})
	requireTyped[*AdmissionError](t, "whole-call admission", err)
	if stats.Logs != 0 || stats.Batches != 0 {
		t.Fatalf("rejected call committed: %+v", stats)
	}
	for _, name := range []string{logRecordSubspace, logTraceSubspace, logEqSubspace} {
		if n := lgCensus(t, ctx, e, name); n != 0 {
			t.Fatalf("rejected call wrote %d %s rows", n, name)
		}
	}
	if stats, err := e.ExportLogs(ctx, []LogRecord{{EventID: "valid-neighbor", Timestamp: 1}}); err != nil || stats.Logs != 1 {
		t.Fatalf("valid neighbor positive control: %+v / %v", stats, err)
	}
}
