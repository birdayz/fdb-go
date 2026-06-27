// fdb-stacktester implements the FDB binding tester stack machine.
// It reads instructions from FDB, executes them, and writes results back.
// Used with bindingtester.py for conformance testing.
//
// Usage: fdb-stacktester <prefix> <api-version> <cluster-file>
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/client"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "usage: fdb-stacktester <prefix> <api-version> [cluster-file]\n")
		os.Exit(1)
	}

	prefix := []byte(os.Args[1])
	apiVersion, err := strconv.Atoi(os.Args[2])
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid api-version: %s\n", os.Args[2])
		os.Exit(1)
	}
	clusterFile := ""
	if len(os.Args) > 3 {
		clusterFile = os.Args[3]
	}
	if clusterFile == "" {
		clusterFile = os.Getenv("FDB_CLUSTER_FILE")
	}
	if clusterFile == "" {
		clusterFile = "/etc/foundationdb/fdb.cluster"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	db, err := client.OpenDatabase(ctx, clusterFile, client.WithAPIVersion(apiVersion))
	if err != nil {
		fmt.Fprintf(os.Stderr, "open database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	sm := NewStackMachine(db, prefix)
	if err := sm.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "stack machine: %v\n", err)
		os.Exit(1)
	}
}
