package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"

	"fdb.dev/gen"
)

func descriptorFixturePath(t *testing.T) string {
	t.Helper()
	return namedDescriptorFixturePath(t, "records.binpb")
}

func namedDescriptorFixturePath(t *testing.T, name string) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("../meta/testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("compiled annotation fixture is not staged: %v", err)
	}
	return path
}

func TestDescriptorSetCLIIndexesAndMetadata(t *testing.T) {
	t.Setenv("FDB_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	path := descriptorFixturePath(t)
	out, err := runCmd(t, "rl", "index", "ls", "--descriptor-set", path, "--no-fdb", "-o", "json")
	if err != nil {
		t.Fatalf("offline indexes: %v\n%s", err, out)
	}
	var indexes []indexListRow
	if err := json.Unmarshal([]byte(out), &indexes); err != nil {
		t.Fatal(err)
	}
	if len(indexes) != 3 {
		t.Fatalf("indexes = %v, want three proto-declared indexes", indexes)
	}
	for i, want := range []struct{ name, typ string }{
		{"Customer$email", "value"}, {"Customer$score", "rank"}, {"Customer$tags", "value"},
	} {
		got := indexes[i]
		if got.Name != want.name || got.Type != want.typ || got.State != "—" || !slices.Equal(got.RecordTypes, []string{"Customer"}) {
			t.Errorf("index[%d] = %+v, want %s/%s for Customer with no FDB state", i, got, want.name, want.typ)
		}
	}
	out, err = runCmd(t, "rl", "index", "describe", "Customer$email", "--descriptor-set", path, "-o", "json")
	if err != nil {
		t.Fatalf("index describe: %v\n%s", err, out)
	}
	var definition struct {
		Options map[string]string `json:"options"`
	}
	if err := json.Unmarshal([]byte(out), &definition); err != nil {
		t.Fatal(err)
	}
	if definition.Options["unique"] != "true" {
		t.Errorf("unique annotation missing from index definition: %s", out)
	}
	out, err = runCmd(t, "rl", "meta", "get", "--descriptor-set", path, "--records-file", "records.proto", "--meta-version", "12")
	if err != nil {
		t.Fatalf("metadata from annotations: %v\n%s", err, out)
	}
	var metadata gen.MetaData
	if err := protojson.Unmarshal([]byte(out), &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.GetVersion() != 12 || len(metadata.GetIndexes()) != 3 || metadata.GetRecords().GetName() != "records.proto" {
		t.Errorf("metadata missing version, indexes, or records descriptor: %s", out)
	}
	_, err = runCmd(t, "rl", "meta", "get", "--descriptor-set", path, "--records-file", "missing.proto")
	// Fang capitalizes the first word of errors; keep the source path out of it.
	if err == nil || !strings.HasPrefix(err.Error(), "load descriptor-set:"+path) {
		t.Fatalf("descriptor error must preserve path casing in the terminal renderer: %v", err)
	}
}

func TestDescriptorSetCLIRejectsConflictingSources(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--descriptor-set", "schema.binpb", "--meta-file", "metadata.pb"}, "conflicting metadata sources"},
		{[]string{"--descriptor-set", "schema.binpb", "--database", "/FRL/db", "--schema", "main"}, "conflicting metadata sources"},
		{[]string{"--records-file", "records.proto"}, "require --descriptor-set"},
		{[]string{"--meta-version", "0"}, "require --descriptor-set"},
		{[]string{"--descriptor-set", "schema.binpb", "--meta-version", "-1"}, "non-negative int32"},
		{[]string{"--descriptor-set", "schema.binpb", "--meta-version", "2147483648"}, "non-negative int32"},
		{[]string{"--descriptor-set", "schema.binpb", "--meta-version", "nope"}, "non-negative int32"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			t.Parallel()
			args := append([]string{"rl", "index", "ls", "--no-fdb"}, tc.args...)
			out, err := runCmd(t, args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%v: error=%v output=%s; want %q", args, err, out, tc.want)
			}
		})
	}
}

