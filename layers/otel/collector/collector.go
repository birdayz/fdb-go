// Package collector adapts the FDB OTel exporter to the OpenTelemetry Collector
// exporter model: a typed component ("fdb_otel"), a factory, Collector YAML
// configuration, and a bounded sending queue that batches spans from concurrent
// upstream sources before flushing them through *otel.Exporter.
//
// The package mirrors the Collector component contracts (factory, default
// config, Start/Shutdown, ConsumeTraces, exporterhelper sending_queue and
// retry_on_failure settings) on OTLP protobuf ResourceSpans, so it carries no
// dependency on the Collector module graph.
package collector

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"fdb.dev/layers/otel"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"gopkg.in/yaml.v3"
)

const (
	// ComponentType is the exporter type key in Collector YAML.
	ComponentType = "fdb_otel"
	// TracesStability is the stability level of the traces exporter.
	TracesStability = "alpha"

	maxConsumers = 64
	maxQueueSize = 10_000_000
	maxFailures  = 64
)

var (
	// ErrQueueFull rejects a request when the sending queue lacks capacity and
	// block_on_overflow is off. The request was not admitted; it is retryable.
	ErrQueueFull = errors.New("fdb_otel: sending queue is full")
	// ErrNotRunning rejects work before Start or after Shutdown.
	ErrNotRunning = errors.New("fdb_otel: exporter is not running")
)

// Sink is the export surface the queue flushes into; *otel.Exporter implements it.
type Sink interface {
	ExportResourceSpans(context.Context, []*tracepb.ResourceSpans) (otel.ExportStats, error)
	Shutdown(context.Context) error
}

var _ Sink = (*otel.Exporter)(nil)

type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

func permanent(format string, args ...any) error {
	return &permanentError{fmt.Errorf("fdb_otel: "+format, args...)}
}

// IsPermanent reports whether retrying err cannot succeed: invalid input,
// configuration, ownership or corruption failures, or a shut-down exporter.
// Transient FDB, budget and per-attempt timeout failures are retryable because
// ExportResourceSpans is an idempotent replace of each span identity.
func IsPermanent(err error) bool {
	if err == nil {
		return false
	}
	var p *permanentError
	if errors.As(err, &p) {
		return true
	}
	var oe *otel.OtelError
	if errors.As(err, &oe) {
		switch oe.Kind {
		case "config", "admission", "ownership", "corruption", "operation":
			return true
		}
	}
	return false
}

// BatchConfig mirrors exporterhelper sending_queue.batch, sized in spans.
type BatchConfig struct {
	FlushTimeout time.Duration `yaml:"flush_timeout"`
	MinSize      int           `yaml:"min_size"`
	MaxSize      int           `yaml:"max_size"`
}

// QueueConfig mirrors exporterhelper sending_queue, sized in spans.
type QueueConfig struct {
	Enabled         bool        `yaml:"enabled"`
	NumConsumers    int         `yaml:"num_consumers"`
	QueueSize       int         `yaml:"queue_size"`
	BlockOnOverflow bool        `yaml:"block_on_overflow"`
	Batch           BatchConfig `yaml:"batch"`
}

// RetryConfig mirrors exporterhelper retry_on_failure (exponential backoff).
// MaxElapsedTime 0 retries until the exporter shuts down.
type RetryConfig struct {
	Enabled         bool          `yaml:"enabled"`
	InitialInterval time.Duration `yaml:"initial_interval"`
	MaxInterval     time.Duration `yaml:"max_interval"`
	MaxElapsedTime  time.Duration `yaml:"max_elapsed_time"`
}

// Config is the fdb_otel exporter configuration. The otel.ExporterConfig keys
// (cluster_file, run, tenant, ...) sit at the top level of the exporter body,
// next to timeout, sending_queue and retry_on_failure.
type Config struct {
	Exporter       otel.ExporterConfig `yaml:"-"`
	Timeout        time.Duration       `yaml:"timeout"`
	SendingQueue   QueueConfig         `yaml:"sending_queue"`
	RetryOnFailure RetryConfig         `yaml:"retry_on_failure"`
}

func defaultConfig() Config {
	return Config{
		Exporter: otel.DefaultExporterConfig(),
		Timeout:  30 * time.Second,
		SendingQueue: QueueConfig{
			Enabled:      true,
			NumConsumers: 4,
			QueueSize:    10000,
			Batch:        BatchConfig{FlushTimeout: 200 * time.Millisecond, MinSize: 512, MaxSize: 4096},
		},
		RetryOnFailure: RetryConfig{Enabled: true, InitialInterval: 5 * time.Second, MaxInterval: 30 * time.Second, MaxElapsedTime: 300 * time.Second},
	}
}

