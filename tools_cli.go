//go:build tools

// This file keeps the CLI deps (cobra, fang, protoconfig, protoyaml,
// lipgloss, readline) listed as DIRECT requires in the root module's
// go.mod. The `cmd/frl` module has its own go.mod but uses these too,
// and our Bazel build runs a single go_deps extension off the root
// go.mod — so they must live here too.
//
// Without these underscore imports, `go mod tidy` drops the packages as
// // indirect, which in turn makes `bazel mod tidy` drop them from use_repo
// in MODULE.bazel, which then breaks gazelle-generated BUILD files in
// cmd/frl (labels like @com_github_spf13_cobra stop resolving).
//
// Nothing else includes the "tools" build tag, so this file never lands
// in any real binary — it only shapes the module graph.

package tools

import (
	_ "buf.build/go/protoyaml"
	_ "charm.land/fang/v2"
	_ "charm.land/lipgloss/v2"
	_ "github.com/birdayz/protobuf-ecosystem/protoconfig"
	_ "github.com/charmbracelet/x/term"
	_ "github.com/chzyer/readline"
	_ "github.com/spf13/cobra"
)
