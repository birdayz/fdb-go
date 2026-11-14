//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// prepareBindingTester patches only a private copy. Bazel's input forest can
// contain symlinks; preserving them would write through into the pinned repo.
func prepareBindingTester(src, tempRoot string) (string, error) {
	dir, err := os.MkdirTemp(tempRoot, "fdb-binding-harness-")
	if err != nil {
		return "", fmt.Errorf("create binding harness: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(dir)
		}
	}()
	dst := filepath.Join(dir, "bindingtester")
	if out, err := exec.Command("cp", "-rL", src, dst).CombinedOutput(); err != nil {
		return "", fmt.Errorf("copy bindingtester: %s: %w", out, err)
	}
	initPy := filepath.Join(dst, "__init__.py")
	data, err := os.ReadFile(initPy)
	if err != nil {
		return "", fmt.Errorf("read bindingtester imports: %w", err)
	}
	patched, err := patchBindingTesterImports(string(data))
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(initPy, []byte(patched), 0o644); err != nil {
		return "", fmt.Errorf("write bindingtester imports: %w", err)
	}
	ok = true
	return dir, nil
}

func patchBindingTesterImports(source string) (string, error) {
	lines := strings.Split(source, "\n")
	var path, util, version int
	for i, line := range lines {
		switch {
		case strings.HasPrefix(line, "sys.path[:0] = "), line == "# Use the installed FDB Python client.":
			lines[i] = "# Use the installed FDB Python client."
			path++
		case line == "import util", line == "from bindingtester import util":
			lines[i] = "from bindingtester import util"
			util++
		case line == "from fdb import LATEST_API_VERSION", line == "LATEST_API_VERSION = 730":
			lines[i] = "LATEST_API_VERSION = 730"
			version++
		}
	}
	if path != 1 || util != 1 || version != 1 {
		return "", fmt.Errorf("unexpected bindingtester imports (path=%d util=%d version=%d); re-fetch the pinned FoundationDB source if it was modified", path, util, version)
	}
	return strings.Join(lines, "\n"), nil
}
