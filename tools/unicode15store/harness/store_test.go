package harness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
)

var (
	captureDir    = flag.String("unicode15-capture-dir", "", "export raw golden JSON to this directory")
	captureBinary = flag.String("unicode15-capture-binary", "", "immutable pre-pin CLI used to export goldens")
	oldBinary     = flag.String("unicode15-old-binary", "", "old compiler CLI for same-store cross-process verification")
	newBinary     = flag.String("unicode15-new-binary", "", "new compiler CLI for same-store cross-process verification")
)

func TestPersistedUnicode15(t *testing.T) {
	t.Parallel()
	t.Logf("compiler=%s", runtime.Version())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cli, err := testcontainers.NewDockerClientWithOpts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	if _, err := cli.Ping(ctx); err != nil {
		t.Skipf("FDB not available (no Docker): %v", err)
	}
	container, err := foundationdbtc.Run(ctx, "", foundationdbtc.WithAPIVersion(730))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := foundationdbtc.Terminate(ctx, container); err != nil {
			t.Error(err)
		}
	})
	cluster, err := container.ClusterFile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	clusterFile := filepath.Join(t.TempDir(), "fdb.cluster")
	if err := os.WriteFile(clusterFile, []byte(cluster), 0o600); err != nil {
		t.Fatal(err)
	}
	fdb.MustAPIVersion(730)
	db, err := fdb.OpenDatabase(clusterFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	t.Run("invalid_proto2_utf8_is_not_persistable", func(t *testing.T) {
		md, _, err := metadata(spec{name: "invalid"})
		if err != nil {
			t.Fatal(err)
		}
		ss := subspace.Sub(tuple.Tuple{"unicode15store-invalid", t.Name(), time.Now().UnixNano()})
		_, err = recordlayer.NewFDBDatabase(db).Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			customer := customers(spec{}, false)[0]
			invalid := "a\xffb"
			customer.Name = &invalid
			_, err = store.SaveRecord(customer)
			return nil, err
		})
		if err == nil || !strings.Contains(err.Error(), "string field name is not valid UTF-8") {
			t.Fatalf("invalid proto2 persistence = %v, want serialization rejection", err)
		}
		_, err = recordlayer.NewFDBDatabase(db).Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			keys, err := rtx.Transaction().GetRange(ss, fdb.RangeOptions{}).GetSliceWithError()
			if err != nil {
				return nil, err
			}
			if len(keys) != 0 {
				return nil, fmt.Errorf("failed invalid-UTF-8 write committed %d keys", len(keys))
			}
			return nil, nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
	if (*captureDir == "") != (*captureBinary == "") {
		t.Fatal("capture requires both -unicode15-capture-dir and -unicode15-capture-binary")
	}
	if (*oldBinary == "") != (*newBinary == "") {
		t.Fatal("cross-process verification requires both compiler binaries")
	}
	prefix := fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano())
	operate := func(binary, prefix, operation string) Report {
		t.Helper()
		if binary == "" {
			report, err := Run(ctx, db, prefix, operation)
			if err != nil {
				t.Fatal(err)
			}
			return report
		}
		command := exec.CommandContext(ctx, binary, "-cluster-file", clusterFile, "-prefix", prefix, "-operation", operation)
		command.Stderr = os.Stderr
		output, err := command.Output()
		if err != nil {
			t.Fatalf("%s %s: %v", binary, operation, err)
		}
		var report Report
		if err := json.Unmarshal(output, &report); err != nil {
			t.Fatal(err)
		}
		return report
	}
	assertTokenizerVersionZero := func(prefix string) {
		t.Helper()
		_, err := recordlayer.NewFDBDatabase(db).Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			for _, s := range specs() {
				if s.collated {
					continue
				}
				space := subspace.Sub(tuple.Tuple{"unicode15store", prefix}).Sub(s.name, recordlayer.IndexSecondarySpaceKey, "text", int64(0))
				keys, err := rtx.Transaction().GetRange(space, fdb.RangeOptions{}).GetSliceWithError()
				if err != nil {
					return nil, err
				}
				if len(keys) != 37 {
					return nil, fmt.Errorf("%s has %d tokenizer-version keys, want 37", s.name, len(keys))
				}
				for _, kv := range keys {
					if hex.EncodeToString(kv.Value) != "14" {
						return nil, fmt.Errorf("%s tokenizer version changed from zero: %x", s.name, kv.Value)
					}
				}
			}
			return nil, nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	assertRecordState := func(prefix string, empty bool) {
		t.Helper()
		_, err := recordlayer.NewFDBDatabase(db).Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			for _, s := range specs() {
				space := subspace.Sub(tuple.Tuple{"unicode15store", prefix}).Sub(s.name, recordlayer.RecordKey)
				keys, err := rtx.Transaction().GetRange(space, fdb.RangeOptions{}).GetSliceWithError()
				if err != nil {
					return nil, err
				}
				wantRecords := len(customers(s, false))
				wantVersions := 37
				if s.collated {
					wantVersions = 0
				}
				if empty {
					wantRecords, wantVersions = 0, 0
				}
				if len(keys) != wantRecords {
					return nil, fmt.Errorf("%s has %d persisted record keys, want %d", s.name, len(keys), wantRecords)
				}
				secondary := subspace.Sub(tuple.Tuple{"unicode15store", prefix}).Sub(s.name, recordlayer.IndexSecondarySpaceKey)
				versions, err := rtx.Transaction().GetRange(secondary, fdb.RangeOptions{}).GetSliceWithError()
				if err != nil {
					return nil, err
				}
				if len(versions) != wantVersions {
					return nil, fmt.Errorf("%s has %d secondary/tokenizer-version keys, want %d", s.name, len(versions), wantVersions)
				}
			}
			return nil, nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	golden := func(operation string, got Report) {
		t.Helper()
		if got.Schema != 1 || len(got.Stores) != 19 {
			t.Fatalf("vacuous report: schema=%d stores=%d", got.Schema, len(got.Stores))
		}
		if *captureDir != "" {
			data, err := json.MarshalIndent(got, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(*captureDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(*captureDir, operation+".json"), append(data, '\n'), 0o644); err != nil {
				t.Fatal(err)
			}
			return
		}
		data, err := os.ReadFile("testdata/" + operation + ".json")
		if err != nil {
			t.Fatal(err)
		}
		var want Report
		if err := json.Unmarshal(data, &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			for i := range got.Stores {
				if i >= len(want.Stores) || !reflect.DeepEqual(got.Stores[i], want.Stores[i]) {
					t.Errorf("%s golden mismatch in %s", operation, got.Stores[i].Name)
				}
			}
			t.FailNow()
		}
	}
	write := operate(*captureBinary, prefix, "write")
	assertPopulated(t, write)
	assertTokenizerVersionZero(prefix)
	assertRecordState(prefix, false)
	golden("write", write)
	rebuilt := operate(*captureBinary, prefix, "rebuild")
	if !reflect.DeepEqual(write, rebuilt) {
		t.Fatal("rebuild changed persisted write bytes or postings")
	}
	golden("rebuild", rebuilt)
	updated := operate(*captureBinary, prefix, "update")
	assertPopulated(t, updated)
	assertTokenizerVersionZero(prefix)
	assertObsoleteTermsAbsent(t, updated)
	golden("update", updated)
	if rebuilt := operate(*captureBinary, prefix, "rebuild"); !reflect.DeepEqual(updated, rebuilt) {
		t.Fatal("rebuild changed persisted update bytes or postings")
	}
	deleted := operate(*captureBinary, prefix, "delete")
	assertEmpty(t, deleted)
	assertRecordState(prefix, true)
	golden("delete", deleted)
	writtenPrefix := prefix + "-delete-written"
	golden("write", operate(*captureBinary, writtenPrefix, "write"))
	deletedWritten := operate(*captureBinary, writtenPrefix, "delete")
	assertEmpty(t, deletedWritten)
	assertRecordState(writtenPrefix, true)
	golden("delete_written", deletedWritten)
	if *oldBinary != "" {
		for i, pair := range [][2]string{{*oldBinary, *newBinary}, {*newBinary, *oldBinary}} {
			crossPrefix := fmt.Sprintf("%s-cross-%d", prefix, i)
			golden("write", operate(pair[0], crossPrefix, "write"))
			golden("write", operate(pair[1], crossPrefix, "inspect"))
			golden("update", operate(pair[1], crossPrefix, "update"))
			assertTokenizerVersionZero(crossPrefix)
			golden("update", operate(pair[0], crossPrefix, "rebuild"))
			deleted := operate(pair[1], crossPrefix, "delete")
			assertEmpty(t, deleted)
			assertRecordState(crossPrefix, true)
			golden("delete", deleted)
			golden("delete", operate(pair[0], crossPrefix, "inspect"))
			directPrefix := crossPrefix + "-delete-written"
			golden("write", operate(pair[0], directPrefix, "write"))
			directDeleted := operate(pair[1], directPrefix, "delete")
			assertEmpty(t, directDeleted)
			assertRecordState(directPrefix, true)
			golden("delete_written", directDeleted)
			t.Logf("cross-process direction %d verified write/inspect/update/rebuild/delete and direct delete of original writer fixture: %s -> %s", i, pair[0], pair[1])
		}
	}
}

func TestGoldenProvenance(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/provenance.json")
	if err != nil {
		t.Fatal(err)
	}
	var provenance struct {
		Schema             int               `json:"schema"`
		SourceCommit       string            `json:"sourceCommit"`
		Compiler           string            `json:"compiler"`
		FoundationDB       string            `json:"foundationdb"`
		WriterBinarySHA256 string            `json:"writerBinarySHA256"`
		GoldenSHA256       map[string]string `json:"goldenSHA256"`
	}
	if err := json.Unmarshal(data, &provenance); err != nil {
		t.Fatal(err)
	}
	if provenance.Schema != 1 || provenance.SourceCommit != "2804215b86cbe3d06b8f865810d628c27a737d23" || provenance.Compiler != "go1.26.10" || provenance.FoundationDB != "7.3.77" || provenance.WriterBinarySHA256 != "f298cf2dd7725ea49c46d4356b3a2f86316ea83fbed0958ba74c0289e08a8ffd" || len(provenance.GoldenSHA256) != 5 {
		t.Fatal("missing or incorrect pre-pin writer identity")
	}
	for _, name := range []string{"write.json", "rebuild.json", "update.json", "delete.json", "delete_written.json"} {
		data, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if digest := fmt.Sprintf("%x", sha256.Sum256(data)); digest != provenance.GoldenSHA256[name] {
			t.Fatalf("%s does not match the captured golden digest", name)
		}
		var report Report
		if err := json.Unmarshal(data, &report); err != nil {
			t.Fatal(err)
		}
		if report.Schema != 1 || len(report.Stores) != 19 {
			t.Fatalf("%s has an empty or incomplete population", name)
		}
		if name == "delete.json" || name == "delete_written.json" {
			assertEmpty(t, report)
		} else {
			assertPopulated(t, report)
		}
	}
}

func TestRunRejectsInvalidArguments(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ prefix, operation string }{{"", "write"}, {"isolated", "unknown"}} {
		if _, err := Run(context.Background(), fdb.Database{}, tc.prefix, tc.operation); err == nil {
			t.Fatalf("Run(%q, %q) accepted invalid arguments", tc.prefix, tc.operation)
		}
	}
}

func assertPopulated(t *testing.T, report Report) {
	t.Helper()
	if len(report.Stores) != 19 {
		t.Fatalf("got %d stores, want 19", len(report.Stores))
	}
	for _, store := range report.Stores {
		if len(store.Index) == 0 {
			t.Fatalf("%s has no raw index keys", store.Name)
		}
		for i, kv := range store.Index {
			if i > 0 && kv.Key <= store.Index[i-1].Key {
				t.Fatalf("%s raw keys are not strictly sorted", store.Name)
			}
		}
		if strings.HasPrefix(store.Name, "text") {
			if len(store.Postings) < 50 {
				t.Fatalf("%s has only %d postings", store.Name, len(store.Postings))
			}
			for _, posting := range store.Postings {
				if strings.HasSuffix(store.Name, "omit") && len(posting.Positions) != 0 {
					t.Fatalf("%s stored positions", store.Name)
				}
			}
		} else {
			if len(store.Records) != len(CollationTexts) || len(store.Order) != len(CollationTexts) || len(store.Counts) == 0 || len(store.Lookups) != len(CollationTexts) {
				t.Fatalf("%s missing collated records/counts/range lookups", store.Name)
			}
			for _, n := range store.Lookups {
				if n < 1 {
					t.Fatalf("%s empty collated index lookup", store.Name)
				}
			}
		}
	}
}

func assertObsoleteTermsAbsent(t *testing.T, report Report) {
	t.Helper()
	for _, store := range report.Stores {
		if strings.HasPrefix(store.Name, "text") {
			for _, kv := range store.Index {
				key, err := hex.DecodeString(kv.Key)
				if err != nil {
					t.Fatal(err)
				}
				parts, err := tuple.Unpack(key)
				if err != nil {
					t.Fatal(err)
				}
				tokenOffset := 2
				if strings.Contains(store.Name, "grouped") {
					tokenOffset++
				}
				if len(parts) <= tokenOffset {
					t.Fatalf("short raw TEXT key: %x", key)
				}
				if parts[tokenOffset] == "oldterm" || parts[tokenOffset] == "common" {
					t.Fatalf("%s obsolete raw token subspace survived: %v", store.Name, parts)
				}
			}
		}
		for _, p := range store.Postings {
			if p.Token == "oldterm" || p.Token == "common" {
				t.Fatalf("%s obsolete posting %q survived update", store.Name, p.Token)
			}
		}
	}
}

func assertEmpty(t *testing.T, report Report) {
	t.Helper()
	for _, store := range report.Stores {
		if len(store.Index) != 0 || len(store.Records) != 0 || len(store.Postings) != 0 || len(store.Order) != 0 {
			t.Fatalf("%s not empty after delete: %+v", store.Name, store)
		}
		// Atomic record counts may retain zero-valued keys after deleting the
		// last record; those exact bytes are pinned by the delete golden too.
		for _, kv := range store.Counts {
			if kv.Value != "0000000000000000" {
				t.Fatalf("%s nonzero deleted count: %+v", store.Name, kv)
			}
		}
	}
}
