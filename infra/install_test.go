package infra

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

const installerVersion = "v0.2.0"

// installerMinRetries is the least curl --retry every installer fetch must pass: its
// backoff (1+2+...+32 s) outlasts about a minute of GitHub release-download 5xx.
const installerMinRetries = 6

// The installer runs unchanged against local release fixtures. curl never calls
// the network, and uname fixes the asset selection independently of the host.
type installerFixture struct {
	root       string
	bin        string
	home       string
	installDir string
	log        string
	boundsLog  string
	script     string
	asset      string
	binary     []byte
}

func newInstallerFixture(t *testing.T) *installerFixture {
	t.Helper()
	root := t.TempDir()
	f := &installerFixture{
		root:       root,
		bin:        filepath.Join(root, "bin"),
		home:       filepath.Join(root, "home"),
		installDir: filepath.Join(root, "install dir"),
		log:        filepath.Join(root, "requests"),
		boundsLog:  filepath.Join(root, "bounds"),
		script:     "../website/static/install.sh",
		asset:      "fdb_" + installerVersion + "_linux_amd64.tar.gz",
		binary:     []byte("#!/bin/sh\n[ \"$*\" = 'version --short' ] || exit 64\nprintf '%s\\n' '" + installerVersion + "'\n"),
	}
	for _, dir := range []string{f.bin, f.home, filepath.Join(root, "tmp")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	stubs := map[string]string{
		"uname": `#!/bin/sh
set -eu
case "$*" in
  -s) printf '%s\n' "${INSTALL_FIXTURE_OS:-Linux}" ;;
  -m) printf '%s\n' "${INSTALL_FIXTURE_ARCH:-x86_64}" ;;
  *) exit 91 ;;
esac
`,
		"curl": `#!/bin/sh
set -eu
out= url= bounds=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    --retry|--connect-timeout|--speed-limit|--speed-time) bounds="$bounds $1 $2"; shift 2 ;;
    -*) shift ;;
    *) url="$1"; shift ;;
  esac
done
[ -n "$out" ] && [ -n "$url" ] || exit 91
printf '%s\n' "$url" >> "$INSTALL_FIXTURE_LOG"
printf '%s%s\n' "$url" "$bounds" >> "$INSTALL_FIXTURE_ROOT/bounds"
case "$url" in
  https://api.example.invalid/project/releases\?per_page=100)
    source="$INSTALL_FIXTURE_ROOT/releases.json" ;;
  https://downloads.example.invalid/project/releases/download/v0.2.0/*)
    source="$INSTALL_FIXTURE_ROOT/${url##*/}" ;;
  *) exit 22 ;;
esac
cp "$source" "$out" || exit 22
`,
	}
	for name, stub := range stubs {
		writeBuildStub(t, filepath.Join(f.bin, name), stub)
	}
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "fdb", Mode: 0o755, Size: int64(len(f.binary))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(f.binary); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	f.write(t, f.asset, archive.Bytes())
	f.write(t, "checksums.txt", []byte(fmt.Sprintf("%x  %s\n", sha256.Sum256(archive.Bytes()), f.asset)))
	f.write(t, "requests", nil)
	f.write(t, "bounds", nil)
	f.write(t, "releases.json", []byte(`[
  {"tag_name":"cmd/frl/v9.9.9"},
  {"tag_name":"cmd/fdb/v9.9.9"},
  {"tag_name":"v9.9.9-rc.1"},
  {"tag_name":"v0.2.0"},
  {"tag_name":"v0.1.0"}
]`))
	return f
}

func (f *installerFixture) write(t *testing.T, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.root, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *installerFixture) run(t *testing.T, overrides map[string]string, args ...string) (string, error) {
	t.Helper()
	script, err := filepath.Abs(f.script)
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"HOME":            f.home,
		"TMPDIR":          filepath.Join(f.root, "tmp"),
		"PATH":            f.bin + ":" + os.Getenv("PATH"),
		"SHELL":           "/bin/sh",
		"NO_COLOR":        "1",
		"FDB_CLI_VERSION": installerVersion,
		"FDB_INSTALL_DIR": f.installDir,
		"FDB_BASE_URL":    "https://downloads.example.invalid/project",
		"FDB_API_URL":     "https://api.example.invalid/project",
		// FDB_VERSION selects the FoundationDB server, not the CLI release.
		"FDB_VERSION":          "7.3.77",
		"INSTALL_FIXTURE_ROOT": f.root,
		"INSTALL_FIXTURE_LOG":  f.log,
		"INSTALL_FIXTURE_OS":   "Linux",
		"INSTALL_FIXTURE_ARCH": "x86_64",
		// Old installer settings must neither override nor supply defaults.
		"FRL_VERSION":     "cmd/frl/v9.9.9",
		"FRL_INSTALL_DIR": filepath.Join(f.root, "legacy"),
		"FRL_BASE_URL":    "https://legacy.example.invalid",
		"FRL_API_URL":     "https://legacy.example.invalid/api",
	}
	for key, value := range overrides {
		env[key] = value
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", append([]string{script}, args...)...)
	cmd.Dir = f.root
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, replaced := env[key]; !replaced {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	for key, value := range env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (f *installerFixture) requests(t *testing.T, want ...string) {
	t.Helper()
	data, err := os.ReadFile(f.log)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Fields(string(data))
	if !slices.Equal(got, want) {
		t.Fatalf("download requests = %q, want %q", got, want)
	}
	if err := f.fetchBounds(len(want)); err != nil {
		t.Fatal(err)
	}
}

// fetchBounds checks that each of the n recorded fetches passed curl at least
// installerMinRetries retries, a connect timeout and a stall bound.
func (f *installerFixture) fetchBounds(n int) error {
	data, err := os.ReadFile(f.boundsLog)
	if err != nil {
		return err
	}
	var lines []string
	if s := strings.TrimSuffix(string(data), "\n"); s != "" {
		lines = strings.Split(s, "\n")
	}
	if len(lines) != n {
		return fmt.Errorf("recorded %d fetches, want %d", len(lines), n)
	}
	for _, line := range lines {
		fields := strings.Fields(line)
		if err := curlBoundsError(fields[1:], installerMinRetries); err != nil {
			return fmt.Errorf("fetch of %s: %w", fields[0], err)
		}
	}
	return nil
}

func (f *installerFixture) checkInstalled(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "fdb" {
		t.Fatalf("install directory must contain only fdb, got %v", entries)
	}
	path := filepath.Join(dir, "fdb")
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, f.binary) {
		t.Fatalf("installed binary differs from verified release: %v", err)
	}
	out, err := exec.Command(path, "version", "--short").CombinedOutput()
	if err != nil || string(out) != installerVersion+"\n" {
		t.Fatalf("installed version = %q, error %v", out, err)
	}
	for _, path := range []string{filepath.Join(f.root, "legacy"), filepath.Join(f.home, ".frl")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("installer touched legacy path %s: %v", path, err)
		}
	}
}

func TestCLIInstallerRootReleases(t *testing.T) {
	t.Parallel()
	for _, version := range []string{installerVersion, "0.2.0", "latest", ""} {
		name := "env=" + version
		if version == "" {
			name = "inherited-server-version-is-not-cli-version"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newInstallerFixture(t)
			out, err := f.run(t, map[string]string{"FDB_CLI_VERSION": version})
			if err != nil {
				t.Fatalf("install root release: %v\n%s", err, out)
			}
			if !strings.Contains(out, "fdb dev up") || !strings.Contains(out, "fdb sql") || strings.Contains(out, "frl") {
				t.Fatalf("installer guidance must use the fdb command tree:\n%s", out)
			}
			f.checkInstalled(t, f.installDir)
			urls := []string{}
			if version == "latest" || version == "" {
				urls = append(urls, "https://api.example.invalid/project/releases?per_page=100")
			}
			urls = append(urls,
				"https://downloads.example.invalid/project/releases/download/"+installerVersion+"/"+f.asset,
				"https://downloads.example.invalid/project/releases/download/"+installerVersion+"/checksums.txt")
			f.requests(t, urls...)
		})
	}
}

