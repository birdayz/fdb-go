package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fdb.dev/cmd/fdb/internal/config"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

func TestIntegration_DescriptorSetInputMaintainsAnnotatedIndexes(t *testing.T) {
	f := requireFixture(t)
	t.Setenv("FDB_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	path := descriptorFixturePath(t)
	db, err := fdb.OpenDatabase(f.clusterFilePath)
	if err != nil {
		t.Fatal(err)
	}
	rec := recordlayer.NewFDBDatabase(db)
	prefix := t.TempDir()
	ss := subspace.Sub("descriptor-cli", prefix)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := rec.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			rtx.Transaction().ClearRange(ss)
			return nil, nil
		}); err != nil {
			t.Errorf("clear descriptor fixture: %v", err)
		}
	})
	target := []string{"--cluster-file", f.clusterFilePath, "--keyspace-tuple", tupleToJSON(tuple.Tuple{"descriptor-cli", prefix}), "--descriptor-set", path, "--meta-version", "12"}
	invoke := func(args ...string) (string, error) {
		t.Helper()
		return runCmd(t, append(args, target...)...)
	}
	out, err := invoke("rl", "store", "create")
	if err == nil || !strings.Contains(err.Error(), "without --yes") {
		t.Fatalf("create without confirmation: %v\n%s", err, out)
	}
	if got := snapshotStoreBytes(t, f.clusterFilePath, ss); got != "" {
		t.Fatalf("unconfirmed create wrote data: %s", got)
	}
	out, err = invoke("rl", "store", "create", "--yes")
	if err != nil || !strings.Contains(out, "metadata version 12") {
		t.Fatalf("create from descriptor: %v\n%s", err, out)
	}
	out, err = invoke("rl", "record", "put", "--type", "Customer", `{"id":42,"email":"reader@example.com","score":10,"tags":["blue","green"]}`, "--yes")
	if err != nil {
		t.Fatalf("put with descriptor metadata: %v\n%s", err, out)
	}
	out, err = invoke("rl", "record", "get", "42")
	if err != nil || !strings.Contains(out, "reader@example.com") {
		t.Fatalf("get with descriptor metadata: %v\n%s", err, out)
	}
	out, err = invoke("rl", "index", "scan", "Customer$email")
	if err != nil || !strings.Contains(out, "reader@example.com") {
		t.Fatalf("annotated index was not maintained: %v\n%s", err, out)
	}
	out, err = invoke("rl", "index", "scan", "Customer$tags")
	if err != nil || !strings.Contains(out, "blue") || !strings.Contains(out, "green") {
		t.Fatalf("annotated repeated index was not maintained: %v\n%s", err, out)
	}
	out, err = invoke("rl", "record", "put", "--type", "Customer", `{"id":43,"email":"reader@example.com"}`, "--yes")
	var violation *recordlayer.RecordIndexUniquenessViolationError
	if !errors.As(err, &violation) || violation.IndexName != "Customer$email" {
		t.Fatalf("duplicate email must fail the proto-declared unique index: %v\n%s", err, out)
	}
	before := snapshotStoreBytes(t, f.clusterFilePath, ss)
	out, err = invoke("rl", "store", "create", "--yes")
	var exists *recordlayer.RecordStoreAlreadyExistsError
	if !errors.As(err, &exists) {
		t.Fatalf("second create must refuse an existing store: %v\n%s", err, out)
	}
	if after := snapshotStoreBytes(t, f.clusterFilePath, ss); after != before {
		t.Fatal("second create changed existing records, indexes, or header")
	}
	_, err = rec.Run(t.Context(), func(rtx *recordlayer.FDBRecordContext) (any, error) {
		rtx.Transaction().ClearRange(ss)
		rtx.Transaction().Set(ss.Pack(tuple.Tuple{"foreign"}), []byte("not a record store"))
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	before = snapshotStoreBytes(t, f.clusterFilePath, ss)
	out, err = invoke("rl", "store", "create", "--yes")
	var nonempty *recordlayer.RecordStoreNoInfoButNotEmptyError
	if !errors.As(err, &nonempty) {
		t.Fatalf("create must refuse a nonempty prefix without a header: %v\n%s", err, out)
	}
	if after := snapshotStoreBytes(t, f.clusterFilePath, ss); after != before {
		t.Fatal("refused create changed foreign keys")
	}
}

func TestIntegration_DescriptorWritesRejectMissingSelectedContext(t *testing.T) {
	f := requireFixture(t)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("FDB_CONFIG", configPath)
	t.Setenv("FDB_CLUSTER_FILE", f.clusterFilePath)
	path := descriptorFixturePath(t)
	prefix := t.TempDir()
	existing := subspace.Sub("descriptor-context", prefix, "existing")
	empty := subspace.Sub("descriptor-context", prefix, "empty")
	db, err := fdb.OpenDatabase(f.clusterFilePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
			tr.ClearRange(subspace.Sub("descriptor-context", prefix))
			return nil, nil
		}); err != nil {
			t.Errorf("clear descriptor context fixture: %v", err)
		}
	})
	target := func(suffix string) []string {
		return []string{"--keyspace-tuple", tupleToJSON(tuple.Tuple{"descriptor-context", prefix, suffix}), "--descriptor-set", path}
	}
	invoke := func(suffix string, args ...string) (string, error) {
		t.Helper()
		return runCmd(t, append(args, target(suffix)...)...)
	}
	if out, err := invoke("existing", "rl", "store", "create", "--yes"); err != nil {
		t.Fatalf("config-free default discovery create: %v\n%s", err, out)
	}
	if out, err := invoke("existing", "rl", "record", "put", "--type", "Customer", `{"id":42,"email":"original@example.com"}`, "--yes"); err != nil {
		t.Fatalf("seed fallback store: %v\n%s", err, out)
	}
	before := snapshotStoreBytes(t, f.clusterFilePath, existing)
	if before == "" || snapshotStoreBytes(t, f.clusterFilePath, empty) != "" {
		t.Fatal("expected one populated fallback store and one empty create prefix")
	}
	if err := os.WriteFile(configPath, []byte("current_context: deleted\ncontexts: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, selection := range []string{"implicit", "explicit"} {
		for _, tc := range []struct {
			suffix string
			args   []string
		}{
			{"empty", []string{"rl", "store", "create", "--yes"}},
			{"existing", []string{"rl", "record", "put", "--type", "Customer", `{"id":42,"email":"replaced@example.com"}`, "--yes"}},
			{"existing", []string{"rl", "record", "delete", "42", "--yes"}},
			{"existing", []string{"rl", "store", "truncate", "--yes"}},
		} {
			t.Run(selection+"/"+strings.Join(tc.args[:3], " "), func(t *testing.T) {
				args := append([]string{}, tc.args...)
				if selection == "explicit" {
					args = append(args, "--context", "deleted")
				}
				out, err := invoke(tc.suffix, args...)
				var missing *config.ContextNotFoundError
				if !errors.Is(err, config.ErrNoContext) || !errors.As(err, &missing) || missing.Name != "deleted" || out != "" {
					t.Errorf("native write reached fallback: output=%q error=%v", out, err)
				}
				if after := snapshotStoreBytes(t, f.clusterFilePath, existing); after != before {
					t.Error("refused native write changed fallback records, indexes, or header")
				}
				if after := snapshotStoreBytes(t, f.clusterFilePath, empty); after != "" {
					t.Error("refused native create populated the fallback prefix")
				}
			})
		}
	}
	out, err := invoke("existing", "rl", "record", "get", "42", "--cluster-file", f.clusterFilePath)
	if err != nil || !strings.Contains(out, "original@example.com") {
		t.Fatalf("explicit cluster override control: %v\n%s", err, out)
	}
}
