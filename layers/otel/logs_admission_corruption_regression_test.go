package otel

import (
	"context"
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
