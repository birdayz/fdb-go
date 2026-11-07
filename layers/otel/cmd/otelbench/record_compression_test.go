package main

import (
	"bytes"
	"context"
	"testing"

	"fdb.dev/layers/otel"
	"fdb.dev/layers/otel/sqlbackend"
)

func TestExecuteRecordCompression(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"none", "zlib-1", "zlib-default"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			backend := &fakeBackend{t: t}
			var stdout, stderr bytes.Buffer
			if code := execute(append(cliArgs("ingest"), "--record-compression="+mode), &stdout, &stderr, backend.open); code != 0 {
				t.Fatalf("mode %s exit %d: %s", mode, code, &stderr)
			}
			e := decodeEvidence(t, stdout.Bytes())
			if backend.cfg.RecordCompression != mode || e.Config.RecordCompression != mode {
				t.Fatalf("compression choice omitted from actual config/receipt: %+v", e)
			}
		})
	}
	// The sql backend's batch saves are the Exporter's SpanWriter, whose
	// default coalesces index write conflicts.
	sql := &fakeBackend{t: t}
	var out, errOut bytes.Buffer
	if code := execute(append(cliArgs("ingest"), "--save-mode=batch", "--record-compression=zlib-1", "--bulk-index-conflicts"), &out, &errOut, sql.open); code != 0 {
		t.Fatalf("sql bulk index conflicts exit %d: %s", code, &errOut)
	}
	if !sql.cfg.BulkIndexConflicts || sql.cfg.Backend != "sql" || !decodeEvidence(t, out.Bytes()).Config.BulkIndexConflicts {
		t.Fatalf("sql bulk index conflicts omitted from actual config/receipt: %+v", sql.cfg)
	}
	for _, args := range [][]string{
		{"--bulk-index-conflicts", "--save-mode=pipelined"},
		{"--bulk-index-conflicts"},
		{"--bulk-index-conflicts", "--save-mode=serial"},
		{"--bulk-index-conflicts", "--save-mode=batch", "--backend=ch-attrs"},
		{"--record-compression=brotli"},
		{"--record-compression=brotli-11"},
		{"--record-compression=zlib-9"},
		// D2 refused Brotli on OTEL_V1 stores; Phase 3B removed the fdb backend.
		{"--record-compression=brotli-1"},
		{"--record-compression=brotli-4"},
		{"--record-compression=zlib-1", "--backend=ch-map"},
		{"--record-compression=brotli-1", "--backend=ch-attrs"},
	} {
		var stdout, stderr bytes.Buffer
		opened := false
		code := execute(append(cliArgs("ingest"), args...), &stdout, &stderr, func(_ context.Context, _ otel.Config, _, _ string, _ sqlbackend.Options) (otel.Backend, error) {
			opened = true
			return nil, nil
		})
		if code != 2 || opened || stdout.Len() != 0 {
			t.Fatalf("invalid compression accepted: args=%v code=%d opened=%t stdout=%s", args, code, opened, &stdout)
		}
	}
}
