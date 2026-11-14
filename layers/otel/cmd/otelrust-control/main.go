// otelrust-control is the Go side of the paired Go/Rust OTel benchmark. One sealed
// otel_paired_job_v1 job per process generates the corpus, sets up or verifies any arm's
// tenant through the pure-Go client, or ingests a Go arm. It never provisions a cluster.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"fdb.dev/layers/otel"
	"fdb.dev/layers/otel/collector"
	"fdb.dev/pkg/fdbgo/client"
	"fdb.dev/pkg/fdbgo/fdb"
)

const (
	jobSchema    = "otel_paired_job_v1"
	reportSchema = "otel_control_report_v1"
	// clientSHA256 is MODULE.bazel's libfdb_c 7.3.77 pin, the only native client accepted.
	clientSHA256    = "617572467897d0360f76ffc33f24b58090ee691e3273750d2ff334cc71aae3fa"
	maxJobBytes     = 1 << 20
	maxClusterBytes = 64 << 10
	maxLayoutBytes  = 64 << 20
	// txnTimeout is the shared fixed transaction policy; there is no retry-count limit.
	txnTimeout = 10 * time.Second
)

type corpusConfig struct {
	Generator  string `json:"generator"`
	Seed       uint64 `json:"seed"`
	Count      int    `json:"count"`
	Tenants    int    `json:"tenants"`
	Attributes int    `json:"attributes"`
	MatchEvery int    `json:"match_every"`
}

// workload holds both schema kinds; which keys a kind has is checked on the raw object.
type workload struct {
	Kind               string `json:"kind"`
	Producers          int    `json:"producers"`
	ProducerBatchSpans int    `json:"producer_batch_spans"`
	Consumers          int    `json:"consumers"`
	QueueCapacitySpans int    `json:"queue_capacity_spans"`
	BatchMinSpans      int    `json:"batch_min_spans"`
	BatchMaxSpans      int    `json:"batch_max_spans"`
	FlushMS            int64  `json:"flush_ms"`
	BlockOnOverflow    bool   `json:"block_on_overflow"`
	ExportTimeoutMS    int64  `json:"export_timeout_ms"`
	RetryEnabled       bool   `json:"retry_enabled"`
	RetryInitialMS     int64  `json:"retry_initial_ms"`
	RetryMaxMS         int64  `json:"retry_max_ms"`
	RetryElapsedMS     int64  `json:"retry_elapsed_ms"`
}

type verification struct {
	QueryOracle      string  `json:"query_oracle"`
	QueryResultsFile *string `json:"query_results_file"`
	K                int     `json:"k"`
	MaxCandidates    int     `json:"max_candidates"`
	MaxBytes         int64   `json:"max_bytes"`
}

// job is every mode's fields of the frozen paired-job-v1 schema.
type job struct {
	Schema                string        `json:"schema"`
	Mode                  string        `json:"mode"`
	Arm                   string        `json:"arm"`
	BinarySHA256          string        `json:"binary_sha256"`
	SourceRevision        string        `json:"source_revision"`
	SourceManifestSHA256  string        `json:"source_manifest_sha256"`
	TimeoutSeconds        int           `json:"timeout_seconds"`
	Output                string        `json:"output"`
	CorpusConfig          corpusConfig  `json:"corpus_config"`
	ExpectedFrames        int           `json:"expected_frames"`
	RowBudget             int           `json:"row_budget"`
	CorpusFile            string        `json:"corpus_file"`
	Run                   string        `json:"run"`
	ClusterFile           string        `json:"cluster_file"`
	ClusterSHA256         string        `json:"cluster_sha256"`
	ClientBackend         string        `json:"client_backend"`
	ClientLibrary         *string       `json:"client_library"`
	ClientLibrarySHA256   *string       `json:"client_library_sha256"`
	CorpusSHA256          string        `json:"corpus_sha256"`
	Workload              workload      `json:"workload"`
	WriterBatchSize       int           `json:"writer_batch_size"`
	WriterWorkers         int           `json:"writer_workers"`
	RecordCompression     string        `json:"record_compression"`
	EnableMessageDeltas   bool          `json:"enable_message_deltas"`
	EnableMessageFullText bool          `json:"enable_message_full_text"`
	BulkIndexConflicts    bool          `json:"bulk_index_conflicts"`
	LayoutFile            string        `json:"layout_file"`
	LayoutSHA256          string        `json:"layout_sha256"`
	Verification          *verification `json:"verification"`
}