var (
	durationType = reflect.TypeOf(time.Duration(0))
	nodeType     = reflect.TypeOf(yaml.Node{})
	configTypes  = []reflect.Type{reflect.TypeOf(otel.ExporterConfig{}), reflect.TypeOf(Config{})}
)

// UnmarshalYAML starts from the factory defaults, rejects unknown keys, and
// decodes the flattened otel.ExporterConfig keys alongside the queue settings.
func (c *Config) UnmarshalYAML(node *yaml.Node) error {
	if err := checkKeys(node, ComponentType, configTypes...); err != nil {
		return err
	}
	type plain Config
	p := plain(defaultConfig())
	if err := node.Decode(&p); err != nil {
		return err
	}
	if err := node.Decode(&p.Exporter); err != nil {
		return err
	}
	*c = Config(p)
	return nil
}

// Validate checks the FDB exporter settings and the queue/retry bounds.
func (c *Config) Validate() error {
	var errs []error
	if err := c.Exporter.Validate(); err != nil {
		errs = append(errs, err)
	}
	if c.Timeout <= 0 {
		errs = append(errs, errors.New("timeout must be positive"))
	}
	if q := c.SendingQueue; q.Enabled {
		if q.NumConsumers < 1 || q.NumConsumers > maxConsumers {
			errs = append(errs, fmt.Errorf("sending_queue.num_consumers must be 1..%d", maxConsumers))
		}
		if q.QueueSize < 1 || q.QueueSize > maxQueueSize {
			errs = append(errs, fmt.Errorf("sending_queue.queue_size must be 1..%d spans", maxQueueSize))
		}
		if q.Batch.FlushTimeout <= 0 {
			errs = append(errs, errors.New("sending_queue.batch.flush_timeout must be positive"))
		}
		if q.Batch.MinSize < 1 || q.Batch.MinSize > q.QueueSize {
			errs = append(errs, errors.New("sending_queue.batch.min_size must be 1..queue_size"))
		}
		if q.Batch.MaxSize < q.Batch.MinSize {
			errs = append(errs, errors.New("sending_queue.batch.max_size must be at least min_size"))
		}
	}
	if r := c.RetryOnFailure; r.Enabled {
		if r.InitialInterval <= 0 {
			errs = append(errs, errors.New("retry_on_failure.initial_interval must be positive"))
		}
		if r.MaxInterval < r.InitialInterval {
			errs = append(errs, errors.New("retry_on_failure.max_interval must be at least initial_interval"))
		}
		if r.MaxElapsedTime < 0 {
			errs = append(errs, errors.New("retry_on_failure.max_elapsed_time must not be negative"))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("%s config: %w", ComponentType, err)
	}
	return nil
}

// checkKeys rejects mapping keys absent from the yaml tags of types,
// recursing into nested structs and maps of structs.
func checkKeys(node *yaml.Node, path string, types ...reflect.Type) error {
	if node.Kind == yaml.AliasNode {
		node = node.Alias
	}
	if node.Kind != yaml.MappingNode {
		return nil // type mismatches surface in Decode.
	}
	fields := map[string]reflect.Type{}
	for _, t := range types {
		for i := range t.NumField() {
			f := t.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
			if name != "" && name != "-" {
				fields[name] = f.Type
			}
		}
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		t, ok := fields[key]
		if !ok {
			return fmt.Errorf("%s: unknown key %q", path, key)
		}
		if err := checkValue(node.Content[i+1], path+"."+key, t); err != nil {
			return err
		}
	}
	return nil
}

func checkValue(node *yaml.Node, path string, t reflect.Type) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch {
	case t == durationType || t == nodeType:
		return nil
	case t.Kind() == reflect.Struct:
		return checkKeys(node, path, t)
	case t.Kind() == reflect.Map && t.Elem().Kind() == reflect.Struct && t.Elem() != nodeType:
		if node.Kind == yaml.AliasNode {
			node = node.Alias
		}
		if node.Kind != yaml.MappingNode {
			return nil
		}
		for i := 0; i+1 < len(node.Content); i += 2 {
			if err := checkValue(node.Content[i+1], path+"."+node.Content[i].Value, t.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}

// ComponentID is a Collector component or pipeline identity: type[/name].
type ComponentID struct {
	Type string
	Name string
}

var componentTypeRE = regexp.MustCompile(`^[a-zA-Z][0-9a-zA-Z_]{0,62}$`)

func ParseComponentID(s string) (ComponentID, error) {
	typ, name, named := strings.Cut(s, "/")
	if !componentTypeRE.MatchString(typ) {
		return ComponentID{}, fmt.Errorf("invalid component id %q: type must match %s", s, componentTypeRE)
	}
	if named && (name == "" || len(name) > 1024 || strings.ContainsFunc(name, func(r rune) bool { return r <= ' ' || r == 0x7f })) {
		return ComponentID{}, fmt.Errorf("invalid component id %q: name must be 1..1024 non-space characters", s)
	}
	return ComponentID{Type: typ, Name: name}, nil
}

func (id ComponentID) String() string {
	if id.Name == "" {
		return id.Type
	}
	return id.Type + "/" + id.Name
}

func compareIDs(a, b ComponentID) int { return strings.Compare(a.String(), b.String()) }

// Pipeline lists the components of one service pipeline in declared order.
type Pipeline struct {
	Receivers  []ComponentID
	Processors []ComponentID
	Exporters  []ComponentID
}

// CollectorConfig is a parsed OpenTelemetry Collector configuration. Every
// component keeps its raw YAML; fdb_otel exporters are also decoded.
type CollectorConfig struct {
	Receivers  map[ComponentID]*yaml.Node
	Processors map[ComponentID]*yaml.Node
	Exporters  map[ComponentID]*yaml.Node
	Connectors map[ComponentID]*yaml.Node
	Extensions map[ComponentID]*yaml.Node

	FDBExporters      map[ComponentID]*Config
	ServiceExtensions []ComponentID
	Pipelines         map[ComponentID]*Pipeline
}

type rawPipeline struct {
	Receivers  []string `yaml:"receivers"`
	Processors []string `yaml:"processors"`
	Exporters  []string `yaml:"exporters"`
}

type rawService struct {
	Extensions []string               `yaml:"extensions"`
	Pipelines  map[string]rawPipeline `yaml:"pipelines"`
	Telemetry  yaml.Node              `yaml:"telemetry"`
}

type rawCollector struct {
	Receivers  map[string]yaml.Node `yaml:"receivers"`
	Processors map[string]yaml.Node `yaml:"processors"`
	Exporters  map[string]yaml.Node `yaml:"exporters"`
	Connectors map[string]yaml.Node `yaml:"connectors"`
	Extensions map[string]yaml.Node `yaml:"extensions"`
	Service    rawService           `yaml:"service"`
}

// LoadPipelineConfig reads and validates a Collector YAML file.
func LoadPipelineConfig(path string) (*CollectorConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("collector config: %w", err)
	}
	return ParseConfig(data)
}

// ParseConfig parses and validates a Collector YAML document, expanding
// ${env:NAME}, ${NAME} and ${env:NAME:-default} references in scalar values.
func ParseConfig(data []byte) (*CollectorConfig, error) {
	return parseConfig(data, os.LookupEnv)
}

func parseConfig(data []byte, lookup func(string) (string, bool)) (*CollectorConfig, error) {
	root, err := parseDocument(data, lookup)
	if err != nil {
		return nil, err
	}
	if err := checkKeys(root, "config", reflect.TypeOf(rawCollector{})); err != nil {
		return nil, fmt.Errorf("collector config: %w", err)
	}
	var raw rawCollector
	if err := root.Decode(&raw); err != nil {
		return nil, fmt.Errorf("collector config: %w", err)
	}
	out := &CollectorConfig{FDBExporters: map[ComponentID]*Config{}, Pipelines: map[ComponentID]*Pipeline{}}
	for _, section := range []struct {
		name string
		in   map[string]yaml.Node
		out  *map[ComponentID]*yaml.Node
	}{{"receivers", raw.Receivers, &out.Receivers}, {"processors", raw.Processors, &out.Processors}, {"exporters", raw.Exporters, &out.Exporters}, {"connectors", raw.Connectors, &out.Connectors}, {"extensions", raw.Extensions, &out.Extensions}} {
		*section.out = make(map[ComponentID]*yaml.Node, len(section.in))
		for key, node := range section.in {
			id, err := ParseComponentID(key)
			if err != nil {
				return nil, fmt.Errorf("collector config: %s: %w", section.name, err)
			}
			(*section.out)[id] = &node
		}
	}
	var errs []error
	for id, node := range out.Exporters {
		if id.Type != ComponentType {
			continue
		}
		cfg, err := decodeExporterNode(node)
		if err == nil {
			err = cfg.Validate()
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("exporters: %s: %w", id, err))
			continue
		}
		out.FDBExporters[id] = cfg
	}
	if out.ServiceExtensions, err = resolveRefs("service.extensions", raw.Service.Extensions, out.Extensions); err != nil {
		errs = append(errs, err)
	}
	if len(raw.Service.Pipelines) == 0 {
		errs = append(errs, errors.New("service.pipelines must define at least one pipeline"))
	}
	for key, rp := range raw.Service.Pipelines {
		id, err := ParseComponentID(key)
		if err != nil {
			errs = append(errs, fmt.Errorf("service.pipelines: %w", err))
			continue
		}
		path := "service.pipelines." + key
		switch id.Type {
		case "traces", "metrics", "logs", "profiles":
		default:
			errs = append(errs, fmt.Errorf("%s: unknown signal %q", path, id.Type))
			continue
		}
		p := &Pipeline{}
		var e1, e2, e3 error
		p.Receivers, e1 = resolveRefs(path+".receivers", rp.Receivers, out.Receivers, out.Connectors)
		p.Processors, e2 = resolveRefs(path+".processors", rp.Processors, out.Processors)
		p.Exporters, e3 = resolveRefs(path+".exporters", rp.Exporters, out.Exporters, out.Connectors)
		errs = append(errs, e1, e2, e3)
		if len(rp.Receivers) == 0 {
			errs = append(errs, fmt.Errorf("%s: must have at least one receiver", path))
		}
		if len(rp.Exporters) == 0 {
			errs = append(errs, fmt.Errorf("%s: must have at least one exporter", path))
		}
		if id.Type != "traces" {
			for _, exp := range p.Exporters {
				if exp.Type == ComponentType {
					errs = append(errs, fmt.Errorf("%s: exporter %s supports only the traces signal", path, exp))
				}
			}
		}
		out.Pipelines[id] = p
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("collector config: %w", err)
	}
	return out, nil
}

// ParseExporterConfig decodes and validates one fdb_otel exporter body (the
// mapping under exporters: fdb_otel:).
func ParseExporterConfig(data []byte) (*Config, error) {
	root, err := parseDocument(data, os.LookupEnv)
	if err != nil {
		return nil, err
	}
	cfg, err := decodeExporterNode(root)
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func decodeExporterNode(node *yaml.Node) (*Config, error) {
	cfg := NewFactory().CreateDefaultConfig()
	if node.Kind == yaml.ScalarNode && node.ShortTag() == "!!null" {
		return cfg, nil
	}
	if node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: exporter config must be a mapping", ComponentType)
	}
	if err := node.Decode(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func parseDocument(data []byte, lookup func(string) (string, bool)) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("collector config: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return nil, errors.New("collector config: empty document")
	}
	root := doc.Content[0]
	if err := expandNode(root, lookup, map[*yaml.Node]bool{}); err != nil {
		return nil, fmt.Errorf("collector config: %w", err)
	}
	return root, nil
}

// expandNode substitutes environment references in scalar values. A plain
// scalar is re-resolved after substitution so ${env:N} can supply an int.
func expandNode(n *yaml.Node, lookup func(string) (string, bool), seen map[*yaml.Node]bool) error {
	if seen[n] {
		return nil
	}
	seen[n] = true
	switch n.Kind {
	case yaml.ScalarNode:
		if !strings.Contains(n.Value, "$") {
			return nil
		}
		v, err := expandString(n.Value, lookup)
		if err != nil {
			return err
		}
		if v != n.Value {
			n.Value = v
			if n.Style == 0 {
				n.Tag = ""
			}
		}
	case yaml.MappingNode:
		for i := 1; i < len(n.Content); i += 2 {
			if err := expandNode(n.Content[i], lookup, seen); err != nil {
				return err
			}
		}
	case yaml.SequenceNode, yaml.DocumentNode:
		for _, c := range n.Content {
			if err := expandNode(c, lookup, seen); err != nil {
				return err
			}
		}
	}
	return nil
}

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func expandString(s string, lookup func(string) (string, bool)) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], "$$"):
			b.WriteByte('$')
			i += 2
		case strings.HasPrefix(s[i:], "${"):
			end := strings.IndexByte(s[i:], '}')
			if end < 0 {
				return "", fmt.Errorf("unterminated reference in %q", s)
			}
			ref := strings.TrimPrefix(s[i+2:i+end], "env:")
			name, def, hasDefault := strings.Cut(ref, ":-")
			if !envNameRE.MatchString(name) {
				return "", fmt.Errorf("invalid environment reference %q", s[i:i+end+1])
			}
			v, ok := lookup(name)
			if !ok && hasDefault {
				v = def
			}
			b.WriteString(v)
			i += end + 1
		default:
			b.WriteByte(s[i])
			i++
		}
	}
	return b.String(), nil
}

