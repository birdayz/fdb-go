//go:build bazelrunfiles

// Package sqltest's census binary: see BUILD.bazel. The build tag keeps a plain
// `go test ./...` from running this file alone, with an empty corpus.
package sqltest

import (
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

func TestMain(m *testing.M) { testkit.Main(m) }
