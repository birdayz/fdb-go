package main

import (
	"bytes"
	"context"
	"testing"

	"fdb.dev/layers/otel"
)

func TestExecuteRecordCompression(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"none", "zlib-1", "brotli-0", "brotli-1", "brotli-3", "brotli-4"} {
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
	for _, values := range []string{"json", "tuple"} {
		backend := &fakeBackend{t: t}
		var stdout, stderr bytes.Buffer
		args := append(cliArgs("ingest"), "--save-mode=batch", "--record-compression=brotli-1", "--posting-value-format="+values, "--bulk-index-conflicts")
		if code := execute(args, &stdout, &stderr, backend.open); code != 0 {
			t.Fatalf("bulk/covering flags exit %d: %s", code, &stderr)
		}
		e := decodeEvidence(t, stdout.Bytes())
		if !backend.cfg.BulkIndexConflicts || backend.cfg.PostingValueFormat != values || !e.Config.BulkIndexConflicts || e.Config.PostingValueFormat != values {
			t.Fatalf("bulk/covering choices omitted from actual config/receipt: %+v", e)
		}
	}
	for _, args := range [][]string{
		{"--posting-value-format=unknown"},
		{"--posting-value-format=tuple", "--backend=ch-map"},
		{"--bulk-index-conflicts"},
		{"--bulk-index-conflicts", "--save-mode=serial"},
		{"--bulk-index-conflicts", "--save-mode=batch", "--backend=ch-attrs"},
		{"--record-compression=brotli"},
		{"--record-compression=brotli-11"},
		{"--record-compression=zlib-9"},
		{"--record-compression=brotli-1", "--backend=ch-map"},
		{"--record-compression=brotli-1", "--backend=ch-attrs"},
	} {
		var stdout, stderr bytes.Buffer
		opened := false
		code := execute(append(cliArgs("ingest"), args...), &stdout, &stderr, func(_ context.Context, _ otel.Config, _, _ string) (otel.Backend, error) {
			opened = true
			return nil, nil
		})
		if code != 2 || opened || stdout.Len() != 0 {
			t.Fatalf("invalid compression accepted: args=%v code=%d opened=%t stdout=%s", args, code, opened, &stdout)
		}
	}
}
