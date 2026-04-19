package cmd

import (
	"fmt"
	"runtime/debug"

	"github.com/spf13/cobra"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print frl version and build info",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			info, ok := debug.ReadBuildInfo()
			if !ok {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), "frl (build info unavailable)")
				return err
			}
			ver := info.Main.Version
			if ver == "" || ver == "(devel)" {
				ver = "dev"
			}
			// Pack a sample tuple through the root-module library to prove
			// cmd/frl's go.work/replace wiring actually reaches the library
			// code. This is a skeleton-phase sanity check; it will be
			// replaced with real store introspection in Phase B.
			sample := tuple.Tuple{"frl", int64(1)}.Pack()
			_, err := fmt.Fprintf(cmd.OutOrStdout(),
				"frl %s (%s %s/%s)\nrecord-layer tuple probe: %x\n",
				ver, info.GoVersion, goos(info), goarch(info), sample)
			return err
		},
	}
}

func goos(info *debug.BuildInfo) string {
	return settingValue(info, "GOOS")
}

func goarch(info *debug.BuildInfo) string {
	return settingValue(info, "GOARCH")
}

func settingValue(info *debug.BuildInfo, key string) string {
	for _, s := range info.Settings {
		if s.Key == key {
			return s.Value
		}
	}
	return "unknown"
}
