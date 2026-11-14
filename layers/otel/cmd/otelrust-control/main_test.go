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
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
)

const frozenSchemaSHA256 = "c7d9ae94ed71db095f0de48999e2683a2c12f96525241f1e9a7471f836f57ec6"

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

// fakeRuntime opens nothing real: it records which client a job asked for.
type fakeRuntime struct {
	pure, native int
}

var errNoCluster = errors.New("fake runtime: no cluster")

func (f *fixture) runtime(rec *fakeRuntime) runtime {
	return runtime{
		executable: func() (string, error) { return f.exe, nil },
		openPure: func(context.Context, string) (fdb.BackendDatabase, error) {
			rec.pure++
			return nil, errNoCluster
		},
		openNative: func(context.Context, string, string) (fdb.BackendDatabase, string, error) {
			rec.native++
			return nil, "", errNoCluster
		},
		usage:     selfUsage,
		joinBound: time.Second,
	}
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
		if c.mode == "ingest" && c.arm == "go_native" {
			// A native ingest hashes the artifact before its report: give it the real pin.
			j["client_library"] = pinnedLibrary(t)
		}
		r, err := runJob(t, f, f.runtime(rec), j)
		if !errors.Is(err, errNoCluster) || r == nil {
			t.Fatalf("%s %s: %v", c.mode, c.arm, err)
		}
		if rec.pure != c.pure || rec.native != c.native {
			t.Fatalf("%s %s opened pure %d, native %d times; want %d, %d", c.mode, c.arm, rec.pure, rec.native, c.pure, c.native)
		}
		if r["status"] != "error" || r["mode"] != c.mode || r["schema"] != reportSchema {
			t.Fatalf("%s %s report %v", c.mode, c.arm, r)
		}
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
	_, err := runJob(t, f, f.runtime(rec), f.job("ingest", "go_native", point2))
	var ce *configError
	if !errors.As(err, &ce) || rec.native != 0 {
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
}

var queueDiagnosticFields = []string{"max_outstanding_spans", "blocked_requests", "blocked_ns", "export_call_spans", "completion_samples", "completion_samples_omitted"}

// diagnosticsProblem checks a decoded ingest report's optional diagnostics, Go or Rust:
// each is exactly an unavailable marker with a reason; the queue object belongs only to
// a collector report, which has it whenever it succeeded; and only a go_native report
// names the rule that proved its mapped client, which a successful one must.
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
	rule, claimed := r["native_mapping_rule"]
	known := rule == exactStat || rule == btrfsSuperblock
	switch {
	case r["arm"] != "go_native" && claimed:
		return fmt.Sprintf("a %v report claims a native mapping rule %v", r["arm"], rule)
	case claimed && !known, r["arm"] == "go_native" && r["status"] == "ok" && !known:
		return fmt.Sprintf("native mapping rule %v", rule)
	}
	return ""
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
	native := func(rule any) map[string]any {
		r := report("collector_queue", "go_native", "ok")
		r["native_mapping_rule"] = rule
		return r
	}
	beforeStart := report("collector_queue", "go_pure", "error")
	beforeStart["queue"] = nil
	for name, r := range map[string]map[string]any{
		"collector ok":           report("collector_queue", "go_pure", "ok"),
		"direct ok":              report("writer_direct", "go_pure", "ok"),
		"collector before start": beforeStart,
		"rust collector":         report("collector_queue", "rust_native", "ok"),
		"native exact_stat":      native(exactStat),
		"native btrfs":           native(btrfsSuperblock),
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
		"marker extra field":       mutate("writer_direct", "go_pure", func(r map[string]any) { r["transaction_spans"].(map[string]any)["value"] = float64(0) }),
		"marker missing reason":    mutate("collector_queue", "go_pure", queueField(map[string]any{"availability": "unavailable"})),
		"no transaction_spans":     mutate("writer_direct", "go_pure", func(r map[string]any) { delete(r, "transaction_spans") }),
		"no queue field":           mutate("writer_direct", "go_pure", func(r map[string]any) { delete(r, "queue") }),
		"queue missing field":      mutate("collector_queue", "go_pure", func(r map[string]any) { delete(r["queue"].(map[string]any), "completion_samples") }),
		"queue extra field":        mutate("collector_queue", "go_pure", func(r map[string]any) { r["queue"].(map[string]any)["depth"] = maps.Clone(marker) }),
		"collector ok, null":       mutate("collector_queue", "go_pure", func(r map[string]any) { r["queue"] = nil }),
		"direct with a queue":      mutate("writer_direct", "go_pure", func(r map[string]any) { r["queue"] = maps.Clone(queue) }),
		"pure claims a mapping":    mutate("writer_direct", "go_pure", func(r map[string]any) { r["native_mapping_rule"] = exactStat }),
		"rust claims a mapping":    mutate("writer_direct", "rust_native", func(r map[string]any) { r["native_mapping_rule"] = exactStat }),
		"native without a rule":    mutate("writer_direct", "go_native", func(map[string]any) {}),
		"native with another rule": native("mount_device"),
		"native with a null rule":  native(nil),
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
