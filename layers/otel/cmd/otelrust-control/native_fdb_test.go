package main

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"fdb.dev/layers/otel"
	"fdb.dev/layers/otel/collector"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/core/catalog"
	foundationdb "fdb.dev/pkg/testcontainers/foundationdb"
	"github.com/bazelbuild/rules_go/go/runfiles"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

var (
	pureRunfile    = flag.String("control-pure", "", "runfile of //layers/otel/cmd/otelrust-control:control-pure")
	nativeRunfile  = flag.String("control-native", "", "runfile of //layers/otel/cmd/otelrust-control:control-native")
	libfdbCRunfile = flag.String("libfdb-c", "", "runfile of @libfdb_c_linux_x86_64//file")
	otelRustFile   = flag.String("otel-rust", "", "runfile of //layers/otel/rust:otel-rust")
)

func runfile(t *testing.T, p string) string {
	t.Helper()
	if p == "" {
		t.Fatal("the -control-pure, -control-native, -libfdb-c and -otel-rust runfile flags are required")
	}
	rf, err := runfiles.New()
	if err != nil {
		t.Fatal(err)
	}
	loc, err := rf.Rlocation(p)
	if err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(loc)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// pinnedLibrary is the repository's libfdb_c 7.3.77 artifact.
func pinnedLibrary(t *testing.T) string { return runfile(t, *libfdbCRunfile) }

// live runs the real executables as child processes against one owned FDB container:
// control-pure for corpus, setup, verify and go_pure ingest, control-native for
// go_native ingest, and the Rust executable for rust_native ingest.
type live struct {
	t                   *testing.T
	ctx                 context.Context
	dir                 string
	pure, pureSHA       string
	native, nativeSHA   string
	rust, rustSHA       string
	library             string
	cluster, clusterSHA string
	corpus, corpusSHA   string
	frames              int
	corpusCfg           map[string]any
	revision, manifest  string
	db                  fdb.BackendDatabase
}

func newLive(t *testing.T) *live {
	setup, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	container, err := foundationdb.Run(setup, "", foundationdb.WithVersion("7.3.77"), foundationdb.WithAPIVersion(730), foundationdb.WithDirectIP())
	if err != nil {
		if foundationdb.SkipWithoutDocker(err) {
			t.Skip("FDB not available (no Docker)")
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { foundationdb.TeardownT(t, container) })
	text, err := container.ClusterFile(setup)
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithTimeout(t.Context(), 15*time.Minute)
	t.Cleanup(stop)
	l := &live{
		t: t, ctx: ctx, dir: t.TempDir(), pure: runfile(t, *pureRunfile), native: runfile(t, *nativeRunfile),
		rust: runfile(t, *otelRustFile), library: pinnedLibrary(t), frames: 24,
	}
	l.cluster = filepath.Join(l.dir, "fdb.cluster")
	l.clusterSHA = writeFile(t, l.cluster, []byte(text))
	if l.pureSHA, err = hashFile(l.pure); err != nil {
		t.Fatal(err)
	}
	if l.nativeSHA, err = hashFile(l.native); err != nil {
		t.Fatal(err)
	}
	if l.pureSHA == l.nativeSHA {
		t.Fatal("control-pure and control-native are the same executable")
	}
	if l.rustSHA, err = hashFile(l.rust); err != nil {
		t.Fatal(err)
	}
	l.revision, l.manifest = strings.Repeat("1", 40), strings.Repeat("2", 64)
	l.corpusCfg = map[string]any{"generator": "otel_fixture_v2", "seed": 283, "count": l.frames, "tenants": 1, "attributes": 8, "match_every": 3}
	// An independent test-side client: it only reads and deliberately corrupts.
	if l.db, err = openPureBackend(ctx, l.cluster); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.db.Close)
	return l
}

// job is a mode's job, sealed for the executable that runs it: control-pure, except
// the go_native and rust_native ingests.
func (l *live) job(name, mode, arm, run, workload, compression string) map[string]any {
	binary := l.pureSHA
	switch {
	case mode == "ingest" && arm == "go_native":
		binary = l.nativeSHA
	case mode == "ingest" && arm == "rust_native":
		binary = l.rustSHA
	}
	j := map[string]any{
		"schema": jobSchema, "mode": mode, "arm": arm, "binary_sha256": binary,
		"source_revision": l.revision, "source_manifest_sha256": l.manifest, "timeout_seconds": 120,
		"output": filepath.Join(l.dir, name+".report.json"), "corpus_config": l.corpusCfg,
		"expected_frames": l.frames, "row_budget": l.frames, "corpus_file": l.corpus,
	}
	if mode == "corpus" {
		return j
	}
	var w any
	if err := json.Unmarshal([]byte(workload), &w); err != nil {
		l.t.Fatal(err)
	}
	j["run"], j["cluster_file"], j["cluster_sha256"], j["corpus_sha256"] = run, l.cluster, l.clusterSHA, l.corpusSHA
	j["workload"], j["writer_batch_size"], j["writer_workers"], j["record_compression"] = w, 64, 1, compression
	j["enable_message_deltas"], j["enable_message_full_text"], j["bulk_index_conflicts"] = false, false, false
	j["client_backend"], j["client_library"], j["client_library_sha256"] = "pure_go", nil, nil
	if arm != "go_pure" {
		j["client_backend"], j["client_library"], j["client_library_sha256"] = "native_c", l.library, clientSHA256
	}
	j["layout_file"] = filepath.Join(l.dir, run+".layout.json")
	if mode != "setup" {
		j["layout_sha256"] = l.layoutSHA(run)
	}
	if mode == "verify" {
		j["verification"] = map[string]any{"query_oracle": "none", "query_results_file": nil, "k": 100, "max_candidates": 20000, "max_bytes": 4 << 20}
	}
	return j
}

func (l *live) layoutSHA(run string) string {
	h, err := hashFile(filepath.Join(l.dir, run+".layout.json"))
	if err != nil {
		l.t.Fatalf("no layout for %s: %v", run, err)
	}
	return h
}

func (l *live) layout(run string) layoutManifest {
	b, err := os.ReadFile(filepath.Join(l.dir, run+".layout.json"))
	if err != nil {
		l.t.Fatal(err)
	}
	m, err := parseManifest(b)
	if err != nil {
		l.t.Fatal(err)
	}
	return m
}

func jobPath(j map[string]any) string {
	return strings.TrimSuffix(j["output"].(string), ".report.json") + ".job.json"
}

// exec runs the executable a job names on it with exactly env, never the test's.
func (l *live) exec(j map[string]any, env ...string) (map[string]any, string, error) {
	l.t.Helper()
	binary, ok := map[any]string{l.pureSHA: l.pure, l.nativeSHA: l.native, l.rustSHA: l.rust}[j["binary_sha256"]]
	if !ok {
		l.t.Fatalf("job %v names no executable", j["binary_sha256"])
	}
	writeFile(l.t, jobPath(j), encode(l.t, j))
	ctx, cancel := context.WithTimeout(l.ctx, 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, jobPath(j))
	cmd.Env = append([]string{}, env...)
	cmd.WaitDelay = 10 * time.Second
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = io.Discard, &stderr
	err := cmd.Run()
	if _, statErr := os.Stat(j["output"].(string)); statErr != nil {
		return nil, stderr.String(), err
	}
	return readReport(l.t, j["output"].(string)), stderr.String(), err
}

// ok runs j, requires success, and requires the report to name exactly what ran: the
// executing binary, the sealed job file and the sealed job itself.
func (l *live) ok(j map[string]any, env ...string) map[string]any {
	l.t.Helper()
	r, stderr, err := l.exec(j, env...)
	if err != nil || r == nil || r["status"] != "ok" || r["error"] != nil {
		l.t.Fatalf("%s %s %s: %v stderr=%q report=%v", j["mode"], j["arm"], j["run"], err, stderr, r)
	}
	jobSHA, err := hashFile(jobPath(j))
	if err != nil {
		l.t.Fatal(err)
	}
	var sealed any
	if err := json.Unmarshal(encode(l.t, j), &sealed); err != nil {
		l.t.Fatal(err)
	}
	if r["binary_sha256"] != j["binary_sha256"] || r["job_sha256"] != jobSHA || r["arm"] != j["arm"] || r["mode"] != j["mode"] ||
		r["source_revision"] != l.revision || !reflect.DeepEqual(r["effective_job"], sealed) {
		l.t.Fatalf("%s %s report names something else than ran: %v", j["mode"], j["arm"], r)
	}
	// The report echoes the sealed client hash exactly: the pin for a native arm's job,
	// null for go_pure and corpus.
	if (j["client_library_sha256"] != nil && j["client_library_sha256"] != clientSHA256) ||
		!reflect.DeepEqual(r["client_library_sha256"], j["client_library_sha256"]) {
		l.t.Fatalf("%s %s client hash %v, sealed %v", j["mode"], j["arm"], r["client_library_sha256"], j["client_library_sha256"])
	}
	// control-pure proved its real mappings hold no libfdb_c; a native ingest proved its
	// mapped client; the build tags name the executable that ran.
	if p := receiptProblem(r); p != "" {
		l.t.Fatalf("%s %s receipt: %s: %v", j["mode"], j["arm"], p, r)
	}
	return r
}

// arm is one arm's correctness chain: Go setup, the arm's own ingest, Go verify.
func (l *live) arm(run, arm, workload, compression string) map[string]any {
	l.t.Helper()
	setup := l.ok(l.job(run+".setup", "setup", arm, run, workload, compression))
	if jsonAt(setup, "baseline", "index_entries", "STORE_META_NAME") != float64(1) || jsonAt(setup, "baseline", "record_identities") != float64(1) {
		l.t.Fatalf("setup baseline %v", setup["baseline"])
	}
	ingest := l.ok(l.job(run+".ingest", "ingest", arm, run, workload, compression))
	checkIngestShape(l.t, ingest)
	// Only a native ingest validated a mapped client; which rule proved it depends on
	// this host's filesystem, and checkIngestShape requires one of the two.
	rule, claimed := ingest["native_mapping_rule"]
	if claimed != (arm != "go_pure") {
		l.t.Fatalf("%s ingest native mapping rule %v", arm, rule)
	}
	if claimed {
		l.t.Logf("%s %s: mapping proved by %v", arm, run, rule)
	}
	if _, ok := setup["native_mapping_rule"]; ok {
		l.t.Fatalf("%s setup claims a native mapping %v", arm, setup)
	}
	n := float64(l.frames)
	if ingest["frames"] != n || ingest["received_spans"] != n || ingest["accepted_spans"] != n || ingest["ingest_resources"] == nil {
		l.t.Fatalf("%s ingest populations %v", arm, ingest)
	}
	ack := ingest["acked_span_operations"].(map[string]any)
	switch arm {
	case "rust_native":
		if ack["availability"] != "available" || ack["value"].(float64) < n {
			l.t.Fatalf("rust_native ACK %v", ack)
		}
	default:
		policy := jsonAt(ingest, "transaction_policy", "client").(string)
		if ack["availability"] != "unavailable" || (arm == "go_native") != strings.Contains(policy, "libfdb_c 7.3.77") {
			l.t.Fatalf("%s ACK %v on %q", arm, ack, policy)
		}
	}
	verify := l.ok(l.job(run+".verify", "verify", arm, run, workload, compression))
	if _, ok := verify["native_mapping_rule"]; ok {
		l.t.Fatalf("%s verify claims a native mapping %v", arm, verify)
	}
	l.verified(verify, arm)
	l.t.Logf("%s %s %s: %v frames ingested and independently verified", arm, run, compression, n)
	return verify
}

func (l *live) verified(verify map[string]any, arm string) {
	l.t.Helper()
	n := float64(l.frames)
	transforms := jsonAt(verify, "census", "validated_span_transforms").(map[string]any)
	sum := transforms["bare_union"].(float64) + transforms["clear_fallback"].(float64) + transforms["zlib_v1"].(float64)
	if verify["distinct_spans"] != n || verify["source_records_exact_match"] != n || verify["source_records_read_standard_serializer"] != n ||
		verify["scanned_distinct_spans"] != n || jsonAt(verify, "census", "matched") != true || jsonAt(verify, "census", "actual", "unexpected_kvs") != float64(0) ||
		sum != n || transforms["unresolved"] != float64(0) || verify["full_rows_equal"] != true || verify["otlp_bytes_equal"] != true ||
		verify["layout_seals_equal"] != true || jsonAt(verify, "oracle", "status") != "not_requested" {
		l.t.Fatalf("%s verify %v", arm, verify)
	}
}

// tenantPrefix is the tenant subspace a manifest's record prefix lies in.
func tenantPrefix(t *testing.T, m layoutManifest) []byte {
	t.Helper()
	records, err := hex.DecodeString(m.RecordPrefix)
	if err != nil || !bytes.HasSuffix(records, tuple.Tuple{int64(recordlayer.RecordKey)}.Pack()) {
		t.Fatalf("record prefix %q", m.RecordPrefix)
	}
	return records[:len(records)-len(tuple.Tuple{int64(recordlayer.RecordKey)}.Pack())]
}

func (l *live) read(begin, end []byte) []fdb.KeyValue {
	l.t.Helper()
	out, err := l.db.ReadTransact(func(tx fdb.ReadTransaction) (any, error) {
		return tx.GetRange(fdb.KeyRange{Begin: fdb.Key(begin), End: fdb.Key(end)}, fdb.RangeOptions{}).GetSliceWithError()
	})
	if err != nil {
		l.t.Fatal(err)
	}
	var kvs []fdb.KeyValue
	for _, kv := range out.([]fdb.KeyValue) {
		kvs = append(kvs, fdb.KeyValue{Key: bytes.Clone(kv.Key), Value: bytes.Clone(kv.Value)})
	}
	return kvs
}

func (l *live) write(fn func(fdb.WritableTransaction)) {
	l.t.Helper()
	if _, err := l.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		fn(tx)
		return nil, nil
	}); err != nil {
		l.t.Fatal(err)
	}
}

