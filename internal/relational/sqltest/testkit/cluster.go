package testkit

import (
	"fdb.dev/internal/fdbclient"
	"fdb.dev/pkg/fdbgo/fdb"
)

// ClusterFile returns the cluster file of the FoundationDB container Main
// started, or "" when Docker is unavailable.
func ClusterFile() string { return clusterFilePath }

// OpenBackend opens clusterFile using the compiled-in FDB client backend.
func OpenBackend(clusterFile string) (fdb.BackendDatabase, error) {
	return fdbclient.Open(clusterFile)
}
