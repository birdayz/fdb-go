package cmd

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
)

func cliExampleRunfile(t *testing.T, name string) string {
	t.Helper()
	root := filepath.Join("..", "..", "..", "..")
	if srcDir := os.Getenv("TEST_SRCDIR"); srcDir != "" {
		workspace := os.Getenv("TEST_WORKSPACE")
		if workspace == "" {
			workspace = "_main"
		}
		root = filepath.Join(srcDir, workspace)
	}
	path, err := filepath.Abs(filepath.Join(root, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("CLI example input not staged: %s: %v", name, err)
	}
	return path
}

func TestWebsiteExampleArchiveMatchesLicensedSources(t *testing.T) {
	t.Parallel()
	const public = "website/static/examples/record-layer/"
	canonical := map[string]string{
		"LICENSE":                       "LICENSE",
		"NOTICE":                        "NOTICE",
		"buf.yaml":                      public + "buf.yaml",
		"records.proto":                 public + "records.proto",
		"record_key_expression.proto":   "proto/apple/record_key_expression.proto",
		"record_metadata.proto":         "proto/apple/record_metadata.proto",
		"record_metadata_options.proto": "proto/apple/record_metadata_options.proto",
		"record_query_plan.proto":       "proto/apple/record_query_plan.proto",
	}
	raw, err := os.ReadFile(cliExampleRunfile(t, public+"record-layer.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	zipped, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer zipped.Close()
	archive := tar.NewReader(zipped)
	seen := make(map[string]bool)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		source, ok := canonical[header.Name]
		if !ok || seen[header.Name] || header.Typeflag != tar.TypeReg || header.Mode != 0o644 {
			t.Fatalf("unexpected, duplicate, or unsafe archive member: %+v", header)
		}
		seen[header.Name] = true
		got, err := io.ReadAll(archive)
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(cliExampleRunfile(t, source))
		if err != nil {
			t.Fatal(err)
		}
		publicBytes, err := os.ReadFile(cliExampleRunfile(t, public+header.Name))
		if err != nil {
			t.Fatal(err)
		}
		if len(want) == 0 || !bytes.Equal(got, want) || !bytes.Equal(publicBytes, want) {
			t.Errorf("%s differs from its canonical source %s (or is empty); run just generate-cli-example", header.Name, source)
		}
	}
	if len(seen) != 8 || len(seen) != len(canonical) {
		t.Fatalf("archive has %d source/license members, want exactly 8", len(seen))
	}
	// Consume the gzip trailer too, so a truncated archive cannot pass after
	// tar's end-of-archive marker without checksum validation.
	if _, err := io.Copy(io.Discard, zipped); err != nil {
		t.Fatal(err)
	}
}

func TestStoreCreateRejectsSQLAndInvalidTargetsBeforeDatabaseAccess(t *testing.T) {
	t.Setenv("FDB_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--database", "/FRL/example", "--schema", "main"}, "SQL DDL"},
		{[]string{"--keyspace-tuple", "[null,null,0]"}, "unsupported type"},
		{[]string{"--keyspace-tuple", "[null,null,0,\"child\"]"}, "unsupported type"},
		{[]string{"--keyspace-tuple", "[null,null]"}, "unsupported type"},
		{[]string{"--keyspace-tuple", "[]"}, "at least one element"},
	} {
		args := append([]string{"rl", "store", "create", "--yes", "--cluster-file", "/does/not/exist.cluster"}, tc.args...)
		out, err := runCmd(t, args...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: error=%v output=%s; want %q before FDB access", tc.args, err, out, tc.want)
		}
	}
}

