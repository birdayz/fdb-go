package client

import (
	"time"
)

// topologyMonitor periodically refreshes the cluster topology from coordinators.
// Also responds to kicks from RPC failures (e.g., broken proxy connections).
// Matches C++ monitorClientDBInfoChange in NativeAPI.actor.cpp.
func (db *database) topologyMonitor() {
	defer db.wg.Done()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			db.refreshTopology()
		case <-db.topologyKick:
			db.refreshTopology()
		case <-db.ctx.Done():
			return
		}
	}
}

// kickTopology triggers an immediate topology refresh. Non-blocking.
// Called when an RPC to a proxy fails (connection error, broken_promise, etc.).
func (db *database) kickTopology() {
	select {
	case db.topologyKick <- struct{}{}:
	default:
	}
}

// refreshTopology tries each coordinator to fetch fresh ClientDBInfo.
// On success, atomically swaps dbInfo if proxies changed.
func (db *database) refreshTopology() {
	for _, addr := range db.clusterFile.Coordinators {
		conn, err := db.getOrDial(db.ctx, addr)
		if err != nil {
			continue
		}
		newInfo, err := db.openDatabaseCoord(db.ctx, conn, addr)
		if err != nil {
			continue
		}
		old := db.dbInfo.Load()
		if old != nil && dbInfoEqual(old, newInfo) {
			return // no change
		}
		// ORDER MATTERS: bump generation BEFORE swapping dbInfo.
		db.proxiesGen.Add(1)
		db.dbInfo.Store(newInfo)
		return
	}
}

// handleConnError evicts a dead connection from the pool and marks the
// endpoint as failed so the failure monitor can wake backoff sleeps on recovery.
func (db *database) handleConnError(addr string) {
	db.connMu.Lock()
	if c, ok := db.connPool[addr]; ok {
		c.Close()
		delete(db.connPool, addr)
	}
	db.connMu.Unlock()
	db.failMon.markFailed(addr)
}

// dbInfoEqual returns true if two DBInfo have identical proxy lists.
func dbInfoEqual(a, b *DBInfo) bool {
	if len(a.GRVProxies) != len(b.GRVProxies) || len(a.CommitProxies) != len(b.CommitProxies) {
		return false
	}
	for i := range a.GRVProxies {
		if a.GRVProxies[i].Address != b.GRVProxies[i].Address ||
			a.GRVProxies[i].Token != b.GRVProxies[i].Token {
			return false
		}
	}
	for i := range a.CommitProxies {
		if a.CommitProxies[i].Address != b.CommitProxies[i].Address ||
			a.CommitProxies[i].Token != b.CommitProxies[i].Token {
			return false
		}
	}
	return true
}
