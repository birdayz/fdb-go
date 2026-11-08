package infra

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// GitHub release downloads have returned 503/504 for minutes at a time. curl's backoff
// doubles from 1 s, so 8 retries span 255 s; --retry-max-time keeps the window bounded.
const (
	ciMinRetries      = 8
	ciMinRetryMaxTime = 180
	ciMaxRetryMaxTime = 600
)

// ciRetryError reports why curl argv does not retry transient errors over a bounded
// window of a few minutes, or nil if it does.
func ciRetryError(args []string) error {
	values := map[string]int{}
	for i := 0; i < len(args); i++ {
		if args[i] != "--retry" && args[i] != "--retry-max-time" {
			continue
		}
		if i+1 == len(args) {
			return fmt.Errorf("curl %s has no value", args[i])
		}
		n, err := strconv.Atoi(args[i+1])
		if err != nil {
			return fmt.Errorf("curl %s %q is not a number", args[i], args[i+1])
		}
		values[args[i]] = n
		i++
	}
	retries, ok := values["--retry"]
	if !ok {
		return fmt.Errorf("curl has no --retry")
	}
	if retries < ciMinRetries {
		return fmt.Errorf("curl --retry %d, want at least %d", retries, ciMinRetries)
	}
	window, ok := values["--retry-max-time"]
	if !ok {
		return fmt.Errorf("curl has no --retry-max-time")
	}
	if window < ciMinRetryMaxTime || window > ciMaxRetryMaxTime {
		return fmt.Errorf("curl --retry-max-time %d, want %d..%d seconds", window, ciMinRetryMaxTime, ciMaxRetryMaxTime)
	}
	return nil
}

func TestCIRetryGuard(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
		want string // error substring; "" passes
	}{
		{"bounded", []string{"-fsSL", "--retry", "8", "--retry-max-time", "300", "-o", "out", "url"}, ""},
		{"nil", nil, "no --retry"},
		{"no-retry", []string{"-fsSL", "-o", "out", "url"}, "no --retry"},
		{"short-retry", []string{"--retry", "3", "--retry-max-time", "300"}, "--retry 3,"},
		{"no-window", []string{"--retry", "8"}, "no --retry-max-time"},
		{"short-window", []string{"--retry", "8", "--retry-max-time", "60"}, "--retry-max-time 60,"},
		{"unbounded-window", []string{"--retry", "8", "--retry-max-time", "3600"}, "--retry-max-time 3600,"},
		{"not-a-number", []string{"--retry", "many", "--retry-max-time", "300"}, "not a number"},
		{"dangling", []string{"--retry-max-time", "300", "--retry"}, "has no value"},
	} {
		err := ciRetryError(tc.args)
		if (tc.want == "" && err != nil) || (tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want))) {
			t.Errorf("%s: ciRetryError = %v, want %q", tc.name, err, tc.want)
		}
	}
}

// curlStub records its argv, one argument per line, then writes CURL_BODY to its -o
// target or, with CURL_FAIL=1, leaves a partial file there and fails like a reset transfer.
const curlStub = `#!/bin/sh
printf '%s\n' "$@" > "$CURL_ARGS"
out=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    *) shift ;;
  esac
done
[ -n "$out" ] || exit 91
if [ "${CURL_FAIL:-}" = 1 ]; then printf partial > "$out"; exit 56; fi
printf '%s' "$CURL_BODY" > "$out"
`

type downloadHarness struct{ root, bin string }

func newDownloadHarness(t *testing.T, stubs map[string]string) *downloadHarness {
	t.Helper()
	h := &downloadHarness{root: t.TempDir()}
	h.bin = filepath.Join(h.root, "bin")
	if err := os.Mkdir(h.bin, 0o700); err != nil {
		t.Fatal(err)
	}
	writeBuildStub(t, filepath.Join(h.bin, "curl"), curlStub)
	for name, body := range stubs {
		writeBuildStub(t, filepath.Join(h.bin, name), body)
	}
	return h
}

// run executes command in dir with the stubs first on PATH. curlArgs is nil when curl
// never ran.
func (h *downloadHarness) run(t *testing.T, dir string, env []string, command ...string) (out string, curlArgs []string, err error) {
	t.Helper()
	argsFile := filepath.Join(h.root, "curl-args")
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "PATH="+h.bin+":"+os.Getenv("PATH"), "CURL_ARGS="+argsFile), env...)
	output, err := cmd.CombinedOutput()
	data, readErr := os.ReadFile(argsFile)
	if readErr == nil {
		curlArgs = strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	} else if !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	return string(output), curlArgs, err
}

