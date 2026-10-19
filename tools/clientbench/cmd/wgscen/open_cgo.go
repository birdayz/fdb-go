//go:build libfdbc

package main

import (
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/libfdbc"
)

func open(clusterFile string) (fdb.BackendDatabase, error) { return libfdbc.Open(clusterFile) }
