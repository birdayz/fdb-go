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
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/transport"
)

// ClusterFile represents an fdb.cluster file.
// Format: "<description>:<id>@<host>:<port>[,<host>:<port>...]"
type ClusterFile struct {
	Description  string
	ID           string
	Coordinators []string // "host:port" addresses
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

// Cluster manages connections to FDB coordinators and monitors
// topology changes (proxy addresses).
type Cluster struct {
	clusterFile *ClusterFile

	mu       sync.RWMutex
	dbInfo   *DBInfo // latest ClientDBInfo from coordinators
	connPool map[string]*transport.Conn

	ctx    context.Context
	cancel context.CancelFunc
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

// NewCluster creates a Cluster from a cluster file path.
func NewCluster(clusterFilePath string) (*Cluster, error) {
	cf, err := ParseClusterFile(clusterFilePath)
	if err != nil {
		return nil, err
	}
	return NewClusterFromConfig(cf), nil
}

// NewClusterFromConfig creates a Cluster from a parsed cluster file.
func NewClusterFromConfig(cf *ClusterFile) *Cluster {
	ctx, cancel := context.WithCancel(context.Background())
	return &Cluster{
		clusterFile: cf,
		connPool:    make(map[string]*transport.Conn),
		ctx:         ctx,
		cancel:      cancel,
	}
}

// Connect establishes connections to coordinators and fetches
// initial cluster topology (ClientDBInfo with proxy addresses).
func (c *Cluster) Connect(ctx context.Context) error {
	var lastErr error
	for _, addr := range c.clusterFile.Coordinators {
		conn, err := c.getOrDial(ctx, addr)
		if err != nil {
			lastErr = err
			continue
		}

		dbInfo, err := c.openDatabaseCoord(ctx, conn, addr)
		if err != nil {
			lastErr = fmt.Errorf("coordinator %s: %w", addr, err)
			continue
		}

		c.mu.Lock()
		c.dbInfo = dbInfo
		c.mu.Unlock()

		return nil
	}
	return fmt.Errorf("failed to connect to any coordinator: %w", lastErr)
}

// GetGRVProxy returns a GRV proxy address for read version requests.
func (c *Cluster) GetGRVProxy() (*ProxyInfo, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.dbInfo == nil || len(c.dbInfo.GRVProxies) == 0 {
		return nil, fmt.Errorf("no GRV proxies available")
	}
	// Simple round-robin (TODO: proper load balancing).
	return &c.dbInfo.GRVProxies[0], nil
}

// GetCommitProxy returns a commit proxy address.
func (c *Cluster) GetCommitProxy() (*ProxyInfo, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.dbInfo == nil || len(c.dbInfo.CommitProxies) == 0 {
		return nil, fmt.Errorf("no commit proxies available")
	}
	return &c.dbInfo.CommitProxies[0], nil
}

// Close shuts down all connections and the topology monitor.
func (c *Cluster) Close() error {
	c.cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	for addr, conn := range c.connPool {
		conn.Close()
		delete(c.connPool, addr)
	}
	return nil
}

func (c *Cluster) getOrDial(ctx context.Context, addr string) (*transport.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if conn, ok := c.connPool[addr]; ok {
		return conn, nil
	}

	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	conn, err := transport.Dial(dialCtx, addr, false)
	if err != nil {
		return nil, err
	}

	c.connPool[addr] = conn
	return conn, nil
}