var (
	commonKeys       = []string{"schema", "mode", "arm", "binary_sha256", "source_revision", "source_manifest_sha256", "timeout_seconds", "output", "corpus_config", "expected_frames", "row_budget", "corpus_file"}
	liveKeys         = []string{"run", "cluster_file", "cluster_sha256", "client_backend", "client_library", "client_library_sha256", "corpus_sha256", "workload", "writer_batch_size", "writer_workers", "record_compression", "enable_message_deltas", "enable_message_full_text", "bulk_index_conflicts", "layout_file"}
	corpusConfigKeys = []string{"generator", "seed", "count", "tenants", "attributes", "match_every"}
	directKeys       = []string{"kind", "producers", "producer_batch_spans"}
	queueKeys        = []string{"kind", "producers", "producer_batch_spans", "consumers", "queue_capacity_spans", "batch_min_spans", "batch_max_spans", "flush_ms", "block_on_overflow", "export_timeout_ms", "retry_enabled", "retry_initial_ms", "retry_max_ms", "retry_elapsed_ms"}
	verificationKeys = []string{"query_oracle", "query_results_file", "k", "max_candidates", "max_bytes"}
	runPattern       = regexp.MustCompile(`^[a-z0-9_]{8,48}$`)
)

// modeKeys are a mode's exact fields: the schema requires every field it allows.
func modeKeys(mode string) []string {
	switch mode {
	case "corpus":
		return commonKeys
	case "setup":
		return slices.Concat(commonKeys, liveKeys)
	case "ingest":
		return slices.Concat(commonKeys, liveKeys, []string{"layout_sha256"})
	case "verify":
		return slices.Concat(commonKeys, liveKeys, []string{"layout_sha256", "verification"})
	}
	return nil
}

func (j *job) live() bool { return j.Mode != "corpus" }

func (j *job) collector() bool { return j.Workload.Kind == "collector_queue" }

// globalWriteBound is the concurrent write transactions the arm may have: per-call
// workers for each Collector consumer, or for each direct producer.
func (j *job) globalWriteBound() int {
	if j.collector() {
		return j.Workload.Consumers * j.WriterWorkers
	}
	return j.Workload.Producers * j.WriterWorkers
}

// configError is a sanitized job, seal or path refusal: it names a field, never a value.
type configError struct{ msg string }

func (e *configError) Error() string { return e.msg }

func invalid(field string) error { return &configError{"invalid sealed job field: " + field} }

func refuse(msg string) error { return &configError{msg} }

func fault(op, reason string) error { return otel.NewError("operation", op, reason, nil) }

// checkStructure accepts exactly one JSON value without a duplicate key at any depth.
func checkStructure(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := walkValue(dec, 0); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data after the JSON value")
	}
	return nil
}

func walkValue(dec *json.Decoder, depth int) error {
	if depth > 16 {
		return errors.New("JSON nested too deeply")
	}
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch tok {
	case json.Delim('{'):
		seen := map[string]bool{}
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return errors.New("duplicate or invalid object key")
			}
			seen[name] = true
			if err := walkValue(dec, depth+1); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	case json.Delim('['):
		for dec.More() {
			if err := walkValue(dec, depth+1); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	}
	return nil
}

// exactObject requires raw to be an object with exactly keys, none null unless nullable.
func exactObject(field string, raw json.RawMessage, keys []string, nullable ...string) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, invalid(field + " (not an object)")
	}
	for _, k := range keys {
		v, ok := obj[k]
		if !ok {
			return nil, invalid(field + "." + k + " (missing)")
		}
		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) && !slices.Contains(nullable, k) {
			return nil, invalid(field + "." + k + " (null)")
		}
	}
	if len(obj) != len(keys) {
		return nil, invalid(field + " (unknown field)")
	}
	return obj, nil
}

