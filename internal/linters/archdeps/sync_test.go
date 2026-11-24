package archdeps

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/tools/go/analysis"
)

// repoRoot locates the repository root from MODULE.bazel. Under Bazel it
// resolves the staged //:MODULE.bazel runfiles symlink (with local=True and
// tags=["external"] in BUILD.bazel so the real workspace is readable and
// never spuriously cached). Under `go test` it walks up from cwd. It fails
// loudly and never skips if MODULE.bazel cannot be found.
func repoRoot(t *testing.T) string {
	t.Helper()
	if sd, ws := os.Getenv("TEST_SRCDIR"), os.Getenv("TEST_WORKSPACE"); sd != "" && ws != "" {
		anchor := filepath.Join(sd, ws, "MODULE.bazel")
		real, err := filepath.EvalSymlinks(anchor)
		if err != nil {
			t.Fatalf("EvalSymlinks(%s): %v (sync tests must never skip)", anchor, err)
		}
		root := filepath.Dir(real)
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
			t.Fatalf("resolved root %s missing go.mod: %v", root, err)
		}
		return root
	}
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "MODULE.bazel")); err == nil {
			if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("MODULE.bazel not found walking up from cwd (sync tests must never skip)")
		}
		dir = parent
	}
}

func parseFileImports(t *testing.T, absPath string) (string, map[string]bool) {
	t.Helper()
	src, err := os.ReadFile(absPath)
	if err != nil {
		t.Fatalf("read %s: %v", absPath, err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, absPath, src, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse %s: %v", absPath, err)
	}
	imps := make(map[string]bool, len(file.Imports))
	for _, spec := range file.Imports {
		imp, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			t.Fatalf("unquote import in %s: %v", absPath, err)
		}
		imps[imp] = true
	}
	return file.Name.Name, imps
}

func relDirToPkgPath(relPath string) string {
	dir := path.Dir(filepath.ToSlash(relPath))
	if dir == "." {
		return "fdb.dev"
	}
	return "fdb.dev/" + dir
}

