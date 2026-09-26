// frl — operator and developer CLI for the Go Record Layer.
//
// A package of the root module fdb.dev, so `go install fdb.dev/cmd/frl@vX.Y.Z`
// builds it at the project's release tag.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"charm.land/fang/v2"

	"fdb.dev/cmd/frl/internal/cmd"
)

func main() {
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