func TestCLIInstallerArgumentAndDirectoryOverrides(t *testing.T) {
	t.Parallel()
	for _, useDefaultDir := range []bool{false, true} {
		t.Run(fmt.Sprintf("default-dir=%t", useDefaultDir), func(t *testing.T) {
			t.Parallel()
			f := newInstallerFixture(t)
			env := map[string]string{"FDB_CLI_VERSION": "v9.9.9"}
			args := []string{"--version", installerVersion}
			dir := filepath.Join(f.root, "argument dir")
			if useDefaultDir {
				env["FDB_INSTALL_DIR"] = ""
				dir = filepath.Join(f.home, ".local", "bin")
			} else {
				args = append(args, "--dir", dir)
			}
			out, err := f.run(t, env, args...)
			if err != nil {
				t.Fatalf("install with overrides: %v\n%s", err, out)
			}
			f.checkInstalled(t, dir)
			f.requests(t,
				"https://downloads.example.invalid/project/releases/download/"+installerVersion+"/"+f.asset,
				"https://downloads.example.invalid/project/releases/download/"+installerVersion+"/checksums.txt")
		})
	}
}

func TestCLIInstallerRejectsSlashedTags(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"cmd/frl/v0.2.0", "cmd/fdb/v0.2.0", "v0.2.0/other", "cmd%2Ffrl%2Fv0.2.0", "v0.2.0\n/other"} {
		for _, viaFlag := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/flag=%t", version, viaFlag), func(t *testing.T) {
				t.Parallel()
				f := newInstallerFixture(t)
				env := map[string]string{"FDB_CLI_VERSION": version}
				var args []string
				if viaFlag {
					env["FDB_CLI_VERSION"] = installerVersion
					args = []string{"--version", version}
				}
				out, err := f.run(t, env, args...)
				if err == nil || !strings.Contains(out, "invalid version:") {
					t.Fatalf("slashed tag must fail validation: %v\n%s", err, out)
				}
				f.requests(t)
				if _, err := os.Stat(f.installDir); !os.IsNotExist(err) {
					t.Fatalf("invalid version created an install directory: %v", err)
				}
			})
		}
	}
}