// ensureBufScript returns the justfile's ensure-buf recipe as a bash script, with the
// pinned buf version substituted as just would.
func ensureBufScript(t *testing.T) (script, version string) {
	t.Helper()
	data, err := os.ReadFile("../justfile")
	if err != nil {
		t.Fatal(err)
	}
	pin := regexp.MustCompile(`(?m)^BUF_VERSION := "([^"]+)"$`).FindSubmatch(data)
	if pin == nil {
		t.Fatal("justfile pins no BUF_VERSION")
	}
	_, rest, ok := strings.Cut(string(data), "\nensure-buf:\n")
	if !ok {
		t.Fatal("missing ensure-buf recipe")
	}
	body, _, _ := strings.Cut(rest, "\n\n")
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimPrefix(line, "    ")
	}
	script = strings.ReplaceAll(strings.Join(lines, "\n")+"\n", "{{BUF_VERSION}}", string(pin[1]))
	if strings.Contains(script, "{{") {
		t.Fatalf("ensure-buf has an interpolation this test does not model:\n%s", script)
	}
	return script, string(pin[1])
}

// TestEnsureBufDownload runs the recipe CI's `just generate` starts with: the download
// retries transient failures, and a failed one leaves neither a new file in .tools nor
// a damaged previous buf, so the version check stays what a later run trusts.
func TestEnsureBufDownload(t *testing.T) {
	t.Parallel()
	script, version := ensureBufScript(t)
	buf := func(v string) string { return "#!/bin/sh\nprintf '%s\\n' '" + v + "'\n" }
	for _, tc := range []struct {
		name, existing string
		fail, fetch    bool
		want           string // .tools/buf afterwards; "" means absent
	}{
		{name: "current", existing: buf(version), want: buf(version)},
		{name: "fresh", fetch: true, want: buf(version)},
		{name: "stale", existing: buf("0.0.1"), fetch: true, want: buf(version)},
		{name: "fresh-failure", fail: true, fetch: true},
		{name: "stale-failure", existing: buf("0.0.1"), fail: true, fetch: true, want: buf("0.0.1")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newDownloadHarness(t, nil)
			work := filepath.Join(h.root, "work")
			tools := filepath.Join(work, ".tools")
			if err := os.MkdirAll(work, 0o700); err != nil {
				t.Fatal(err)
			}
			if tc.existing != "" {
				if err := os.Mkdir(tools, 0o700); err != nil {
					t.Fatal(err)
				}
				writeBuildStub(t, filepath.Join(tools, "buf"), tc.existing)
			}
			recipe := filepath.Join(h.root, "ensure-buf.sh")
			if err := os.WriteFile(recipe, []byte(script), 0o600); err != nil {
				t.Fatal(err)
			}
			fail := "0"
			if tc.fail {
				fail = "1"
			}
			out, args, err := h.run(t, work, []string{"CURL_BODY=" + buf(version), "CURL_FAIL=" + fail}, "bash", recipe)
			if (err != nil) != tc.fail {
				t.Fatalf("ensure-buf error = %v, want failure %t\n%s", err, tc.fail, out)
			}
			if (args != nil) != tc.fetch {
				t.Fatalf("curl ran = %t, want %t\n%s", args != nil, tc.fetch, out)
			}
			if tc.fetch {
				if err := ciRetryError(args); err != nil {
					t.Fatalf("buf download: %v", err)
				}
				release := "https://github.com/bufbuild/buf/releases/download/v" + version + "/buf-"
				if !slices.ContainsFunc(args, func(arg string) bool { return strings.HasPrefix(arg, release) }) {
					t.Fatalf("curl args %q do not fetch %s*", args, release)
				}
			}
			entries, err := os.ReadDir(tools)
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, entry := range entries {
				names = append(names, entry.Name())
			}
			if tc.want == "" {
				if len(names) != 0 {
					t.Fatalf("failed download left %q in .tools", names)
				}
				return
			}
			if !slices.Equal(names, []string{"buf"}) {
				t.Fatalf(".tools = %q, want only buf", names)
			}
			path := filepath.Join(tools, "buf")
			data, err := os.ReadFile(path)
			if err != nil || string(data) != tc.want {
				t.Fatalf(".tools/buf = %q, want %q: %v", data, tc.want, err)
			}
			if info, err := os.Stat(path); err != nil || (tc.fetch && !tc.fail && info.Mode().Perm() != 0o755) {
				t.Fatalf("installed buf is not mode 0755: %v, %v", info, err)
			}
		})
	}
}