// refused requires j to fail with an error report naming a seal.
func (l *live) refused(j map[string]any) {
	l.t.Helper()
	r, stderr, err := l.exec(j)
	if err == nil || r == nil || r["status"] != "error" || !strings.Contains(fmt.Sprint(r["error"]), "seal") {
		l.t.Fatalf("%s %s on a corrupted seal: %v %q %v", j["mode"], j["arm"], err, stderr, r)
	}
}

// TestControlAgainstOwnedFDB is the composed correctness chain: the control generates
// a corpus and sets up each arm, each arm's real executable ingests on its own client,
// and an independent pure-Go verify checks every row and the whole tenant. These are
// correctness tests, not measurements.
func TestControlAgainstOwnedFDB(t *testing.T) {
	t.Parallel()
	l := newLive(t)
	l.corpus = filepath.Join(l.dir, "corpus")
	corpus := l.ok(l.job("corpus", "corpus", "go_pure", "", "", ""))
	if corpus["frames"] != float64(l.frames) {
		t.Fatalf("corpus %v", corpus)
	}
	var err error
	if l.corpusSHA, err = hashFile(l.corpus); err != nil {
		t.Fatal(err)
	}
	direct := `{"kind":"writer_direct","producers":3,"producer_batch_spans":4}`
	var executed []string
	for i, c := range []struct{ arm, workload, compression string }{
		{"go_pure", point2, "zlib-1"},
		{"go_native", point2, "zlib-1"},
		{"rust_native", point2, "zlib-1"},
		{"go_pure", direct, "none"},
		{"go_native", direct, "zlib-default"},
		{"rust_native", direct, "zlib-default"},
	} {
		l.arm(fmt.Sprintf("control_smoke_%d", i), c.arm, c.workload, c.compression)
		executed = append(executed, c.arm)
	}
	if len(executed) != 6 {
		t.Fatalf("executed arms %v", executed)
	}
	// Sequential steps on this test's goroutine: the helpers fail through l.t.
	l.alternatives(t, "control_smoke_0")
	l.sealsBeforeOpen(t)
	l.refusals(t)
	l.unsettled(t)
}

