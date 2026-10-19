//go:build !libfdbc

package main

import "fdb.dev/pkg/fdbgo/fdb"

func open(clusterFile string) (fdb.BackendDatabase, error) { return fdb.OpenDatabase(clusterFile) }
