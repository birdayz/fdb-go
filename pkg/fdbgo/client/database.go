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
	dbInfo     atomic.Pointer[DBInfo]
	proxiesGen atomic.Uint64
	// Kick this channel to trigger an immediate topology refresh.
	topologyKick chan struct{} // buffered(1), non-blocking send

	// Connection pool. C++ uses FlowTransport; we need explicit pool.
	connMu   sync.RWMutex
	connPool map[string]*transport.Conn

	// Location cache. C++: CoalescedKeyRangeMap<Reference<LocationInfo>>.
	locCache locationCache

	// Per-endpoint health tracking. Wakes GRV backoff on recovery.
	failMon *failureMonitor

	// GRV cache + batcher. C++: cachedReadVersion, versionBatcher.
	grvCache   grvCache
	grvBatcher grvBatcher

	// Lifecycle.
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	connected chan struct{}
	wg        sync.WaitGroup
}

// GetGRVProxy returns a GRV proxy address for read version requests.
func (db *database) getGRVProxy() (*ProxyInfo, error) {
	info := db.dbInfo.Load()
	if info == nil || len(info.GRVProxies) == 0 {
		return nil, fmt.Errorf("no GRV proxies available")
	}
	// Simple round-robin (TODO: proper load balancing).
	return &info.GRVProxies[0], nil
}

// getCommitProxy returns a commit proxy address.
func (db *database) getCommitProxy() (*ProxyInfo, error) {
	info := db.dbInfo.Load()
	if info == nil || len(info.CommitProxies) == 0 {
		return nil, fmt.Errorf("no commit proxies available")
	}
	return &info.CommitProxies[0], nil
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

	// Check if we have an existing connection to the same port via a different
	// address (e.g., proxy at 172.x.x.x:PORT when we connected via localhost:PORT).
	// In single-node clusters, all FDB processes share one address, so we can
	// reuse the coordinator connection for proxy/storage requests.
	_, targetPort, _ := net.SplitHostPort(addr)
	for existingAddr, c := range db.connPool {
		if !c.IsClosed() {
			_, existingPort, _ := net.SplitHostPort(existingAddr)
			if existingPort == targetPort {
				db.connPool[addr] = c // cache under new key too
				return c, false, nil
			}
		}
	}

	dialCtx, cancel := context.WithTimeout(ctx, DefaultRPCTimeout)
	defer cancel()

	c, dialErr := transport.DialWith(dialCtx, addr, false, db.dialFn)
	if dialErr != nil {
		// Fallback: if the internal address failed (e.g., Docker networking),
		// try the coordinator address with the same port.
		if len(db.clusterFile.Coordinators) > 0 {
			_, coordPort, _ := net.SplitHostPort(db.clusterFile.Coordinators[0])
			if targetPort == coordPort {
				coordAddr := db.clusterFile.Coordinators[0]
				c, dialErr = transport.DialWith(dialCtx, coordAddr, false, db.dialFn)
				if dialErr == nil {
					db.connPool[addr] = c
					return c, true, nil
				}
			}
		}
		return nil, false, dialErr
	}

	db.connPool[addr] = c
	return c, true, nil
}

// bootstrap connects to coordinators and fetches initial cluster topology.
func (db *database) bootstrap(ctx context.Context) error {
	var lastErr error
	for _, addr := range db.clusterFile.Coordinators {
		conn, err := db.getOrDial(ctx, addr)
		if err != nil {
			lastErr = err
			continue
		}

		dbInfo, err := db.openDatabaseCoord(ctx, conn, addr)
		if err != nil {
			lastErr = fmt.Errorf("coordinator %s: %w", addr, err)
			continue
		}

		db.dbInfo.Store(dbInfo)
		close(db.connected)
		return nil
	}
	return fmt.Errorf("failed to connect to any coordinator: %w", lastErr)
}

// Database is the public API entry point.
// C++: Database is a Reference<DatabaseContext> — a handle, not state.
//
// Safe for concurrent use by multiple goroutines.
type Database struct {
	db *database
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
		clusterFile:  cf,
		dialFn:       dialFn,
		connPool:     make(map[string]*transport.Conn),
		topologyKick: make(chan struct{}, 1),
		connected:    make(chan struct{}),
		ctx:          bgCtx,
		cancel:       cancel,
		failMon:      newFailureMonitor(),
		locCache: locationCache{
			maxSize: 600_000,
		},
		grvBatcher: grvBatcher{
			batchTime: 1 * time.Millisecond,
		},
	}

	if err := db.bootstrap(ctx); err != nil {
		cancel()
		return nil, fmt.Errorf("connect to cluster: %w", err)
	}

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
			if retryErr := tx.OnError(err); retryErr != nil {
				return nil, retryErr // non-retryable
			}
			continue // tx has been reset in place, retryCount/backoff preserved
		}

		if err := tx.Commit(ctx); err != nil {
			if retryErr := tx.OnError(err); retryErr != nil {
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
			if retryErr := tx.OnError(err); retryErr != nil {
				return nil, retryErr
			}
			continue
		}
		return result, nil
	}
}

// CreateTransaction creates a new transaction.
func (d *Database) CreateTransaction() *Transaction {
	return &Transaction{
		db:    d.db,
		state: txStateActive,
	}
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
