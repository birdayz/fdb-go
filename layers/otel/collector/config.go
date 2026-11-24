package collector

import (
	"errors"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"fdb.dev/layers/otel"
	"gopkg.in/yaml.v3"
)

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

// Config puts otel.ExporterConfig keys at the top level of the fdb_otel body,
// alongside timeout, sending_queue and retry_on_failure.
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
// A key the relational store removed (order, hash_bytes,
// posting_value_format) is refused as removed rather than unknown.
func (c *Config) UnmarshalYAML(node *yaml.Node) error {
	type plain Config
	p := plain(defaultConfig())
	if err := checkKeys(node, ComponentType, configTypes...); err != nil {
		if removed := removedExporterKey(node); removed != nil {
			return removed
		}
		return err
	}
	if err := node.Decode(&p); err != nil {
		return err
	}
	if err := exporterBody(node).Decode(&p.Exporter); err != nil {
		return err
	}
	*c = Config(p)
	return nil
}

// removedExporterKey is otel.ExporterConfig's refusal of a key the
// relational store removed, or nil when the flattened body names none.
func removedExporterKey(node *yaml.Node) error {
	var scratch otel.ExporterConfig
	var removed *otel.RemovedConfigKeyError
	if err := exporterBody(node).Decode(&scratch); errors.As(err, &removed) {
		return err
	}
	return nil
}

// exporterBody is node without Config's own keys: the flattened
// otel.ExporterConfig body.
func exporterBody(node *yaml.Node) *yaml.Node {
	if node.Kind == yaml.AliasNode {
		node = node.Alias
	}
	if node.Kind != yaml.MappingNode {
		return node
	}
	own := map[string]bool{}
	t := reflect.TypeOf(Config{})
	for i := range t.NumField() {
		if name, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ","); name != "" && name != "-" {
			own[name] = true
		}
	}
	out := *node
	out.Content = nil
	for i := 0; i+1 < len(node.Content); i += 2 {
		if !own[node.Content[i].Value] {
			out.Content = append(out.Content, node.Content[i], node.Content[i+1])
		}
	}
	return &out
}