// parseJob decodes a sealed job strictly: one value, no duplicate, unknown, missing or
// null field at any level, and only the fields the job's mode and workload kind allow.
func parseJob(data []byte) (*job, error) {
	if err := checkStructure(data); err != nil {
		return nil, invalid("JSON (one value without duplicate keys)")
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil || top == nil {
		return nil, invalid("JSON (not an object)")
	}
	var mode string
	if raw, ok := top["mode"]; !ok || json.Unmarshal(raw, &mode) != nil {
		return nil, invalid("mode")
	}
	keys := modeKeys(mode)
	if keys == nil {
		return nil, invalid("mode")
	}
	if _, err := exactObject("job", data, keys, "client_library", "client_library_sha256"); err != nil {
		return nil, err
	}
	if _, err := exactObject("corpus_config", top["corpus_config"], corpusConfigKeys); err != nil {
		return nil, err
	}
	if mode != "corpus" {
		var kind struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(top["workload"], &kind); err != nil {
			return nil, invalid("workload")
		}
		keys := map[string][]string{"writer_direct": directKeys, "collector_queue": queueKeys}[kind.Kind]
		if keys == nil {
			return nil, invalid("workload.kind")
		}
		if _, err := exactObject("workload", top["workload"], keys); err != nil {
			return nil, err
		}
	}
	if mode == "verify" {
		if _, err := exactObject("verification", top["verification"], verificationKeys, "query_results_file"); err != nil {
			return nil, err
		}
	}
	var j job
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&j); err != nil {
		return nil, invalid("JSON (a field has the wrong type or range)")
	}
	return &j, nil
}

func lowerHex(s string, n int) bool {
	return len(s) == n && strings.Trim(s, "0123456789abcdef") == ""
}

func cleanAbsolute(p string) bool {
	return len(p) > 1 && filepath.IsAbs(p) && filepath.Clean(p) == p && !strings.ContainsRune(p, 0)
}

// checker keeps a validation's first failure.
type checker struct{ err error }

func (c *checker) ok(cond bool, field string) {
	if c.err == nil && !cond {
		c.err = invalid(field)
	}
}

func between[T int | int64](v, lo, hi T) bool { return v >= lo && v <= hi }

// validate applies every schema and cross-field rule; no field has a default.
func validate(j *job, jobPath string) error {
	c := &checker{}
	c.ok(j.Schema == jobSchema, "schema")
	c.ok(slices.Contains([]string{"go_pure", "go_native", "rust_native"}, j.Arm), "arm")
	c.ok(j.Mode != "ingest" || j.Arm != "rust_native", "arm (rust_native ingest runs the Rust executable)")
	c.ok(lowerHex(j.BinarySHA256, 64), "binary_sha256")
	c.ok(lowerHex(j.SourceRevision, 40), "source_revision")
	c.ok(lowerHex(j.SourceManifestSHA256, 64), "source_manifest_sha256")
	c.ok(between(j.TimeoutSeconds, 1, 3600), "timeout_seconds")
	cc := j.CorpusConfig
	c.ok(cc.Generator == "otel_fixture_v2", "corpus_config.generator")
	c.ok(between(cc.Count, 2, 1_000_000), "corpus_config.count")
	c.ok(cc.Tenants == 1, "corpus_config.tenants")
	c.ok(cc.Attributes == 8, "corpus_config.attributes")
	c.ok(between(cc.MatchEvery, 1, 1_000_000), "corpus_config.match_every")
	c.ok(between(j.ExpectedFrames, 1, 1_000_000) && j.ExpectedFrames == cc.Count, "expected_frames")
	c.ok(between(j.RowBudget, 1, 10_000_000) && j.RowBudget >= j.ExpectedFrames, "row_budget")
	paths := []string{jobPath, j.Output, j.CorpusFile}
	if j.Mode == "ingest" {
		paths = append(paths, diagnosticPath(j.Output))
	}
	if j.live() {
		c.ok(runPattern.MatchString(j.Run), "run")
		c.ok(lowerHex(j.ClusterSHA256, 64), "cluster_sha256")
		c.ok(lowerHex(j.CorpusSHA256, 64), "corpus_sha256")
		if j.Arm == "go_pure" {
			c.ok(j.ClientBackend == "pure_go", "client_backend")
			c.ok(j.ClientLibrary == nil && j.ClientLibrarySHA256 == nil, "client_library (pure_go has none)")
		} else {
			c.ok(j.ClientBackend == "native_c", "client_backend")
			c.ok(j.ClientLibrary != nil && j.ClientLibrarySHA256 != nil, "client_library (native_c needs one)")
			c.ok(j.ClientLibrarySHA256 == nil || *j.ClientLibrarySHA256 == clientSHA256, "client_library_sha256 (only the pinned libfdb_c 7.3.77)")
			if j.ClientLibrary != nil {
				paths = append(paths, *j.ClientLibrary)
			}
		}
		c.ok(between(j.WriterBatchSize, 1, 64), "writer_batch_size")
		c.ok(between(j.WriterWorkers, 1, 8), "writer_workers")
		c.ok(j.Arm != "rust_native" || j.WriterWorkers == 1, "writer_workers (the Rust writer supports only 1)")
		c.ok(slices.Contains([]string{"none", "zlib-1", "zlib-default"}, j.RecordCompression), "record_compression")
		c.ok(!j.EnableMessageDeltas, "enable_message_deltas")
		c.ok(!j.EnableMessageFullText, "enable_message_full_text")
		c.ok(!j.BulkIndexConflicts, "bulk_index_conflicts")
		validateWorkload(c, j.Workload)
		paths = append(paths, j.ClusterFile, j.LayoutFile)
	}
	if j.Mode == "ingest" || j.Mode == "verify" {
		c.ok(lowerHex(j.LayoutSHA256, 64), "layout_sha256")
	}
	if v := j.Verification; v != nil {
		c.ok(v.QueryOracle == "none", "verification.query_oracle (source_1600_v1 is not wired yet)")
		c.ok(v.QueryResultsFile == nil, "verification.query_results_file (none writes no results)")
		c.ok(between(v.K, 1, 1_000_000), "verification.k")
		c.ok(between(v.MaxCandidates, 1, 10_000_000), "verification.max_candidates")
		c.ok(between(v.MaxBytes, 1, 1_000_000_000), "verification.max_bytes")
	}
	for i, p := range paths {
		c.ok(cleanAbsolute(p), "paths must be clean and absolute")
		c.ok(!slices.Contains(paths[i+1:], p), "paths must be distinct")
	}
	return c.err
}