func resolveRefs(path string, refs []string, sets ...map[ComponentID]*yaml.Node) ([]ComponentID, error) {
	out := make([]ComponentID, 0, len(refs))
	seen := map[ComponentID]bool{}
	var errs []error
	for _, ref := range refs {
		id, err := ParseComponentID(ref)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
			continue
		}
		if seen[id] {
			errs = append(errs, fmt.Errorf("%s: %s referenced more than once", path, id))
			continue
		}
		seen[id] = true
		found := false
		for _, set := range sets {
			if _, ok := set[id]; ok {
				found = true
			}
		}
		if !found {
			errs = append(errs, fmt.Errorf("%s: %s is not configured", path, id))
			continue
		}
		out = append(out, id)
	}
	return out, errors.Join(errs...)
}

// TracesExporters returns the fdb_otel exporters used by traces pipelines.
func (c *CollectorConfig) TracesExporters() []ComponentID {
	var out []ComponentID
	for id, p := range c.Pipelines {
		if id.Type != "traces" {
			continue
		}
		for _, exp := range p.Exporters {
			if exp.Type == ComponentType && !slices.Contains(out, exp) {
				out = append(out, exp)
			}
		}
	}
	slices.SortFunc(out, compareIDs)
	return out
}

// Settings carries the component identity; Open overrides how Start opens the
// sink (nil opens *otel.Exporter on the configured FDB cluster).
type Settings struct {
	ID   ComponentID
	Open func(context.Context, otel.ExporterConfig) (Sink, error)
}

