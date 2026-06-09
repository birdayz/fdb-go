package foundationdb

import (
	"context"
	"fmt"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
)

// options holds the resolved configuration for a FoundationDB container.
type options struct {
	database       string
	apiVersion     int
	version        string
	fdbPort        int
	tenantMode     string
	storageEngine  string
	redundancyMode string
	autoInit       bool
	directIP       bool
	startupTimeout time.Duration
	network        *testcontainers.DockerNetwork // user-provided network (nil = create one)
	networkAliases []string
	knobs          map[string]string // server knobs: --knob_NAME=VALUE
	processCount   int               // fdbserver processes per container (default 1)
	dataOnDisk     bool              // store /var/fdb/data on disk (not tmpfs) for large datasets
}

func defaultOptions() options {
	return options{
		database:       "test",
		apiVersion:     defaultAPIVersion,
		version:        fdbVersion(),
		fdbPort:        defaultFDBPort,
		tenantMode:     "optional_experimental",
		storageEngine:  "memory",
		redundancyMode: "single",
		autoInit:       true,
		startupTimeout: 60 * time.Second,
		processCount:   1,
	}
}

// Option configures the FoundationDB container. Options implement
// [testcontainers.ContainerCustomizer] so they can be mixed with standard
// testcontainers options in the [Run] call.
type Option struct {
	applyFn func(*options) error
}

// Customize implements [testcontainers.ContainerCustomizer]. Module options
// don't modify the container request directly — they configure our options struct.
func (o Option) Customize(_ *testcontainers.GenericContainerRequest) error {
	return nil
}

func (o Option) apply(cfg *options) error {
	return o.applyFn(cfg)
}

// WithDatabase sets the database name. This is metadata for callers — FDB itself
// doesn't have named databases (it has one keyspace per cluster).
func WithDatabase(name string) Option {
	return Option{applyFn: func(o *options) error {
		o.database = name
		return nil
	}}
}

// WithAPIVersion sets the FDB API version. This is metadata for callers to know
// which version to pass to fdb.MustAPIVersion(). It doesn't configure the container.
func WithAPIVersion(version int) Option {
	return Option{applyFn: func(o *options) error {
		o.apiVersion = version
		return nil
	}}
}

// WithVersion sets the FoundationDB Docker image tag.
func WithVersion(version string) Option {
	return Option{applyFn: func(o *options) error {
		o.version = version
		return nil
	}}
}

// WithFDBPort overrides the default FDB port (4500). This sets the FDB_PORT
// environment variable in the container.
func WithFDBPort(port int) Option {
	return Option{applyFn: func(o *options) error {
		if port < 1 || port > 65535 {
			return fmt.Errorf("invalid port %d: must be 1-65535", port)
		}
		o.fdbPort = port
		return nil
	}}
}

// WithTenantMode configures FDB tenant mode for the configure command.
// Valid values: "disabled", "optional_experimental", "required".
// Default: "optional_experimental".
func WithTenantMode(mode string) Option {
	return Option{applyFn: func(o *options) error {
		switch mode {
		case "disabled", "optional_experimental", "required":
			o.tenantMode = mode
		default:
			return fmt.Errorf("invalid tenant mode %q: must be disabled, optional_experimental, or required", mode)
		}
		return nil
	}}
}

// WithStorageEngine configures the FDB storage engine for the configure command.
// Valid values: "memory" (default, fast for tests), "ssd" (persistent).
func WithStorageEngine(engine string) Option {
	return Option{applyFn: func(o *options) error {
		switch engine {
		case "memory", "ssd", "ssd-1", "ssd-2", "ssd-redwood-1", "ssd-rocksdb-v1", "ssd-sharded-rocksdb":
			o.storageEngine = engine
		default:
			return fmt.Errorf("invalid storage engine %q", engine)
		}
		return nil
	}}
}

// WithRedundancyMode configures FDB replication for the configure command.
// Valid values: "single" (default), "double", "triple".
func WithRedundancyMode(mode string) Option {
	return Option{applyFn: func(o *options) error {
		switch mode {
		case "single", "double", "triple":
			o.redundancyMode = mode
		default:
			return fmt.Errorf("invalid redundancy mode %q: must be single, double, or triple", mode)
		}
		return nil
	}}
}

// WithoutInit skips automatic database initialization in [Run].
// Use [Container.InitializeDatabase] for manual initialization.
func WithoutInit() Option {
	return Option{applyFn: func(o *options) error {
		o.autoInit = false
		return nil
	}}
}

