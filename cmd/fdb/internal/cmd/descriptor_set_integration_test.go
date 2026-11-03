package cmd

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
