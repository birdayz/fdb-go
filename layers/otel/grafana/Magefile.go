//go:build mage

// Backend build with the plugin SDK's mage targets: `go run github.com/magefile/mage -v`
// builds dist/gpx_foundationdb_<os>_<arch> (executable name from src/plugin.json).
package main

import (
	"github.com/grafana/grafana-plugin-sdk-go/build"
	"github.com/magefile/mage/mg"
)

// Default builds the shipped platforms.
var Default = BuildAll

// BuildAll builds linux/amd64, linux/arm64 and darwin/arm64.
func BuildAll() {
	b := build.Build{}
	mg.Deps(b.Linux, b.LinuxARM64, b.DarwinARM64)
}
