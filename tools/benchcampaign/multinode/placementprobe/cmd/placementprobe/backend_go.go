//go:build !libfdbc

package main

import (
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/tools/benchcampaign/multinode/placementprobe"
)

// The pure-Go backend: exact for ballast and control operations, but its
// addresses come from the client location cache, so probes need --diagnostic
// and never pass placement_receipt integrity. Build placementprobe_cgo for
// placement receipts.
func openCluster(clusterFile string, timeoutMillis int64) (placementprobe.Cluster, func(), error) {
	if err := fdb.APIVersion(apiVersion); err != nil {
		return nil, nil, err
	}
	db, err := fdb.OpenDatabase(clusterFile)
	if err != nil {
		return nil, nil, err
	}
	return placementprobe.GoCluster{DB: db, TimeoutMillis: timeoutMillis, MaxAttempts: 10}, db.Close, nil
}