// Validate checks the FDB exporter settings and the queue/retry bounds.
func (c *Config) Validate() error {
	var errs []error
	if err := c.Exporter.Validate(); err != nil {
		errs = append(errs, err)
	}
	if c.Timeout <= 0 {
		errs = append(errs, componentErrorf("timeout must be positive"))
	}
	if q := c.SendingQueue; q.Enabled {
		if q.NumConsumers < 1 || q.NumConsumers > maxConsumers {
			errs = append(errs, componentErrorf("sending_queue.num_consumers must be 1..%d", maxConsumers))
		}
		if q.QueueSize < 1 || q.QueueSize > maxQueueSize {
			errs = append(errs, componentErrorf("sending_queue.queue_size must be 1..%d spans", maxQueueSize))
		}
		if q.Batch.FlushTimeout <= 0 {
			errs = append(errs, componentErrorf("sending_queue.batch.flush_timeout must be positive"))
		}
		if q.Batch.MinSize < 1 || q.Batch.MinSize > q.QueueSize {
			errs = append(errs, componentErrorf("sending_queue.batch.min_size must be 1..queue_size"))
		}
		if q.Batch.MaxSize < q.Batch.MinSize {
			errs = append(errs, componentErrorf("sending_queue.batch.max_size must be at least min_size"))
		}
	}
	if r := c.RetryOnFailure; r.Enabled {
		if r.InitialInterval <= 0 {
			errs = append(errs, componentErrorf("retry_on_failure.initial_interval must be positive"))
		}
		if r.MaxInterval < r.InitialInterval {
			errs = append(errs, componentErrorf("retry_on_failure.max_interval must be at least initial_interval"))
		}
		if r.MaxElapsedTime < 0 {
			errs = append(errs, componentErrorf("retry_on_failure.max_elapsed_time must not be negative"))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return componentErrorf("%s config: %w", ComponentType, err)
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
			return componentErrorf("%s: unknown key %q", path, key)
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
		return ComponentID{}, componentErrorf("invalid component id %q: type must match %s", s, componentTypeRE)
	}
	if named && (name == "" || len(name) > 1024 || strings.ContainsFunc(name, func(r rune) bool { return r <= ' ' || r == 0x7f })) {
		return ComponentID{}, componentErrorf("invalid component id %q: name must be 1..1024 non-space characters", s)
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

// CollectorConfig is the supported subset of Collector-model YAML configuration. Every
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
		return nil, componentErrorf("collector config: %w", err)
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
		return nil, componentErrorf("collector config: %w", err)
	}
	var raw rawCollector
	if err := root.Decode(&raw); err != nil {
		return nil, componentErrorf("collector config: %w", err)
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
				return nil, componentErrorf("collector config: %s: %w", section.name, err)
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
			errs = append(errs, componentErrorf("exporters: %s: %w", id, err))
			continue
		}
		out.FDBExporters[id] = cfg
	}
	if out.ServiceExtensions, err = resolveRefs("service.extensions", raw.Service.Extensions, out.Extensions); err != nil {
		errs = append(errs, err)
	}
	if len(raw.Service.Pipelines) == 0 {
		errs = append(errs, componentErrorf("service.pipelines must define at least one pipeline"))
	}
	for key, rp := range raw.Service.Pipelines {
		id, err := ParseComponentID(key)
		if err != nil {
			errs = append(errs, componentErrorf("service.pipelines: %w", err))
			continue
		}
		path := "service.pipelines." + key
		switch id.Type {
		case "traces", "metrics", "logs", "profiles":
		default:
			errs = append(errs, componentErrorf("%s: unknown signal %q", path, id.Type))
			continue
		}
		p := &Pipeline{}
		var e1, e2, e3 error
		p.Receivers, e1 = resolveRefs(path+".receivers", rp.Receivers, out.Receivers, out.Connectors)
		p.Processors, e2 = resolveRefs(path+".processors", rp.Processors, out.Processors)
		p.Exporters, e3 = resolveRefs(path+".exporters", rp.Exporters, out.Exporters, out.Connectors)
		errs = append(errs, e1, e2, e3)
		if len(rp.Receivers) == 0 {
			errs = append(errs, componentErrorf("%s: must have at least one receiver", path))
		}
		if len(rp.Exporters) == 0 {
			errs = append(errs, componentErrorf("%s: must have at least one exporter", path))
		}
		if id.Type != "traces" {
			for _, exp := range p.Exporters {
				if exp.Type == ComponentType {
					errs = append(errs, componentErrorf("%s: exporter %s supports only the traces signal", path, exp))
				}
			}
		}
		out.Pipelines[id] = p
	}
	if err := errors.Join(errs...); err != nil {
		return nil, componentErrorf("collector config: %w", err)
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
		return nil, componentErrorf("%s: exporter config must be a mapping", ComponentType)
	}
	if err := node.Decode(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func parseDocument(data []byte, lookup func(string) (string, bool)) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, componentErrorf("collector config: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return nil, componentErrorf("collector config: empty document")
	}
	root := doc.Content[0]
	if err := expandNode(root, lookup, map[*yaml.Node]bool{}); err != nil {
		return nil, componentErrorf("collector config: %w", err)
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
				return "", componentErrorf("unterminated reference in %q", s)
			}
			ref := strings.TrimPrefix(s[i+2:i+end], "env:")
			name, def, hasDefault := strings.Cut(ref, ":-")
			if !envNameRE.MatchString(name) {
				return "", componentErrorf("invalid environment reference %q", s[i:i+end+1])
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
			errs = append(errs, componentErrorf("%s: %w", path, err))
			continue
		}
		if seen[id] {
			errs = append(errs, componentErrorf("%s: %s referenced more than once", path, id))
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
			errs = append(errs, componentErrorf("%s: %s is not configured", path, id))
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
