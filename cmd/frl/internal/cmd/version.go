package cmd

import (
	"encoding/json"
	"fmt"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// versionInfo is the data shape shared by the text and JSON renderers.
type versionInfo struct {
	Version   string `json:"version"`
	GoVersion string `json:"go_version"`
	GOOS      string `json:"goos"`
	GOARCH    string `json:"goarch"`
}

func newVersionCmd() *cobra.Command {
	var (
		shortOnly bool
		outputFmt string
	)
	c := &cobra.Command{
		Use:   "version",
		Short: "Print frl version and build info",
		Example: `  frl version
  frl version --short                # just the version string
  frl version -o json | jq -r .version`,
		Long: "Prints the frl version and build info (Go toolchain + GOOS/GOARCH). " +
			"--short collapses the output to just the version string, " +
			"suitable for scripting.\n\n" +
			"--output / -o: 'text' (default) or 'json' " +
			"({version, go_version, goos, goarch}). Ignored with --short.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateOutputFormat(outputFmt, "text", "json"); err != nil {
				return err
			}
			v := readVersion()
			if shortOnly {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), v.Version)
				return err
			}
			if outputFmt == "json" {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(v)
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(),
				"frl %s (%s %s/%s)\n",
				v.Version, v.GoVersion, v.GOOS, v.GOARCH)
			return err
		},
	}
	c.Flags().BoolVar(&shortOnly, "short", false, "print only the version string")
	c.Flags().StringVarP(&outputFmt, "output", "o", "text", "output format: text or json")
	return c
}

// stampedVersion is set at link time:
//
//	go build -ldflags "-X fdb.dev/cmd/frl/internal/cmd.stampedVersion=v1.2.3" ./cmd/frl
//
// (or Bazel x_defs). This is the honest versioning path for shipped
// artifacts — rules_go strips debug.ReadBuildInfo's module version, so
// an unstamped Bazel binary reports "dev". `go install` binaries still
// get their real version from ReadBuildInfo.
var stampedVersion string

// readVersion prefers the link-time stamp, then runtime/debug build
// info. Returns "dev" / "unknown" sentinels when neither is available.
func readVersion() versionInfo {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		v := stampedVersion
		if v == "" {
			v = "unknown"
		}
		return versionInfo{
			Version:   v,
			GoVersion: "unknown",
			GOOS:      "unknown",
			GOARCH:    "unknown",
		}
	}
	ver := stampedVersion
	if ver == "" {
		ver = info.Main.Version
	}
	if ver == "" || ver == "(devel)" {
		ver = "dev"
	}
	return versionInfo{
		Version:   ver,
		GoVersion: info.GoVersion,
		GOOS:      settingValue(info, "GOOS"),
		GOARCH:    settingValue(info, "GOARCH"),
	}
}

func settingValue(info *debug.BuildInfo, key string) string {
	for _, s := range info.Settings {
		if s.Key == key {
			return s.Value
		}
	}
	return "unknown"
}
