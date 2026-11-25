package client

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"fdb.dev/internal/fdbgo/transport"
	"fdb.dev/internal/fdbwire"
)

// ClusterFile represents an fdb.cluster file.
// Format: "<description>:<id>@<host>:<port>[,<host>:<port>...]"
type ClusterFile struct {
	Description  string
	ID           string
	Coordinators []string // "host:port" addresses for TCP connection (":tls" suffix stripped)
	// UseTLS is true when the coordinators carry the ":tls" suffix (FDB
	// FLAG_TLS). A real cluster is uniformly TLS, so this is a single flag;
	// mixed TLS/non-TLS coordinators are rejected at parse time.
	UseTLS bool
}

// ParseClusterFile reads and parses an fdb.cluster file.
func ParseClusterFile(path string) (*ClusterFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open cluster file: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		return ParseClusterString(line)
	}
	return nil, fmt.Errorf("empty cluster file: %s", path)
}

// trimConnString strips all whitespace and `#…`-to-end-of-line comments from a connection
// string, matching C++ ClusterConnectionString's trim() (MonitorLeader.actor.cpp:34-50), which
// runs before parsing. This makes "desc : id @ host:port" and inline `# comments` parse
// identically to libfdb_c/Java instead of being rejected.
func trimConnString(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '#' {
			// Skip the comment up to and including the next newline (the outer i++ consumes it).
			i++
			for i < len(s) && s[i] != '\n' && s[i] != '\r' {
				i++
			}
		} else if c != ' ' && c != '\n' && c != '\r' && c != '\t' {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// ParseClusterString parses a cluster connection string.
func ParseClusterString(s string) (*ClusterFile, error) {
	// Format: "description:id@host1:port1,host2:port2,host3:port3"
	s = trimConnString(s) // C++ trim() before parsing (MonitorLeader.actor.cpp:34)
	atIdx := strings.LastIndex(s, "@")
	if atIdx < 0 {
		return nil, fmt.Errorf("invalid cluster string: missing '@': %q", s)
	}

	prefix := s[:atIdx]
	addrs := s[atIdx+1:]
	if addrs == "" {
		return nil, fmt.Errorf("no coordinators in cluster string: %q", s)
	}

	colonIdx := strings.Index(prefix, ":")
	if colonIdx < 0 {
		return nil, fmt.Errorf("invalid cluster string: missing ':' in prefix: %q", s)
	}

	cf := &ClusterFile{
		Description: prefix[:colonIdx],
		ID:          prefix[colonIdx+1:],
	}

	// Validate the description:id key to C++'s parseKey acceptance set so a
	// persisted cluster key is always parseable by a C++/Java client (RFC-111 §8).
	if !validClusterKeyPart(cf.Description, true) || !validClusterKeyPart(cf.ID, false) {
		return nil, fmt.Errorf("invalid cluster key %q: description must be [a-zA-Z0-9_], id must be [a-zA-Z0-9]", prefix)
	}

	tlsCount := 0
	seenCoords := make(map[string]bool)
	for _, addr := range strings.Split(addrs, ",") {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			// C++ NetworkAddress::parse("") throws connection_string_invalid on an empty
			// coordinator segment — a leading/trailing/double comma, or a comma left dangling
			// after trim() removed an inline comment (e.g. "h:4500, # disabled" → "h:4500,").
			// (MonitorLeader.actor.cpp:92.) Do NOT silently skip it — that accepts strings C++
			// rejects, breaking cross-tool cluster-file compatibility.
			return nil, fmt.Errorf("empty coordinator in cluster string: %q", s)
		}
		// Faithful to C++ NetworkAddress::parse (flow/network.cpp): strip a
		// trailing "(fromHostname)" marker, then a trailing ":tls" — but only
		// when the string is longer than 4 chars (a bare ":tls" is not a flag,
		// it's an invalid address). The remaining text is host:port.
		addr = strings.TrimSuffix(addr, "(fromHostname)")
		isTLS := false
		if len(addr) > 4 && strings.HasSuffix(addr, ":tls") {
			isTLS = true
			addr = addr[:len(addr)-len(":tls")]
		}
		// Validate the token to C++'s acceptance set: a coordinator is valid iff
		// isHostname(tok) || NetworkAddress::parse(tok) (RFC-111 §8). This is
		// stricter than a bare net.SplitHostPort (which accepts foo:abc, :1234,
		// 1.2.3.4.5:port, ...): the cluster file is cross-tool shared state, and the
		// re-watch persist path must never write a token a C++/Java client can't
		// parse.
		if !isHostnameToken(addr) && !isNetworkAddressToken(addr) {
			return nil, fmt.Errorf("invalid coordinator address %q", addr)
		}
		// Reject duplicate coordinators — C++ ClusterConnectionString throws
		// connection_string_invalid on a duplicate address/hostname
		// (MonitorLeader.actor.cpp:109/117). Dedup on the CANONICAL parsed form
		// (coordDedupKey) so normalized duplicates (leading-zero port,
		// compressed-vs-expanded IPv6) collide the way C++'s NetworkAddress set
		// does — keeping Go-accept a subset of C++-accept so the persist path never
		// writes a dup-coordinator file C++/Java can't read.
		dedupKey := coordDedupKey(addr)
		if seenCoords[dedupKey] {
			return nil, fmt.Errorf("duplicate coordinator address %q", addr)
		}
		seenCoords[dedupKey] = true
		cf.Coordinators = append(cf.Coordinators, addr)
		if isTLS {
			tlsCount++
		}
	}

	// A real cluster is uniformly TLS or uniformly plaintext. A database-level
	// flag can't represent a mix, so reject it rather than silently dialing some
	// coordinators plaintext.
	if tlsCount > 0 && tlsCount != len(cf.Coordinators) {
		return nil, fmt.Errorf("mixed TLS and non-TLS coordinators not supported: %q", s)
	}
	cf.UseTLS = tlsCount > 0

	return cf, nil
}

// getGRVProxy returns a GRV proxy address using round-robin selection.
func (db *database) getGRVProxy() (*ProxyInfo, error) {
	info := db.dbInfo.Load()
	if info == nil || len(info.GRVProxies) == 0 {
		return nil, fmt.Errorf("no GRV proxies available")
	}
	idx := db.proxyRR.nextGRV(len(info.GRVProxies))
	return &info.GRVProxies[idx], nil
}

// getCommitProxy returns a commit proxy address using round-robin selection,
// together with the cluster epoch that proxy belongs to. ONE load: the epoch a
// commit binds to is the epoch of the proxy set it is about to use, never one
// sampled separately and overtaken by a handoff in between.
func (db *database) getCommitProxy() (*ProxyInfo, int64, error) {
	info := db.dbInfo.Load()
	if info == nil || len(info.CommitProxies) == 0 {
		return nil, 0, fmt.Errorf("no commit proxies available")
	}
	n := len(info.CommitProxies)
	idx := db.proxyRR.nextCommit(n)
	// basicLoadBalance skips failed endpoints and addresses (LoadBalance.actor.h:779-805); with
	// every address failed, Go's dial of the first is C++'s wait for one to come back.
	now := nowSeconds()
	var probe *ProxyInfo
	for i := 0; i < n; i++ {
		p := &info.CommitProxies[(idx+i)%n]
		if db.failMon.endpointFailed(p.Address, p.Token) {
			continue
		}
		if db.failMon == nil || !db.failMon.excluded(p.Address, now) {
			return p, info.Epoch, nil
		}
		if probe == nil {
			probe = p
		}
	}
	if probe != nil {
		return probe, info.Epoch, nil
	}
	return nil, 0, fmt.Errorf("no live commit proxies")
}

// getGRVProxies returns all GRV proxies from the current topology and the
// cluster epoch they belong to, from ONE load — see getCommitProxy.
func (db *database) getGRVProxies() ([]ProxyInfo, int64) {
	info := db.dbInfo.Load()
	if info == nil {
		return nil, 0
	}
	return info.GRVProxies, info.Epoch
}

// getCommitProxies returns all commit proxies from the current topology.
func (db *database) getCommitProxies() []ProxyInfo {
	info := db.dbInfo.Load()
	if info == nil {
		return nil
	}
	return info.CommitProxies
}

// warmConnections pre-establishes TCP connections to all known proxies.
// Called after bootstrap to eliminate cold-start latency on first transaction.
// Dials GRV and commit proxies in parallel. Errors are silently ignored —
// connections will be established on demand if pre-warming fails.
func (db *database) warmConnections(ctx context.Context) {
	info := db.dbInfo.Load()
	if info == nil {
		return
	}

	// Collect unique addresses from all proxy types.
	seen := make(map[string]bool)
	var addrs []string
	for _, p := range info.GRVProxies {
		if !seen[p.Address] {
			seen[p.Address] = true
			addrs = append(addrs, p.Address)
		}
	}
	for _, p := range info.CommitProxies {
		if !seen[p.Address] {
			seen[p.Address] = true
			addrs = append(addrs, p.Address)
		}
	}

	// Dial all in parallel.
	var wg sync.WaitGroup
	for _, addr := range addrs {
		wg.Add(1)
		go func(a string) {
			defer wg.Done()
			db.getOrDial(ctx, a)
		}(addr)
	}
	wg.Wait()
}

// bootstrap connects to coordinators and fetches initial cluster topology.
// Tries all coordinators in parallel — first success wins. Retries with
// backoff on transient errors (e.g., failed_to_progress 1216).
func (db *database) bootstrap(ctx context.Context) error {
	backoff := 500 * time.Millisecond
	for {
		snap := db.connRecord.get()
		info, addr, err := db.tryAllCoordinators(ctx, snap)
		switch {
		case err == nil && info.Forward != "":
			// Path A: a coordinator forwarded us to a new set. Adopt + retry now.
			if db.followForward(snap, info.Forward) {
				db.onCoordinatorSetAdopted()
				continue
			}
			// self/empty/over-bound forward → fall through to backoff.
			err = fmt.Errorf("coordinator forward could not be followed")
		case err == nil:
			db.forwardHops = 0
			// New coordinators answered — persist a forward adopted in memory on a
			// previous iteration now that the set is confirmed reachable (Path A).
			db.connRecord.persistIfDirty()
			db.installProxySet(info)
			db.bootstrapCoordinator = addr
			close(db.connected)
			return nil
		default:
			// Path B: all coordinators unreachable — another process may have
			// rotated the set and rewritten the cluster file. Re-read it.
			if db.connRecord.adoptStoredIfChanged() {
				db.onCoordinatorSetAdopted()
				continue
			}
		}

		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("failed to connect to any coordinator: %w", err)
		case <-timer.C:
			if backoff < BootstrapMaxBackoff {
				backoff *= 2
			}
		}
	}
}