// alternatives rewrites stored spans of a verified tenant as other legal
// representations of the same unions, then as an illegal one; the census, the standard
// serializer, the full rows and the full scan accept exactly the legal ones.
func (l *live) alternatives(t *testing.T, run string) {
	m := l.layout(run)
	records, _ := hex.DecodeString(m.RecordPrefix)
	var compressed []fdb.KeyValue
	for _, kv := range l.read(records, append(bytes.Clone(records), 0xff)) {
		if len(kv.Value) > 6 && kv.Value[0] == 4 && bytes.HasSuffix(kv.Key, tuple.Tuple{int64(0)}.Pack()) {
			compressed = append(compressed, kv)
		}
	}
	if len(compressed) < 3 {
		t.Fatalf("only %d unsplit compressed span records", len(compressed))
	}
	union := func(v []byte) []byte {
		zr, err := zlib.NewReader(bytes.NewReader(v[6:]))
		if err != nil {
			t.Fatal(err)
		}
		u, err := io.ReadAll(zr)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	fallback := append([]byte{2}, union(compressed[0].Value)...)
	u1 := union(compressed[1].Value)
	other := zlibForm(len(u1), deflate(t, u1, zlib.BestCompression, 2))
	if bytes.Equal(other, compressed[1].Value) {
		t.Fatal("the alternative stream must differ from the stored one")
	}
	l.write(func(tx fdb.WritableTransaction) {
		tx.Set(compressed[0].Key, fallback)
		tx.Set(compressed[1].Key, other)
	})
	r := l.ok(l.job(run+".verify-alternative", "verify", "go_pure", run, point2, "zlib-1"))
	l.verified(r, "go_pure")
	if jsonAt(r, "census", "validated_span_transforms", "clear_fallback") != float64(1) {
		t.Fatalf("transforms %v", jsonAt(r, "census", "validated_span_transforms"))
	}
	// A legal header over a truncated stream fails the census.
	v := compressed[2].Value
	l.write(func(tx fdb.WritableTransaction) { tx.Set(compressed[2].Key, v[:len(v)-1]) })
	bad, _, err := l.exec(l.job(run+".verify-truncated", "verify", "go_pure", run, point2, "zlib-1"))
	if err == nil || bad == nil || bad["status"] != "error" || jsonAt(bad, "census", "matched") != false ||
		jsonAt(bad, "census", "validated_span_transforms", "unresolved") != float64(1) {
		t.Fatalf("a truncated stream: %v %v", err, bad)
	}
}

// sealsBeforeOpen corrupts one genuinely sealed key per fresh namespace, by tamper and
// by delete, of the tenant header and of the catalog database record a committed
// metadata open would recreate: ingest and verify both refuse, and an independent
// read afterwards shows exactly the corruption, nothing repaired.
func (l *live) sealsBeforeOpen(t *testing.T) {
	header := func(m layoutManifest) ([]byte, []byte) {
		key := append(tenantPrefix(t, m), tuple.Tuple{int64(recordlayer.StoreInfoKey)}.Pack()...)
		return key, append(bytes.Clone(key), 0)
	}
	catalogRecord := func(run string) func(layoutManifest) ([]byte, []byte) {
		return func(layoutManifest) ([]byte, []byte) {
			path, _ := tenantPaths(run)
			info := prefixRange(catalog.DefaultCatalogSubspace().Sub(int64(recordlayer.RecordKey)).Sub(catalog.DatabaseInfoRecordTypeKey, path))
			return info.Begin.FDBKey(), info.End.FDBKey()
		}
	}
	l.corruptSeal(t, "control_smoke_header_tamper", header, true)
	l.corruptSeal(t, "control_smoke_header_delete", header, false)
	l.corruptSeal(t, "control_smoke_catalog_tamper", catalogRecord("control_smoke_catalog_tamper"), true)
	l.corruptSeal(t, "control_smoke_catalog_delete", catalogRecord("control_smoke_catalog_delete"), false)
}

// corruptSeal sets up run, takes the first key of span, which must exist and be sealed,
// and tampers with its value or deletes it. The read-back must show the corruption
// before the refusals and unchanged after both of them.
func (l *live) corruptSeal(t *testing.T, run string, span func(layoutManifest) ([]byte, []byte), tamper bool) {
	l.ok(l.job(run+".setup", "setup", "go_pure", run, point2, "zlib-1"))
	m := l.layout(run)
	begin, end := span(m)
	before := l.read(begin, end)
	if len(before) == 0 || !sealed(m, before[0].Key) {
		t.Fatalf("%s: the premise is a present, sealed key", run)
	}
	key := fdb.Key(before[0].Key)
	want := slices.Clone(before[1:])
	if tamper {
		tampered := append(bytes.Clone(before[0].Value), 0)
		want = append([]fdb.KeyValue{{Key: key, Value: tampered}}, want...)
		l.write(func(tx fdb.WritableTransaction) { tx.Set(key, tampered) })
	} else {
		l.write(func(tx fdb.WritableTransaction) { tx.Clear(key) })
	}
	readsAsCorrupted := func() bool {
		got := l.read(begin, end)
		return slices.EqualFunc(got, want, func(a, b fdb.KeyValue) bool {
			return bytes.Equal(a.Key, b.Key) && bytes.Equal(a.Value, b.Value)
		})
	}
	if !readsAsCorrupted() {
		t.Fatalf("%s: the corruption was not applied", run)
	}
	l.refused(l.job(run+".ingest", "ingest", "go_pure", run, point2, "zlib-1"))
	l.refused(l.job(run+".verify", "verify", "go_pure", run, point2, "zlib-1"))
	if !readsAsCorrupted() {
		t.Fatalf("%s: a refused run changed the corrupted sealed key", run)
	}
}

func sealed(m layoutManifest, key []byte) bool {
	for _, c := range m.Checks {
		if c.Key == hex.EncodeToString(key) {
			return true
		}
	}
	return false
}

// flavorRefused requires the executable j is sealed for to refuse it as a job error:
// a failed exit naming why, and no report, diagnostic or other output created.
func (l *live) flavorRefused(j map[string]any, why string, outputs ...string) {
	l.t.Helper()
	r, stderr, err := l.exec(j)
	if err == nil || r != nil || !strings.Contains(stderr, why) {
		l.t.Fatalf("%s %s on the other executable: %v %q %v", j["mode"], j["arm"], err, stderr, r)
	}
	for _, p := range append(outputs, diagnosticPath(j["output"].(string))) {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			l.t.Fatalf("a refused %s %s created %s", j["mode"], j["arm"], p)
		}
	}
}

