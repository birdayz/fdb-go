// frl — operator and developer CLI for the Go Record Layer.
//
// Lives in its own Go module (separate `go.mod`) so library consumers of
// github.com/birdayz/fdb-record-layer-go do not inherit the CLI's deps
// (cobra, fang, protoconfig). A root `go.work` at the repo root ties the
// two modules together for local development.
package main

import (
	"context"
	"os"

	"charm.land/fang/v2"

	"github.com/birdayz/fdb-record-layer-go/cmd/frl/internal/cmd"
)

func main() {
	if err := fang.Execute(context.Background(), cmd.NewRoot()); err != nil {
		os.Exit(1)
	}
}