func TestCLIInstallerRefusesLegacyArchivesAndInvalidChecksums(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"legacy-archive", "checksum-mismatch", "missing-checksums", "nested-releases"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			f := newInstallerFixture(t)
			env := map[string]string{}
			wantError := ""
			urls := []string{"https://downloads.example.invalid/project/releases/download/" + installerVersion + "/" + f.asset}
			switch mode {
			case "legacy-archive":
				if err := os.Rename(filepath.Join(f.root, f.asset), filepath.Join(f.root, strings.Replace(f.asset, "fdb_", "frl_", 1))); err != nil {
					t.Fatal(err)
				}
				wantError = "download failed:"
			case "checksum-mismatch":
				f.write(t, "checksums.txt", []byte(strings.Repeat("0", 64)+"  "+f.asset+"\n"))
				wantError = "sha256 mismatch"
			case "missing-checksums":
				if err := os.Remove(filepath.Join(f.root, "checksums.txt")); err != nil {
					t.Fatal(err)
				}
				wantError = "refusing to install an unverified binary"
			case "nested-releases":
				f.write(t, "releases.json", []byte(`[{"tag_name":"cmd/frl/v0.2.0"},{"tag_name":"cmd/fdb/v0.2.0"}]`))
				env["FDB_CLI_VERSION"] = "latest"
				urls = []string{"https://api.example.invalid/project/releases?per_page=100"}
				wantError = "none tagged vX.Y.Z"
			}
			if mode == "checksum-mismatch" || mode == "missing-checksums" {
				urls = append(urls, "https://downloads.example.invalid/project/releases/download/"+installerVersion+"/checksums.txt")
			}
			out, err := f.run(t, env)
			if err == nil || !strings.Contains(out, wantError) {
				t.Fatalf("installer must reject %s: %v\n%s", mode, err, out)
			}
			f.requests(t, urls...)
			if _, err := os.Stat(f.installDir); !os.IsNotExist(err) {
				t.Fatalf("failed verification created an install directory: %v", err)
			}
		})
	}
}