// refusals: inherited network options, an unmapped client copy, each executable's
// foreign jobs and a reused namespace.
func (l *live) refusals(t *testing.T) {
	// Setup and verify of a native arm never initialize the native client: an inherited
	// network option is ignored by them, and refused by the native ingest before a write.
	run, env := "control_smoke_refused", "FDB_NETWORK_OPTION_TRACE_ENABLE="
	l.ok(l.job(run+".setup", "setup", "go_native", run, point2, "zlib-1"), env)
	r, stderr, err := l.exec(l.job(run+".native", "ingest", "go_native", run, point2, "zlib-1"), env)
	if err == nil || r == nil || r["status"] != "error" || !strings.Contains(fmt.Sprint(r["error"]), envRefused) || !strings.Contains(stderr, envRefused) {
		t.Fatalf("an inherited network option: %v %q %v", err, stderr, r)
	}
	checkIngestShape(t, r)
	if r["ingest_wall_ns"] != float64(0) || r["ingest_resources"] != nil || r["frames"] != float64(0) {
		t.Fatalf("a refused ingest measured something: %v", r)
	}
	// The refused run left only the sealed baseline: a pure ingest's preflight accepts it.
	l.ok(l.job(run+".pure", "ingest", "go_pure", run, point2, "zlib-1"))
	l.verified(l.ok(l.job(run+".verify", "verify", "go_native", run, point2, "zlib-1"), env), "go_native")

	// A client copy with the pinned bytes but another inode is not the mapped client.
	copied := filepath.Join(l.dir, "copy", "libfdb_c.so")
	if err := os.MkdirAll(filepath.Dir(copied), 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(l.library)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, copied, b)
	run = "control_smoke_mapped"
	l.ok(l.job(run+".setup", "setup", "go_native", run, point2, "zlib-1"))
	j := l.job(run+".native", "ingest", "go_native", run, point2, "zlib-1")
	j["client_library"] = copied
	if r, _, err := l.exec(j); err == nil || r == nil || r["status"] != "error" || !strings.Contains(fmt.Sprint(r["error"]), "not the sealed job artifact") {
		t.Fatalf("an unmapped client copy: %v %v", err, r)
	}

	// Each executable refuses the other's jobs before reserving a report: control-native
	// a setup of its own arm, control-pure a go_native ingest sealed for it.
	run = "control_smoke_flavor"
	l.ok(l.job(run+".setup", "setup", "go_native", run, point2, "zlib-1"))
	wrong := l.job(run+".native-setup", "setup", "go_native", run+"_other", point2, "zlib-1")
	wrong["binary_sha256"] = l.nativeSHA
	l.flavorRefused(wrong, "control-native runs only go_native ingest", wrong["layout_file"].(string))
	wrong = l.job(run+".pure-ingest", "ingest", "go_native", run, point2, "zlib-1")
	wrong["binary_sha256"] = l.pureSHA
	l.flavorRefused(wrong, "control-pure never runs a native ingest")
	// The refusals left the sealed baseline: control-native ingests it as sealed.
	l.ok(l.job(run+".ingest", "ingest", "go_native", run, point2, "zlib-1"))

	// A namespace is set up once: a second setup cannot adopt the reserved one.
	run = "control_smoke_reused"
	l.ok(l.job(run+".setup", "setup", "go_native", run, point2, "zlib-1"))
	again := l.job(run+".twice", "setup", "go_native", run, point2, "zlib-1")
	again["layout_file"] = filepath.Join(l.dir, run+".second.layout.json")
	if r, _, err := l.exec(again); err == nil || r == nil || r["status"] != "error" || !strings.Contains(fmt.Sprint(r["error"]), "already exists") {
		t.Fatalf("a reused namespace: %v %v", err, r)
	}
}

