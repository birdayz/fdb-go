package main

import (
	"flag"
	"go/build"
	"path/filepath"
	"testing"

	"github.com/bazelbuild/rules_go/go/runfiles"
)

var backendSource = flag.String("backend-source", "", "runfile path to backend_go.go")

func TestBackendBuildTags(t *testing.T) {
	t.Parallel()
	dir := "."
	if *backendSource != "" {
		path, err := runfiles.Rlocation(*backendSource)
		if err != nil {
			t.Fatal(err)
		}
		dir = filepath.Dir(path)
	}
	for _, tc := range []struct {
		name   string
		cgo    bool
		native bool
	}{
		{name: "default_without_cgo"},
		{name: "default_with_cgo", cgo: true},
		{name: "libfdbc_without_cgo", native: true},
		{name: "libfdbc_with_cgo", cgo: true, native: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := build.Default
			ctx.CgoEnabled = tc.cgo
			ctx.BuildTags = nil
			if tc.native {
				ctx.BuildTags = []string{"libfdbc"}
			}
			for file, want := range map[string]bool{"backend_go.go": !tc.native, "backend_cgo.go": tc.native} {
				got, err := ctx.MatchFile(dir, file)
				if err != nil {
					t.Fatal(err)
				}
				if got != want {
					t.Errorf("%s selected=%v, want %v (cgo=%v, libfdbc=%v)", file, got, want, tc.cgo, tc.native)
				}
			}
		})
	}
}