func TestCLIInstallerSourceBuildGuidance(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"help", "api-failure", "empty-releases", "nested-releases", "unsupported-os", "unsupported-architecture"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			f := newInstallerFixture(t)
			env := map[string]string{"FDB_CLI_VERSION": "latest"}
			var args, requests []string
			want := ""
			switch mode {
			case "help":
				args = []string{"--help"}
				want = "build from a current checkout:"
			case "api-failure":
				if err := os.Remove(filepath.Join(f.root, "releases.json")); err != nil {
					t.Fatal(err)
				}
				want = "could not reach the GitHub release API"
			case "empty-releases":
				f.write(t, "releases.json", []byte("[]"))
				want = "has published no releases"
			case "nested-releases":
				f.write(t, "releases.json", []byte(`[{"tag_name":"cmd/fdb/v0.2.0"}]`))
				want = "none tagged vX.Y.Z"
			case "unsupported-os":
				env["INSTALL_FIXTURE_OS"] = "UnsupportedOS"
				want = "unsupported OS: UnsupportedOS"
			case "unsupported-architecture":
				env["INSTALL_FIXTURE_ARCH"] = "unsupported-arch"
				want = "unsupported architecture: unsupported-arch"
			}
			if mode == "api-failure" || mode == "empty-releases" || mode == "nested-releases" {
				requests = []string{"https://api.example.invalid/project/releases?per_page=100"}
			}
			out, err := f.run(t, env, args...)
			if (err == nil) != (mode == "help") || !strings.Contains(out, want) {
				t.Fatalf("%s: error=%v, want %q\n%s", mode, err, want, out)
			}
			if !strings.Contains(out, "https://fdb.dev/docs/cli/#install") || strings.Contains(out, "go install fdb.dev/cmd/fdb@latest") {
				t.Fatalf("source guidance must work before the first renamed CLI release:\n%s", out)
			}
			f.requests(t, requests...)
			if _, err := os.Stat(f.installDir); !os.IsNotExist(err) {
				t.Fatalf("guidance installed a binary: %v", err)
			}
		})
	}
}

func TestCLIInstallerUninstallsOnlyFDB(t *testing.T) {
	t.Parallel()
	f := newInstallerFixture(t)
	if out, err := f.run(t, nil); err != nil {
		t.Fatalf("install before uninstall: %v\n%s", err, out)
	}
	config := filepath.Join(f.home, ".fdb", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(config), 0o700); err != nil {
		t.Fatal(err)
	}
	preserved := map[string]string{
		config:                             "current_context: local\n",
		filepath.Join(f.installDir, "frl"): "existing unrelated binary",
	}
	for path, content := range preserved {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out, err := f.run(t, nil, "--uninstall")
	if err != nil || !strings.Contains(out, "~/.fdb") {
		t.Fatalf("uninstall fdb: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(f.installDir, "fdb")); !os.IsNotExist(err) {
		t.Fatalf("uninstall left fdb: %v", err)
	}
	for path, want := range preserved {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != want {
			t.Fatalf("uninstall changed unrelated path %s: %q, %v", path, data, err)
		}
	}
	f.requests(t,
		"https://downloads.example.invalid/project/releases/download/"+installerVersion+"/"+f.asset,
		"https://downloads.example.invalid/project/releases/download/"+installerVersion+"/checksums.txt")
}

// TestCLIInstallerFetchBounds is the positive control for the check requests() applies to
// every fetch: weakened installers still install, but fail it.
func TestCLIInstallerFetchBounds(t *testing.T) {
	t.Parallel()
	shipped, err := os.ReadFile("../website/static/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	retry := regexp.MustCompile(`--retry [0-9]+ `).FindAllString(string(shipped), -1)
	if len(retry) != 1 {
		t.Fatalf("installer passes curl --retry %d times, want once", len(retry))
	}
	for _, tc := range []struct {
		name, old, new string
		n              int
		want           string
	}{
		{"shipped", "", "", 2, ""},
		{"miscounted", "", "", 1, "recorded 2 fetches, want 1"},
		{"short-retry", retry[0], "--retry 3 ", 2, "--retry 3, want at least 6"},
		{"no-retry", retry[0], "", 2, "no --retry"},
		{"no-connect-timeout", " --connect-timeout 15", "", 2, "no --connect-timeout"},
		{"no-stall-bound", " --speed-limit 1024 --speed-time 60", "", 2, "no --speed-limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			script := string(shipped)
			if tc.old != "" {
				if n := strings.Count(script, tc.old); n != 1 {
					t.Fatalf("installer contains %q %d times, want once", tc.old, n)
				}
				script = strings.Replace(script, tc.old, tc.new, 1)
			}
			f := newInstallerFixture(t)
			f.script = filepath.Join(f.root, "install.sh")
			f.write(t, "install.sh", []byte(script))
			if out, err := f.run(t, nil); err != nil {
				t.Fatalf("install: %v\n%s", err, out)
			}
			f.checkInstalled(t, f.installDir)
			err := f.fetchBounds(tc.n)
			if (tc.want == "" && err != nil) || (tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want))) {
				t.Fatalf("bounds check = %v, want %q", err, tc.want)
			}
		})
	}
	if err := (&installerFixture{boundsLog: filepath.Join(t.TempDir(), "absent")}).fetchBounds(0); !os.IsNotExist(err) {
		t.Fatalf("missing bounds log: %v, want not-exist", err)
	}
}