func TestDescriptorSetCLIContextAndSourceOverrides(t *testing.T) {
	path := descriptorFixturePath(t)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("FDB_CONFIG", configPath)
	writeConfig := func(metadata string) {
		t.Helper()
		raw := fmt.Sprintf("current_context: local\ncontexts:\n  - name: local\n    cluster_file: /nonexistent/cluster.file\n    keyspace_path: /app/records\n    metadata:\n%s", metadata)
		if err := os.WriteFile(configPath, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig(fmt.Sprintf("      descriptor_set:\n        path: %q\n        records_file: records.proto\n        metadata_version: 17\n", path))
	out, err := runCmd(t, "rl", "meta", "get")
	if err != nil {
		t.Fatalf("context descriptor source: %v\n%s", err, out)
	}
	var metadata gen.MetaData
	if err := protojson.Unmarshal([]byte(out), &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.GetVersion() != 17 || len(metadata.GetIndexes()) != 3 {
		t.Fatalf("context descriptor settings not applied: %s", out)
	}
	out, err = runCmd(t, "rl", "index", "ls", "--no-fdb")
	if err != nil || !strings.Contains(out, "Customer$email") {
		t.Fatalf("offline context source: %v\n%s", err, out)
	}
	metaPath := writeDemoMetaFile(t, 0)
	out, err = runCmd(t, "rl", "meta", "types", "ls", "--meta-file", metaPath)
	if err != nil || !strings.Contains(out, "Order") {
		t.Fatalf("meta-file override of descriptor context: %v\n%s", err, out)
	}
	writeConfig("      meta_store_keyspace: /app/_meta\n")
	out, err = runCmd(t, "rl", "meta", "get", "--descriptor-set", path)
	if err != nil || !strings.Contains(out, "Customer$email") {
		t.Fatalf("descriptor override must not dial the context's unavailable FDB metadata store: %v\n%s", err, out)
	}
	if err := os.WriteFile(configPath, []byte("current_context: sql\ncontexts:\n  - name: sql\n    database: /FRL/db\n    schema: main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = runCmd(t, "rl", "meta", "get", "--descriptor-set", path)
	if err == nil || !strings.Contains(err.Error(), "--descriptor-set cannot be combined with relational context") {
		t.Fatalf("descriptor source must not replace a SQL catalog: %v", err)
	}
}

func TestDescriptorSetCLICompletions(t *testing.T) {
	t.Setenv("FDB_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	_, assertNoFDB := completionFDBTrap(t)
	path := descriptorFixturePath(t)
	multiple := namedDescriptorFixturePath(t, "multiple.binpb")
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"record-types", []string{"rl", "record", "scan", "--descriptor-set", path, "--type", ""}, []string{"Customer"}},
		{"ambiguous-without-selector", []string{"rl", "record", "scan", "--descriptor-set", multiple, "--type", ""}, nil},
		{"select-annotated-records", []string{"rl", "index", "describe", "--descriptor-set", multiple, "--records-file", "records.proto", "--meta-version", "12", ""}, []string{"Customer$email", "Customer$score", "Customer$tags"}},
		{"select-other-records", []string{"rl", "record", "scan", "--descriptor-set", multiple, "--records-file", "other_records.proto", "--type", ""}, []string{"OtherRecord"}},
		{"unknown-records-file", []string{"rl", "record", "scan", "--descriptor-set", path, "--records-file", "missing.proto", "--type", ""}, nil},
		{"version-before-indexes", []string{"rl", "index", "describe", "--descriptor-set", path, "--meta-version", "2", ""}, nil},
		{"explicit-zero", []string{"rl", "record", "scan", "--descriptor-set", path, "--meta-version", "0", "--type", ""}, nil},
		{"positional-record-types", []string{"rl", "meta", "types", "describe", "--descriptor-set", path, ""}, []string{"Customer"}},
		{"missing-descriptor-set", []string{"rl", "record", "scan", "--descriptor-set", "/missing/schema.binpb", "--type", ""}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertDescriptorCompletion(t, tc.args, tc.want)
			assertNoFDB()
		})
	}
}

func TestDescriptorSetCLICompletionAddressing(t *testing.T) {
	clusterFile, assertNoFDB := completionFDBTrap(t)
	path := descriptorFixturePath(t)
	metaFile := writeDemoMetaFile(t, 0)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("FDB_CONFIG", configPath)
	for _, tc := range []struct {
		name    string
		context string
		flags   []string
		want    []string
	}{
		{name: "native-context", want: []string{"Customer"}},
		{name: "relational-override", flags: []string{"--database", "/FRL/app", "--schema", "main"}},
		{name: "partial-database", flags: []string{"--database", "/FRL/app"}},
		{name: "partial-schema", flags: []string{"--schema", "main"}},
		{name: "relational-context", context: "relational"},
		{name: "tuple-overrides-relational-context", context: "relational", flags: []string{"--keyspace-tuple", `["native"]`}, want: []string{"Customer"}},
		{name: "invalid-tuple", flags: []string{"--keyspace-tuple", "not-json"}},
		{name: "conflicting-tuple", flags: []string{"--keyspace-tuple", `["native"]`, "--database", "/FRL/app", "--schema", "main"}},
		{name: "conflicting-sources", flags: []string{"--descriptor-set", path, "--meta-file", metaFile}},
		{name: "descriptor-with-relational-flags", flags: []string{"--descriptor-set", path, "--database", "/FRL/app", "--schema", "main"}},
		{name: "descriptor-with-relational-context", context: "relational", flags: []string{"--descriptor-set", path}},
		{name: "context-does-not-authorize-records-selector", flags: []string{"--records-file", "records.proto"}},
		{name: "fdb-metadata-source", context: "fdb-store"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			address := "    keyspace_path: /native\n"
			metadata := fmt.Sprintf("    metadata:\n      descriptor_set:\n        path: %q\n", path)
			if tc.context == "relational" {
				address = "    database: /FRL/app\n    schema: main\n"
			}
			if tc.context == "fdb-store" {
				metadata = "    metadata:\n      meta_store_keyspace: /app/_meta\n"
			}
			raw := fmt.Sprintf("current_context: local\ncontexts:\n  - name: local\n    cluster_file: %q\n%s%s", clusterFile, address, metadata)
			if err := os.WriteFile(configPath, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			args := append([]string{"rl", "record", "scan"}, tc.flags...)
			args = append(args, "--type", "")
			assertDescriptorCompletion(t, args, tc.want)
			assertNoFDB()
		})
	}
}

func assertDescriptorCompletion(t *testing.T, args, want []string) {
	t.Helper()
	root := NewRoot()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	root.SetContext(ctx)
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs(append([]string{"__complete"}, args...))
	if err := root.Execute(); err != nil {
		t.Fatalf("completion %v: %v\nstdout: %s\nstderr: %s", args, err, stdout.String(), stderr.String())
	}
	lines := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
	if directive := lines[len(lines)-1]; directive != fmt.Sprintf(":%d", cobra.ShellCompDirectiveNoFileComp) {
		t.Fatalf("completion directive = %q; want NoFileComp", directive)
	}
	got := lines[:len(lines)-1]
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("completion %v = %v; want %v", args, got, want)
	}
	if got := stderr.String(); got != "Completion ended with directive: ShellCompDirectiveNoFileComp\n" {
		t.Errorf("completion emitted diagnostics: %q", got)
	}
}

func completionFDBTrap(t *testing.T) (string, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var connections atomic.Int32
	accepted := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			_ = conn.Close()
			select {
			case accepted <- struct{}{}:
			default:
			}
		}
	}()
	assertNoFDB := func() {
		t.Helper()
		if got := connections.Load(); got != 1 {
			t.Errorf("completion dialed FDB: %d connections beyond the trap's positive control", got-1)
		}
	}
	t.Cleanup(func() {
		_ = listener.Close()
		<-done
		assertNoFDB()
	})
	probe, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = probe.Close()
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("FDB trap did not observe its positive-control connection")
	}
	path := filepath.Join(t.TempDir(), "completion.cluster")
	if err := os.WriteFile(path, []byte("completion:completion@"+listener.Addr().String()), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FDB_CLUSTER_FILE", path)
	return path, assertNoFDB
}

func TestMetadataVersionValue(t *testing.T) {
	t.Parallel()
	var value metadataVersionValue
	var absent *metadataVersionValue
	if value.String() != "" || absent.String() != "" || value.Type() != "int32" {
		t.Fatal("an unset version must not become version zero")
	}
	for _, raw := range []string{"0", "12", "2147483647"} {
		if err := value.Set(raw); err != nil || value.value == nil || value.String() != raw {
			t.Fatalf("Set(%q): value=%s error=%v", raw, &value, err)
		}
	}
	for _, raw := range []string{"-1", "2147483648", "nope", ""} {
		if err := value.Set(raw); err == nil || value.String() != "2147483647" {
			t.Errorf("invalid version %q was accepted or changed the previous value: %s", raw, &value)
		}
	}
}
