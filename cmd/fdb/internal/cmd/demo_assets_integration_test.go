package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func demoAssetPath(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("..", "..", "demo", name)
	if srcDir := os.Getenv("TEST_SRCDIR"); srcDir != "" {
		workspace := os.Getenv("TEST_WORKSPACE")
		if workspace == "" {
			workspace = "_main"
		}
		path = filepath.Join(srcDir, workspace, "cmd", "fdb", "demo", name)
	}
	path, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("shipped demo asset %s is not staged (//cmd/fdb:demo_files): %v", path, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		t.Fatalf("shipped demo asset %s is empty", path)
	}
	return path
}

func TestIntegration_DemoAssets_BootstrapSeedAndDiscoverPrimaryKey(t *testing.T) {
	schemaPath := demoAssetPath(t, "schema.sql")
	seedPath := demoAssetPath(t, "seed.sql")
	bindConfig(t) // t.Setenv, not parallel: do not replace any sibling's config.

	// Run the shipped files byte-for-byte; reserve their database/template
	// to this test so the disposable shared fixture stays isolated.
	const database = "/FRL/demo"
	run := func(args ...string) (string, string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		root := NewRoot()
		var out, errOut bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&errOut)
		root.SetIn(strings.NewReader(""))
		root.SetArgs(append(args, "--cluster-file", fixture.clusterFilePath))
		err := root.ExecuteContext(ctx)
		return out.String(), errOut.String(), err
	}
	if out, errOut, err := run("sql", "--database", database, "-f", schemaPath); err != nil {
		t.Fatalf("shipped schema.sql: %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	t.Cleanup(func() {
		for _, stmt := range []string{"DROP DATABASE IF EXISTS " + database, "DROP SCHEMA TEMPLATE IF EXISTS orders_tpl"} {
			if out, errOut, err := run("sql", "--database", database, "-c", stmt); err != nil {
				t.Errorf("clean up demo %q: %v\nstdout:\n%s\nstderr:\n%s", stmt, err, out, errOut)
			}
		}
	})

	for pass := 1; pass <= 2; pass++ {
		if out, errOut, err := run("sql", "--database", database, "--schema", "main", "-f", seedPath); err != nil {
			t.Fatalf("shipped seed.sql pass %d: %v\nstdout:\n%s\nstderr:\n%s", pass, err, out, errOut)
		}
		out, errOut, err := run("sql", "--database", database, "--schema", "main", "-o", "json", "-c", "SELECT count(*) AS n FROM orders")
		if err != nil {
			t.Fatalf("count after seed pass %d: %v\nstdout:\n%s\nstderr:\n%s", pass, err, out, errOut)
		}
		var rows []struct {
			Count int64 `json:"N"`
		}
		if err := json.Unmarshal([]byte(out), &rows); err != nil || len(rows) != 1 || rows[0].Count != 1000 {
			t.Fatalf("count after seed pass %d = %+v, decode error=%v; want exactly 1000\nstdout:\n%s", pass, rows, err, out)
		}
	}

	type envelope struct {
		PrimaryKey string         `json:"primary_key"`
		RecordType string         `json:"record_type"`
		Record     map[string]any `json:"record"`
	}
	out, errOut, err := run("rl", "record", "scan", "--database", database, "--schema", "main", "--limit", "1")
	if err != nil {
		t.Fatalf("discover demo primary key: %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	var scanned envelope
	if err := json.Unmarshal([]byte(out), &scanned); err != nil || scanned.PrimaryKey == "" || scanned.RecordType != "ORDERS" ||
		scanned.Record["ORDER_ID"] != "1" || scanned.Record["CUSTOMER"] != "carol" || scanned.Record["PRICE"] != 198.57 {
		t.Fatalf("first seeded record = %+v, decode error=%v\nstdout:\n%s", scanned, err, out)
	}
	// Copy the primary_key envelope field exactly. In particular, do not guess
	// the Record Layer type-key prefix from the SQL primary-key column.
	out, errOut, err = run("rl", "record", "get", scanned.PrimaryKey, "--database", database, "--schema", "main")
	if err != nil {
		t.Fatalf("get discovered demo key %q: %v\nstdout:\n%s\nstderr:\n%s", scanned.PrimaryKey, err, out, errOut)
	}
	var fetched envelope
	if err := json.Unmarshal([]byte(out), &fetched); err != nil || !reflect.DeepEqual(fetched, scanned) {
		t.Fatalf("discovered-key get = %+v, want %+v; decode error=%v\nstdout:\n%s", fetched, scanned, err, out)
	}
}
