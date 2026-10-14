package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These paths all refuse (exit 2) before openCluster, so no FoundationDB is
// needed; the test also makes `just test` compile main.go with the pure-Go
// backend.
func TestRefusesBeforeTouchingTheCluster(t *testing.T) {
	dir := t.TempDir()
	cluster := filepath.Join(dir, "fdb.cluster")
	if err := os.WriteFile(cluster, []byte("fdbbenchvpc:mn20261014@10.88.0.11:4500,10.88.0.12:4500,10.88.0.13:4500\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(dir, "fixture.json")
	if err := os.WriteFile(fixture, []byte(`{"phase":"fixture","payload":"small","rows":1000,"prefix_bytes":3}`), 0o644); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	badCluster := filepath.Join(dir, "bad.cluster")
	if err := os.WriteFile(badCluster, []byte("no connection string\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.json")
	for name, args := range map[string][]string{
		"no command":              {},
		"unknown command":         {"bogus"},
		"unknown store":           {"probe", "--cluster-file", cluster, "--store", "benchfetch/nope", "--fixture-receipt", fixture, "--output", out},
		"wrong prefix length":     {"probe", "--cluster-file", cluster, "--store", "benchfetch/small", "--fixture-receipt", fixture, "--output", out},
		"no fixture receipt":      {"ballast-clear", "--cluster-file", cluster, "--store", "benchfetch/small", "--output", out},
		"ballast on control":      {"ballast-write", "--cluster-file", cluster, "--store", "benchfetch/control-split", "--fixture-receipt", fixture, "--ballast-mib", "1", "--output", out},
		"control on benchmark":    {"control-clear", "--cluster-file", cluster, "--store", "benchfetch/small", "--output", out},
		"control with fixture":    {"control-clear", "--cluster-file", cluster, "--store", "benchfetch/control-split", "--fixture-receipt", fixture, "--output", out},
		"output inside a repo":    {"control-clear", "--cluster-file", cluster, "--store", "benchfetch/control-split", "--output", filepath.Join(repo, "o.json")},
		"output exists":           {"control-clear", "--cluster-file", cluster, "--store", "benchfetch/control-split", "--output", fixture},
		"malformed cluster file":  {"control-clear", "--cluster-file", badCluster, "--store", "benchfetch/control-split", "--output", out},
		"missing cluster file":    {"control-clear", "--store", "benchfetch/control-split", "--output", out},
		"mutate without a defect": {"control-mutate", "--cluster-file", cluster, "--store", "benchfetch/control-split", "--output", out},
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(args, &stdout, &stderr); code != 2 {
				t.Fatalf("exit %d, want 2 (stderr %q)", code, stderr.String())
			}
			if _, err := os.Stat(out); err == nil {
				t.Fatal("an output was written by a refused command")
			}
		})
	}
	var stderr bytes.Buffer
	run([]string{"bogus"}, &bytes.Buffer{}, &stderr)
	if !strings.Contains(stderr.String(), "benchfetch/control-split") {
		t.Fatalf("usage must list the owned stores: %q", stderr.String())
	}
}
