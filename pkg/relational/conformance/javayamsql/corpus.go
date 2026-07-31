package javayamsql

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// CorpusSubdir is the vendored corpus's location relative to the repo root.
const CorpusSubdir = "third_party/apple/fdb-record-layer/yaml-tests/src/test/resources"

// Corpus is a rooted view of the `.yamsql` files.
//
// Include targets are resolved against this root, never against the including
// file's directory: Java hands the include string straight to
// ClassLoader.getResourceAsStream, so `include: include-block/includes/x.yamsql`
// means the same thing no matter which file writes it.
type Corpus struct {
	FS fs.FS
}

// OpenCorpus locates the vendored corpus by walking up from the working
// directory until [CorpusSubdir] is found.
//
// Walking up is what makes the package work identically under `go test` (cwd is
// the package dir) and under `bazel test` (cwd is the runfiles tree), without
// either needing to know the other's layout.
func OpenCorpus() (*Corpus, error) {
	dir, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	for {
		candidate := filepath.Join(dir, filepath.FromSlash(CorpusSubdir))
		if st, err := os.Stat(candidate); err == nil && st.IsDir() {
			return &Corpus{FS: os.DirFS(candidate)}, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, fmt.Errorf("could not locate %s above the working directory", CorpusSubdir)
		}
		dir = parent
	}
}

// List returns every `.yamsql` path in the corpus, root-relative and sorted.
func (c *Corpus) List() ([]string, error) {
	var out []string
	err := fs.WalkDir(c.FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ".yamsql") {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// ParseFile parses one corpus file as a top-level entry point, expanding its
// include graph inline.
func (c *Corpus) ParseFile(p string) (*File, error) {
	src, err := fs.ReadFile(c.FS, p)
	if err != nil {
		return nil, &ParseError{Path: p, Err: fmt.Errorf("could not find %q in resources bundle", p)}
	}
	parser := &parser{corpus: c, txnSetups: map[string]string{}}
	return parser.parseFile(p, src, true)
}