// Factory creates fdb_otel traces exporters.
type Factory struct{}

func NewFactory() Factory { return Factory{} }

func (Factory) Type() string            { return ComponentType }
func (Factory) TracesStability() string { return TracesStability }

// CreateDefaultConfig returns defaults; cluster_file and run remain required.
func (Factory) CreateDefaultConfig() *Config {
	c := defaultConfig()
	return &c
}

// CreateTraces validates cfg and returns an exporter that opens FDB on Start.
func (Factory) CreateTraces(set Settings, cfg *Config) (*TracesExporter, error) {
	if cfg == nil {
		return nil, fmt.Errorf("%s: nil config", ComponentType)
	}
	if set.ID.Type == "" {
		set.ID.Type = ComponentType
	}
	if set.ID.Type != ComponentType {
		return nil, fmt.Errorf("%s: component id %s has the wrong type", ComponentType, set.ID)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	open := set.Open
	if open == nil {
		open = func(ctx context.Context, c otel.ExporterConfig) (Sink, error) {
			e, err := otel.NewExporter(ctx, c)
			if err != nil {
				return nil, err
			}
			return e, nil
		}
	}
	return &TracesExporter{id: set.ID, cfg: *cfg, open: open, remaining: map[uint64]int{}, wake: make(chan struct{}, 1), changed: make(chan struct{}), done: make(chan struct{})}, nil
}

// Capabilities mirrors consumer.Capabilities.
type Capabilities struct{ MutatesData bool }

// Stats are cumulative exporter counters; QueuedSpans is the current backlog
// of admitted spans not yet exported or failed.
type Stats struct {
	AcceptedRequests int64 `json:"accepted_requests"`
	AcceptedSpans    int64 `json:"accepted_spans"`
	RejectedSpans    int64 `json:"rejected_spans"`
	ExportedSpans    int64 `json:"exported_spans"`
	StoredSpans      int64 `json:"stored_spans"`
	DuplicateSpans   int64 `json:"duplicate_spans"`
	FailedSpans      int64 `json:"failed_spans"`
	Flushes          int64 `json:"flushes"`
	Exports          int64 `json:"exports"`
	Retries          int64 `json:"retries"`
	QueuedSpans      int   `json:"queued_spans"`
}

const (
	stateCreated = iota
	stateStarted
	stateStopping
	stateStopped
)

type request struct {
	seq   uint64
	spans []*tracepb.ResourceSpans
	count int
}

type batch struct {
	parts []request
	count int
}

func (b batch) resourceSpans() []*tracepb.ResourceSpans {
	var out []*tracepb.ResourceSpans
	for _, p := range b.parts {
		out = append(out, p.spans...)
	}
	return out
}

// TracesExporter is the fdb_otel traces exporter component. With the sending
// queue enabled, ConsumeTraces admits spans into a bounded queue (measured in
// spans) shared by all upstream sources; one batcher merges requests into
// batches of min_size..max_size spans (or whatever is queued after
// flush_timeout) and num_consumers workers export them with retry.
type TracesExporter struct {
	id   ComponentID
	cfg  Config
	open func(context.Context, otel.ExporterConfig) (Sink, error)

	mu          sync.Mutex
	state       int
	sink        Sink
	pending     []request
	flushWanted bool
	outstanding int
	nextSeq     uint64
	order       []uint64 // admitted, incomplete request sequences, ascending
	remaining   map[uint64]int
	failures    []error
	dropped     int
	stats       Stats
	shutdownErr error

	wake       chan struct{} // batcher wakeup, capacity 1
	changed    chan struct{} // closed and replaced on every completion
	batches    chan batch
	workCtx    context.Context
	cancelWork context.CancelFunc
	workers    sync.WaitGroup
	active     sync.WaitGroup // synchronous exports in flight
	done       chan struct{}
}

func (x *TracesExporter) ID() ComponentID            { return x.id }
func (x *TracesExporter) Config() Config             { return x.cfg }
func (x *TracesExporter) Capabilities() Capabilities { return Capabilities{} }

// Sink returns the opened sink (an *otel.Exporter by default), nil before Start.
func (x *TracesExporter) Sink() Sink {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.sink
}

func (x *TracesExporter) Stats() Stats {
	x.mu.Lock()
	defer x.mu.Unlock()
	s := x.stats
	s.QueuedSpans = x.outstanding
	return s
}

// Start opens the sink and starts the batcher and consumers.
func (x *TracesExporter) Start(ctx context.Context) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.state != stateCreated {
		return fmt.Errorf("%s: %s already started", ComponentType, x.id)
	}
	sink, err := x.open(ctx, x.cfg.Exporter)
	if err != nil {
		return err
	}
	x.sink = sink
	x.workCtx, x.cancelWork = context.WithCancel(context.Background())
	x.state = stateStarted
	if q := x.cfg.SendingQueue; q.Enabled {
		x.batches = make(chan batch)
		x.workers.Add(1 + q.NumConsumers)
		go x.batchLoop()
		for range q.NumConsumers {
			go x.consumeLoop()
		}
	}
	return nil
}