// OpenDatabase opens a database connection using a cluster file.
// The provided ctx is used for the initial bootstrap (coordinator connection).
// Background goroutines use an internal context cancelled by Close().
func OpenDatabase(ctx context.Context, clusterFilePath string, opts ...Option) (*Database, error) {
	clusterFilePath, err := LookupClusterFileName(clusterFilePath)
	if err != nil {
		return nil, fmt.Errorf("resolve cluster file: %w", err)
	}
	cf, err := ParseClusterFile(clusterFilePath)
	if err != nil {
		return nil, err
	}

	// Remember the file path so coordinator-set changes can be persisted back to it
	// (RFC-111). OpenDatabaseFromConfig (no path) is memory-only.
	opts = append(opts, withClusterFilePath(clusterFilePath))
	return OpenDatabaseFromConfig(ctx, cf, opts...)
}

// checkAPIVersion validates a WithAPIVersion value: unset is api_version_unset
// (2200), outside [MinAPIVersion, MaxAPIVersion] api_version_not_supported (2203).
func checkAPIVersion(v int) error {
	if v == 0 {
		return &wire.FDBError{Code: 2200} // api_version_unset
	}
	if v < MinAPIVersion || v > MaxAPIVersion {
		return &wire.FDBError{Code: 2203} // api_version_not_supported
	}
	return nil
}