// WithDirectIP makes [Container.ClusterFile] return the container's bridge IP
// instead of localhost:mappedPort. This avoids Docker DNAT which causes FDB
// canonicalRemotePort assertion spam under high connection churn.
//
// Direct IP requires the test process to route to Docker bridge IPs. This works
// on Linux hosts but NOT from Bazel's linux-sandbox.
func WithDirectIP() Option {
	return Option{applyFn: func(o *options) error {
		o.directIP = true
		return nil
	}}
}

// WithStartupTimeout sets the timeout for waiting for the FDB container to start.
// Default: 60 seconds.
func WithStartupTimeout(timeout time.Duration) Option {
	return Option{applyFn: func(o *options) error {
		o.startupTimeout = timeout
		return nil
	}}
}

// WithNetwork attaches the container to an existing Docker network instead of
// creating a new one. The network will NOT be removed on [Container.Terminate].
// Additional aliases can be specified for DNS resolution within the network.
func WithNetwork(nw *testcontainers.DockerNetwork, aliases ...string) Option {
	return Option{applyFn: func(o *options) error {
		o.network = nw
		o.networkAliases = append(o.networkAliases, aliases...)
		return nil
	}}
}

// WithNetworkAliases adds network aliases for the container. These are DNS names
// resolvable by other containers on the same Docker network.
func WithNetworkAliases(aliases ...string) Option {
	return Option{applyFn: func(o *options) error {
		o.networkAliases = append(o.networkAliases, aliases...)
		return nil
	}}
}

// WithProcessCount sets the number of fdbserver processes to run inside the
// container. Default is 1. When n > 1, additional processes are started on
// ports 4501..4500+n-1 after the primary process joins the cluster.
//
// Use with WithRedundancyMode("double") or ("triple") to enable data
// replication across processes. Multiple storage servers enable shard splits,
// which is required for testing cross-shard GetRange behavior.
//
// Example:
//
//	Run(ctx, "",
//	    WithProcessCount(3),
//	    WithRedundancyMode("double"),
//	    WithKnob("min_shard_bytes", "40000"),
//	)
func WithProcessCount(n int) Option {
	return Option{applyFn: func(o *options) error {
		if n < 1 || n > 10 {
			return fmt.Errorf("process count must be 1-10, got %d", n)
		}
		o.processCount = n
		return nil
	}}
}

// WithDataOnDisk stores /var/fdb/data on disk (the container's writable layer)
// instead of the default tmpfs. The default tmpfs keeps tests fast and avoids a
// leaked anonymous volume, but caps a dataset at host RAM; use this (with an SSD
// storage engine, e.g. WithStorageEngine("ssd-redwood-1")) for datasets larger
// than memory. The container's anonymous data volume is on disk and is cleaned
// up with the container.
func WithDataOnDisk() Option {
	return Option{applyFn: func(o *options) error {
		o.dataOnDisk = true
		return nil
	}}
}

// WithKnob sets a server-side FDB knob. Knobs are passed as --knob_NAME=VALUE
// to the fdbserver process. This is useful for forcing shard splits
// (e.g., WithKnob("min_shard_bytes", "40000")) or other server behavior changes.
//
// The knob is injected by modifying the entrypoint script inside the container.
// Multiple WithKnob calls accumulate.
func WithKnob(name, value string) Option {
	return Option{applyFn: func(o *options) error {
		// Validate: knob names must be alphanumeric + underscore (FDB convention).
		// Values must not contain shell metacharacters (injected via sed).
		for _, c := range name {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_') {
				return fmt.Errorf("invalid knob name %q: must be [a-zA-Z0-9_]", name)
			}
		}
		for _, c := range value {
			if c == '\'' || c == '|' || c == ';' || c == '`' || c == '$' {
				return fmt.Errorf("invalid knob value %q: contains shell metacharacter", value)
			}
		}
		if o.knobs == nil {
			o.knobs = make(map[string]string)
		}
		o.knobs[name] = value
		return nil
	}}
}

// CreateNetwork creates a new Docker network. This is a convenience for tests
// that need to share a network between multiple containers without letting
// each [Run] call create its own.
//
// The caller is responsible for removing the network.
func CreateNetwork(ctx context.Context) (*testcontainers.DockerNetwork, error) {
	return network.New(ctx, network.WithDriver("bridge"))
}
