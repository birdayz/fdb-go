// Package client implements the FDB client transaction lifecycle.
// This is the Go equivalent of NativeAPI.actor.cpp.
package client

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/transport"
)

// ClusterFile represents an fdb.cluster file.
// Format: "<description>:<id>@<host>:<port>[,<host>:<port>...]"
type ClusterFile struct {
	Description  string
	ID           string
	Coordinators []string // "host:port" addresses for TCP connection
	InternalKey  string   // optional: full internal cluster key for request clusterKey field
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

// ParseClusterString parses a cluster connection string.
func ParseClusterString(s string) (*ClusterFile, error) {
	// Format: "description:id@host1:port1,host2:port2,host3:port3"
	atIdx := strings.LastIndex(s, "@")
	if atIdx < 0 {
		return nil, fmt.Errorf("invalid cluster string: missing '@': %q", s)
	}

	prefix := s[:atIdx]
	addrs := s[atIdx+1:]

	colonIdx := strings.Index(prefix, ":")
	if colonIdx < 0 {
		return nil, fmt.Errorf("invalid cluster string: missing ':' in prefix: %q", s)
	}

	cf := &ClusterFile{
		Description: prefix[:colonIdx],
		ID:          prefix[colonIdx+1:],
	}

	for _, addr := range strings.Split(addrs, ",") {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			continue
		}
		// Validate it's host:port.
		if _, _, err := net.SplitHostPort(addr); err != nil {
			return nil, fmt.Errorf("invalid coordinator address %q: %w", addr, err)
		}
		cf.Coordinators = append(cf.Coordinators, addr)
	}

	if len(cf.Coordinators) == 0 {
		return nil, fmt.Errorf("no coordinators in cluster string: %q", s)
	}

	return cf, nil
}

// DBInfo holds the current cluster topology.
// Received from coordinators via OpenDatabaseCoordRequest.
type DBInfo struct {
	ID            transport.UID
	GRVProxies    []ProxyInfo
	CommitProxies []ProxyInfo
	ClusterID     transport.UID
}

// ProxyInfo holds addressing info for a proxy.
type ProxyInfo struct {
	Address string // "host:port"
	Token   transport.UID
}

// TransactionDefaults holds database-level defaults applied to every new
// transaction. Matches C++ DatabaseContext::transactionDefaults.
type TransactionDefaults struct {
	Timeout        int64 // FDB_DB_OPTION_TRANSACTION_TIMEOUT (ms), 0 = disabled
	RetryLimit     int   // FDB_DB_OPTION_TRANSACTION_RETRY_LIMIT
	MaxRetryDelay  int64 // FDB_DB_OPTION_TRANSACTION_MAX_RETRY_DELAY (ms), 0 = use default
	SizeLimit      int64 // FDB_DB_OPTION_TRANSACTION_SIZE_LIMIT, 0 = disabled
	HasRetryLimit  bool
	ReadSystemKeys bool // read \xff/* keys by default
	AccessSysKeys  bool // read+write \xff/* keys by default
}

