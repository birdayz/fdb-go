package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
)

const frozenSchemaSHA256 = "1581c3dd4891579949d52cb5ac70717d54f5488784e17a2c993c5769aaacb18b"

// point2 is the sealed pilot configuration's workload, verbatim.
const point2 = `{"kind":"collector_queue","producers":8,"producer_batch_spans":1,"consumers":8,"queue_capacity_spans":10000,"batch_min_spans":64,"batch_max_spans":256,"flush_ms":200,"block_on_overflow":true,"export_timeout_ms":120000,"retry_enabled":true,"retry_initial_ms":500,"retry_max_ms":10000,"retry_elapsed_ms":300000}`

const point1 = `{"kind":"collector_queue","producers":1,"producer_batch_spans":1,"consumers":1,"queue_capacity_spans":10000,"batch_min_spans":64,"batch_max_spans":64,"flush_ms":200,"block_on_overflow":true,"export_timeout_ms":120000,"retry_enabled":true,"retry_initial_ms":500,"retry_max_ms":10000,"retry_elapsed_ms":300000}`

const direct = `{"kind":"writer_direct","producers":2,"producer_batch_spans":4}`

func schemaDoc(t *testing.T) map[string]any {
	t.Helper()
	b, err := os.ReadFile("testdata/paired-job-v1.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func jsonAt(doc any, keys ...any) any {
	for _, k := range keys {
		switch k := k.(type) {
		case string:
			doc = doc.(map[string]any)[k]
		case int:
			doc = doc.([]any)[k]
		}
	}
	return doc
}

func keySet(v any) []string {
	var out []string
	for k := range v.(map[string]any) {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func sorted(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}

// The control's field lists are the frozen schema's own: a schema change fails here.
func TestFrozenSchemaPinned(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("testdata/paired-job-v1.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != frozenSchemaSHA256 {
		t.Fatalf("schema fixture drifted from the frozen %s", frozenSchemaSHA256)
	}
	doc := schemaDoc(t)
	for i, mode := range []string{"corpus", "setup", "ingest", "verify"} {
		branch := jsonAt(doc, "oneOf", i)
		if jsonAt(branch, "properties", "mode", "const") != mode {
			t.Fatalf("oneOf[%d] is not %s", i, mode)
		}
		if got, want := keySet(jsonAt(branch, "properties")), sorted(modeKeys(mode)); !slices.Equal(got, want) {
			t.Fatalf("%s fields %v, want %v", mode, want, got)
		}
		var required []string
		for _, r := range jsonAt(branch, "required").([]any) {
			required = append(required, r.(string))
		}
		if !slices.Equal(sorted(required), sorted(modeKeys(mode))) {
			t.Fatalf("%s requires %v", mode, required)
		}
	}
	props := jsonAt(doc, "properties")
	for _, c := range []struct {
		got  any
		want []string
	}{
		{jsonAt(props, "corpus_config", "properties"), corpusConfigKeys},
		{jsonAt(props, "workload", "oneOf", 0, "properties"), directKeys},
		{jsonAt(props, "workload", "oneOf", 1, "properties"), queueKeys},
		{jsonAt(props, "verification", "properties"), verificationKeys},
	} {
		if !slices.Equal(keySet(c.got), sorted(c.want)) {
			t.Fatalf("fields %v, want %v", keySet(c.got), sorted(c.want))
		}
	}
}

// fixture is a directory of real job inputs and a job builder over them.
type fixture struct {
	dir, exe, cluster, corpus, layout, library string
	exeSHA, clusterSHA, corpusSHA, layoutSHA   string
}

func writeFile(t *testing.T, p string, b []byte) string {
	t.Helper()
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{dir: t.TempDir()}
	f.exe, f.cluster, f.corpus, f.layout = filepath.Join(f.dir, "control"), filepath.Join(f.dir, "fdb.cluster"), filepath.Join(f.dir, "corpus"), filepath.Join(f.dir, "layout.json")
	f.exeSHA = writeFile(t, f.exe, []byte("stand-in executable"))
	f.clusterSHA = writeFile(t, f.cluster, []byte("test:test@127.0.0.1:1\n"))
	f.corpusSHA = writeFile(t, f.corpus, []byte("frames"))
	f.layoutSHA = writeFile(t, f.layout, []byte(`{"version":1,"descriptor_sha256":"","record_prefix":"","index_prefixes":{},"checks":[],"empty_ranges":[],"messages":"none"}`))
	// The pinned client's hash is only compared as a string outside a native ingest.
	f.library = filepath.Join(f.dir, "libfdb_c.so")
	writeFile(t, f.library, []byte("not the client"))
	return f
}

// job is a valid job of mode for arm and workload (verbatim JSON) over f's files.
func (f *fixture) job(mode, arm, workload string) map[string]any {
	j := map[string]any{
		"schema": jobSchema, "mode": mode, "arm": arm, "binary_sha256": f.exeSHA,
		"source_revision": strings.Repeat("a", 40), "source_manifest_sha256": strings.Repeat("b", 64),
		"timeout_seconds": 60, "output": filepath.Join(f.dir, "report.json"),
		"corpus_config":   map[string]any{"generator": "otel_fixture_v2", "seed": 283, "count": 4, "tenants": 1, "attributes": 8, "match_every": 2},
		"expected_frames": 4, "row_budget": 4, "corpus_file": f.corpus,
	}
	if mode == "corpus" {
		j["corpus_file"] = filepath.Join(f.dir, "new-corpus")
		return j
	}
	var w any
	if err := json.Unmarshal([]byte(workload), &w); err != nil {
		panic(err)
	}
	j["run"], j["cluster_file"], j["cluster_sha256"], j["corpus_sha256"] = "control_test_run", f.cluster, f.clusterSHA, f.corpusSHA
	j["workload"], j["writer_batch_size"], j["writer_workers"], j["record_compression"] = w, 64, 1, "zlib-1"
	j["enable_message_deltas"], j["enable_message_full_text"], j["bulk_index_conflicts"] = false, false, false
	j["client_backend"], j["client_library"], j["client_library_sha256"] = "pure_go", nil, nil
	if arm != "go_pure" {
		j["client_backend"], j["client_library"], j["client_library_sha256"] = "native_c", f.library, clientSHA256
	}
	j["layout_file"] = f.layout
	switch mode {
	case "setup":
		j["layout_file"] = filepath.Join(f.dir, "new-layout.json")
	case "ingest":
		j["layout_sha256"] = f.layoutSHA
	case "verify":
		j["layout_sha256"] = f.layoutSHA
		j["verification"] = map[string]any{"query_oracle": "none", "query_results_file": nil, "k": 100, "max_candidates": 20000, "max_bytes": 4 << 20}
	}
	return j
}

func encode(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// parsed is parseJob then validate, as run applies them.
func parsed(data []byte, jobPath string) (*job, error) {
	j, err := parseJob(data)
	if err != nil {
		return nil, err
	}
	return j, validate(j, jobPath)
}

func clone(m map[string]any) map[string]any {
	var out map[string]any
	b, _ := json.Marshal(m)
	_ = json.Unmarshal(b, &out)
	return out
}

func TestJobEnvelopeIsStrict(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	jobPath := filepath.Join(f.dir, "job.json")
	valid := map[string]map[string]any{
		"corpus":           f.job("corpus", "go_pure", ""),
		"setup native":     f.job("setup", "go_native", point2),
		"setup rust":       f.job("setup", "rust_native", direct),
		"ingest point1":    f.job("ingest", "go_pure", point1),
		"ingest point2":    f.job("ingest", "go_native", point2),
		"ingest direct":    f.job("ingest", "go_pure", direct),
		"verify rust":      f.job("verify", "rust_native", point2),
		"verify pure":      f.job("verify", "go_pure", direct),
		"corpus rust arm":  f.job("corpus", "rust_native", ""),
		"verify go native": f.job("verify", "go_native", point1),
	}
	for name, j := range valid {
		if _, err := parsed(encode(t, j), jobPath); err != nil {
			t.Fatalf("valid %s job refused: %v", name, err)
		}
	}
	base := f.job("ingest", "go_pure", point2)
	type mutation struct {
		name string
		edit func(map[string]any)
	}
	set := func(k string, v any) func(map[string]any) { return func(j map[string]any) { j[k] = v } }
	nested := func(obj, k string, v any) func(map[string]any) {
		return func(j map[string]any) { j[obj].(map[string]any)[k] = v }
	}
	drop := func(obj, k string) func(map[string]any) {
		return func(j map[string]any) { delete(j[obj].(map[string]any), k) }
	}
	cases := []mutation{
		{"unknown field", set("extra", 1)},
		{"unknown corpus_config field", nested("corpus_config", "extra", 1)},
		{"unknown workload field", nested("workload", "extra", 1)},
		{"direct workload with queue fields", func(j map[string]any) { j["workload"].(map[string]any)["kind"] = "writer_direct" }},
		{"wrong schema", set("schema", "otel_paired_job_v0")},
		{"wrong mode", set("mode", "drain")},
		{"wrong arm", set("arm", "go")},
		{"rust_native ingest", func(j map[string]any) {
			j["arm"], j["client_backend"], j["client_library"], j["client_library_sha256"] = "rust_native", "native_c", f.library, clientSHA256
		}},
		{"pure arm with native backend", set("client_backend", "native_c")},
		{"pure arm with a library", set("client_library", f.library)},
		{"bad binary hash", set("binary_sha256", strings.Repeat("A", 64))},
		{"short revision", set("source_revision", "abc")},
		{"stale layout hash form", set("layout_sha256", "x")},
		{"timeout 0", set("timeout_seconds", 0)},
		{"timeout fraction", set("timeout_seconds", 1.5)},
		{"generator", nested("corpus_config", "generator", "otel_fixture_v1")},
		{"seed negative", nested("corpus_config", "seed", -1)},
		{"tenants", nested("corpus_config", "tenants", 2)},
		{"attributes", nested("corpus_config", "attributes", 4)},
		{"count 1", nested("corpus_config", "count", 1)},
		{"expected frames differ", set("expected_frames", 3)},
		{"row budget short", set("row_budget", 3)},
		{"run name", set("run", "Upper_Case_Run")},
		{"writer batch 65", set("writer_batch_size", 65)},
		{"writer workers 9", set("writer_workers", 9)},
		{"compression", set("record_compression", "brotli")},
		{"message deltas", set("enable_message_deltas", true)},
		{"message full text", set("enable_message_full_text", true)},
		{"bulk conflicts", set("bulk_index_conflicts", true)},
		{"bool as string", set("bulk_index_conflicts", "false")},
		{"producers 33", nested("workload", "producers", 33)},
		{"consumers 0", nested("workload", "consumers", 0)},
		{"min above capacity", nested("workload", "batch_min_spans", 10001)},
		{"max below min", nested("workload", "batch_max_spans", 63)},
		{"producer batch above capacity", func(j map[string]any) {
			w := j["workload"].(map[string]any)
			w["queue_capacity_spans"], w["batch_min_spans"], w["batch_max_spans"], w["producer_batch_spans"] = 2, 1, 2, 3
		}},
		{"retry enabled at 0ms", nested("workload", "retry_initial_ms", 0)},
		{"retry max below initial", nested("workload", "retry_max_ms", 499)},
		{"flush 0", nested("workload", "flush_ms", 0)},
		{"relative path", set("corpus_file", "corpus")},
		{"unclean path", set("corpus_file", f.dir+"/./corpus")},
		{"aliased string paths", set("output", f.corpus)},
		{"job path as output", set("output", jobPath)},
	}
	for _, k := range modeKeys("ingest") {
		cases = append(cases, mutation{"missing " + k, func(j map[string]any) { delete(j, k) }})
		if k != "client_library" && k != "client_library_sha256" {
			cases = append(cases, mutation{"null " + k, set(k, nil)})
		}
	}
	for _, k := range corpusConfigKeys {
		cases = append(cases, mutation{"missing corpus_config." + k, drop("corpus_config", k)}, mutation{"null corpus_config." + k, nested("corpus_config", k, nil)})
	}
	for _, k := range queueKeys {
		cases = append(cases, mutation{"missing workload." + k, drop("workload", k)}, mutation{"null workload." + k, nested("workload", k, nil)})
	}
	for _, c := range cases {
		j := clone(base)
		c.edit(j)
		if _, err := parsed(encode(t, j), jobPath); err == nil {
			t.Errorf("%s was accepted", c.name)
		}
	}
	// The other modes' own fields.
	for name, edit := range map[string]func(map[string]any){
		"setup with layout_sha256":   set("layout_sha256", f.layoutSHA),
		"ingest with verification":   set("verification", map[string]any{}),
		"verify oracle not wired":    nested("verification", "query_oracle", "source_1600_v1"),
		"verify results with none":   nested("verification", "query_results_file", filepath.Join(f.dir, "q.ndjson")),
		"verify k 0":                 nested("verification", "k", 0),
		"verify unknown field":       nested("verification", "extra", 0),
		"verify missing max_bytes":   drop("verification", "max_bytes"),
		"corpus with a cluster file": set("cluster_file", f.cluster),
		"rust workers 2":             set("writer_workers", 2),
		"native without library":     set("client_library", nil),
		"native other pin":           set("client_library_sha256", strings.Repeat("0", 64)),
	} {
		mode := map[string]string{"setup with layout_sha256": "setup", "ingest with verification": "ingest", "corpus with a cluster file": "corpus"}[name]
		arm := "go_native"
		if mode == "" {
			mode = "verify"
		}
		if name == "rust workers 2" {
			arm = "rust_native"
		}
		j := f.job(mode, arm, point2)
		edit(j)
		if _, err := parsed(encode(t, j), jobPath); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	// Duplicates and extra values are refused before any field is read.
	text := string(encode(t, base))
	for name, bad := range map[string]string{
		"duplicate top-level":   strings.Replace(text, `{`, `{"mode":"ingest",`, 1),
		"duplicate nested":      strings.Replace(text, `"corpus_config":{`, `"corpus_config":{"seed":1,`, 1),
		"duplicate in queue":    strings.Replace(text, `"workload":{`, `"workload":{"flush_ms":200,`, 1),
		"two JSON values":       text + "{}",
		"trailing bracket":      text + "]",
		"not an object":         `[` + text + `]`,
		"case-folded duplicate": strings.Replace(text, `{`, `{"MODE":"ingest",`, 1),
	} {
		if _, err := parsed([]byte(bad), jobPath); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// fakeRuntime opens nothing real: it records which client a job asked for, and how
// often the mappings and resource usage were read.
type fakeRuntime struct {
	pure, native, maps, usage int
}

var errNoCluster = errors.New("fake runtime: no cluster")

// The two compiled flavors, as the fakes present them.
var (
	pureFlavor   = flavor{tags: []string{}}
	nativeFlavor = flavor{native: true, tags: []string{"libfdbc_backend"}}
)

// cleanMaps is a real-format mapping table of a pure process: no libfdb_c anywhere.
const cleanMaps = "00400000-00c00000 r-xp 00000000 08:01 1234 /opt/control-pure\n" +
	"00c00000-00d00000 rw-p 00800000 08:01 1234 /opt/control-pure\n" +
	"01000000-01200000 rw-p 00000000 00:00 0 [heap]\n" +
	"7f0000000000-7f0000200000 r-xp 00000000 08:01 77 /usr/lib/x86_64-linux-gnu/libc.so.6\n" +
	"7ffc00000000-7ffc00021000 rw-p 00000000 00:00 0 [stack]\n" +
	"ffffffffff600000-ffffffffff601000 --xp 00000000 00:00 0 [vsyscall]\n"

// runtime is control-pure over the fakes; nativeRuntime is control-native.
func (f *fixture) runtime(rec *fakeRuntime) runtime {
	return runtime{
		flavor: pureFlavor,
		maps: func() ([]byte, error) {
			rec.maps++
			return []byte(cleanMaps), nil
		},
		executable: func() (string, error) { return f.exe, nil },
		openPure: func(context.Context, string) (fdb.BackendDatabase, error) {
			rec.pure++
			return nil, errNoCluster
		},
		openNative: func(context.Context, string, string) (fdb.BackendDatabase, string, error) {
			rec.native++
			return nil, "", errNoCluster
		},
		usage: func() (syscall.Rusage, error) {
			rec.usage++
			return selfUsage()
		},
		joinBound: time.Second,
	}
}

func (f *fixture) nativeRuntime(rec *fakeRuntime) runtime {
	rt := f.runtime(rec)
	rt.flavor = nativeFlavor
	return rt
}

func readReport(t *testing.T, p string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var r map[string]any
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("report %s: %v", b, err)
	}
	return r
}

func runJob(t *testing.T, f *fixture, rt runtime, j map[string]any) (map[string]any, error) {
	t.Helper()
	jobPath := filepath.Join(t.TempDir(), "job.json")
	writeFile(t, jobPath, encode(t, j))
	err := run(rt, jobPath)
	if _, statErr := os.Stat(j["output"].(string)); statErr != nil {
		return nil, err
	}
	return readReport(t, j["output"].(string)), err
}

// Setup and verify of every arm use only the pure-Go client; only go_native ingest
// asks for the native one.
func TestDispatchSetupAndVerifyArePureGo(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		mode, arm, workload string
		pure, native        int
	}{
		{"setup", "go_pure", point2, 1, 0},
		{"setup", "go_native", point2, 1, 0},
		{"setup", "rust_native", direct, 1, 0},
		{"verify", "go_pure", point2, 1, 0},
		{"verify", "go_native", point2, 1, 0},
		{"verify", "rust_native", direct, 1, 0},
		{"ingest", "go_pure", point2, 1, 0},
		{"ingest", "go_native", point2, 0, 1},
		{"ingest", "go_native", direct, 0, 1},
	} {
		f := newFixture(t)
		rec := &fakeRuntime{}
		j := f.job(c.mode, c.arm, c.workload)
		rt, tags, reads := f.runtime(rec), []any{}, 1
		if c.native > 0 {
			// A native ingest hashes the artifact before its report: give it the real pin.
			j["client_library"] = pinnedLibrary(t)
			rt, tags, reads = f.nativeRuntime(rec), []any{"libfdbc_backend"}, 0
		}
		r, err := runJob(t, f, rt, j)
		if !errors.Is(err, errNoCluster) || r == nil {
			t.Fatalf("%s %s: %v", c.mode, c.arm, err)
		}
		if rec.pure != c.pure || rec.native != c.native || rec.maps != reads {
			t.Fatalf("%s %s opened pure %d, native %d times, read maps %d; want %d, %d, %d", c.mode, c.arm, rec.pure, rec.native, rec.maps, c.pure, c.native, reads)
		}
		if r["status"] != "error" || r["mode"] != c.mode || r["schema"] != reportSchema || !reflect.DeepEqual(r["build_tags"], tags) {
			t.Fatalf("%s %s report %v", c.mode, c.arm, r)
		}
		// control-pure checked its mappings; control-native never got to prove its client.
		if mapped, ok := r["native_library_mapped"]; (c.native == 0) != ok || (ok && mapped != false) {
			t.Fatalf("%s %s native_library_mapped %v", c.mode, c.arm, mapped)
		}
		if _, ok := r["native_mapping_rule"]; ok {
			t.Fatalf("%s %s claims a native mapping", c.mode, c.arm)
		}
	}
}

// Each executable runs exactly its jobs: control-native only go_native ingest,
// control-pure everything else, including setup and verify of the native arms.
func TestFlavorAdmits(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	pairs := 0
	for _, mode := range []string{"corpus", "setup", "ingest", "verify"} {
		for _, arm := range []string{"go_pure", "go_native", "rust_native"} {
			if mode == "ingest" && arm == "rust_native" {
				continue // refused by validate: the Rust executable runs it
			}
			pairs++
			j := parseTestJob(t, f.job(mode, arm, point2))
			nativeIngest := mode == "ingest" && arm == "go_native"
			if err := pureFlavor.admits(j); (err == nil) == nativeIngest {
				t.Errorf("control-pure %s %s: %v", mode, arm, err)
			}
			if err := nativeFlavor.admits(j); (err == nil) != nativeIngest {
				t.Errorf("control-native %s %s: %v", mode, arm, err)
			}
		}
	}
	if pairs != 11 {
		t.Fatalf("%d mode and arm pairs", pairs)
	}
}

// A job for the other executable is refused like any job error: before the report or
// diagnostic is reserved, the corpus is read or written, the mappings are read, a
// client opens or a resource sample is taken.
func TestFlavorRefusesBeforeTheReport(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		mode, arm string
		native    bool
	}{
		{"corpus", "go_pure", true},
		{"setup", "go_native", true},
		{"verify", "rust_native", true},
		{"ingest", "go_pure", true},
		{"ingest", "go_native", false},
	} {
		f := newFixture(t)
		rec := &fakeRuntime{}
		rt := f.runtime(rec)
		if c.native {
			rt = f.nativeRuntime(rec)
		}
		j := f.job(c.mode, c.arm, point2)
		if c.mode == "ingest" && c.arm == "go_native" {
			j["client_library"] = pinnedLibrary(t)
		}
		_, err := runJob(t, f, rt, j)
		var ce *configError
		if !errors.As(err, &ce) || !strings.Contains(err.Error(), "control-") {
			t.Fatalf("%s %s on the other executable: %v", c.mode, c.arm, err)
		}
		output := j["output"].(string)
		fresh := []string{output, diagnosticPath(output)}
		if c.mode == "corpus" {
			fresh = append(fresh, j["corpus_file"].(string))
		}
		if c.mode == "setup" {
			fresh = append(fresh, j["layout_file"].(string))
		}
		for _, p := range fresh {
			if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s %s created %s", c.mode, c.arm, p)
			}
		}
		if *rec != (fakeRuntime{}) {
			t.Fatalf("%s %s had side effects %+v", c.mode, c.arm, *rec)
		}
	}
}

// control-pure refuses to run with a libfdb_c mapped, or without a readable, well-formed
// mapping table: the report is its error, claims nothing about a mapped client, and
// nothing was read, written, opened or measured.
func TestPureMappingFailureIsTheReportError(t *testing.T) {
	t.Parallel()
	reads := map[string]func() ([]byte, error){
		"a mapped client": func() ([]byte, error) {
			return []byte(cleanMaps + mapping("08:01", "99", "/usr/lib/libfdb_c.so") + "\n"), nil
		},
		"unreadable":     func() ([]byte, error) { return nil, errors.New("permission denied") },
		"malformed":      func() ([]byte, error) { return []byte("not a mapping\n"), nil },
		"an empty table": func() ([]byte, error) { return nil, nil },
		"a deleted client": func() ([]byte, error) {
			return []byte(mapping("08:01", "99", "/opt/libfdb_c.so (deleted)") + "\n"), nil
		},
		"a versioned client": func() ([]byte, error) { return []byte(mapping("08:01", "99", "/tmp/libfdb_c_7.3.77.so") + "\n"), nil },
	}
	if len(reads) != 6 {
		t.Fatalf("%d mapping failures", len(reads))
	}
	runs := 0
	for name, read := range reads {
		for _, mode := range []string{"corpus", "setup", "ingest", "verify"} {
			runs++
			f := newFixture(t)
			rec := &fakeRuntime{}
			rt := f.runtime(rec)
			rt.maps = read
			j := f.job(mode, "go_native", point2)
			if mode == "ingest" {
				j = f.job(mode, "go_pure", point2)
			}
			r, err := runJob(t, f, rt, j)
			if err == nil || r == nil || r["status"] != "error" || !reflect.DeepEqual(r["build_tags"], []any{}) {
				t.Fatalf("%s, %s: %v %v", name, mode, err, r)
			}
			if _, ok := r["native_library_mapped"]; ok {
				t.Fatalf("%s, %s: a failed check claimed %v", name, mode, r["native_library_mapped"])
			}
			if *rec != (fakeRuntime{}) {
				t.Fatalf("%s, %s had side effects %+v", name, mode, *rec)
			}
			if mode == "corpus" {
				if _, err := os.Lstat(j["corpus_file"].(string)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("%s: a corpus was written", name)
				}
			}
			if mode == "ingest" {
				checkIngestShape(t, r)
				if r["ingest_resources"] != nil || r["ingest_wall_ns"] != float64(0) || r["frames"] != float64(0) {
					t.Fatalf("%s: a refused ingest measured %v", name, r)
				}
			}
		}
	}
	if runs != 24 {
		t.Fatalf("%d refused runs, want 6 failures in 4 modes", runs)
	}
}

// The mapping table check accepts a pure process's table and refuses any libfdb_c,
// live, deleted, renamed or versioned, and any table it cannot parse.
func TestCheckNoNativeClient(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		maps string
		ok   bool
	}{
		"a pure process":         {cleanMaps, true},
		"no trailing newline":    {strings.TrimSuffix(cleanMaps, "\n"), true},
		"a client path in a dir": {cleanMaps + mapping("08:01", "5", "/opt/fdb/libfdb_c.so"), false},
		"a mapped client":        {cleanMaps + mapping("08:01", "99", "/usr/lib/libfdb_c.so"), false},
		"a deleted client":       {cleanMaps + mapping("08:01", "99", "/usr/lib/libfdb_c.so (deleted)"), false},
		"a renamed client":       {cleanMaps + mapping("08:01", "99", "/tmp/libfdb_c.so.old"), false},
		"a versioned client":     {cleanMaps + mapping("08:01", "99", "/tmp/libfdb_c_7.3.77.so"), false},
		"a spaced client path":   {cleanMaps + mapping("08:01", "99", "/tmp/a b/libfdb_c.so"), false},
		"a client on btrfs":      {cleanMaps + mapping("00:1d", "99", "/home/x/libfdb_c.so"), false},
		"empty":                  {"", false},
		"a blank line":           {cleanMaps + "\n", false},
		"too few fields":         {cleanMaps + "7f00-7f01 r-xp 00000000 08:01\n", false},
		"a bad address":          {cleanMaps + "7f00 r-xp 00000000 08:01 5 /x\n", false},
		"bad permissions":        {cleanMaps + "7f00-7f01 rwx 00000000 08:01 5 /x\n", false},
		"a bad offset":           {cleanMaps + "7f00-7f01 r-xp 0000000g 08:01 5 /x\n", false},
		"a bad device":           {cleanMaps + "7f00-7f01 r-xp 00000000 0801 5 /x\n", false},
		"a bad inode":            {cleanMaps + "7f00-7f01 r-xp 00000000 08:01 5a /x\n", false},
		"unrelated libfdb text":  {cleanMaps + mapping("08:01", "5", "/usr/lib/libfdb.so"), true},
		"a one-byte range":       {cleanMaps + "7f00-7f01 r-xp 00000000 08:01 5 /x\n", true},
		"a zero-width range":     {cleanMaps + "7f00-7f00 r-xp 00000000 08:01 5 /x\n", false},
		"a reversed range":       {cleanMaps + "7f01-7f00 r-xp 00000000 08:01 5 /x\n", false},
		"a 32-bit device half":   {cleanMaps + "7f00-7f01 r-xp 00000000 ffffffff:01 5 /x\n", true},
		"a 33-bit device half":   {cleanMaps + "7f00-7f01 r-xp 00000000 100000000:01 5 /x\n", false},
	}
	accepted := 0
	for _, c := range cases {
		if c.ok {
			accepted++
		}
	}
	if len(cases) != 23 || accepted != 5 {
		t.Fatalf("%d cases, %d accepted", len(cases), accepted)
	}
	for name, c := range cases {
		if err := checkNoNativeClient(c.maps); (err == nil) != c.ok {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// This test binary is control-pure's flavor, so its own real mapping table passes.
func TestRealPureProcessMaps(t *testing.T) {
	t.Parallel()
	table, err := productionRuntime().maps()
	if err != nil {
		t.Fatal(err)
	}
	if err := checkNoNativeClient(string(table)); err != nil {
		t.Fatalf("the pure test process: %v", err)
	}
}

// Seal, path and output refusals happen before a report is reserved.
func TestSealsAndPathsRefuseBeforeTheReport(t *testing.T) {
	t.Parallel()
	zeros := strings.Repeat("0", 64)
	for name, edit := range map[string]func(*testing.T, *fixture, map[string]any){
		"binary hash":  func(_ *testing.T, _ *fixture, j map[string]any) { j["binary_sha256"] = zeros },
		"cluster hash": func(_ *testing.T, _ *fixture, j map[string]any) { j["cluster_sha256"] = zeros },
		"corpus hash":  func(_ *testing.T, _ *fixture, j map[string]any) { j["corpus_sha256"] = zeros },
		"layout hash":  func(_ *testing.T, _ *fixture, j map[string]any) { j["layout_sha256"] = zeros },
		"reused output": func(t *testing.T, _ *fixture, j map[string]any) {
			writeFile(t, j["output"].(string), []byte("old"))
		},
		"missing corpus": func(_ *testing.T, f *fixture, j map[string]any) { j["corpus_file"] = filepath.Join(f.dir, "absent") },
		"symlinked alias": func(t *testing.T, f *fixture, j map[string]any) {
			alias := filepath.Join(f.dir, "alias")
			if err := os.Symlink(f.corpus, alias); err != nil {
				t.Fatal(err)
			}
			j["cluster_file"] = alias
		},
		"two outputs alias through a symlinked directory": func(t *testing.T, f *fixture, j map[string]any) {
			link := filepath.Join(f.dir, "linkdir")
			if err := os.Symlink(f.dir, link); err != nil {
				t.Fatal(err)
			}
			j["mode"], j["layout_file"], j["output"] = "setup", filepath.Join(f.dir, "same.json"), filepath.Join(link, "same.json")
			delete(j, "layout_sha256")
		},
		"preexisting diagnostic": func(t *testing.T, _ *fixture, j map[string]any) {
			writeFile(t, diagnosticPath(j["output"].(string)), []byte("old diagnostic"))
		},
		"diagnostic hardlinked to an input": func(t *testing.T, f *fixture, j map[string]any) {
			if err := os.Link(f.cluster, diagnosticPath(j["output"].(string))); err != nil {
				t.Fatal(err)
			}
		},
		"diagnostic aliasing an input through a symlinked directory": func(t *testing.T, f *fixture, j map[string]any) {
			other := t.TempDir()
			link := filepath.Join(f.dir, "otherdir")
			if err := os.Symlink(other, link); err != nil {
				t.Fatal(err)
			}
			j["output"] = filepath.Join(link, "r.json")
			j["corpus_file"] = filepath.Join(other, "r.json.unsettled-diagnostic.json")
			j["corpus_sha256"] = writeFile(t, j["corpus_file"].(string), []byte("frames"))
		},
		"bad layout manifest": func(t *testing.T, f *fixture, j map[string]any) {
			j["layout_sha256"] = writeFile(t, f.layout, []byte(`{"version":1,"unknown":true}`))
		},
	} {
		f := newFixture(t)
		j := f.job("ingest", "go_pure", point2)
		edit(t, f, j)
		rec := &fakeRuntime{}
		output := j["output"].(string)
		before, _ := os.ReadFile(output)
		diagBefore, _ := os.ReadFile(diagnosticPath(output))
		jobPath := filepath.Join(t.TempDir(), "job.json")
		writeFile(t, jobPath, encode(t, j))
		err := run(f.runtime(rec), jobPath)
		var ce *configError
		if !errors.As(err, &ce) {
			t.Fatalf("%s: %v", name, err)
		}
		if after, _ := os.ReadFile(output); string(after) != string(before) {
			t.Fatalf("%s wrote a report", name)
		}
		if after, _ := os.ReadFile(diagnosticPath(output)); string(after) != string(diagBefore) {
			t.Fatalf("%s changed a diagnostic", name)
		}
		if rec.pure+rec.native != 0 {
			t.Fatalf("%s opened a client", name)
		}
	}
}

// A native ingest checks the artifact itself before any report.
func TestNativeIngestNeedsThePinnedArtifact(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	rec := &fakeRuntime{}
	_, err := runJob(t, f, f.nativeRuntime(rec), f.job("ingest", "go_native", point2))
	var ce *configError
	if !errors.As(err, &ce) || !strings.Contains(err.Error(), "pinned libfdb_c") || rec.native != 0 {
		t.Fatalf("an unpinned client artifact: %v", err)
	}
}

// An unsettled ingest leaves no report: only its own empty reservation is removed, and
// the diagnostic is new, owner-only, synced and never comparison-eligible.
func TestUnsettledCompletion(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	unsettled := func() *unsettledError {
		return &unsettledError{frames: 3, received: 3, admitted: 2, first: errors.New("first failure")}
	}
	j := &job{Mode: "ingest", Arm: "go_native", BinarySHA256: strings.Repeat("e", 64), Output: filepath.Join(dir, "report.json")}
	out, err := newOutput(j.Output)
	if err != nil {
		t.Fatal(err)
	}
	err = completeUnsettled(out, j, strings.Repeat("d", 64), unsettled())
	var u *unsettledError
	if !errors.As(err, &u) {
		t.Fatalf("completion: %v", err)
	}
	if _, err := os.Lstat(j.Output); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the reserved report remains: %v", err)
	}
	info, err := os.Stat(diagnosticPath(j.Output))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("diagnostic %v %v", info, err)
	}
	d := readReport(t, diagnosticPath(j.Output))
	if d["diagnostic_kind"] != unsettledKind || d["comparison_eligible"] != false || d["frames"] != float64(3) || d["admitted_spans"] != float64(2) ||
		d["first_error"] != "first failure" || d["job_sha256"] != strings.Repeat("d", 64) {
		t.Fatalf("diagnostic %v", d)
	}
	for _, k := range []string{"schema", "status", "accepted_spans", "export_attempts", "ingest_wall_ns", "ingest_resources"} {
		if _, ok := d[k]; ok {
			t.Fatalf("the diagnostic carries the report field %s", k)
		}
	}
	// A file another writer put at the output path is not removed.
	theirs := &job{Mode: "ingest", Arm: "go_pure", Output: filepath.Join(dir, "theirs.json")}
	out, err = newOutput(theirs.Output)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(theirs.Output); err != nil {
		t.Fatal(err)
	}
	writeFile(t, theirs.Output, []byte("someone else's"))
	if err := completeUnsettled(out, theirs, "", unsettled()); !errors.As(err, &u) {
		t.Fatalf("completion: %v", err)
	}
	if b, _ := os.ReadFile(theirs.Output); string(b) != "someone else's" {
		t.Fatal("another writer's output was removed")
	}
	// A diagnostic that appeared meanwhile is never overwritten; the run still fails.
	raced := &job{Mode: "ingest", Arm: "go_pure", Output: filepath.Join(dir, "raced.json")}
	out, err = newOutput(raced.Output)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, diagnosticPath(raced.Output), []byte("old diagnostic"))
	if err := completeUnsettled(out, raced, "", unsettled()); !errors.As(err, &u) {
		t.Fatalf("completion: %v", err)
	}
	if b, _ := os.ReadFile(diagnosticPath(raced.Output)); string(b) != "old diagnostic" {
		t.Fatal("an existing diagnostic was overwritten")
	}
}

func TestCorpusIsDeterministicAndFramed(t *testing.T) {
	t.Parallel()
	var corpora [][]byte
	for range 2 {
		f := newFixture(t)
		j := f.job("corpus", "go_pure", "")
		r, err := runJob(t, f, f.runtime(&fakeRuntime{}), j)
		if err != nil || r["status"] != "ok" || r["frames"] != float64(4) {
			t.Fatalf("corpus: %v %v", err, r)
		}
		b, err := os.ReadFile(j["corpus_file"].(string))
		if err != nil {
			t.Fatal(err)
		}
		if sum := sha256.Sum256(b); r["corpus_sha256"] != hex.EncodeToString(sum[:]) {
			t.Fatal("report corpus_sha256 differs from the file")
		}
		info, _ := os.Stat(j["corpus_file"].(string))
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("corpus mode %v", info.Mode())
		}
		corpora = append(corpora, b)
	}
	if string(corpora[0]) != string(corpora[1]) {
		t.Fatal("the same corpus_config produced different corpora")
	}
	p := filepath.Join(t.TempDir(), "c")
	writeFile(t, p, corpora[0])
	in, err := openCorpus(p, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer in.close()
	wires, err := in.next(10)
	if err != nil || len(wires) != 4 || !in.ended {
		t.Fatalf("framed corpus: %d frames, %v", len(wires), err)
	}
	for _, w := range wires {
		if _, err := decodeFrame(w); err != nil {
			t.Fatal(err)
		}
	}
	// The row budget and frame bounds are enforced while reading.
	short, _ := openCorpus(p, 3)
	defer short.close()
	if _, err := short.next(10); err == nil {
		t.Fatal("a corpus over its row budget was read")
	}
	writeFile(t, p, corpora[0][:len(corpora[0])-1])
	cut, _ := openCorpus(p, 4)
	defer cut.close()
	if _, err := cut.next(10); err == nil {
		t.Fatal("a truncated frame was read")
	}
}

// Every ingest report, even one that never started, has integer counts and timings,
// null resources only with an error and reason, and no census-only field.
func TestEarlyIngestReportShape(t *testing.T) {
	t.Parallel()
	for _, workload := range []string{point2, direct} {
		f := newFixture(t)
		r, err := runJob(t, f, f.runtime(&fakeRuntime{}), f.job("ingest", "go_pure", workload))
		if !errors.Is(err, errNoCluster) {
			t.Fatal(err)
		}
		checkIngestShape(t, r)
		for _, k := range []string{"frames", "received_spans", "accepted_spans", "rejected_spans", "failed_spans", "queued_spans_at_exit", "ingest_wall_ns", "admission_phase_ns", "drain_ns"} {
			if r[k] != float64(0) {
				t.Fatalf("early %s = %v, want 0", k, r[k])
			}
		}
		if r["ingest_resources"] != nil || r["ingest_resources_unavailable_reason"] != notStarted {
			t.Fatalf("early resources %v", r)
		}
	}
}

// checkIngestShape is the frozen ingest report contract on a decoded report.
func checkIngestShape(t *testing.T, r map[string]any) {
	t.Helper()
	doc := schemaDoc(t)
	for _, k := range jsonAt(doc, "$defs", "ingest_ack_and_samples", "required").([]any) {
		if _, ok := r[k.(string)]; !ok {
			t.Fatalf("report lacks %s", k)
		}
	}
	for _, k := range []string{"frames", "received_spans", "accepted_spans", "rejected_spans", "failed_spans", "queued_spans_at_exit", "ingest_wall_ns", "admission_phase_ns", "drain_ns"} {
		v, ok := r[k].(float64)
		if !ok || v < 0 || v != float64(int64(v)) {
			t.Fatalf("%s = %v is not a nonnegative integer", k, r[k])
		}
	}
	for _, k := range []string{"distinct_spans", "spans_per_second", "rate_numerator", "cpu_ms_per_distinct_span", "cpu_denominator"} {
		if _, ok := r[k]; ok {
			t.Fatalf("an ingest report carries %s", k)
		}
	}
	for _, k := range []string{"acked_span_operations", "committed_batches", "native_retries", "outer_retries", "export_attempts", "queue_retries"} {
		m, ok := r[k].(map[string]any)
		if !ok {
			t.Fatalf("%s is not a marker", k)
		}
		switch m["availability"] {
		case "available":
			if v, ok := m["value"].(float64); !ok || v < 0 || len(m) != 2 {
				t.Fatalf("%s = %v", k, m)
			}
		case "unavailable":
			if s, ok := m["reason"].(string); !ok || strings.TrimSpace(s) == "" || len(m) != 2 {
				t.Fatalf("%s = %v", k, m)
			}
		default:
			t.Fatalf("%s = %v", k, m)
		}
	}
	collectorJob := jsonAt(r, "effective_job", "workload", "kind") == "collector_queue"
	for _, s := range r["samples"].([]any) {
		sample := s.(map[string]any)
		if _, ok := sample["acked_span_operations"]; ok {
			t.Fatal("a sample carries the top-level ACK marker")
		}
		if v, ok := sample["direct_write_acked_operations"]; ok && (collectorJob || v == nil) {
			t.Fatalf("sample %v", sample)
		}
	}
	_, reason := r["ingest_resources_unavailable_reason"]
	if r["ingest_resources"] == nil {
		if r["status"] != "error" || r["error"] == nil || !reason {
			t.Fatalf("null resources on %v", r)
		}
	} else if reason {
		t.Fatal("measured resources carry an unavailable reason")
	}
	if p := diagnosticsProblem(r); p != "" {
		t.Fatal(p)
	}
	if p := receiptProblem(r); p != "" {
		t.Fatal(p)
	}
}

var queueDiagnosticFields = []string{"max_outstanding_spans", "blocked_requests", "blocked_ns", "export_call_spans", "completion_samples", "completion_samples_omitted"}

// diagnosticsProblem checks a decoded ingest report's optional diagnostics, Go or Rust:
// each is exactly an unavailable marker with a reason, and the queue object belongs
// only to a collector report, which has it whenever it succeeded.
func diagnosticsProblem(r map[string]any) string {
	off := func(v any) bool {
		m, ok := v.(map[string]any)
		reason, _ := m["reason"].(string)
		return ok && len(m) == 2 && m["availability"] == "unavailable" && strings.TrimSpace(reason) != ""
	}
	if !off(r["transaction_spans"]) {
		return fmt.Sprintf("transaction_spans %v", r["transaction_spans"])
	}
	queue, present := r["queue"]
	if !present {
		return "the report has no queue field"
	}
	settled := false
	if q, ok := queue.(map[string]any); ok && len(q) == len(queueDiagnosticFields) {
		settled = true
		for _, k := range queueDiagnosticFields {
			settled = settled && off(q[k])
		}
	}
	collectorJob := jsonAt(r, "effective_job", "workload", "kind") == "collector_queue"
	if (!collectorJob && queue != nil) || (collectorJob && !settled && (r["status"] == "ok" || queue != nil)) {
		return fmt.Sprintf("queue diagnostics %v", queue)
	}
	return ""
}

// receiptProblem checks any decoded report's client receipt. A native ingest, Go or
// Rust, claims a mapped libfdb_c only with the rule that proved it, and a successful one
// must; every other report never claims a native mapping, and a successful one proved
// no libfdb_c was mapped. A Go report names its compiled build tags: [] for
// control-pure, ["libfdbc_backend"] for control-native's go_native ingest.
func receiptProblem(r map[string]any) string {
	nativeIngest := r["mode"] == "ingest" && (r["arm"] == "go_native" || r["arm"] == "rust_native")
	ok := r["status"] == "ok"
	rule, claimed := r["native_mapping_rule"]
	known := rule == exactStat || rule == btrfsSuperblock
	mapped, checked := r["native_library_mapped"]
	if nativeIngest {
		if (claimed && !known) || (checked && mapped != true) || (claimed && !checked) || (ok && (!known || !checked)) {
			return fmt.Sprintf("a native ingest receipt: rule %v, mapped %v", rule, mapped)
		}
		// A successful or proved native ingest names the one pinned client its job sealed;
		// any echoed client hash is the sealed one.
		sealed, _ := r["effective_job"].(map[string]any)
		hash, echoed := r["client_library_sha256"]
		if ((ok || checked) && (hash != clientSHA256 || sealed["client_library_sha256"] != clientSHA256)) ||
			(echoed && sealed != nil && hash != sealed["client_library_sha256"]) {
			return fmt.Sprintf("a native ingest receipt for client %v, sealed %v", hash, sealed["client_library_sha256"])
		}
	} else if claimed || (checked && mapped != false) || (ok && !checked) {
		return fmt.Sprintf("a %v %v receipt: rule %v, mapped %v", r["mode"], r["arm"], rule, mapped)
	}
	if r["mode"] == "ingest" && r["arm"] == "rust_native" {
		return "" // the Rust executable has no Go build tags
	}
	want := []any{}
	if r["mode"] == "ingest" && r["arm"] == "go_native" {
		want = []any{"libfdbc_backend"}
	}
	if !reflect.DeepEqual(r["build_tags"], want) {
		return fmt.Sprintf("build_tags %v, want %v", r["build_tags"], want)
	}
	return ""
}

// The receipt accepts what each executable truthfully reports and refuses a missing,
// premature or contradictory claim about the native client or the build.
func TestReceiptContract(t *testing.T) {
	t.Parallel()
	report := func(mode, arm, status string, fields map[string]any) map[string]any {
		r := map[string]any{"mode": mode, "arm": arm, "status": status, "build_tags": []any{}}
		if mode == "ingest" && arm == "go_native" {
			r["build_tags"] = []any{"libfdbc_backend"}
		}
		if mode == "ingest" && arm == "rust_native" {
			delete(r, "build_tags")
		}
		if mode == "ingest" && arm != "go_pure" {
			r["client_library_sha256"] = clientSHA256
			r["effective_job"] = map[string]any{"client_library_sha256": clientSHA256}
		}
		for k, v := range fields {
			if v == nil {
				delete(r, k)
				continue
			}
			r[k] = v
		}
		return r
	}
	pure := map[string]any{"native_library_mapped": false}
	proved := func(rule any) map[string]any {
		return map[string]any{"native_library_mapped": true, "native_mapping_rule": rule}
	}
	// set changes one field of a report, null included.
	set := func(r map[string]any, field string, v any) map[string]any {
		r[field] = v
		return r
	}
	sealedFor := func(hash string) map[string]any { return map[string]any{"client_library_sha256": hash} }
	other := strings.Repeat("0", 64)
	for name, r := range map[string]map[string]any{
		"corpus":                        report("corpus", "go_pure", "ok", pure),
		"setup of a native arm":         report("setup", "go_native", "ok", pure),
		"verify of the Rust arm":        report("verify", "rust_native", "ok", pure),
		"go_pure ingest":                report("ingest", "go_pure", "ok", pure),
		"pure, failed before a check":   report("setup", "go_pure", "error", nil),
		"go_native exact_stat":          report("ingest", "go_native", "ok", proved(exactStat)),
		"go_native btrfs_superblock":    report("ingest", "go_native", "ok", proved(btrfsSuperblock)),
		"go_native, failed unproved":    report("ingest", "go_native", "error", nil),
		"go_native, proved then failed": report("ingest", "go_native", "error", map[string]any{"native_library_mapped": true}),
		"rust_native":                   report("ingest", "rust_native", "ok", proved(exactStat)),
	} {
		if p := receiptProblem(r); p != "" {
			t.Errorf("%s: %s", name, p)
		}
	}
	refused := map[string]map[string]any{
		"pure ok, unchecked":           report("setup", "go_native", "ok", nil),
		"pure claims a mapped client":  report("verify", "go_pure", "ok", map[string]any{"native_library_mapped": true}),
		"pure claims a rule":           report("ingest", "go_pure", "ok", map[string]any{"native_library_mapped": false, "native_mapping_rule": exactStat}),
		"pure, a string":               report("corpus", "go_pure", "ok", map[string]any{"native_library_mapped": "false"}),
		"pure with native tags":        report("setup", "go_pure", "ok", map[string]any{"native_library_mapped": false, "build_tags": []any{"libfdbc_backend"}}),
		"pure without tags":            report("setup", "go_pure", "ok", map[string]any{"native_library_mapped": false, "build_tags": nil}),
		"pure with null tags":          report("setup", "go_pure", "ok", map[string]any{"native_library_mapped": false, "build_tags": []any(nil)}),
		"go_native ok without a rule":  report("ingest", "go_native", "ok", map[string]any{"native_library_mapped": true}),
		"go_native ok, unmapped":       report("ingest", "go_native", "ok", map[string]any{"native_library_mapped": false, "native_mapping_rule": exactStat}),
		"go_native rule, no mapping":   report("ingest", "go_native", "ok", map[string]any{"native_mapping_rule": exactStat}),
		"go_native another rule":       report("ingest", "go_native", "ok", proved("mount_device")),
		"go_native null rule":          report("ingest", "go_native", "ok", proved([]any(nil))),
		"go_native error, unmapped":    report("ingest", "go_native", "error", map[string]any{"native_library_mapped": false}),
		"go_native with pure tags":     report("ingest", "go_native", "ok", map[string]any{"native_library_mapped": true, "native_mapping_rule": exactStat, "build_tags": []any{}}),
		"rust_native without a rule":   report("ingest", "rust_native", "ok", nil),
		"rust_native rule, no mapping": report("ingest", "rust_native", "ok", map[string]any{"native_mapping_rule": exactStat}),
	}
	// The client hash of each native arm: missing, null, another client sealed and echoed,
	// a stale echo of another sealed job, and a proved failure naming another client.
	for _, arm := range []string{"go_native", "rust_native"} {
		refused[arm+" without a client hash"] = report("ingest", arm, "ok", map[string]any{"native_library_mapped": true, "native_mapping_rule": exactStat, "client_library_sha256": nil})
		refused[arm+" with a null client hash"] = set(report("ingest", arm, "ok", proved(exactStat)), "client_library_sha256", nil)
		refused[arm+" for another client"] = report("ingest", arm, "ok", map[string]any{"native_library_mapped": true, "native_mapping_rule": exactStat, "client_library_sha256": other, "effective_job": sealedFor(other)})
		refused[arm+" with a stale client hash"] = report("ingest", arm, "ok", map[string]any{"native_library_mapped": true, "native_mapping_rule": exactStat, "effective_job": sealedFor(other)})
		refused[arm+" proved, failed, another client"] = report("ingest", arm, "error", map[string]any{"native_library_mapped": true, "client_library_sha256": other, "effective_job": sealedFor(other)})
		refused[arm+" unproved, an inconsistent echo"] = report("ingest", arm, "error", map[string]any{"client_library_sha256": other})
	}
	if len(refused) != 28 {
		t.Fatalf("%d refusal cases", len(refused))
	}
	for name, r := range refused {
		if receiptProblem(r) == "" {
			t.Errorf("%s was accepted: %v", name, r)
		}
	}
}

// The explicit unavailable diagnostics accept exactly the markers the Go report emits
// and refuse measured stand-ins, claimed observations and malformed markers.
func TestDiagnosticsContract(t *testing.T) {
	t.Parallel()
	var marker, queue map[string]any
	if err := json.Unmarshal(encode(t, diagnosticOff()), &marker); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encode(t, queueDiagnosticsOff()), &queue); err != nil {
		t.Fatal(err)
	}
	if len(marker) != 2 || marker["availability"] != "unavailable" || marker["reason"] != diagnosticsOff || !slices.Equal(keySet(queue), sorted(queueDiagnosticFields)) {
		t.Fatalf("emitted markers %v %v", marker, queue)
	}
	report := func(kind, arm, status string) map[string]any {
		var q any
		if kind == "collector_queue" {
			q = maps.Clone(queue)
		}
		return map[string]any{
			"arm": arm, "status": status, "effective_job": map[string]any{"workload": map[string]any{"kind": kind}},
			"transaction_spans": maps.Clone(marker), "queue": q,
		}
	}
	beforeStart := report("collector_queue", "go_pure", "error")
	beforeStart["queue"] = nil
	for name, r := range map[string]map[string]any{
		"collector ok":           report("collector_queue", "go_pure", "ok"),
		"direct ok":              report("writer_direct", "go_pure", "ok"),
		"collector before start": beforeStart,
		"rust collector":         report("collector_queue", "rust_native", "ok"),
	} {
		if p := diagnosticsProblem(r); p != "" {
			t.Errorf("%s: %s", name, p)
		}
	}
	mutate := func(kind, arm string, change func(map[string]any)) map[string]any {
		r := report(kind, arm, "ok")
		change(r)
		return r
	}
	queueField := func(v any) func(map[string]any) {
		return func(r map[string]any) { r["queue"].(map[string]any)["blocked_ns"] = v }
	}
	for name, r := range map[string]map[string]any{
		"measured empty map":   mutate("writer_direct", "go_pure", func(r map[string]any) { r["transaction_spans"] = map[string]any{} }),
		"measured empty array": mutate("writer_direct", "go_pure", func(r map[string]any) { r["transaction_spans"] = []any{} }),
		"measured zero":        mutate("collector_queue", "go_pure", queueField(float64(0))),
		"queue empty array":    mutate("collector_queue", "go_pure", queueField([]any{})),
		"claimed available":    mutate("collector_queue", "go_pure", queueField(map[string]any{"availability": "available", "reason": "x"})),
		"blank reason": mutate("writer_direct", "go_pure", func(r map[string]any) {
			r["transaction_spans"] = map[string]any{"availability": "unavailable", "reason": " "}
		}),
		"marker extra field":    mutate("writer_direct", "go_pure", func(r map[string]any) { r["transaction_spans"].(map[string]any)["value"] = float64(0) }),
		"marker missing reason": mutate("collector_queue", "go_pure", queueField(map[string]any{"availability": "unavailable"})),
		"no transaction_spans":  mutate("writer_direct", "go_pure", func(r map[string]any) { delete(r, "transaction_spans") }),
		"no queue field":        mutate("writer_direct", "go_pure", func(r map[string]any) { delete(r, "queue") }),
		"queue missing field":   mutate("collector_queue", "go_pure", func(r map[string]any) { delete(r["queue"].(map[string]any), "completion_samples") }),
		"queue extra field":     mutate("collector_queue", "go_pure", func(r map[string]any) { r["queue"].(map[string]any)["depth"] = maps.Clone(marker) }),
		"collector ok, null":    mutate("collector_queue", "go_pure", func(r map[string]any) { r["queue"] = nil }),
		"direct with a queue":   mutate("writer_direct", "go_pure", func(r map[string]any) { r["queue"] = maps.Clone(queue) }),
	} {
		if diagnosticsProblem(r) == "" {
			t.Errorf("%s was accepted: %v", name, r)
		}
	}
}

func TestMeasuredResources(t *testing.T) {
	t.Parallel()
	start := syscall.Rusage{Utime: syscall.Timeval{Sec: 1}, Stime: syscall.Timeval{Sec: 2}}
	end := syscall.Rusage{Utime: syscall.Timeval{Sec: 3, Usec: 500000}, Stime: syscall.Timeval{Sec: 2, Usec: 250000}, Maxrss: 1000}
	res, _ := measured(start, nil, end, nil)
	if res == nil || res.UserCPUSeconds != 2.5 || res.SystemCPUSeconds != 0.25 || res.CPUSeconds != 2.75 || res.PeakRSSBytes != 1024000 || res.Scope != "first_read_through_flush" {
		t.Fatalf("resources %+v", res)
	}
	for name, c := range map[string]struct {
		start, end syscall.Rusage
		err        error
	}{
		"no RSS":           {start, syscall.Rusage{Utime: end.Utime, Stime: end.Stime}, nil},
		"negative CPU":     {end, syscall.Rusage{Maxrss: 1}, nil},
		"getrusage failed": {start, end, syscall.EINVAL},
	} {
		if res, reason := measured(c.start, nil, c.end, c.err); res != nil || reason == "" {
			t.Fatalf("%s measured %+v", name, res)
		}
	}
}