// ConsumeTraces admits spans for export. Spans with invalid identities fail
// the whole request synchronously, so one source cannot poison a merged batch.
// With the queue enabled the call returns once admitted; export failures are
// reported by Flush and Stats. The exporter owns spans after the call.
func (x *TracesExporter) ConsumeTraces(ctx context.Context, spans []*tracepb.ResourceSpans) error {
	n, err := admitSpans(spans)
	if err != nil {
		x.reject(n)
		return err
	}
	if n == 0 {
		return nil
	}
	if !x.cfg.SendingQueue.Enabled {
		return x.exportSync(ctx, spans, n)
	}
	return x.enqueue(ctx, spans, n)
}

func (x *TracesExporter) reject(n int) {
	x.mu.Lock()
	x.stats.RejectedSpans += int64(n)
	x.mu.Unlock()
}

// admitSpans counts non-nil spans and applies the exporter's identity checks.
func admitSpans(in []*tracepb.ResourceSpans) (int, error) {
	n := 0
	var err error
	for _, rs := range in {
		for _, ss := range rs.GetScopeSpans() {
			for _, sp := range ss.GetSpans() {
				if sp == nil {
					continue
				}
				n++
				switch {
				case err != nil:
				case !nonzeroID(sp.TraceId, 16) || !nonzeroID(sp.SpanId, 8):
					err = permanent("span %d: expected nonzero 16-byte trace and 8-byte span identifiers", n)
				case sp.StartTimeUnixNano > math.MaxInt64:
					err = permanent("span %d: start_time_unix_nano exceeds int64", n)
				}
			}
		}
	}
	return n, err
}