func TestIntegration_WebsiteProtobufRecordsAndIndexMaintenance(t *testing.T) {
	f := requireFixture(t)
	path := cliExampleRunfile(t, "website/static/examples/record-layer/schema.binpb")
	prefix := "website-example-" + filepath.Base(t.TempDir())
	ss := subspace.Sub(prefix, "contacts")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	config := fmt.Sprintf(`current_context: contacts
contexts:
- name: contacts
  cluster_file: %q
  keyspace_path: /%s/contacts
  metadata:
    descriptor_set:
      path: %q
      records_file: records.proto
`, f.clusterFilePath, prefix, path)
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FDB_CONFIG", configPath)
	db, err := fdb.OpenDatabase(f.clusterFilePath)
	if err != nil {
		t.Fatal(err)
	}
	rec := recordlayer.NewFDBDatabase(db)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := rec.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			rtx.Transaction().ClearRange(ss)
			return nil, nil
		}); err != nil {
			t.Errorf("clear website example: %v", err)
		}
	})
	run := func(args ...string) string {
		t.Helper()
		out, err := runCmd(t, args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return out
	}
	out := run("rl", "store", "create", "--yes")
	if !strings.Contains(out, "metadata version 4") {
		t.Fatalf("website annotations did not produce version 4: %s", out)
	}
	assertReadable := func() {
		t.Helper()
		out := run("rl", "index", "ls", "-o", "json")
		var indexes []indexListRow
		if err := json.Unmarshal([]byte(out), &indexes); err != nil {
			t.Fatal(err)
		}
		if len(indexes) != 4 {
			t.Fatalf("website example must declare four indexes: %s", out)
		}
		for i, want := range []struct{ name, typ string }{
			{"Contact$city", "value"}, {"Contact$email", "value"}, {"Contact$score", "rank"}, {"Contact$tags", "value"},
		} {
			if indexes[i].Name != want.name || indexes[i].Type != want.typ || indexes[i].State != "READABLE" {
				t.Errorf("index %d = %+v, want %+v READABLE", i, indexes[i], want)
			}
		}
	}
	assertReadable()
	run("rl", "record", "put", "--type", "Contact", `{"id":1,"name":"Ada","email":"ada@example.com","city":"London","score":100,"tags":["math","friends"]}`, "--yes")
	run("rl", "record", "put", "--type", "Contact", `{"id":2,"name":"Grace","email":"grace@example.com","city":"New York","score":200,"tags":["computing","friends"]}`, "--yes")
	if got := run("rl", "record", "get", "1"); !strings.Contains(got, `"id":"1"`) || !strings.Contains(got, "ada@example.com") {
		t.Fatalf("native primary-key get: %s", got)
	}
	assertEntries := func(index string, want []string) {
		t.Helper()
		out := run("rl", "index", "scan", index)
		decoder := json.NewDecoder(strings.NewReader(out))
		var got []string
		for {
			var row struct {
				Index      string `json:"index"`
				Values     string `json:"index_values"`
				PrimaryKey string `json:"primary_key"`
			}
			err := decoder.Decode(&row)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil || row.Index != index {
				t.Fatalf("index row %+v, decode error %v: %s", row, err, out)
			}
			got = append(got, row.Values+"/"+row.PrimaryKey)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s entries = %v, want %v", index, got, want)
		}
	}
	assertEntries("Contact$email", []string{"ada@example.com/1", "grace@example.com/2"})
	assertEntries("Contact$city", []string{"London/1", "New York/2"})
	assertEntries("Contact$score", []string{"100/1", "200/2"})
	assertEntries("Contact$tags", []string{"computing/2", "friends/1", "friends/2", "math/1"})
	before := snapshotStoreBytes(t, f.clusterFilePath, ss)
	out, err = runCmd(t, "rl", "record", "put", "--type", "Contact", `{"id":3,"email":"ada@example.com"}`, "--yes")
	var violation *recordlayer.RecordIndexUniquenessViolationError
	if !errors.As(err, &violation) || violation.IndexName != "Contact$email" {
		t.Fatalf("unique website annotation was not enforced: %v\n%s", err, out)
	}
	if after := snapshotStoreBytes(t, f.clusterFilePath, ss); after != before {
		t.Fatal("failed unique write changed records or indexes")
	}
	run("rl", "record", "put", "--type", "Contact", `{"id":1,"name":"Ada","email":"ada@example.com","city":"Paris","score":110,"tags":["math"]}`, "--yes")
	assertEntries("Contact$city", []string{"New York/2", "Paris/1"})
	assertEntries("Contact$score", []string{"110/1", "200/2"})
	assertEntries("Contact$tags", []string{"computing/2", "friends/2", "math/1"})
	run("rl", "index", "set-state", "Contact$city", "disabled", "--yes")
	if out, err := runCmd(t, "rl", "index", "scan", "Contact$city"); err == nil || !strings.Contains(err.Error(), "DISABLED") {
		t.Fatalf("disabled index must not scan: %v\n%s", err, out)
	}
	run("rl", "index", "build", "Contact$city", "--yes")
	run("rl", "index", "rebuild", "Contact$tags", "--yes")
	assertReadable()
	assertEntries("Contact$city", []string{"New York/2", "Paris/1"})
	assertEntries("Contact$tags", []string{"computing/2", "friends/2", "math/1"})
	run("rl", "record", "delete", "2", "--dry-run")
	run("rl", "record", "get", "2")
	run("rl", "record", "delete", "2", "--yes")
	assertEntries("Contact$email", []string{"ada@example.com/1"})
	assertEntries("Contact$tags", []string{"math/1"})
}