// OpenDatabaseFromConfig creates and bootstraps a Database from a ClusterFile.
// See WithDialFunc / WithTLSConfig for options.
func OpenDatabaseFromConfig(ctx context.Context, cf *ClusterFile, opts ...Option) (*Database, error) {
	o := applyOptions(opts)

	// Mandatory-set API version (RFC-149): C++ cannot open a DB without
	// fdb_select_api_version, so "unset" never occurs there. Reject an unset
	// version here so the apiVersionAtLeast gate can never silently no-op.
	if err := checkAPIVersion(o.apiVersion); err != nil {
		return nil, err
	}

	// Resolve transport security (WithTLSConfig > ":tls"+FDB_TLS_* > plaintext).
	// A non-nil tlsConfig is the only "use TLS" signal. The default-config-dir
	// stat inside resolveTLSConfig is reached only for a TLS cluster, so a
	// plaintext open never touches /etc/foundationdb.
	tlsConfig, err := openTLSConfig(o, cf)
	if err != nil {
		return nil, err
	}

	logger := o.logger
	if logger == nil {
		logger = slog.Default()
	}

	bgCtx, cancel := context.WithCancel(context.Background())
	// Create the failure monitor first so the QueueModel can consult it for the
	// read load-balancing exclusion gate (RFC-115 §1; C++ loadBalance gates on
	// IFailureMonitor.failed before the QueueModel).
	failMon := newFailureMonitor()
	queueModel := newQueueModel()
	queueModel.failMon = failMon
	db := &database{
		connRecord:        newConnRecord(cf, o.clusterFilePath, logger),
		dialFn:            o.dialFn,
		dialOpts:          o.dialOpts,
		tlsConfig:         tlsConfig,
		logger:            logger,
		rangeByteCeiling:  o.rangeByteCeiling,
		tracingSampleRate: o.tracingSampleRate,
		tracer:            o.tracer,
		apiVersion:        o.apiVersion,
		connPool:          make(map[string]*transport.Conn),
		dialing:           make(map[string]*dialCall),
		proxiesChanged:    make(chan struct{}),
		connected:         make(chan struct{}),
		ctx:               bgCtx,
		cancel:            cancel,
		timeouts:          newTimeoutQueue(),
		failMon:           failMon,
		queueModel:        queueModel,
		// hedgeEnabled default: set after struct init (atomic.Bool zero = false)
		locCache: locationCache{
			maxSize: 600_000,
		},
		grvBatchers: [3]*grvBatcher{
			grvBatcherBatch:           {priority: grvPriorityBatch},
			grvBatcherDefault:         {priority: grvPriorityDefault},
			grvBatcherSystemImmediate: {priority: grvPrioritySystemImmediate},
		},
	}

	db.rangeReadPanics.name, db.rangeReadPanics.db = "rangeRead", db
	db.resolveKeyPanics.name, db.resolveKeyPanics.db = "resolveKey", db
	db.readTags = readTagSampling{rate: readTagSampleRate}
	if o.readTagSampling != nil {
		db.readTags = *o.readTagSampling
	}
	db.hedgeEnabled.Store(true)                       // default: hedge on
	db.maxWatches.Store(defaultMaxOutstandingWatches) // C++ DEFAULT_MAX_OUTSTANDING_WATCHES (10000)

	if err := db.bootstrap(ctx); err != nil {
		cancel()
		return nil, fmt.Errorf("connect to cluster: %w", err)
	}

	// Pre-warm connections to all known proxies. The first transaction
	// would dial lazily, but pre-warming saves ~5-10ms on first RPC.
	// Errors are ignored — connections will be established on demand.
	db.warmConnections(ctx)

	// Start topology monitor goroutine.
	db.wg.Add(1)
	go db.topologyMonitor()

	return &Database{db: db}, nil
}