func nonzeroID(b []byte, size int) bool {
	return len(b) == size && slices.ContainsFunc(b, func(c byte) bool { return c != 0 })
}

func (x *TracesExporter) exportSync(ctx context.Context, spans []*tracepb.ResourceSpans, n int) error {
	x.mu.Lock()
	if x.state != stateStarted {
		x.stats.RejectedSpans += int64(n)
		x.mu.Unlock()
		return ErrNotRunning
	}
	x.active.Add(1)
	x.stats.AcceptedRequests++
	x.stats.AcceptedSpans += int64(n)
	x.mu.Unlock()
	defer x.active.Done()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(x.workCtx, cancel)
	defer stop()
	st, err := x.exportWithRetry(ctx, spans)
	x.mu.Lock()
	defer x.mu.Unlock()
	x.record(st, n, err)
	return err
}

func (x *TracesExporter) enqueue(ctx context.Context, spans []*tracepb.ResourceSpans, n int) error {
	q := x.cfg.SendingQueue
	if n > q.QueueSize {
		x.reject(n)
		return permanent("request of %d spans exceeds sending_queue.queue_size %d", n, q.QueueSize)
	}
	x.mu.Lock()
	for {
		if x.state != stateStarted {
			x.stats.RejectedSpans += int64(n)
			x.mu.Unlock()
			return ErrNotRunning
		}
		if x.outstanding+n <= q.QueueSize {
			break
		}
		if !q.BlockOnOverflow {
			x.stats.RejectedSpans += int64(n)
			x.mu.Unlock()
			return ErrQueueFull
		}
		wait := x.changed
		x.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			x.reject(n)
			return fmt.Errorf("%w: %w", ErrQueueFull, ctx.Err())
		}
		x.mu.Lock()
	}
	seq := x.nextSeq
	x.nextSeq++
	x.outstanding += n
	x.remaining[seq] = n
	x.order = append(x.order, seq)
	x.pending = append(x.pending, request{seq: seq, spans: spans, count: n})
	x.stats.AcceptedRequests++
	x.stats.AcceptedSpans += int64(n)
	x.mu.Unlock()
	x.signal()
	return nil
}

