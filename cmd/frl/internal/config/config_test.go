package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/proto"

	configv1 "fdb.dev/cmd/frl/gen/frl/config/v1"
)

// tempConfigPath returns a fresh file path inside t.TempDir(). Tests use
// LoadFrom/SaveTo against this path so they can still call t.Parallel().
// Env-var-reading tests (TestPath_*) are serial because t.Setenv and
// t.Parallel are incompatible.
func tempConfigPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "config.yaml")
}

func TestPath_HonoursEnvOverride(t *testing.T) {
	// Not parallel: t.Setenv forbids t.Parallel.
	t.Setenv(envConfigPath, "/custom/path.yaml")
	got, err := Path()
	if err != nil {
		t.Fatalf("Path returned error: %v", err)
	}
	if got != "/custom/path.yaml" {
		t.Errorf("Path = %q, want /custom/path.yaml", got)
	}
}

func TestPath_DefaultFallsBackToHome(t *testing.T) {
	// Not parallel: mutates env.
	t.Setenv(envConfigPath, "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	got, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	want := filepath.Join(home, defaultDir, defaultFile)
	if got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
}

func TestLoadFrom_MissingFileReturnsEmptyConfig(t *testing.T) {
	t.Parallel()
	path := tempConfigPath(t) // file does not exist yet

	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom returned error on missing file: %v", err)
	}
	if cfg == nil {
		t.Fatal("LoadFrom returned nil config; want empty")
	}
	if cfg.GetCurrentContext() != "" || len(cfg.GetContexts()) != 0 {
		t.Errorf("LoadFrom on missing file returned non-empty config: %+v", cfg)
	}
}

func TestSaveLoad_RoundTrip(t *testing.T) {
	t.Parallel()
	path := tempConfigPath(t)

	want := &configv1.Config{
		CurrentContext: "prod",
		Contexts: []*configv1.Context{
			{
				Name:         "prod",
				ClusterFile:  "/etc/fdb/prod.cluster",
				KeyspacePath: "/myapp/prod",
				Metadata: &configv1.MetadataSource{
					Source: &configv1.MetadataSource_MetaStoreKeyspace{
						MetaStoreKeyspace: "/myapp/prod/_meta",
					},
				},
			},
			{
				Name:         "local",
				ClusterFile:  "/tmp/fdb.cluster",
				KeyspacePath: "/dev",
				Metadata: &configv1.MetadataSource{
					Source: &configv1.MetadataSource_MetaFile{
						MetaFile: "/tmp/meta.pb",
					},
				},
			},
		},
	}

	if err := SaveTo(path, want); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if info.Size() == 0 {
		t.Fatal("SaveTo produced empty file")
	}

	got, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if !proto.Equal(want, got) {
		t.Errorf("round-trip mismatch:\n want: %+v\n got:  %+v", want, got)
	}
}

func TestSaveTo_AtomicRenameLeavesNoTempFile(t *testing.T) {
	t.Parallel()
	path := tempConfigPath(t)

	if err := SaveTo(path, &configv1.Config{CurrentContext: "x"}); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("found leftover tmp file after SaveTo: %s", e.Name())
		}
	}
}

func TestResolveContext_ByExplicitName(t *testing.T) {
	t.Parallel()
	cfg := &configv1.Config{
		CurrentContext: "prod",
		Contexts: []*configv1.Context{
			{Name: "prod"},
			{Name: "local"},
		},
	}
	got, err := ResolveContext(cfg, "local")
	if err != nil {
		t.Fatalf("ResolveContext: %v", err)
	}
	if got.GetName() != "local" {
		t.Errorf("resolved %q, want local", got.GetName())
	}
}

func TestResolveContext_FallsBackToCurrent(t *testing.T) {
	t.Parallel()
	cfg := &configv1.Config{
		CurrentContext: "prod",
		Contexts: []*configv1.Context{
			{Name: "prod"},
			{Name: "local"},
		},
	}
	got, err := ResolveContext(cfg, "")
	if err != nil {
		t.Fatalf("ResolveContext empty: %v", err)
	}
	if got.GetName() != "prod" {
		t.Errorf("resolved %q, want prod", got.GetName())
	}
}

func TestResolveContext_MissingName(t *testing.T) {
	t.Parallel()
	cfg := &configv1.Config{Contexts: []*configv1.Context{{Name: "prod"}}}
	_, err := ResolveContext(cfg, "nope")
	if !errors.Is(err, ErrNoContext) {
		t.Errorf("error = %v, want errors.Is ErrNoContext", err)
	}
}

func TestResolveContext_NoCurrentAndNoName(t *testing.T) {
	t.Parallel()
	cfg := &configv1.Config{Contexts: []*configv1.Context{{Name: "prod"}}}
	_, err := ResolveContext(cfg, "")
	if !errors.Is(err, ErrNoContext) {
		t.Errorf("error = %v, want errors.Is ErrNoContext", err)
	}
}

func TestLoadFrom_AcceptsSnakeCaseYAML(t *testing.T) {
	t.Parallel()
	path := tempConfigPath(t)

	// Write snake_case YAML — protoyaml accepts both snake_case and
	// camelCase field names on read, so users aren't forced to use one style.
	raw := `current_context: prod
contexts:
  - name: prod
    cluster_file: /etc/fdb/prod.cluster
    keyspace_path: /myapp/prod
    metadata:
      meta_store_keyspace: /myapp/prod/_meta
`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if cfg.GetCurrentContext() != "prod" {
		t.Errorf("current_context = %q, want prod", cfg.GetCurrentContext())
	}
	if len(cfg.GetContexts()) != 1 {
		t.Fatalf("contexts = %d, want 1", len(cfg.GetContexts()))
	}
	ctx := cfg.GetContexts()[0]
	if ctx.GetClusterFile() != "/etc/fdb/prod.cluster" {
		t.Errorf("cluster_file = %q", ctx.GetClusterFile())
	}
	if ctx.GetMetadata().GetMetaStoreKeyspace() != "/myapp/prod/_meta" {
		t.Errorf("meta_store_keyspace = %q", ctx.GetMetadata().GetMetaStoreKeyspace())
	}
}