// validateWorkload is the Rust executable's workload rules, plus the schema's bounds.
func validateWorkload(c *checker, w workload) {
	c.ok(between(w.Producers, 1, 32), "workload.producers")
	c.ok(between(w.ProducerBatchSpans, 1, 64), "workload.producer_batch_spans")
	if w.Kind != "collector_queue" {
		return
	}
	ms := func(v int64) bool { return between(v, 1, 3_600_000) }
	c.ok(between(w.Consumers, 1, 32), "workload.consumers")
	c.ok(between(w.QueueCapacitySpans, 1, 10_000_000), "workload.queue_capacity_spans")
	c.ok(between(w.BatchMinSpans, 1, 10_000_000) && w.BatchMinSpans <= w.QueueCapacitySpans, "workload.batch_min_spans")
	c.ok(between(w.BatchMaxSpans, 1, 10_000_000) && w.BatchMaxSpans >= w.BatchMinSpans, "workload.batch_max_spans")
	// A larger producer call could never be admitted.
	c.ok(w.ProducerBatchSpans <= w.QueueCapacitySpans, "workload.producer_batch_spans")
	c.ok(ms(w.FlushMS), "workload.flush_ms")
	c.ok(ms(w.ExportTimeoutMS), "workload.export_timeout_ms")
	// retry_on_failure validation: an enabled retry needs a positive interval.
	c.ok(between(w.RetryInitialMS, 0, 3_600_000) && (!w.RetryEnabled || w.RetryInitialMS > 0), "workload.retry_initial_ms")
	c.ok(ms(w.RetryMaxMS) && w.RetryMaxMS >= w.RetryInitialMS, "workload.retry_max_ms")
	c.ok(between(w.RetryElapsedMS, 0, 3_600_000), "workload.retry_elapsed_ms")
}