// unsettled runs a real ingest in process whose Collector worker ignores cancellation:
// no report remains, only the exclusive diagnostic, and no window end was sampled.
func (l *live) unsettled(t *testing.T) {
	name := "control_smoke_unsettled"
	workload := `{"kind":"collector_queue","producers":1,"producer_batch_spans":1,"consumers":1,"queue_capacity_spans":1,"batch_min_spans":1,"batch_max_spans":1,"flush_ms":1,"block_on_overflow":true,"export_timeout_ms":3600000,"retry_enabled":true,"retry_initial_ms":500,"retry_max_ms":10000,"retry_elapsed_ms":300000}`
	l.ok(l.job(name+".setup", "setup", "go_pure", name, workload, "zlib-1"))
	j := l.job(name+".ingest", "ingest", "go_pure", name, workload, "zlib-1")
	j["timeout_seconds"] = 3
	writeFile(t, jobPath(j), encode(t, j))
	// Registered before any assertion: once released, the Collector must join and shut
	// its sink down, and no late report may appear.
	release, done := heldExports(t, func() {
		if _, err := os.Lstat(j["output"].(string)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a report appeared after the unsettled ingest was released: %v", err)
		}
	})
	usage := 0
	// This test process is control-pure's flavor and seals control-pure's hash.
	rt := productionRuntime()
	rt.executable = func() (string, error) { return l.pure, nil }
	rt.openNative = func(context.Context, string, string) (fdb.BackendDatabase, string, error) {
		return nil, "", errors.New("a pure ingest opened the native client")
	}
	rt.usage = func() (syscall.Rusage, error) {
		usage++
		return selfUsage()
	}
	rt.joinBound = 300 * time.Millisecond
	rt.sink = func(context.Context, otel.ExporterConfig) (collector.Sink, error) {
		return &scriptedSink{done: done, export: func(context.Context, int, []*tracepb.ResourceSpans) error {
			<-release
			return nil
		}}, nil
	}
	err := run(rt, jobPath(j))
	var u *unsettledError
	if !errors.As(err, &u) || usage != 1 {
		t.Fatalf("an unsettled ingest: %v, %d resource samples", err, usage)
	}
	if _, err := os.Lstat(j["output"].(string)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an unsettled ingest left a report: %v", err)
	}
	info, err := os.Stat(diagnosticPath(j["output"].(string)))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("diagnostic %v %v", info, err)
	}
	d := readReport(t, diagnosticPath(j["output"].(string)))
	if d["diagnostic_kind"] != unsettledKind || d["comparison_eligible"] != false || d["admitted_spans"] != float64(1) || d["schema"] != nil {
		t.Fatalf("diagnostic %v", d)
	}
}