// TestWebsiteHugoDownload runs the website build's Hugo install step: the download
// retries transient failures, and a failed one stops the step before dpkg runs.
func TestWebsiteHugoDownload(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../.github/workflows/website.yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Env   map[string]string `yaml:"env"`
			Steps []struct {
				Name string `yaml:"name"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	build := workflow.Jobs["build"]
	version := build.Env["HUGO_VERSION"]
	var script string
	for _, step := range build.Steps {
		if step.Name == "Install Hugo CLI (extended)" {
			script = step.Run
		}
	}
	if script == "" || version == "" {
		t.Fatal("website build has no pinned Hugo install step")
	}
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("fail=%t", fail), func(t *testing.T) {
			t.Parallel()
			h := newDownloadHarness(t, map[string]string{"sudo": "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$SUDO_LOG\"\n"})
			step := strings.ReplaceAll(script, "${{ runner.temp }}", h.root)
			if strings.Contains(step, "${{") {
				t.Fatalf("Hugo step has an expression this test does not model:\n%s", step)
			}
			sudoLog := filepath.Join(h.root, "sudo.log")
			env := []string{"HUGO_VERSION=" + version, "SUDO_LOG=" + sudoLog, "CURL_BODY=deb", "CURL_FAIL=0"}
			if fail {
				env[len(env)-1] = "CURL_FAIL=1"
			}
			out, args, err := h.run(t, h.root, env, "bash", "-eo", "pipefail", "-c", step)
			if (err != nil) != fail {
				t.Fatalf("Hugo step error = %v, want failure %t\n%s", err, fail, out)
			}
			if err := ciRetryError(args); err != nil {
				t.Fatalf("Hugo download: %v", err)
			}
			url := "https://github.com/gohugoio/hugo/releases/download/v" + version + "/hugo_extended_" + version + "_linux-amd64.deb"
			if !slices.Contains(args, url) {
				t.Fatalf("curl args %q do not fetch %s", args, url)
			}
			calls, err := os.ReadFile(sudoLog)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			want := ""
			if !fail {
				want = "dpkg -i " + filepath.Join(h.root, "hugo.deb") + "\n"
			}
			if string(calls) != want {
				t.Fatalf("sudo calls = %q, want %q", calls, want)
			}
		})
	}
}

// TestFetchVerifiedRetries runs the provisioning helper every pinned runner tool install
// downloads through.
func TestFetchVerifiedRetries(t *testing.T) {
	t.Parallel()
	var config struct {
		Files []struct {
			Path    string `yaml:"path"`
			Content string `yaml:"content"`
		} `yaml:"write_files"`
	}
	renderWith(t, nil, &config)
	var helper string
	for _, file := range config.Files {
		if file.Path == "/usr/local/bin/fetch-verified.sh" {
			helper = file.Content
		}
	}
	if helper == "" {
		t.Fatal("cloud-init installs no fetch-verified.sh")
	}
	h := newDownloadHarness(t, nil)
	script := filepath.Join(h.root, "fetch-verified.sh")
	if err := os.WriteFile(script, []byte(helper), 0o600); err != nil {
		t.Fatal(err)
	}
	body := "pinned artifact\n"
	dest := filepath.Join(h.root, "artifact")
	url := "https://github.com/example/tool/releases/download/v1.0.0/tool"
	out, args, err := h.run(t, h.root, []string{"CURL_BODY=" + body}, "bash", script, url, fmt.Sprintf("%x", sha256.Sum256([]byte(body))), dest)
	if err != nil {
		t.Fatalf("fetch-verified.sh: %v\n%s", err, out)
	}
	if err := ciRetryError(args); err != nil {
		t.Fatalf("fetch-verified.sh download: %v", err)
	}
	if !slices.Contains(args, url) {
		t.Fatalf("curl args %q do not fetch %s", args, url)
	}
	if data, err := os.ReadFile(dest); err != nil || string(data) != body {
		t.Fatalf("fetched artifact = %q, want %q: %v", data, body, err)
	}
}