// database is the per-database state container (unexported).
// Matches C++ DatabaseContext in fdbclient/DatabaseContext.h.
// All per-database state lives here: topology, caches, connections, batchers.
//
// Safe for concurrent use by multiple goroutines after creation.
type database struct {
	// Immutable after creation.
	clusterFile *ClusterFile
	dialFn      transport.DialFunc // nil = net.DialTimeout

	// Topology: atomically swapped on coordinator refresh.
	// C++: clientInfo (AsyncVar<ClientDBInfo>)
	dbInfo atomic.Pointer[DBInfo]
	// Kick this channel to trigger an immediate topology refresh.
	topologyKick chan struct{} // buffered(1), non-blocking send
	// Broadcast: closed when proxy list changes, then replaced with a fresh channel.
	// Commit monitors this to detect mid-commit proxy changes (C++ onProxiesChanged).
	proxiesChangedMu sync.Mutex
	proxiesChanged   chan struct{}

	// Connection pool. C++ uses FlowTransport; we need explicit pool.
	connMu   sync.RWMutex
	connPool map[string]*transport.Conn

	// Location cache. C++: CoalescedKeyRangeMap<Reference<LocationInfo>>.
	locCache locationCache

	// Per-endpoint health tracking. Wakes GRV backoff on recovery.
	failMon *failureMonitor

	// Load balancing: QueueModel for storage servers, round-robin for proxies.
	queueModel *QueueModel
	proxyRR    proxyRoundRobin

	// GRV cache + per-priority batchers. C++: cachedReadVersion, versionBatcher.
	// One batcher per priority level: [BATCH, DEFAULT, SYSTEM_IMMEDIATE].
	grvCache    grvCache
	grvBatchers [3]*grvBatcher

	// minAcceptableReadVersion tracks the minimum version this client has seen
	// from the cluster. SetReadVersion below this throws transaction_too_old
	// client-side, matching C++ DatabaseContext::validateVersion().
	minAcceptableReadVersion atomic.Int64

	// Tag throttle state — updated from GRV reply tagThrottleInfo.
	// Maps priority -> (tag -> throttle limits). Matches C++ cx->throttledTags.
	tagThrottles tagThrottleState

	// Transaction defaults — applied to every new transaction.
	// Matches C++ DatabaseContext::transactionDefaults.
	txDefaults TransactionDefaults

	// Speculative second request (hedge) control.
	// When true, read RPCs send a hedge request to a second server after
	// secondDelay if the primary is slow. Matches C++ BACKUP_REQUEST_DELAY.
	// Default: true (always on, same as C++).
	hedgeEnabled atomic.Bool

	// Lifecycle.
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	connected chan struct{}
	wg        sync.WaitGroup
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

// getCommitProxy returns a commit proxy address using round-robin selection.
func (db *database) getCommitProxy() (*ProxyInfo, error) {
	info := db.dbInfo.Load()
	if info == nil || len(info.CommitProxies) == 0 {
		return nil, fmt.Errorf("no commit proxies available")
	}
	idx := db.proxyRR.nextCommit(len(info.CommitProxies))
	return &info.CommitProxies[idx], nil
}

// getGRVProxies returns all GRV proxies from the current topology.
func (db *database) getGRVProxies() []ProxyInfo {
	info := db.dbInfo.Load()
	if info == nil {
		return nil
	}
	return info.GRVProxies
}

// getCommitProxies returns all commit proxies from the current topology.
func (db *database) getCommitProxies() []ProxyInfo {
	info := db.dbInfo.Load()
	if info == nil {
		return nil
	}
	return info.CommitProxies
}

func (db *database) getOrDial(ctx context.Context, addr string) (*transport.Conn, error) {
	conn, dialed, err := db.getOrDialConn(ctx, addr)
	if err != nil {
		return nil, err
	}
	if dialed {
		db.failMon.markAlive(addr)
	}
	return conn, nil
}

// getOrDialConn returns a pooled or freshly-dialed connection.
// dialed is true when a new TCP connection was established (cache miss).
func (db *database) getOrDialConn(ctx context.Context, addr string) (conn *transport.Conn, dialed bool, err error) {
	db.connMu.Lock()
	defer db.connMu.Unlock()

	if c, ok := db.connPool[addr]; ok {
		if !c.IsClosed() {
			return c, false, nil
		}
		delete(db.connPool, addr)
	}

	// Dial a new connection. C++ FlowTransport creates one Peer (TCP connection)
	// per unique NetworkAddress. No address aliasing or port-matching — each
	// ip:port gets its own connection.
	//
	// TODO: C++ FlowTransport deduplicates bidirectional connections via
	// ConnectionID exchange in ConnectPacket. When two processes connect to
	// each other simultaneously, the lower-priority connection is dropped.
	// We don't need this as a pure client (we never accept incoming connections),
	// but should implement it if we ever add server-side functionality.
	dialCtx, cancel := context.WithTimeout(ctx, DefaultRPCTimeout)
	defer cancel()

	c, dialErr := transport.DialWith(dialCtx, addr, false, db.dialFn)
	if dialErr != nil {
		return nil, false, dialErr
	}

	db.connPool[addr] = c
	return c, true, nil
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
		info, err := db.tryAllCoordinators(ctx)
		if err == nil {
			db.dbInfo.Store(info)
			close(db.connected)
			return nil
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

// tryAllCoordinators races all coordinators in parallel, returning the first
// successful response. Matches C++ quorum(ok,1) pattern.
func (db *database) tryAllCoordinators(ctx context.Context) (*DBInfo, error) {
	if len(db.clusterFile.Coordinators) == 0 {
		// Defensive — production cluster-file parsing rejects empty
		// coordinator lists, but a hand-constructed *database in tests can
		// reach this path. Without the guard, the for-loop below returns
		// (nil, nil) and refreshTopology eventually nil-derefs in dbInfoEqual.
		return nil, fmt.Errorf("no coordinators configured")
	}
	if len(db.clusterFile.Coordinators) == 1 {
		// Fast path: single coordinator, no goroutine overhead.
		return db.tryOneCoordinator(ctx, db.clusterFile.Coordinators[0])
	}

	type result struct {
		info *DBInfo
		err  error
	}
	ch := make(chan result, len(db.clusterFile.Coordinators))
	raceCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	for _, addr := range db.clusterFile.Coordinators {
		go func(addr string) {
			info, err := db.tryOneCoordinator(raceCtx, addr)
			ch <- result{info, err}
		}(addr)
	}

	var lastErr error
	for range db.clusterFile.Coordinators {
		r := <-ch
		if r.err == nil {
			cancel() // cancel remaining attempts
			return r.info, nil
		}
		lastErr = r.err
	}
	return nil, lastErr
}

func (db *database) tryOneCoordinator(ctx context.Context, addr string) (*DBInfo, error) {
	conn, err := db.getOrDial(ctx, addr)
	if err != nil {
		return nil, err
	}
	info, err := db.openDatabaseCoord(ctx, conn, addr)
	if err != nil {
		return nil, fmt.Errorf("coordinator %s: %w", addr, err)
	}
	return info, nil
}

// Database is the public API entry point.
// C++: Database is a Reference<DatabaseContext> — a handle, not state.
//
// Safe for concurrent use by multiple goroutines.
type Database struct {
	db *database
}

// SetHedgeEnabled controls speculative second requests (hedge) for read RPCs.
// When enabled (default), slow reads are rescued by sending a backup request
// to a second server after max(10ms, 2×latency). Disable for debugging or
// benchmarking the non-hedged path.
func (d *Database) SetHedgeEnabled(enabled bool) {
	d.db.hedgeEnabled.Store(enabled)
}

// HedgeEnabled returns whether speculative second requests are active.
func (d *Database) HedgeEnabled() bool {
	return d.db.hedgeEnabled.Load()
}

// OpenDatabase opens a database connection using a cluster file.
// The provided ctx is used for the initial bootstrap (coordinator connection).
// Background goroutines use an internal context cancelled by Close().
func OpenDatabase(ctx context.Context, clusterFilePath string) (*Database, error) {
	cf, err := ParseClusterFile(clusterFilePath)
	if err != nil {
		return nil, err
	}

	return OpenDatabaseFromConfig(ctx, cf, nil)
}

// OpenDatabaseFromConfig creates and bootstraps a Database from a ClusterFile.
// dialFn may be nil for default dialing.
func OpenDatabaseFromConfig(ctx context.Context, cf *ClusterFile, dialFn transport.DialFunc) (*Database, error) {
	bgCtx, cancel := context.WithCancel(context.Background())
	db := &database{
		clusterFile:    cf,
		dialFn:         dialFn,
		connPool:       make(map[string]*transport.Conn),
		topologyKick:   make(chan struct{}, 1),
		proxiesChanged: make(chan struct{}),
		connected:      make(chan struct{}),
		ctx:            bgCtx,
		cancel:         cancel,
		failMon:        newFailureMonitor(),
		queueModel:     newQueueModel(),
		// hedgeEnabled default: set after struct init (atomic.Bool zero = false)
		locCache: locationCache{
			maxSize: 600_000,
		},
		grvBatchers: [3]*grvBatcher{
			grvBatcherBatch:           {batchTime: 1 * time.Millisecond, priority: grvPriorityBatch},
			grvBatcherDefault:         {batchTime: 1 * time.Millisecond, priority: grvPriorityDefault},
			grvBatcherSystemImmediate: {batchTime: 1 * time.Millisecond, priority: grvPrioritySystemImmediate},
		},
	}

	db.hedgeEnabled.Store(true) // default: hedge on (matches C++)

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

// SetDialFunc sets a custom dialer for all new connections.
// Must be called before OpenDatabase or any transactions.
func (d *Database) SetDialFunc(fn transport.DialFunc) {
	d.db.dialFn = fn
}

// Transact runs a function in a transaction with automatic retry.
// This is the primary API for interacting with FDB.
// The transaction is reused across retries — retryCount and backoff escalate.
func (d *Database) Transact(ctx context.Context, fn func(tx *Transaction) (any, error)) (any, error) {
	tx := d.CreateTransaction()
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		result, err := fn(tx)
		if err != nil {
			if retryErr := tx.OnError(ctx, err); retryErr != nil {
				return nil, retryErr // non-retryable
			}
			continue // tx has been reset in place, retryCount/backoff preserved
		}

		if err := tx.Commit(ctx); err != nil {
			if retryErr := tx.OnError(ctx, err); retryErr != nil {
				return nil, retryErr
			}
			continue // for commit_unknown_result: self-conflicting applied
		}

		return result, nil
	}
}

// ReadTransact runs a read-only function in a transaction with automatic retry.
// The transaction is never committed — only reads are performed.
func (d *Database) ReadTransact(ctx context.Context, fn func(tx *Transaction) (any, error)) (any, error) {
	tx := d.CreateTransaction()
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		result, err := fn(tx)
		if err != nil {
			if retryErr := tx.OnError(ctx, err); retryErr != nil {
				return nil, retryErr
			}
			continue
		}
		return result, nil
	}
}

// CreateTransaction creates a new transaction.
// Database-level defaults (timeout, retry limit, system key access) are applied.
func (d *Database) CreateTransaction() *Transaction {
	tx := &Transaction{
		db:           d.db,
		tenantId:     NoTenantID,
		creationTime: time.Now(),
	}
	// Pre-allocate RYW writes map to avoid make() on first Set.
	tx.ryw.writes = make(map[string]rywEntry, 4)
	// Apply database-level defaults (matches C++ applyTxDefaults).
	td := &d.db.txDefaults
	if td.Timeout > 0 {
		tx.SetTimeout(td.Timeout)
	}
	if td.HasRetryLimit {
		tx.SetRetryLimit(int64(td.RetryLimit))
	}
	if td.MaxRetryDelay > 0 {
		tx.SetMaxRetryDelay(td.MaxRetryDelay)
	}
	if td.SizeLimit > 0 {
		tx.SetSizeLimit(td.SizeLimit)
	}
	if td.ReadSystemKeys {
		tx.SetReadSystemKeys()
	}
	if td.AccessSysKeys {
		tx.SetAccessSystemKeys()
	}
	return tx
}

// SetTransactionTimeout sets the default timeout (in milliseconds) for all
// transactions created from this database. Matches FDB_DB_OPTION_TRANSACTION_TIMEOUT.
func (d *Database) SetTransactionTimeout(ms int64) {
	d.db.txDefaults.Timeout = ms
}

// SetTransactionRetryLimit sets the default retry limit for all transactions.
// Matches FDB_DB_OPTION_TRANSACTION_RETRY_LIMIT.
func (d *Database) SetTransactionRetryLimit(retries int64) {
	d.db.txDefaults.RetryLimit = int(retries)
	d.db.txDefaults.HasRetryLimit = true
}

// SetTransactionMaxRetryDelay sets the default max retry delay (ms) for all
// transactions. Matches FDB_DB_OPTION_TRANSACTION_MAX_RETRY_DELAY.
func (d *Database) SetTransactionMaxRetryDelay(ms int64) {
	d.db.txDefaults.MaxRetryDelay = ms
}

// SetTransactionSizeLimit sets the default size limit for all transactions.
// Matches FDB_DB_OPTION_TRANSACTION_SIZE_LIMIT.
func (d *Database) SetTransactionSizeLimit(limit int64) {
	d.db.txDefaults.SizeLimit = limit
}

// SetDefaultReadSystemKeys makes all new transactions automatically call
// SetReadSystemKeys(), allowing reads of \xff-prefixed system keys.
func (d *Database) SetDefaultReadSystemKeys() {
	d.db.txDefaults.ReadSystemKeys = true
}

// SetDefaultAccessSystemKeys makes all new transactions automatically call
// SetAccessSystemKeys(), allowing reads AND writes of \xff-prefixed system keys.
func (d *Database) SetDefaultAccessSystemKeys() {
	d.db.txDefaults.AccessSysKeys = true
}

// InvalidateGRVCache resets the GRV cache so the next transaction fetches
// a fresh read version from the GRV proxy. Use after external writes.
func (d *Database) InvalidateGRVCache() {
	d.db.grvCache.version.Store(0)
}

// GetDBInfo returns the current cluster topology (proxy lists, cluster ID).
// Returns nil if not yet connected.
func (d *Database) GetDBInfo() *DBInfo {
	return d.db.dbInfo.Load()
}

// Close shuts down the database connection. Idempotent.
// Cancels background goroutines, waits for them to exit, closes all connections.
func (d *Database) Close() error {
	d.db.closeOnce.Do(func() {
		d.db.cancel()
		d.db.wg.Wait()
		d.db.connMu.Lock()
		for addr, conn := range d.db.connPool {
			conn.Close()
			delete(d.db.connPool, addr)
		}
		d.db.connMu.Unlock()
	})
	return nil
}
