package main

import (
	"bytes"
	"context"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
)

// The quickstart is linked from README.md: it must actually run, and run twice
// (re-runnable). It rotted once while only compiled: a pre-/FRL database path
// and a NOT NULL primary key, both rejected, with setup errors logged and
// skipped.
func TestQuickstartRunsTwice(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	container, err := foundationdbtc.Run(ctx, "",
		foundationdbtc.WithAPIVersion(730),
		foundationdbtc.WithStorageEngine("memory"),
	)
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

	const want = "order 2: bob spent 250\n" +
		"totals by customer:\n" +
		"  alice  orders=2 total=175\n" +
		"  bob    orders=1 total=250\n"
	for i := range 2 {
		var out bytes.Buffer
		if err := run(ctx, clusterFile, &out); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
		if out.String() != want {
			t.Fatalf("run %d output:\n%s\nwant:\n%s", i+1, out.String(), want)
		}
	}
}

func TestQuickstartDSN(t *testing.T) {
	t.Parallel()
	clusterFile := filepath.Join(t.TempDir(), "cluster & #+.file")
	u, err := url.Parse(dsn("/FRL/QUICKSTART", clusterFile, "app"))
	if err != nil {
		t.Fatal(err)
	}
	if u.Path != "/FRL/QUICKSTART" || u.Query().Get("cluster_file") != clusterFile || u.Query().Get("schema") != "APP" {
		t.Fatalf("DSN lost case or escaped path: %s", u)
	}
}

func TestQuickstartStopsOnSetupError(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := run(context.Background(), filepath.Join(t.TempDir(), "missing.cluster"), &out)
	if err == nil || !strings.Contains(err.Error(), "setup") {
		t.Fatalf("run error = %v; want setup error", err)
	}
	if out.Len() != 0 {
		t.Fatalf("failed setup printed success: %s", out.String())
	}
}