func (x *TracesExporter) signal() {
	select {
	case x.wake <- struct{}{}:
	default:
	}
}

// Flush forces the queued partial batch out and waits until every request
// admitted before the call has been exported or failed. It returns the export
// failures recorded since the previous Flush.
func (x *TracesExporter) Flush(ctx context.Context) error {
	x.mu.Lock()
	if x.state == stateCreated {
		x.mu.Unlock()
		return ErrNotRunning
	}
	target := x.nextSeq
	x.flushWanted = true
	x.signal()
	for len(x.order) > 0 && x.order[0] < target {
		wait := x.changed
		x.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return ctx.Err()
		}
		x.mu.Lock()
	}
	errs := x.failures
	if x.dropped > 0 {
		errs = append(errs, fmt.Errorf("fdb_otel: %d further export failures omitted", x.dropped))
	}
	x.failures, x.dropped = nil, 0
	x.mu.Unlock()
	return errors.Join(errs...)
}

// Shutdown stops admission, drains the queue, and closes the sink. If ctx
// expires first, in-flight exports are canceled, remaining spans fail, and the
// sink still closes in the background.
func (x *TracesExporter) Shutdown(ctx context.Context) error {
	x.mu.Lock()
	cancel := x.cancelWork
	switch x.state {
	case stateCreated:
		x.state = stateStopped
		close(x.done)
	case stateStarted:
		x.state = stateStopping
		go func() {
			x.workers.Wait()
			x.active.Wait()
			err := x.sink.Shutdown(context.Background())
			x.mu.Lock()
			x.shutdownErr = err
			x.state = stateStopped
			x.mu.Unlock()
			x.cancelWork()
			close(x.done)
		}()
	}
	x.mu.Unlock()
	x.signal()
	select {
	case <-x.done:
	case <-ctx.Done():
		select {
		case <-x.done:
		default:
			if cancel != nil {
				cancel()
			}
			return fmt.Errorf("%s shutdown: %w", ComponentType, ctx.Err())
		}
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.shutdownErr
}

func (x *TracesExporter) batchLoop() {
	defer x.workers.Done()
	defer close(x.batches)
	q := x.cfg.SendingQueue
	timer := time.NewTimer(q.Batch.FlushTimeout)
	timer.Stop()
	armed, expired := false, false
	var cur []request
	count := 0
	for {
		x.mu.Lock()
		for _, r := range x.pending {
			count += r.count
		}
		cur = append(cur, x.pending...)
		x.pending = nil
		stopping := x.state != stateStarted
		force := expired || x.flushWanted || stopping
		x.flushWanted, expired = false, false
		x.mu.Unlock()
		dispatched := false
		for count > 0 && (count >= q.Batch.MinSize || force) {
			var b batch
			b, cur, count = cutBatch(cur, count, q.Batch.MaxSize)
			x.dispatch(b)
			dispatched = true
		}
		if dispatched || count == 0 {
			timer.Stop()
			armed = false
		}
		if count > 0 && !armed {
			timer.Reset(q.Batch.FlushTimeout)
			armed = true
		}
		if stopping && count == 0 {
			return
		}
		select {
		case <-x.wake:
		case <-timer.C:
			armed, expired = false, true
		}
	}
}

func (x *TracesExporter) dispatch(b batch) {
	select {
	case x.batches <- b:
		x.mu.Lock()
		x.stats.Flushes++
		x.mu.Unlock()
	case <-x.workCtx.Done():
		x.complete(b.parts, otel.ExportStats{}, fmt.Errorf("%s: export abandoned: %w", ComponentType, x.workCtx.Err()))
	}
}

// cutBatch takes up to max spans from the front of cur, splitting a request
// at a span boundary when needed.
func cutBatch(cur []request, count, max int) (batch, []request, int) {
	take := min(count, max)
	b := batch{count: take}
	for take > 0 {
		r := cur[0]
		if r.count <= take {
			b.parts = append(b.parts, r)
			take -= r.count
			cur = cur[1:]
			continue
		}
		head, tail := splitSpans(r.spans, take)
		b.parts = append(b.parts, request{seq: r.seq, spans: head, count: take})
		cur[0] = request{seq: r.seq, spans: tail, count: r.count - take}
		take = 0
	}
	return b, cur, count - b.count
}

// splitSpans moves the first n non-nil spans into head, sharing resources and
// scopes and keeping wrapper unknown fields on both sides.
func splitSpans(in []*tracepb.ResourceSpans, n int) (head, tail []*tracepb.ResourceSpans) {
	for _, rs := range in {
		if rs == nil {
			continue
		}
		var h, t *tracepb.ResourceSpans
		for _, ss := range rs.ScopeSpans {
			if ss == nil {
				continue
			}
			var hs, ts []*tracepb.Span
			for _, sp := range ss.Spans {
				switch {
				case sp == nil:
				case n > 0:
					hs = append(hs, sp)
					n--
				default:
					ts = append(ts, sp)
				}
			}
			if len(hs) > 0 {
				if h == nil {
					h = shallowResourceSpans(rs)
				}
				h.ScopeSpans = append(h.ScopeSpans, shallowScopeSpans(ss, hs))
			}
			if len(ts) > 0 {
				if t == nil {
					t = shallowResourceSpans(rs)
				}
				t.ScopeSpans = append(t.ScopeSpans, shallowScopeSpans(ss, ts))
			}
		}
		if h != nil {
			head = append(head, h)
		}
		if t != nil {
			tail = append(tail, t)
		}
	}
	return head, tail
}

func shallowResourceSpans(rs *tracepb.ResourceSpans) *tracepb.ResourceSpans {
	out := &tracepb.ResourceSpans{Resource: rs.Resource, SchemaUrl: rs.SchemaUrl}
	out.ProtoReflect().SetUnknown(rs.ProtoReflect().GetUnknown())
	return out
}

func shallowScopeSpans(ss *tracepb.ScopeSpans, spans []*tracepb.Span) *tracepb.ScopeSpans {
	out := &tracepb.ScopeSpans{Scope: ss.Scope, SchemaUrl: ss.SchemaUrl, Spans: spans}
	out.ProtoReflect().SetUnknown(ss.ProtoReflect().GetUnknown())
	return out
}

func (x *TracesExporter) consumeLoop() {
	defer x.workers.Done()
	for b := range x.batches {
		st, err := x.exportWithRetry(x.workCtx, b.resourceSpans())
		if err == nil || !IsPermanent(err) || len(b.parts) == 1 {
			x.complete(b.parts, st, err)
			continue
		}
		// A permanent failure fails the whole merged call before any write;
		// re-export each source's part so only the offending request fails.
		for _, p := range b.parts {
			st, err := x.exportWithRetry(x.workCtx, p.spans)
			x.complete([]request{p}, st, err)
		}
	}
}

func (x *TracesExporter) exportWithRetry(ctx context.Context, spans []*tracepb.ResourceSpans) (otel.ExportStats, error) {
	r := x.cfg.RetryOnFailure
	started := time.Now()
	interval := r.InitialInterval
	for {
		attempt, cancel := context.WithTimeout(ctx, x.cfg.Timeout)
		st, err := x.sink.ExportResourceSpans(attempt, spans)
		cancel()
		x.mu.Lock()
		x.stats.Exports++
		x.mu.Unlock()
		if err == nil || !r.Enabled || IsPermanent(err) || ctx.Err() != nil {
			return st, err
		}
		if r.MaxElapsedTime > 0 && time.Since(started)+interval > r.MaxElapsedTime {
			return st, fmt.Errorf("%s: retries exhausted after %s: %w", ComponentType, time.Since(started).Round(time.Millisecond), err)
		}
		x.mu.Lock()
		x.stats.Retries++
		x.mu.Unlock()
		wait := time.NewTimer(interval)
		select {
		case <-wait.C:
		case <-ctx.Done():
			wait.Stop()
			return st, errors.Join(err, ctx.Err())
		}
		interval = min(2*interval, r.MaxInterval)
	}
}

// record updates counters for one export outcome; callers hold x.mu.
func (x *TracesExporter) record(st otel.ExportStats, spans int, err error) {
	x.stats.StoredSpans += st.Spans
	x.stats.DuplicateSpans += st.DuplicateSpans
	if err != nil {
		x.stats.FailedSpans += int64(spans)
	} else {
		x.stats.ExportedSpans += int64(spans)
	}
}

func (x *TracesExporter) complete(parts []request, st otel.ExportStats, err error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	spans := 0
	for _, p := range parts {
		spans += p.count
		x.outstanding -= p.count
		if x.remaining[p.seq] -= p.count; x.remaining[p.seq] <= 0 {
			delete(x.remaining, p.seq)
		}
	}
	for len(x.order) > 0 {
		if _, open := x.remaining[x.order[0]]; open {
			break
		}
		x.order = x.order[1:]
	}
	x.record(st, spans, err)
	if err != nil {
		if len(x.failures) < maxFailures {
			x.failures = append(x.failures, fmt.Errorf("%d spans: %w", spans, err))
		} else {
			x.dropped++
		}
	}
	close(x.changed)
	x.changed = make(chan struct{})
}
