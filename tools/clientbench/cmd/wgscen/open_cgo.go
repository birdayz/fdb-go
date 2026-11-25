//go:build libfdbc

package main

import (
	"fdb.dev/internal/fdbclient/libfdbc"
	"fdb.dev/pkg/fdbgo/fdb"
)

func open(clusterFile string) (fdb.BackendDatabase, error) { return libfdbc.Open(clusterFile) }