// checkFiles requires every input to be an existing regular file and every output to be
// new, with no two paths naming one file through a link.
func checkFiles(j *job, jobPath string) error {
	inputs := []string{jobPath}
	outputs := []string{j.Output}
	if j.Mode == "ingest" {
		// Never written unless the Collector cannot settle, but never reusable either.
		outputs = append(outputs, diagnosticPath(j.Output))
	}
	switch j.Mode {
	case "corpus":
		outputs = append(outputs, j.CorpusFile)
	case "setup":
		inputs = append(inputs, j.ClusterFile, j.CorpusFile)
		outputs = append(outputs, j.LayoutFile)
	default:
		inputs = append(inputs, j.ClusterFile, j.CorpusFile, j.LayoutFile)
	}
	if j.ClientLibrary != nil {
		inputs = append(inputs, *j.ClientLibrary)
	}
	var infos []os.FileInfo
	var resolved []string
	for _, p := range inputs {
		info, err := os.Stat(p)
		if err != nil || !info.Mode().IsRegular() {
			return refuse("a sealed input is not a readable regular file")
		}
		for _, other := range infos {
			if os.SameFile(info, other) {
				return refuse("two sealed paths name the same file")
			}
		}
		infos = append(infos, info)
		target, err := filepath.EvalSymlinks(p)
		if err != nil {
			return refuse("a sealed input path cannot be resolved")
		}
		resolved = append(resolved, target)
	}
	for _, p := range outputs {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			return refuse("an output path already exists; outputs are never reused")
		}
		dir, err := filepath.EvalSymlinks(filepath.Dir(p))
		if err != nil {
			return refuse("an output directory does not exist")
		}
		target := filepath.Join(dir, filepath.Base(p))
		if slices.Contains(resolved, target) {
			return refuse("two sealed paths name the same file")
		}
		resolved = append(resolved, target)
	}
	return nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", refuse("cannot open a sealed file")
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", refuse("cannot read a sealed file")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// readSealed reads a whole sealed file once, so its hash and its parsed content agree.
func readSealed(path string, limit int64) ([]byte, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", refuse("cannot open a sealed file")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, "", refuse("cannot read a sealed file")
	}
	if int64(len(b)) > limit {
		return nil, "", refuse("a sealed file exceeds its size bound")
	}
	sum := sha256.Sum256(b)
	return b, hex.EncodeToString(sum[:]), nil
}

