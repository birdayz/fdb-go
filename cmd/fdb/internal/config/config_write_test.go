package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	configv1 "fdb.dev/cmd/fdb/gen/fdb/config/v1"
)

func TestSetCurrentContextUpdatesAcceptedFieldSpellings(t *testing.T) {
	for _, field := range []string{"current_context", "currentContext"} {
		t.Run(field, func(t *testing.T) {
			path := tempConfigPath(t)
			t.Setenv("FDB_CONFIG", path)
			raw := "# retained guidance\n" + field + ": old\ncontexts:\n- name: old\n- name: new\n"
			if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			before, err := LoadFrom(path)
			if err != nil || before.GetCurrentContext() != "old" {
				t.Fatalf("fixture must load the original selection: %v, %v", before, err)
			}
			if err := SetCurrentContext("new"); err != nil {
				t.Fatal(err)
			}
			got, err := LoadFrom(path)
			if err != nil || got.GetCurrentContext() != "new" {
				t.Fatalf("persisted selection = %v, error %v; want new", got, err)
			}
			rendered, err := os.ReadFile(path)
			if err != nil || !strings.Contains(string(rendered), "# retained guidance") {
				t.Fatalf("context switch lost guidance: %q, %v", rendered, err)
			}
		})
	}
}

func TestSaveToConcurrentWritersKeepOneCompleteDocument(t *testing.T) {
	t.Parallel()
	path := tempConfigPath(t)
	const writers = 24
	start := make(chan struct{})
	errors := make(chan error, writers)
	var group sync.WaitGroup
	for i := range writers {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			name := fmt.Sprintf("writer-%d", i)
			cfg := &configv1.Config{CurrentContext: name, Contexts: []*configv1.Context{{Name: name, ClusterFile: strings.Repeat(name+"/", 4096)}}}
			errors <- SaveTo(path, cfg)
		}()
	}
	close(start)
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Errorf("concurrent SaveTo: %v", err)
		}
	}
	got, err := LoadFrom(path)
	if err != nil || len(got.GetContexts()) != 1 {
		t.Fatalf("final config is incomplete: %v, %v", got, err)
	}
	selected := got.GetCurrentContext()
	if got.GetContexts()[0].GetName() != selected || got.GetContexts()[0].GetClusterFile() != strings.Repeat(selected+"/", 4096) {
		t.Fatalf("concurrent writers mixed document contents: %v", got)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		t.Fatalf("temporary config files remain: %v, %v", entries, err)
	}
}

func TestSaveToFailedRenameRemovesTemporaryFile(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	path := filepath.Join(parent, "config.yaml")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := SaveTo(path, &configv1.Config{CurrentContext: "new"}); err == nil || !strings.Contains(err.Error(), "rename") {
		t.Fatalf("directory target must reject replacement: %v", err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 1 || !entries[0].IsDir() || entries[0].Name() != "config.yaml" {
		t.Fatalf("failed rename left a temporary file or changed the target: %v, %v", entries, err)
	}
}
