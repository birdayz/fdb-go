// fdb — fdb.dev CLI for FoundationDB, Record Layer, and SQL.
//
// A package of the root module fdb.dev, so `go install fdb.dev/cmd/fdb@vX.Y.Z`
// builds it at the project's release tag.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"charm.land/fang/v2"

	"fdb.dev/cmd/fdb/internal/cmd"
	"fdb.dev/pkg/relational/core/keyspace"
)

func main() {
	// Java's CLI (RelationalSQLLine) and server register the FRL domain at
	// startup, so /FRL/DB paths work; fdb does the same.
	keyspace.RegisterDomainIfNotExists("FRL")
	// Cancel the command context on SIGINT / SIGTERM so Ctrl-C during a
	// long `record scan` / `store dump` flows through to FDB's range
	// iterator instead of waiting for the FDB tx timeout. signal.Stop
	// via the returned cancel func restores default signal handling on
	// exit (so a second Ctrl-C during shutdown still kills the process).
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := fang.Execute(ctx, cmd.NewRoot()); err != nil {
		os.Exit(1)
	}
}
