package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
)

func TestRecordQuickstartRunsTwice(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	container, err := foundationdbtc.Run(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := container.Terminate(ctx); err != nil {
			t.Error(err)
		}
	})
	clusterFile, err := container.ClusterFilePath(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		var out bytes.Buffer
		if err := run(ctx, clusterFile, &out); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
		if got, want := out.String(), "order 1001: price=25\n"; got != want {
			t.Fatalf("run %d output = %q; want %q", i+1, got, want)
		}
	}
}

func TestRecordQuickstartReturnsOpenError(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := run(context.Background(), filepath.Join(t.TempDir(), "missing.cluster"), &out)
	if err == nil || !strings.Contains(err.Error(), "open database") {
		t.Fatalf("run error = %v; want database open error", err)
	}
	if out.Len() != 0 {
		t.Fatalf("failed open printed success: %s", out.String())
	}
}
