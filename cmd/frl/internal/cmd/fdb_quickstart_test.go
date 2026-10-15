package cmd

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/bazelbuild/rules_go/go/runfiles"

	"fdb.dev/cmd/frl/internal/config"
)

// The quickstarts in README.md and on the website run the examples with
// FDB_CLUSTER_FILE set to the file `frl fdb up` writes. Pin that literal path
// so a change to the frl config directory or the default container name fails
// here instead of breaking the published instructions.
func TestQuickstartClusterFilePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("FRL_CONFIG", "")
	cfgPath, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(home, filepath.Join(filepath.Dir(cfgPath), defaultFdbContainer+".cluster"))
	if err != nil {
		t.Fatal(err)
	}
	want := "FDB_CLUSTER_FILE=~/" + filepath.ToSlash(rel)

	assignment := regexp.MustCompile(`FDB_CLUSTER_FILE=\S*`)
	for _, doc := range []string{"README.md", "website/content/docs/getting-started.md"} {
		path, err := runfiles.Rlocation("_main/" + doc)
		if err != nil {
			t.Fatal(err)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var commands []string
		for _, afterFence := range strings.Split(string(content), "```sh\n")[1:] {
			if block, _, closed := strings.Cut(afterFence, "\n```"); closed {
				commands = append(commands, strings.Split(block, "\n")...)
			}
		}
		var up, runs int
		for _, line := range commands {
			if strings.TrimSpace(line) == "frl fdb up" {
				up++
			}
			for _, got := range assignment.FindAllString(line, -1) {
				runs++
				if got != want {
					t.Errorf("%s: %q, want %q (the file frl fdb up writes)", doc, got, want)
				}
			}
		}
		if up == 0 || runs == 0 {
			t.Errorf("%s: want a `frl fdb up` line and at least one %s command; found %d and %d", doc, want, up, runs)
		}
	}
}
