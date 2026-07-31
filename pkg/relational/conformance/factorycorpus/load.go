package factorycorpus

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"fdb.dev/pkg/relational/conformance/yamsql"
)

// File is one committed corpus file: its provenance header and the scenario
// the yamsql runner executes.
type File struct {
	Path     string
	Header   Header
	Scenario *yamsql.Scenario
}

// Load reads one committed corpus file.
//
// Scenario validation is NOT re-implemented here: it delegates to yamsql.Load,
// so the corpus inherits the strict KnownFields decoding and every
// assertion-cannot-be-silently-dropped check that loader grew (an `exec:` key
// the struct did not have once made months of DML stanzas run as empty
// statements). What this loader adds is the header contract and the
// cross-checks that bind the header to the document — a header describing a
// different file than the one under it is worse than no header, because the
// census would report it with total confidence.
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	h, err := ParseHeader(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	s, err := yamsql.Load(path)
	if err != nil {
		return nil, err
	}
	if h.Name != s.Name {
		return nil, fmt.Errorf("%s: header name %q != document name %q", path, h.Name, s.Name)
	}
	base := strings.TrimSuffix(filepath.Base(path), ".yaml")
	if h.Name != base {
		return nil, fmt.Errorf("%s: header name %q != file basename %q", path, h.Name, base)
	}
	if len(s.Tests) == 0 {
		return nil, fmt.Errorf("%s: scenario has no tests", path)
	}
	return &File{Path: path, Header: h, Scenario: s}, nil
}

// LoadDir reads every committed corpus file in dir.
//
// The glob is ONE LEVEL and `*.yaml`, matching every other corpus consumer in
// the tree, and the directory is this package's own — the hand-authored yamsql
// corpus feeds three generated root documents (FEATURE_MATRIX.md,
// SQL_COVERAGE.md, SQL_ANSI_CONFORMANCE.md) whose ledgers count hand-written
// evidence; a factory batch landing there would bury it under machine volume.
//
// An empty directory is an ERROR, not an empty result: every caller here is a
// gate, and a gate over zero files is green forever.
func LoadDir(dir string) ([]*File, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, fmt.Errorf("glob %s: %w", dir, err)
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("no *.yaml under %s: a corpus gate over an empty corpus passes vacuously", dir)
	}
	sort.Strings(matches)
	files := make([]*File, 0, len(matches))
	seen := map[string]string{}
	for _, m := range matches {
		f, err := Load(m)
		if err != nil {
			return nil, err
		}
		if prev, dup := seen[f.Header.DedupKey]; dup {
			return nil, fmt.Errorf("%s and %s share dedup key %s: the corpus is committing the same (feature vector, plan shape) point twice, which is volume without coverage",
				prev, m, f.Header.DedupKey)
		}
		seen[f.Header.DedupKey] = m
		files = append(files, f)
	}
	return files, nil
}

// TestdataDir is the corpus directory relative to this package.
const TestdataDir = "testdata"
