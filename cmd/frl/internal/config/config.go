// Package config loads and writes the frl YAML config file (default
// location: ~/.frl/config.yaml) using protoconfig. The file is structured
// as a `frl.config.v1.Config` message — proto-backed so the schema is
// versioned and forwards-compatible.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"buf.build/go/protoyaml"
	"github.com/birdayz/protobuf-ecosystem/protoconfig"

	configv1 "github.com/birdayz/fdb-record-layer-go/cmd/frl/gen/frl/config/v1"
)

// DefaultPath is the canonical on-disk location. Override with FRL_CONFIG.
const (
	envConfigPath = "FRL_CONFIG"
	defaultDir    = ".frl"
	defaultFile   = "config.yaml"
)

// ErrNoContext is returned by ResolveContext when the selected context
// does not exist in the loaded config.
var ErrNoContext = errors.New("context not found")

// Path returns the effective config path — FRL_CONFIG env var if set,
// else ~/.frl/config.yaml.
func Path() (string, error) {
	if p := os.Getenv(envConfigPath); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, defaultDir, defaultFile), nil
}

// Load is a convenience wrapper over LoadFrom that uses Path().
func Load() (*configv1.Config, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	return LoadFrom(path)
}

// LoadFrom reads the YAML file at path and returns the parsed Config.
// A missing file is not an error — protoconfig.Optional() makes the
// YAML source soft, so first-run commands (like `config use-context`)
// can operate on a zero value and then write back.
func LoadFrom(path string) (*configv1.Config, error) {
	cfg, err := protoconfig.Load(
		&configv1.Config{},
		protoconfig.FromYAMLFile(path, protoconfig.Optional()),
	)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", path, err)
	}
	return cfg, nil
}

// Save is a convenience wrapper over SaveTo that uses Path().
func Save(cfg *configv1.Config) error {
	path, err := Path()
	if err != nil {
		return err
	}
	return SaveTo(path, cfg)
}

// SaveTo writes the Config to path as YAML, creating parent directories
// as needed. The existing file is atomically replaced via a temp file +
// rename, so a crash mid-write cannot leave a truncated config.
func SaveTo(path string, cfg *configv1.Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	bytes, err := protoyaml.MarshalOptions{Indent: 2}.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, bytes, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}

// ResolveContext looks up a named context in the config. An empty name
// resolves Config.current_context. Returns ErrNoContext if the name is
// empty and no current context is set, or if the named context is absent.
func ResolveContext(cfg *configv1.Config, name string) (*configv1.Context, error) {
	if name == "" {
		name = cfg.GetCurrentContext()
	}
	if name == "" {
		return nil, fmt.Errorf("%w: no context specified and current_context is empty", ErrNoContext)
	}
	for _, ctx := range cfg.GetContexts() {
		if ctx.GetName() == name {
			return ctx, nil
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrNoContext, name)
}