// checkSeals compares every sealed hash with the file it names and parses the layout.
func checkSeals(rt runtime, j *job) (*layoutManifest, error) {
	exe, err := rt.executable()
	if err != nil {
		return nil, refuse("cannot find the running executable")
	}
	if h, err := hashFile(exe); err != nil || h != j.BinarySHA256 {
		return nil, refuse("running executable SHA256 mismatch")
	}
	if !j.live() {
		return nil, nil
	}
	if _, h, err := readSealed(j.ClusterFile, maxClusterBytes); err != nil || h != j.ClusterSHA256 {
		return nil, refuse("cluster file SHA256 mismatch")
	}
	if h, err := hashFile(j.CorpusFile); err != nil || h != j.CorpusSHA256 {
		return nil, refuse("corpus SHA256 mismatch")
	}
	if j.Mode == "ingest" && j.ClientBackend == "native_c" {
		// Only the measured native ingest loads the artifact; setup and verify are pure Go.
		if h, err := hashFile(*j.ClientLibrary); err != nil || h != clientSHA256 {
			return nil, refuse("client library is not the pinned libfdb_c")
		}
	}
	if j.Mode == "setup" {
		return nil, nil
	}
	b, h, err := readSealed(j.LayoutFile, maxLayoutBytes)
	if err != nil || h != j.LayoutSHA256 {
		return nil, refuse("layout SHA256 mismatch")
	}
	m, err := parseManifest(b)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// runtime is what a job may open and measure; tests substitute parts of it.
type runtime struct {
	executable func() (string, error)
	openPure   func(ctx context.Context, clusterFile string) (fdb.BackendDatabase, error)
	openNative func(ctx context.Context, library, clusterFile string) (fdb.BackendDatabase, string, error)
	usage      func() (syscall.Rusage, error)
	// joinBound bounds the join-only wait after an expired Shutdown canceled the
	// Collector's work; it grants no further export time.
	joinBound time.Duration
	// sink, when set, replaces the measured Exporter behind the real Collector (tests).
	sink func(context.Context, otel.ExporterConfig) (collector.Sink, error)
}

func productionRuntime() runtime {
	return runtime{
		executable: os.Executable,
		openPure:   openPureBackend,
		openNative: openNativeBackend,
		usage:      selfUsage,
		joinBound:  30 * time.Second,
	}
}

// openPureBackend opens the production pure-Go client on clusterFile.
func openPureBackend(ctx context.Context, clusterFile string) (fdb.BackendDatabase, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := fdb.APIVersion(730); err != nil {
		return nil, err
	}
	cf, err := client.ParseClusterFile(clusterFile)
	if err != nil {
		return nil, err
	}
	db, err := fdb.OpenDatabaseFromConfig(ctx, cf)
	if err != nil {
		return nil, err
	}
	return db, nil
}

// ingestBackend opens the client the ingest arm measures; setup and verify never call it.
// rule is the native mapping proof (exactStat or btrfsSuperblock), empty for the pure client.
func (rt runtime) ingestBackend(ctx context.Context, j *job) (backend fdb.BackendDatabase, rule string, err error) {
	if j.ClientBackend == "native_c" {
		backend, rule, err = rt.openNative(ctx, *j.ClientLibrary, j.ClusterFile)
		if err == nil && rule != exactStat && rule != btrfsSuperblock {
			backend.Close()
			return nil, "", fault("client", "the native open proved no mapping rule")
		}
		return backend, rule, err
	}
	backend, err = rt.openPure(ctx, j.ClusterFile)
	return backend, "", err
}

func selfUsage() (syscall.Rusage, error) {
	var ru syscall.Rusage
	err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return ru, err
}

type artifact struct {
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// reportCommon is every mode's report envelope; mode reports embed it.
type reportCommon struct {
	Schema               string          `json:"schema"`
	Mode                 string          `json:"mode"`
	Arm                  string          `json:"arm"`
	Status               string          `json:"status"`
	Error                *string         `json:"error"`
	SourceRevision       string          `json:"source_revision"`
	SourceManifestSHA256 string          `json:"source_manifest_sha256"`
	BinarySHA256         string          `json:"binary_sha256"`
	JobSHA256            string          `json:"job_sha256"`
	CorpusSHA256         *string         `json:"corpus_sha256"`
	LayoutSHA256         *string         `json:"layout_sha256"`
	ClusterSHA256        *string         `json:"cluster_sha256"`
	ClientLibrarySHA256  *string         `json:"client_library_sha256"`
	Boundary             string          `json:"boundary"`
	EffectiveJob         json.RawMessage `json:"effective_job"`
	Artifacts            []artifact      `json:"artifacts"`
}

func (r *reportCommon) common() *reportCommon { return r }

type report interface{ common() *reportCommon }

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func newCommon(j *job, jobSHA256 string, effective json.RawMessage, boundary string) reportCommon {
	r := reportCommon{
		Schema: reportSchema, Mode: j.Mode, Arm: j.Arm, Status: "error",
		SourceRevision: j.SourceRevision, SourceManifestSHA256: j.SourceManifestSHA256,
		BinarySHA256: j.BinarySHA256, JobSHA256: jobSHA256,
		CorpusSHA256: optional(j.CorpusSHA256), LayoutSHA256: optional(j.LayoutSHA256),
		ClusterSHA256: optional(j.ClusterSHA256), ClientLibrarySHA256: j.ClientLibrarySHA256,
		Boundary: boundary, EffectiveJob: effective, Artifacts: []artifact{},
	}
	return r
}

func fileArtifact(kind, path string) (artifact, error) {
	h, err := hashFile(path)
	if err != nil {
		return artifact{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return artifact{}, err
	}
	return artifact{Kind: kind, Path: path, SHA256: h, Bytes: info.Size()}, nil
}

// newOutput creates a file that did not exist, readable only by its owner.
func newOutput(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
}

func writeReport(f *os.File, r report) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// run executes the job at jobPath. Job, seal and path failures return before the
// report is reserved; after that every failure is a synced status=error report.
func run(rt runtime, jobPath string) error {
	start := time.Now()
	if !cleanAbsolute(jobPath) {
		return invalid("job path")
	}
	data, jobSHA256, err := readSealed(jobPath, maxJobBytes)
	if err != nil {
		return err
	}
	j, err := parseJob(data)
	if err != nil {
		return err
	}
	if err := validate(j, jobPath); err != nil {
		return err
	}
	if err := checkFiles(j, jobPath); err != nil {
		return err
	}
	layout, err := checkSeals(rt, j)
	if err != nil {
		return err
	}
	var effective bytes.Buffer
	if err := json.Compact(&effective, data); err != nil {
		return invalid("JSON")
	}
	out, err := newOutput(j.Output)
	if err != nil {
		return refuse("output must be a new writable file")
	}
	defer out.Close()
	ctx, cancel := context.WithDeadline(context.Background(), start.Add(time.Duration(j.TimeoutSeconds)*time.Second))
	defer cancel()
	var r report
	var outcome error
	switch j.Mode {
	case "corpus":
		cr := &corpusReport{reportCommon: newCommon(j, jobSHA256, effective.Bytes(), corpusBoundary), CorpusConfig: j.CorpusConfig}
		r, outcome = cr, generateCorpus(ctx, j, cr)
	case "setup":
		sr := &setupReport{reportCommon: newCommon(j, jobSHA256, effective.Bytes(), setupBoundary)}
		r, outcome = sr, runSetup(ctx, rt, j, sr)
	case "ingest":
		ir := newIngestReport(j, newCommon(j, jobSHA256, effective.Bytes(), ingestBoundary))
		r, outcome = ir, runIngest(ctx, rt, j, layout, ir)
	default:
		vr := newVerifyReport(newCommon(j, jobSHA256, effective.Bytes(), verifyBoundary))
		r, outcome = vr, runVerify(ctx, rt, j, layout, vr)
	}
	var unsettled *unsettledError
	if errors.As(outcome, &unsettled) {
		return completeUnsettled(out, j, jobSHA256, unsettled)
	}
	c := r.common()
	if outcome != nil {
		msg := outcome.Error()
		c.Status, c.Error = "error", &msg
	} else {
		c.Status, c.Error = "ok", nil
	}
	if err := writeReport(out, r); err != nil {
		return fmt.Errorf("cannot write the run report: %w", err)
	}
	return outcome
}

// unsettledKind names the diagnostic of an ingest whose Collector never settled.
const unsettledKind = "otel_control_unsettled_collector_v1"

// diagnosticPath is where an unsettled ingest writes its diagnostic instead of a report.
func diagnosticPath(output string) string { return output + ".unsettled-diagnostic.json" }

// unsettledDiagnostic holds only what is stable once the producers have returned; it
// is not a control report and never eligible for comparison.
type unsettledDiagnostic struct {
	DiagnosticKind     string  `json:"diagnostic_kind"`
	ComparisonEligible bool    `json:"comparison_eligible"`
	Mode               string  `json:"mode"`
	Arm                string  `json:"arm"`
	JobSHA256          string  `json:"job_sha256"`
	BinarySHA256       string  `json:"binary_sha256"`
	Explanation        string  `json:"explanation"`
	Frames             int64   `json:"frames"`
	ReceivedSpans      int64   `json:"received_spans"`
	AdmittedSpans      int64   `json:"admitted_spans"`
	FirstError         *string `json:"first_error"`
}

// completeUnsettled replaces the run's reserved, still empty report with the
// diagnostic: it removes only the file this run created, creates the diagnostic
// exclusively and syncs it, and always returns a failure.
func completeUnsettled(out *os.File, j *job, jobSHA256 string, u *unsettledError) error {
	mine, statErr := out.Stat()
	out.Close()
	if statErr == nil {
		if cur, err := os.Lstat(j.Output); err == nil && os.SameFile(mine, cur) && cur.Size() == 0 {
			if err := os.Remove(j.Output); err != nil {
				return errors.Join(u, refuse("cannot remove the reserved report"))
			}
		}
	}
	d := unsettledDiagnostic{
		DiagnosticKind: unsettledKind, Mode: j.Mode, Arm: j.Arm, JobSHA256: jobSHA256, BinarySHA256: j.BinarySHA256,
		Explanation: "the Collector's workers did not join within the bounded join-only wait after the job deadline canceled their work; no settled counters, resources, timings or report exist",
		Frames:      u.frames, ReceivedSpans: u.received, AdmittedSpans: u.admitted,
	}
	if u.first != nil {
		msg := u.first.Error()
		d.FirstError = &msg
	}
	b, err := json.Marshal(d)
	if err != nil {
		return errors.Join(u, err)
	}
	f, err := newOutput(diagnosticPath(j.Output))
	if err != nil {
		return errors.Join(u, refuse("the unsettled diagnostic path is not a new writable file"))
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return errors.Join(u, err)
	}
	if err := f.Sync(); err != nil {
		return errors.Join(u, err)
	}
	return u
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: otelrust-control ABSOLUTE_JOB_JSON")
		os.Exit(2)
	}
	if err := run(productionRuntime(), os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
