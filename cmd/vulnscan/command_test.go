package main

import (
	"archive/zip"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testTool(t *testing.T, name string) string {
	t.Helper()
	path := os.Getenv(name)
	if path == "" {
		t.Fatalf("missing declared test tool %s", name)
	}
	if !filepath.IsAbs(path) {
		root, workspace := os.Getenv("TEST_SRCDIR"), os.Getenv("TEST_WORKSPACE")
		if root == "" || workspace == "" {
			t.Fatal("declared tools require the Bazel runfiles environment")
		}
		path = filepath.Join(root, workspace, path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	return path
}

func fixtureFile(t *testing.T, root, name, text string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestScanCommandWithPinnedScanner(t *testing.T) {
	t.Parallel()
	goBin := testTool(t, "VULNSCAN_TEST_GO")
	scanner := testTool(t, "VULNSCAN_TEST_SCANNER")
	wrapper := testTool(t, "VULNSCAN_TEST_WRAPPER")
	if govulncheckVersion != "v1.9.0" {
		t.Fatal("update the pinned Bazel scanner alongside the runtime scanner")
	}
	root, cache, proxy := t.TempDir(), t.TempDir(), t.TempDir()
	fixtureFile(t, root, "go.mod", "module fdb.dev\n\ngo 1.26.0\n\nrequire fixture.invalid/vulnerable v1.0.0\n")
	fixtureFile(t, root, "pkg/library/library.go", "package library\nfunc Safe() {}\n")
	fixtureFile(t, root, "pkg/testcontainers/helper/helper.go", "package helper\nimport \"fixture.invalid/vulnerable\"\nfunc Call() { vulnerable.Bad() }\n")
	fixtureFile(t, root, "cmd/fdb/main.go", "package main\nimport \"fdb.dev/pkg/testcontainers/helper\"\nfunc main() { helper.Call() }\n")
	// Serve the synthetic dependency through a local Go proxy so it retains a
	// release version: local replace directives have no vulnerable version.
	const dep = "module fixture.invalid/vulnerable\n\ngo 1.26.0\n"
	fixtureFile(t, proxy, "fixture.invalid/vulnerable/@v/v1.0.0.mod", dep)
	fixtureFile(t, proxy, "fixture.invalid/vulnerable/@v/v1.0.0.info", `{"Version":"v1.0.0","Time":"2026-01-01T00:00:00Z"}`)
	fixtureFile(t, proxy, "fixture.invalid/vulnerable/@v/list", "v1.0.0\n")
	archive, err := os.Create(filepath.Join(proxy, "fixture.invalid/vulnerable/@v/v1.0.0.zip"))
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(archive)
	for name, text := range map[string]string{"go.mod": dep, "bad.go": "package vulnerable\nfunc Bad() {}\n"} {
		entry, err := zw.Create("fixture.invalid/vulnerable@v1.0.0/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(text)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}

	stamp := time.Now().UTC().Format(time.RFC3339)
	fixtureFile(t, cache, "fetched-at", stamp)
	fixtureFile(t, cache, "db/index/db.json", `{"modified":"`+stamp+`"}`)
	fixtureFile(t, cache, "db/index/modules.json", `[{"path":"fixture.invalid/vulnerable","vulns":[{"id":"GO-2099-0001","modified":"`+stamp+`","fixed":"1.1.0"}]}]`)
	advisory := map[string]any{
		"schema_version": "1.3.1", "id": "GO-2099-0001", "modified": stamp, "published": stamp,
		"summary": "Synthetic regression advisory, not a claim about a published dependency",
		"affected": []any{map[string]any{
			"package":            map[string]string{"name": "fixture.invalid/vulnerable", "ecosystem": "Go"},
			"ranges":             []any{map[string]any{"type": "SEMVER", "events": []any{map[string]string{"introduced": "0"}, map[string]string{"fixed": "1.1.0"}}}},
			"ecosystem_specific": map[string]any{"imports": []any{map[string]any{"path": "fixture.invalid/vulnerable", "symbols": []string{"Bad"}}}},
		}},
	}
	raw, err := json.Marshal(advisory)
	if err != nil {
		t.Fatal(err)
	}
	fixtureFile(t, cache, "db/ID/GO-2099-0001.json", string(raw))
	scannerBytes, err := os.ReadFile(scanner)
	if err != nil {
		t.Fatal(err)
	}
	cachedScanner := filepath.Join(cache, "bin", govulncheckVersion, "govulncheck")
	if err := os.MkdirAll(filepath.Dir(cachedScanner), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachedScanner, scannerBytes, 0o700); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "PATH="+filepath.Dir(goBin)+":"+os.Getenv("PATH"),
		"GOROOT="+filepath.Dir(filepath.Dir(goBin)), "GOTOOLCHAIN=local", "GOWORK=off", "GOENV=off", "CGO_ENABLED=0",
		"GOPROXY=file://"+proxy, "GOSUMDB=off", "GOPRIVATE=", "GONOPROXY=", "GONOSUMDB=", "GOTELEMETRY=off",
		"GOCACHE="+t.TempDir(), "GOMODCACHE="+t.TempDir(), "GOFLAGS=-mod=mod -modcacherw", "VULNSCAN_DB_URL=http://127.0.0.1:1/must-not-fetch")
	run := func(args ...string) (string, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, wrapper, append([]string{"-cache", cache, "-attempts", "1"}, args...)...)
		cmd.Dir, cmd.Env = root, env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	out, err := run()
	if err == nil || !strings.Contains(out, "this code calls 1 known vulnerability/ies: GO-2099-0001") {
		t.Fatalf("default command missed CLI-only call through excluded helper: err=%v\n%s", err, out)
	}
	out, err = run("-patterns", "./pkg/...")
	if err != nil || !strings.Contains(out, "no called vulnerabilities") {
		t.Fatalf("library-only negative control: err=%v\n%s", err, out)
	}
	fixtureFile(t, root, "cmd/fdb/main.go", "package main\nfunc main() {}\n")
	out, err = run()
	if err != nil || !strings.Contains(out, "no called vulnerabilities") {
		t.Fatalf("removed CLI call negative control: err=%v\n%s", err, out)
	}
	out, err = run("-patterns", "./pkg/testcontainers/...")
	if err == nil || !strings.Contains(out, "the package list came back empty, so the scan would have covered nothing") {
		t.Fatalf("empty filtered population did not fail at the gate: err=%v\n%s", err, out)
	}
}
