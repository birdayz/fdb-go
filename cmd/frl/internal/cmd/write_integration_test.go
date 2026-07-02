// End-to-end tests for the write wave (RFC-174 §3.3/§5): record
// put/delete with dry-run + confirm gates, index rebuild/set-state,
// meta apply, store lock/truncate, and the `__SYS/CATALOG` write guard.
package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
)

func TestIntegration_RecordPutDelete_GuardedRoundTrip(t *testing.T) {
	bindConfig(t)
	ss := subspace.Sub("frl", "integration")
	orderJSON := `{"order_id": 999, "price": 42}`

	// Dry run writes nothing — byte-identical store before/after.
	before := snapshotStoreBytes(t, fixture.clusterFilePath, ss)
	out, err := runCmd(t, "record", "put", "--type", "Order", orderJSON, "--dry-run")
	if err != nil {
		t.Fatalf("put --dry-run: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, `"order_id":"999"`) && !strings.Contains(out, `"order_id": "999"`) &&
		!strings.Contains(out, `"order_id":999`) {
		t.Errorf("dry-run should print the would-be record:\n%s", out)
	}
	if after := snapshotStoreBytes(t, fixture.clusterFilePath, ss); after != before {
		t.Fatal("put --dry-run mutated the store")
	}

	// Without --yes on non-interactive stdin: refused, nothing written.
	if _, err := runCmd(t, "record", "put", "--type", "Order", orderJSON); err == nil {
		t.Fatal("put without --yes must be refused on non-interactive stdin")
	}
	if after := snapshotStoreBytes(t, fixture.clusterFilePath, ss); after != before {
		t.Fatal("refused put mutated the store")
	}

	// Real put, then read it back.
	if out, err := runCmd(t, "record", "put", "--type", "Order", orderJSON, "--yes"); err != nil {
		t.Fatalf("put --yes: %v\noutput: %s", err, out)
	}
	out, err = runCmd(t, "record", "get", "999")
	if err != nil {
		t.Fatalf("get after put: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "42") {
		t.Errorf("saved record missing price:\n%s", out)
	}

	// Index maintenance ran transactionally: the new price is in the index.
	out, err = runCmd(t, "index", "scan", "Order$price", "--limit", "0")
	if err != nil {
		t.Fatalf("index scan: %v", err)
	}
	if !strings.Contains(out, `"primary_key":"999"`) {
		t.Errorf("index entry for put record missing:\n%s", out)
	}

	// Delete it; delete again → already absent, exit 0 (C3 semantics).
	if out, err := runCmd(t, "record", "delete", "999", "--yes"); err != nil || !strings.Contains(out, "deleted") {
		t.Fatalf("delete: %v\noutput: %s", err, out)
	}
	out, err = runCmd(t, "record", "delete", "999", "--yes")
	if err != nil {
		t.Fatalf("second delete must succeed (maybe-committed retry semantics): %v", err)
	}
	if !strings.Contains(out, "already absent") {
		t.Errorf("second delete should report already absent:\n%s", out)
	}
}

func TestIntegration_IndexRebuild_WriteOnlyToReadable(t *testing.T) {
	bindConfig(t)

	// rebuild = clear + mark WRITE_ONLY + online build + mark READABLE.
	out, err := runCmd(t, "index", "rebuild", "Order$price", "--yes")
	if err != nil {
		t.Fatalf("index rebuild: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "built Order$price") {
		t.Errorf("rebuild output missing summary:\n%s", out)
	}

	// The index is READABLE again and its entries match the records.
	out, err = runCmd(t, "index", "ls")
	if err != nil || !strings.Contains(out, "READABLE") {
		t.Fatalf("index ls after rebuild: %v\n%s", err, out)
	}
	out, err = runCmd(t, "index", "scan", "Order$price", "--limit", "0")
	if err != nil {
		t.Fatalf("index scan after rebuild: %v", err)
	}
	for _, want := range []string{"100", "200", "300"} {
		if !strings.Contains(out, want) {
			t.Errorf("rebuilt index missing price %s:\n%s", want, out)
		}
	}
}

func TestIntegration_IndexSetState_ReadableRequiresBuilt(t *testing.T) {
	bindConfig(t)

	// write-only flips cleanly.
	if out, err := runCmd(t, "index", "set-state", "Order$price", "write-only", "--yes"); err != nil {
		t.Fatalf("set-state write-only: %v\n%s", err, out)
	}
	// disabled clears build progress…
	if out, err := runCmd(t, "index", "set-state", "Order$price", "disabled", "--yes"); err != nil {
		t.Fatalf("set-state disabled: %v\n%s", err, out)
	}
	// …so readable must now refuse (index not built).
	if _, err := runCmd(t, "index", "set-state", "Order$price", "readable", "--yes"); err == nil {
		t.Fatal("set-state readable on an unbuilt index must fail")
	}
	// build repairs it end-to-end.
	if out, err := runCmd(t, "index", "build", "Order$price", "--yes"); err != nil {
		t.Fatalf("index build: %v\n%s", err, out)
	}
	out, err := runCmd(t, "index", "ls")
	if err != nil || !strings.Contains(out, "READABLE") {
		t.Fatalf("index not READABLE after build: %v\n%s", err, out)
	}
}

func TestIntegration_MetaApply_ValidatorGate(t *testing.T) {
	requireFixture(t)
	// Dedicated metadata store keyspace; nothing else touches it.
	const metaKeyspace = "/frl/meta-apply-test"

	tmp := t.TempDir()
	configFile := filepath.Join(tmp, "config.yaml")
	cfgYAML := fmt.Sprintf(`current_context: apply
contexts:
  - name: apply
    cluster_file: %s
    keyspace_path: /frl/meta-apply-store
    metadata:
      meta_store_keyspace: %s
`, fixture.clusterFilePath, metaKeyspace)
	if err := os.WriteFile(configFile, []byte(cfgYAML), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("FRL_CONFIG", configFile)

	// v1 metadata file.
	v1 := buildMetaWith(t)
	v1Path := filepath.Join(tmp, "v1.pb")
	writeMetaFile(t, v1, v1Path)

	// Empty store: refuse without --force-initial.
	if _, err := runCmd(t, "meta", "apply", "--file", v1Path, "--yes"); err == nil {
		t.Fatal("apply into empty store must require --force-initial")
	}
	if out, err := runCmd(t, "meta", "apply", "--file", v1Path, "--force-initial", "--yes"); err != nil {
		t.Fatalf("initial apply: %v\n%s", err, out)
	}

	// Compatible evolution (new index bumps version) applies.
	v2 := buildMetaWith(t, func(b *recordlayer.RecordMetaDataBuilder) {
		b.AddIndex("Order", recordlayer.NewIndex("Order$price", recordlayer.Field("price")))
	})
	v2Path := filepath.Join(tmp, "v2.pb")
	writeMetaFile(t, v2, v2Path)
	if out, err := runCmd(t, "meta", "apply", "--file", v2Path, "--yes"); err != nil {
		t.Fatalf("evolved apply: %v\n%s", err, out)
	}

	// Incompatible evolution (dropping the index without a version bump
	// forward — reapplying v1 over v2) refuses.
	if _, err := runCmd(t, "meta", "apply", "--file", v1Path, "--yes"); err == nil {
		t.Fatal("apply of an incompatible (backwards) evolution must refuse")
	}

	// Path B completion (RFC-174 Slice 5): the metadata-only commands
	// read from the FDBMetaDataStore the context points at — the path
	// they used to reject with "not supported".
	out, err := runCmd(t, "meta", "get")
	if err != nil {
		t.Fatalf("meta get via meta_store_keyspace: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Order$price") {
		t.Errorf("meta get missing v2 index:\n%s", out)
	}
	out, err = runCmd(t, "meta", "types", "ls")
	if err != nil || !strings.Contains(out, "Order") {
		t.Fatalf("meta types ls via meta_store_keyspace: %v\n%s", err, out)
	}
	out, err = runCmd(t, "index", "describe", "Order$price")
	if err != nil || !strings.Contains(out, "price") {
		t.Fatalf("index describe via meta_store_keyspace: %v\n%s", err, out)
	}

	// The store really holds v2: read it back directly.
	db, err := fdb.OpenDatabase(fixture.clusterFilePath)
	if err != nil {
		t.Fatalf("open FDB: %v", err)
	}
	rec := recordlayer.NewFDBDatabase(db)
	ss, err := parseKeyspacePath(metaKeyspace)
	if err != nil {
		t.Fatalf("parse keyspace: %v", err)
	}
	metaStore := recordlayer.NewFDBMetaDataStore(ss)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := rec.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		return metaStore.LoadRecordMetaDataProto(rtx.Transaction())
	})
	if err != nil {
		t.Fatalf("read back metadata: %v", err)
	}
	stored, _ := result.(*gen.MetaData)
	if stored.GetVersion() != int32(v2.Version()) {
		t.Errorf("stored version = %d; want %d", stored.GetVersion(), v2.Version())
	}
}

func TestIntegration_StoreLock_BlocksWrites(t *testing.T) {
	bindConfig(t)

	if out, err := runCmd(t, "store", "lock", "forbid-record-update", "--reason", "e2e", "--yes"); err != nil {
		t.Fatalf("store lock: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_, _ = runCmd(t, "store", "unlock", "--yes")
	})

	out, err := runCmd(t, "store", "info")
	if err != nil || !strings.Contains(out, "FORBID_RECORD_UPDATE") || !strings.Contains(out, "e2e") {
		t.Fatalf("store info should show the lock + reason: %v\n%s", err, out)
	}

	// A record write against the locked store fails.
	if _, err := runCmd(t, "record", "put", "--type", "Order", `{"order_id": 888}`, "--yes"); err == nil {
		t.Fatal("record put against a locked store must fail")
	}

	if out, err := runCmd(t, "store", "unlock", "--yes"); err != nil {
		t.Fatalf("store unlock: %v\n%s", err, out)
	}
	out, err = runCmd(t, "store", "info")
	if err != nil || !strings.Contains(out, "unlocked") {
		t.Fatalf("store info should show unlocked: %v\n%s", err, out)
	}
}

func TestIntegration_StoreTruncate_DedicatedStore(t *testing.T) {
	requireFixture(t)
	// Truncate a DEDICATED store so the shared fixture stays intact.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := fdb.OpenDatabase(fixture.clusterFilePath)
	if err != nil {
		t.Fatalf("open FDB: %v", err)
	}
	rec := recordlayer.NewFDBDatabase(db)
	md := buildMetaWith(t)
	ss := subspace.Sub("frl", "truncate-test")
	if err := seedStore(ctx, rec, md, ss); err != nil {
		t.Fatalf("seed: %v", err)
	}

	tmp := t.TempDir()
	metaFile := filepath.Join(tmp, "meta.pb")
	writeMetaFile(t, md, metaFile)
	configFile := filepath.Join(tmp, "config.yaml")
	cfgYAML := fmt.Sprintf(`current_context: trunc
contexts:
  - name: trunc
    cluster_file: %s
    keyspace_path: /frl/truncate-test
    metadata:
      meta_file: %s
`, fixture.clusterFilePath, metaFile)
	if err := os.WriteFile(configFile, []byte(cfgYAML), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("FRL_CONFIG", configFile)

	// Gate: no --yes → refused.
	if _, err := runCmd(t, "store", "truncate"); err == nil {
		t.Fatal("truncate without --yes must be refused")
	}
	if out, err := runCmd(t, "store", "truncate", "--yes"); err != nil {
		t.Fatalf("truncate: %v\n%s", err, out)
	}
	out, err := runCmd(t, "record", "scan", "--limit", "0")
	if err != nil {
		t.Fatalf("scan after truncate: %v", err)
	}
	if strings.Contains(out, "primary_key") {
		t.Errorf("records survived truncate:\n%s", out)
	}
}

// The catalog's true prefix is a NESTED tuple (subspace.Sub packs a
// tuple.Tuple argument as one nested element), which no CLI addressing
// mode can express — different-depth nestings can never prefix-overlap,
// so the catalog is byte-unreachable through keyspace_path,
// keyspace_tuple, or database/schema addressing. guardNotCatalog is
// defense-in-depth for future addressing modes; pin it against the REAL
// catalog subspace bytes.
func TestWriteGuard_CatalogNeverAWriteTarget(t *testing.T) {
	t.Parallel()
	catalog := relationalKeyspace().CatalogSubspace()
	if err := guardNotCatalog(catalog); err == nil {
		t.Fatal("the catalog subspace itself must be refused")
	} else if !strings.Contains(err.Error(), "CATALOG") {
		t.Errorf("guard error should name the catalog: %v", err)
	}
	// A range inside the catalog and one containing it are both refused.
	if err := guardNotCatalog(catalog.Sub("TEMPLATES")); err == nil {
		t.Error("a subspace inside the catalog must be refused")
	}
	if err := guardNotCatalog(subspace.FromBytes(catalog.Bytes()[:2])); err == nil {
		t.Error("a prefix containing the catalog must be refused")
	}
	// A normal store subspace passes.
	if err := guardNotCatalog(subspace.Sub("frl", "integration")); err != nil {
		t.Errorf("ordinary store subspace refused: %v", err)
	}
}

// writeMetaFile persists md as a meta.pb for --file / meta_file wiring.
func writeMetaFile(t *testing.T, md *recordlayer.RecordMetaData, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()
	if err := recordlayer.WriteRecordMetaData(md, f); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// An interrupted build (here: --time-limit expiring) leaves resumable
// range-set state; rerunning with the same settings completes the build.
// This is the in-process analog of kill-and-resume: per-range progress
// commits atomically, so however the process stops, a rerun continues.
func TestIntegration_IndexBuild_InterruptedThenResumed(t *testing.T) {
	bindConfig(t)

	// Force WRITE_ONLY so the build has real work.
	if out, err := runCmd(t, "index", "set-state", "Order$price", "write-only", "--yes"); err != nil {
		t.Fatalf("set-state: %v\n%s", err, out)
	}
	// A 1ns time limit interrupts immediately — partial or zero progress.
	if _, err := runCmd(t, "index", "build", "Order$price", "--limit", "1", "--time-limit", "1ns", "--yes"); err == nil {
		t.Log("time-limited build completed anyway (tiny store) — resume still exercised below")
	}
	// Resume with matching settings finishes and marks READABLE.
	if out, err := runCmd(t, "index", "build", "Order$price", "--limit", "1", "--yes"); err != nil {
		t.Fatalf("resume build: %v\n%s", err, out)
	}
	out, err := runCmd(t, "index", "scan", "Order$price", "--limit", "0")
	if err != nil {
		t.Fatalf("index scan after resume: %v", err)
	}
	for _, want := range []string{"100", "200", "300"} {
		if !strings.Contains(out, want) {
			t.Errorf("resumed index missing price %s:\n%s", want, out)
		}
	}
}

// frl status reports all four wiring legs against the live fixture.
func TestIntegration_Status_AllChecks(t *testing.T) {
	bindConfig(t)
	out, err := runCmd(t, "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	for _, want := range []string{"cluster:   ok", "store:     ok", "metadata:  ok (3 record types)"} {
		if !strings.Contains(out, want) {
			t.Errorf("status missing %q:\n%s", want, out)
		}
	}
	// Catalog presence depends on whether the sql fixture ran in this
	// process — accept either verdict, but the line must be present.
	if !strings.Contains(out, "catalog:   ok") && !strings.Contains(out, "catalog:   missing") {
		t.Errorf("status missing catalog line:\n%s", out)
	}
}

// A FULL_STORE lock must never be permanent: Open() rejects the locked
// store, so `store unlock` arms the builder's bypass with the header's
// stored reason (codex P1 — without that, unlock could never run). The
// empty-reason variant pins the library fix underneath: the bypass is
// nullable like Java's @Nullable String, so "" is a valid bypass value.
func TestIntegration_StoreLock_FullStoreUnlockable(t *testing.T) {
	requireFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := fdb.OpenDatabase(fixture.clusterFilePath)
	if err != nil {
		t.Fatalf("open FDB: %v", err)
	}
	rec := recordlayer.NewFDBDatabase(db)
	md := buildMetaWith(t)
	ss := subspace.Sub("frl", "fullstore-test")
	if err := seedStore(ctx, rec, md, ss); err != nil {
		t.Fatalf("seed: %v", err)
	}

	tmp := t.TempDir()
	metaFile := filepath.Join(tmp, "meta.pb")
	writeMetaFile(t, md, metaFile)
	configFile := filepath.Join(tmp, "config.yaml")
	cfgYAML := fmt.Sprintf(`current_context: fullstore
contexts:
  - name: fullstore
    cluster_file: %s
    keyspace_path: /frl/fullstore-test
    metadata:
      meta_file: %s
`, fixture.clusterFilePath, metaFile)
	if err := os.WriteFile(configFile, []byte(cfgYAML), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("FRL_CONFIG", configFile)

	if out, err := runCmd(t, "store", "lock", "full-store", "--reason", "maintenance", "--yes"); err != nil {
		t.Fatalf("lock full-store: %v\n%s", err, out)
	}
	// The store really is fully locked: plain opens fail.
	if _, err := runCmd(t, "record", "scan", "--limit", "1"); err == nil {
		t.Fatal("scan of a fully locked store must fail")
	}
	// Managing the lock still works: change the reason while locked…
	if out, err := runCmd(t, "store", "lock", "full-store", "--reason", "handover", "--yes"); err != nil {
		t.Fatalf("re-lock with new reason: %v\n%s", err, out)
	}
	// …and unlock clears it.
	if out, err := runCmd(t, "store", "unlock", "--yes"); err != nil {
		t.Fatalf("unlock full-store: %v\n%s", err, out)
	}
	if out, err := runCmd(t, "record", "scan", "--limit", "1"); err != nil {
		t.Fatalf("scan after unlock: %v\n%s", err, out)
	}

	// Empty-reason full-store lock: the worst case — still unlockable.
	if out, err := runCmd(t, "store", "lock", "full-store", "--yes"); err != nil {
		t.Fatalf("lock full-store (no reason): %v\n%s", err, out)
	}
	if _, err := runCmd(t, "record", "scan", "--limit", "1"); err == nil {
		t.Fatal("scan of a fully locked store must fail (empty reason)")
	}
	if out, err := runCmd(t, "store", "unlock", "--yes"); err != nil {
		t.Fatalf("unlock empty-reason full-store: %v\n%s", err, out)
	}
	if out, err := runCmd(t, "record", "scan", "--limit", "1"); err != nil {
		t.Fatalf("scan after empty-reason unlock: %v\n%s", err, out)
	}
}

// A --type typo on record delete must fail loudly, not fall through to
// the raw primary key (codex P1: on a prefix-keyed store the unprefixed
// key can address a DIFFERENT record).
func TestIntegration_RecordDelete_UnknownTypeRefused(t *testing.T) {
	bindConfig(t)
	// Nonexistent pk so that even a regression cannot damage the shared
	// fixture store.
	_, err := runCmd(t, "record", "delete", "99999", "--type", "Bogus", "--yes")
	if err == nil {
		t.Fatal("delete with unknown --type must fail, not fall through to the raw key")
	}
	if !strings.Contains(err.Error(), "not found") || !strings.Contains(err.Error(), "Order") {
		t.Errorf("error should be the standard 'not found — available: …' message, got: %v", err)
	}
	// Dry-run takes the same gate.
	if _, err := runCmd(t, "record", "delete", "99999", "--type", "Bogus", "--dry-run"); err == nil {
		t.Fatal("dry-run delete with unknown --type must fail too")
	}
}

// meta apply is idempotent and race-guarded (Graefe impl-review + FDB
// C++ dev C5): re-applying current metadata is a no-op success, and the
// save transaction re-checks that the store still holds exactly what the
// operator confirmed against.
func TestIntegration_MetaApply_AlreadyCurrentAndRaceGuard(t *testing.T) {
	requireFixture(t)
	const metaKeyspace = "/frl/meta-apply-race-test"

	tmp := t.TempDir()
	configFile := filepath.Join(tmp, "config.yaml")
	cfgYAML := fmt.Sprintf(`current_context: race
contexts:
  - name: race
    cluster_file: %s
    keyspace_path: /frl/meta-apply-race-store
    metadata:
      meta_store_keyspace: %s
`, fixture.clusterFilePath, metaKeyspace)
	if err := os.WriteFile(configFile, []byte(cfgYAML), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("FRL_CONFIG", configFile)

	v1 := buildMetaWith(t)
	v1Path := filepath.Join(tmp, "v1.pb")
	writeMetaFile(t, v1, v1Path)
	if out, err := runCmd(t, "meta", "apply", "--file", v1Path, "--force-initial", "--yes"); err != nil {
		t.Fatalf("initial apply: %v\n%s", err, out)
	}

	// Idempotency: the same file again succeeds without writing.
	out, err := runCmd(t, "meta", "apply", "--file", v1Path, "--yes")
	if err != nil {
		t.Fatalf("re-apply of current metadata must succeed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "already current") {
		t.Errorf("re-apply should report 'already current':\n%s", out)
	}

	// Race guard, exercised at the transaction helper level (the CLI
	// cannot interleave a concurrent evolver between its own prompt and
	// save): the operator confirmed against v1, but the store moved on.
	v2 := buildMetaWith(t, func(b *recordlayer.RecordMetaDataBuilder) {
		b.AddIndex("Order", recordlayer.NewIndex("Order$price", recordlayer.Field("price")))
	})
	v3 := buildMetaWith(t, func(b *recordlayer.RecordMetaDataBuilder) {
		b.AddIndex("Order", recordlayer.NewIndex("Order$price", recordlayer.Field("price")))
		b.AddIndex("Order", recordlayer.NewIndex("Order$quantity", recordlayer.Field("quantity")))
	})
	v1Proto, err := v1.ToProto()
	if err != nil {
		t.Fatalf("v1 proto: %v", err)
	}
	v2Proto, err := v2.ToProto()
	if err != nil {
		t.Fatalf("v2 proto: %v", err)
	}
	v3Proto, err := v3.ToProto()
	if err != nil {
		t.Fatalf("v3 proto: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := fdb.OpenDatabase(fixture.clusterFilePath)
	if err != nil {
		t.Fatalf("open FDB: %v", err)
	}
	rec := recordlayer.NewFDBDatabase(db)
	ss, err := parseKeyspacePath(metaKeyspace)
	if err != nil {
		t.Fatalf("parse keyspace: %v", err)
	}
	metaStore := recordlayer.NewFDBMetaDataStore(ss)

	// A concurrent evolver lands v2 after the operator confirmed v1.
	if _, err := rec.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		return nil, metaStore.SaveRecordMetaData(rtx.Transaction(), v2Proto)
	}); err != nil {
		t.Fatalf("out-of-band evolution: %v", err)
	}

	// Applying v3 with v1 as the confirmed base must refuse.
	_, err = rec.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		return metaApplySave(rtx, metaStore, v1Proto, v3Proto)
	})
	if err == nil || !strings.Contains(err.Error(), "metadata changed while awaiting confirmation") {
		t.Fatalf("race guard must trip, got: %v", err)
	}

	// Maybe-committed retry shape: confirmed v1, writing v2, store
	// already holds v2 → already-current success, no re-archive.
	outcomeAny, err := rec.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		return metaApplySave(rtx, metaStore, v1Proto, v2Proto)
	})
	if err != nil {
		t.Fatalf("retry-shaped apply must succeed: %v", err)
	}
	if outcome, _ := outcomeAny.(metaApplyOutcome); outcome != metaApplyAlreadyCurrent {
		t.Fatalf("expected metaApplyAlreadyCurrent, got %v", outcomeAny)
	}
}
