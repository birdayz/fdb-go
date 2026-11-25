package client

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestNoInlineVTables ensures client code uses types.* vtable constants,
// not inline wire.VTable{...} literals.
func TestNoInlineVTables(t *testing.T) {
	t.Parallel()

	pattern := regexp.MustCompile(`wire\.VTable\{`)

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}

	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue // test files can use inline vtables for test setup
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			if pattern.MatchString(line) {
				t.Errorf("%s:%d: inline vtable literal — use types.*VTable constant instead:\n  %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}