func TestAllowlistsInSyncWithRepo(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)

	if len(l3Allow) != 0 {
		t.Errorf("l3Allow has %d entries; L3->L4/L5 must remain empty (0 prod violations)", len(l3Allow))
	}
	if len(l6Allow) != 0 {
		t.Errorf("l6Allow has %d entries; L6->L7 must remain empty (0 prod violations)", len(l6Allow))
	}

	type allowSpec struct {
		name      string
		allow     map[string]map[string]bool
		appliesFn func(pkgPath, relPath, imp string) bool
	}

	specs := []allowSpec{
		{
			name:  "r1Allow",
			allow: r1Allow,
			appliesFn: func(pkgPath, _ string, imp string) bool {
				return withinAny(pkgPath, recordLayerRoots) && withinAny(imp, relationalRoots)
			},
		},
		{
			name:  "r2Allow",
			allow: r2Allow,
			appliesFn: func(pkgPath, _ string, imp string) bool {
				return withinAny(pkgPath, plannerRoots) && isR2Forbidden(imp)
			},
		},
		{
			name:  "r3Allow",
			allow: r3Allow,
			appliesFn: func(pkgPath, _ string, imp string) bool {
				return withinAny(pkgPath, fdbClientRoots) && (withinAny(imp, recordLayerRoots) || withinAny(imp, relationalRoots))
			},
		},
		{
			name:  "r4Allow",
			allow: r4Allow,
			appliesFn: func(pkgPath, relPath, imp string) bool {
				inR4 := withinAny(pkgPath, coreRuntimeRoots) && !withinAny(pkgPath, coreRuntimeExemptions)
				return inR4 && !strings.HasSuffix(relPath, "_test.go") && withinAny(imp, r4ForbiddenRoots)
			},
		},
		{
			name:  "r6Allow",
			allow: r6Allow,
			appliesFn: func(pkgPath, relPath, imp string) bool {
				return withinAny(pkgPath, fdbClientRoots) && !strings.HasSuffix(relPath, "_test.go") && within(imp, otelRoot)
			},
		},
		{
			name:  "l0Allow",
			allow: l0Allow,
			appliesFn: func(pkgPath, _ string, imp string) bool {
				return withinAny(pkgPath, r2AllowedLeaves) && isL0Forbidden(imp)
			},
		},
		{
			name:  "l3Allow",
			allow: l3Allow,
			appliesFn: func(pkgPath, relPath, imp string) bool {
				inL3 := withinAny(pkgPath, recordLayerRoots) && !withinAny(pkgPath, r2AllowedLeaves) && !withinAny(pkgPath, plannerRoots) && !withinAny(pkgPath, executorRoots)
				return inL3 && !strings.HasSuffix(relPath, "_test.go") && (withinAny(imp, plannerRoots) || withinAny(imp, executorRoots))
			},
		},
		{
			name:  "l6Allow",
			allow: l6Allow,
			appliesFn: func(pkgPath, relPath, imp string) bool {
				return withinAny(pkgPath, relationalCoreAPIRoots) && !strings.HasSuffix(relPath, "_test.go") && withinAny(imp, sqlDriverRoots)
			},
		},
	}

	for _, sp := range specs {
		for relPath, allowedImps := range sp.allow {
			if len(allowedImps) == 0 {
				t.Errorf("stale %s entry: %s has empty import map", sp.name, relPath)
				continue
			}
			absPath := filepath.Join(root, filepath.FromSlash(relPath))
			if _, err := os.Stat(absPath); err != nil {
				t.Errorf("stale %s entry: %s does not exist: %v", sp.name, relPath, err)
				continue
			}
			_, actualImps := parseFileImports(t, absPath)
			pkgPath := relDirToPkgPath(relPath)
			for imp, enabled := range allowedImps {
				if !enabled {
					t.Errorf("stale %s entry: %s -> %s is false (remove dead entry)", sp.name, relPath, imp)
					continue
				}
				if !actualImps[imp] {
					t.Errorf("stale %s entry: %s no longer imports %s (retire it)", sp.name, relPath, imp)
				}
				if !sp.appliesFn(pkgPath, relPath, imp) {
					t.Errorf("stale %s entry: %s -> %s does not trigger %s", sp.name, relPath, imp, sp.name)
				}
			}
		}
	}

	for pkgPath, enabled := range r5Allow {
		if !enabled {
			t.Errorf("stale r5Allow entry: %s is false (remove dead entry)", pkgPath)
			continue
		}
		if !within(pkgPath, "fdb.dev") {
			t.Errorf("invalid r5Allow entry %q: must be under fdb.dev", pkgPath)
			continue
		}
		relDir := strings.TrimPrefix(pkgPath, "fdb.dev/")
		if pkgPath == "fdb.dev" {
			relDir = "."
		}
		absDir := filepath.Join(root, filepath.FromSlash(relDir))
		entries, err := os.ReadDir(absDir)
		if err != nil {
			t.Errorf("stale r5Allow entry: cannot read dir for %s (%s): %v", pkgPath, absDir, err)
			continue
		}
		goFiles := 0
		foundTestFramework := false
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
				continue
			}
			goFiles++
			_, imps := parseFileImports(t, filepath.Join(absDir, entry.Name()))
			for imp := range imps {
				if withinAny(imp, r5TestFrameworkRoots) {
					foundTestFramework = true
					break
				}
			}
		}
		if goFiles == 0 {
			t.Errorf("stale r5Allow entry: package %s has no .go files", pkgPath)
			continue
		}
		if !foundTestFramework {
			t.Errorf("stale r5Allow entry: package %s no longer imports any frozen test framework (retire it)", pkgPath)
		}
	}
}

func TestRepoHasNoUnallowlistedViolations(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)

	const minGoFiles = 500
	scanned := 0

	err := filepath.WalkDir(root, func(absPath string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if absPath != root {
				if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "bazel-") || name == "external" || name == "testdata" || name == "third_party" {
					return filepath.SkipDir
				}
				// Skip nested Go modules (e.g. layers/otel/grafana) that have their own go.mod.
				if _, err := os.Stat(filepath.Join(absPath, "go.mod")); err == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 || !strings.HasSuffix(name, ".go") {
			return nil
		}
		rel, err := filepath.Rel(root, absPath)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		src, err := os.ReadFile(absPath)
		if err != nil {
			return err
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, rel, src, parser.ImportsOnly)
		if err != nil {
			return err
		}
		scanned++
		pkgPath := relDirToPkgPath(rel)
		pass := &analysis.Pass{
			Analyzer: Analyzer,
			Fset:     fset,
			Files:    []*ast.File{file},
			Pkg:      types.NewPackage(pkgPath, file.Name.Name),
			Report: func(diag analysis.Diagnostic) {
				t.Errorf("%s: %s", rel, diag.Message)
			},
		}
		_, runErr := Analyzer.Run(pass)
		return runErr
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}
	if scanned < minGoFiles {
		t.Fatalf("scanned %d Go files under %s, want at least %d (not the real repo tree)", scanned, root, minGoFiles)
	}
	t.Logf("verified %d Go files across %s with 0 unallowlisted violations", scanned, root)
}
