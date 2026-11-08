package infra

import (
	"crypto/sha256"
	"errors"
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

// GitHub release downloads have returned 503/504 for minutes, some only after 40-90 s.
// CI retries 8 times within 600 s; an attempt below 1 KiB/s for 60 s is cut and retried.
const (
	ciMinRetries      = 8
	ciMinRetryMaxTime = 180
	ciMaxRetryMaxTime = 600
)

var curlNumericFlags = []string{"--retry", "--retry-max-time", "--connect-timeout", "--speed-limit", "--speed-time"}

// curlFlags reads curl's numeric options and whether -f is set, alone, as --fail or in a
// cluster such as -fsSL.
func curlFlags(args []string) (values map[string]int, fail bool, err error) {
	values = map[string]int{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case slices.Contains(curlNumericFlags, arg):
			if i+1 == len(args) {
				return nil, false, fmt.Errorf("curl %s has no value", arg)
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil {
				return nil, false, fmt.Errorf("curl %s %q is not a number", arg, args[i+1])
			}
			values[arg] = n
			i++
		case arg == "--fail" || (len(arg) > 1 && arg[0] == '-' && arg[1] != '-' && strings.Contains(arg, "f")):
			fail = true
		}
	}
	return values, fail, nil
}

func curlFlagIn(values map[string]int, flag string, lo, hi int) error {
	n, ok := values[flag]
	switch {
	case !ok:
		return fmt.Errorf("curl has no %s", flag)
	case n < lo:
		return fmt.Errorf("curl %s %d, want at least %d", flag, n, lo)
	case hi > 0 && n > hi:
		return fmt.Errorf("curl %s %d, want at most %d", flag, n, hi)
	}
	return nil
}

// curlBoundsError reports why curl argv does not bound a download: at least minRetries
// retries, a connect timeout and a stall bound on every attempt.
func curlBoundsError(args []string, minRetries int) error {
	values, _, err := curlFlags(args)
	if err != nil {
		return err
	}
	for _, b := range []struct {
		flag   string
		lo, hi int // hi 0 is unbounded
	}{
		{"--retry", minRetries, 0},
		{"--connect-timeout", 1, 60},
		{"--speed-limit", 1, 0},
		{"--speed-time", 1, 120},
	} {
		if err := curlFlagIn(values, b.flag, b.lo, b.hi); err != nil {
			return err
		}
	}
	return nil
}

// ciCurlError also requires -f, without which curl saves an HTTP error page and exits 0,
// and a retry window of a few minutes.
func ciCurlError(args []string) error {
	if err := curlBoundsError(args, ciMinRetries); err != nil {
		return err
	}
	values, fail, _ := curlFlags(args)
	if !fail {
		return errors.New("curl has no -f, so an HTTP error page is saved as the download")
	}
	return curlFlagIn(values, "--retry-max-time", ciMinRetryMaxTime, ciMaxRetryMaxTime)
}

func TestCurlBoundsGuard(t *testing.T) {
	t.Parallel()
	const ci = "-fsSL --retry 8 --retry-max-time 600 --connect-timeout 15 --speed-limit 1024 --speed-time 60 -o out url"
	const installer = "-fsSL --retry 6 --connect-timeout 15 --speed-limit 1024 --speed-time 60 -o out url"
	for _, tc := range []struct {
		name, argv string
		ci         bool
		want       string // error substring; "" passes
	}{
		{"ci", ci, true, ""},
		{"ci-long-fail", strings.Replace(ci, "-fsSL", "--fail -sSL", 1), true, ""},
		{"ci-empty", "", true, "no --retry"},
		{"ci-no-fail", strings.Replace(ci, "-fsSL", "-sSL", 1), true, "no -f"},
		{"ci-short-retry", strings.Replace(ci, "--retry 8", "--retry 6", 1), true, "--retry 6, want at least 8"},
		{"ci-no-window", strings.Replace(ci, " --retry-max-time 600", "", 1), true, "no --retry-max-time"},
		{"ci-short-window", strings.Replace(ci, "max-time 600", "max-time 60", 1), true, "--retry-max-time 60, want at least"},
		{"ci-long-window", strings.Replace(ci, "max-time 600", "max-time 3600", 1), true, "--retry-max-time 3600, want at most"},
		{"no-connect-timeout", strings.Replace(ci, " --connect-timeout 15", "", 1), true, "no --connect-timeout"},
		{"long-connect-timeout", strings.Replace(ci, "--connect-timeout 15", "--connect-timeout 300", 1), true, "--connect-timeout 300, want at most"},
		{"no-speed-limit", strings.Replace(ci, " --speed-limit 1024", "", 1), true, "no --speed-limit"},
		{"no-speed-time", strings.Replace(ci, " --speed-time 60", "", 1), true, "no --speed-time"},
		{"long-speed-time", strings.Replace(ci, "--speed-time 60", "--speed-time 600", 1), true, "--speed-time 600, want at most"},
		{"not-a-number", strings.Replace(ci, "--retry 8", "--retry many", 1), true, "not a number"},
		{"dangling", ci + " --retry", true, "has no value"},
		{"installer", installer, false, ""},
		{"installer-short-retry", strings.Replace(installer, "--retry 6", "--retry 3", 1), false, "--retry 3, want at least 6"},
		{"installer-no-speed-time", strings.Replace(installer, " --speed-time 60", "", 1), false, "no --speed-time"},
	} {
		err := ciCurlError(strings.Fields(tc.argv))
		if !tc.ci {
			err = curlBoundsError(strings.Fields(tc.argv), installerMinRetries)
		}
		if (tc.want == "" && err != nil) || (tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want))) {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
}

// curlStub records argv and serves CURL_BODY to -o. CURL_FAIL=http is a 503: exit 22 under
// -f, else the error page saved with exit 0. CURL_FAIL=partial is a reset mid-transfer.
const curlStub = `#!/bin/sh
printf '%s\n' "$@" > "$CURL_ARGS"
out= fail=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    --fail) fail=1; shift ;;
    --*) shift ;;
    -*f*) fail=1; shift ;;
    *) shift ;;
  esac
done
[ -n "$out" ] || exit 91
case "${CURL_FAIL:-}" in
  http) [ -z "$fail" ] || exit 22; printf '503 Service Unavailable' > "$out"; exit 0 ;;
  partial) printf partial > "$out"; exit 56 ;;
esac
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

func readRepoFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", path))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// downloadSites are the CI curl downloads; check runs one from its source file and joins
// every misbehaviour, so a mutated source can be shown to fail.
var downloadSites = map[string]struct {
	file  string
	check func(t *testing.T, src string) error
}{
	"hugo":           {".github/workflows/website.yml", checkHugo},
	"fetch-verified": {"infra/cloud-init.yaml", checkFetchVerified},
}

func TestCIDownloads(t *testing.T) {
	t.Parallel()
	for name, site := range downloadSites {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := site.check(t, readRepoFile(t, site.file)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestCIDownloadMutations is the positive control: each site's check fails once its source
// loses -f or the stall bound.
func TestCIDownloadMutations(t *testing.T) {
	t.Parallel()
	const stall = " --speed-limit 1024 --speed-time 60"
	for _, m := range []struct{ name, site, old, new, want string }{
		{"hugo-no-fail", "hugo", "curl -fsSL --retry", "curl -sSL --retry", "http: step succeeded"},
		{"hugo-no-stall-bound", "hugo", stall, "", "no --speed-limit"},
		{"fetch-verified-no-fail", "fetch-verified", "curl -fsSL --retry", "curl -sSL --retry", "http: exit status 1, want curl's exit 22"},
		{"fetch-verified-no-stall-bound", "fetch-verified", stall, "", "no --speed-limit"},
	} {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			site := downloadSites[m.site]
			src := readRepoFile(t, site.file)
			if n := strings.Count(src, m.old); n != 1 {
				t.Fatalf("%s contains %q %d times, want once", site.file, m.old, n)
			}
			err := site.check(t, strings.Replace(src, m.old, m.new, 1))
			if err == nil || !strings.Contains(err.Error(), m.want) {
				t.Fatalf("mutated %s: %v, want %q", m.site, err, m.want)
			}
		})
	}
}

// ensureBufScript returns the justfile's ensure-buf recipe as a bash script, with the
// pinned buf version substituted as just would.
func ensureBufScript(justfile string) (script, version string, err error) {
	pin := regexp.MustCompile(`(?m)^BUF_VERSION := "([^"]+)"$`).FindStringSubmatch(justfile)
	if pin == nil {
		return "", "", errors.New("justfile pins no BUF_VERSION")
	}
	_, rest, ok := strings.Cut(justfile, "\nensure-buf:\n")
	if !ok {
		return "", "", errors.New("missing ensure-buf recipe")
	}
	body, _, _ := strings.Cut(rest, "\n\n")
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimPrefix(line, "    ")
	}
	script = strings.ReplaceAll(strings.Join(lines, "\n")+"\n", "{{BUF_VERSION}}", pin[1])
	if strings.Contains(script, "{{") {
		return "", "", fmt.Errorf("ensure-buf has an interpolation this test does not model:\n%s", script)
	}
	return script, pin[1], nil
}

// goStub records its argv and installs GO_BUF as $GOBIN/buf; GO_FAIL=1 fails after leaving
// a partial binary there, the worst a failed install can do.
const goStub = `#!/bin/sh
printf '%s\n' "$@" > "$GO_ARGS"
case "${GOBIN:-}" in /*) ;; *) echo "go: GOBIN must be an absolute path" >&2; exit 2 ;; esac
mkdir -p "$GOBIN"
if [ "${GO_FAIL:-}" = 1 ]; then printf partial > "$GOBIN/buf"; exit 1; fi
printf '%s' "$GO_BUF" > "$GOBIN/buf"
chmod 755 "$GOBIN/buf"
`

// TestEnsureBufInstall runs the recipe `just generate` starts with; the mutations are its
// positive control.
func TestEnsureBufInstall(t *testing.T) {
	t.Parallel()
	justfile := readRepoFile(t, "justfile")
	for _, m := range []struct{ name, old, new, want string }{
		{"shipped", "", "", ""},
		{"direct-install", `GOBIN="$PWD/$tmp"`, `GOBIN="$PWD/.tools"`, "failure-stale: .tools/buf ="},
		{"no-version-check", `grep -q "{{BUF_VERSION}}"`, `grep -q "{{BUF_VERSION}}-never"`, "current: go ran = true, want false"},
		{"unpinned", "buf@v{{BUF_VERSION}}", "buf@latest", "fresh: go args ="},
	} {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			src := justfile
			if m.old != "" {
				if n := strings.Count(src, m.old); n != 1 {
					t.Fatalf("justfile contains %q %d times, want once", m.old, n)
				}
				src = strings.Replace(src, m.old, m.new, 1)
			}
			err := checkEnsureBuf(t, src)
			if (m.want == "" && err != nil) || (m.want != "" && (err == nil || !strings.Contains(err.Error(), m.want))) {
				t.Fatalf("ensure-buf: %v, want %q", err, m.want)
			}
		})
	}
}

// checkEnsureBuf runs ensure-buf against a go stub: a failed install leaves .tools and any
// previous buf untouched, so the version check stays what later runs trust.
func checkEnsureBuf(t *testing.T, justfile string) error {
	script, version, err := ensureBufScript(justfile)
	if err != nil {
		return err
	}
	buf := func(v string) string { return "#!/bin/sh\nprintf '%s\\n' '" + v + "'\n" }
	wantArgs := []string{"install", "github.com/bufbuild/buf/cmd/buf@v" + version}
	var errs []error
	for _, tc := range []struct {
		name, existing string
		fail           bool
		want           string // .tools/buf afterwards; "" means absent
	}{
		{name: "current", existing: buf(version), want: buf(version)},
		{name: "fresh", want: buf(version)},
		{name: "stale", existing: buf("0.0.1"), want: buf(version)},
		{name: "failure-no-buf", fail: true},
		{name: "failure-stale", existing: buf("0.0.1"), fail: true, want: buf("0.0.1")},
	} {
		h := newDownloadHarness(t, map[string]string{"go": goStub})
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
		goArgs := filepath.Join(h.root, "go-args")
		fail := "0"
		if tc.fail {
			fail = "1"
		}
		out, curlArgs, runErr := h.run(t, work, []string{"GO_ARGS=" + goArgs, "GO_BUF=" + buf(version), "GO_FAIL=" + fail}, "bash", recipe)
		var args []string
		if data, err := os.ReadFile(goArgs); err == nil {
			args = strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if err := ensureBufOutcome(tools, tc.existing != buf(version), tc.fail, tc.want, curlArgs, args, wantArgs, runErr); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w\n%s", tc.name, err, out))
		}
	}
	return errors.Join(errs...)
}

func ensureBufOutcome(tools string, build, fail bool, want string, curlArgs, goArgs, wantGoArgs []string, runErr error) error {
	switch {
	case runErr == nil && fail:
		return errors.New("ensure-buf succeeded, want failure")
	case runErr != nil && !fail:
		return fmt.Errorf("ensure-buf: %w", runErr)
	case curlArgs != nil:
		return fmt.Errorf("ensure-buf downloads with curl %q", curlArgs)
	case (goArgs != nil) != build:
		return fmt.Errorf("go ran = %t, want %t", goArgs != nil, build)
	case build && !slices.Equal(goArgs, wantGoArgs):
		return fmt.Errorf("go args = %q, want %q", goArgs, wantGoArgs)
	}
	entries, err := os.ReadDir(tools)
	if err != nil {
		return err
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if want == "" {
		if len(names) != 0 {
			return fmt.Errorf("failed install left %q in .tools", names)
		}
		return nil
	}
	if !slices.Equal(names, []string{"buf"}) {
		return fmt.Errorf(".tools = %q, want only buf", names)
	}
	path := filepath.Join(tools, "buf")
	data, err := os.ReadFile(path)
	if err != nil || string(data) != want {
		return fmt.Errorf(".tools/buf = %q, want %q: %v", data, want, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm()&0o100 == 0 {
		return fmt.Errorf("buf is not executable: %v, %v", info, err)
	}
	return nil
}

// checkHugo runs the website build's Hugo install step: a failed download stops the step
// before dpkg runs.
func checkHugo(t *testing.T, workflow string) error {
	var wf struct {
		Jobs map[string]struct {
			Env   map[string]string `yaml:"env"`
			Steps []struct {
				Name string `yaml:"name"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(workflow), &wf); err != nil {
		return err
	}
	build := wf.Jobs["build"]
	version := build.Env["HUGO_VERSION"]
	var script string
	for _, step := range build.Steps {
		if step.Name == "Install Hugo CLI (extended)" {
			script = step.Run
		}
	}
	if script == "" || version == "" {
		return errors.New("website build has no pinned Hugo install step")
	}
	var errs []error
	for _, mode := range []string{"ok", "http", "partial"} {
		h := newDownloadHarness(t, map[string]string{"sudo": "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$SUDO_LOG\"\n"})
		step := strings.ReplaceAll(script, "${{ runner.temp }}", h.root)
		if strings.Contains(step, "${{") {
			return fmt.Errorf("Hugo step has an expression this test does not model:\n%s", step)
		}
		sudoLog := filepath.Join(h.root, "sudo.log")
		env := []string{"HUGO_VERSION=" + version, "SUDO_LOG=" + sudoLog, "CURL_BODY=deb", "CURL_FAIL=" + mode}
		out, args, err := h.run(t, h.root, env, "bash", "-eo", "pipefail", "-c", step)
		if err := hugoOutcome(h.root, version, mode != "ok", args, err, sudoLog); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w\n%s", mode, err, out))
		}
	}
	return errors.Join(errs...)
}

func hugoOutcome(root, version string, fail bool, args []string, runErr error, sudoLog string) error {
	if runErr == nil && fail {
		return errors.New("step succeeded, want failure")
	}
	if runErr != nil && !fail {
		return fmt.Errorf("step: %w", runErr)
	}
	if err := ciCurlError(args); err != nil {
		return err
	}
	url := "https://github.com/gohugoio/hugo/releases/download/v" + version + "/hugo_extended_" + version + "_linux-amd64.deb"
	if !slices.Contains(args, url) {
		return fmt.Errorf("curl args %q do not fetch %s", args, url)
	}
	calls, err := os.ReadFile(sudoLog)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	want := ""
	if !fail {
		want = "dpkg -i " + filepath.Join(root, "hugo.deb") + "\n"
	}
	if string(calls) != want {
		return fmt.Errorf("sudo calls = %q, want %q", calls, want)
	}
	return nil
}

// checkFetchVerified runs the provisioning helper every pinned runner tool downloads through.
func checkFetchVerified(t *testing.T, template string) error {
	rendered, err := renderTemplate(template, templateVars)
	if err != nil {
		return err
	}
	var config struct {
		Files []struct {
			Path    string `yaml:"path"`
			Content string `yaml:"content"`
		} `yaml:"write_files"`
	}
	if err := yaml.Unmarshal([]byte(rendered), &config); err != nil {
		return err
	}
	var helper string
	for _, file := range config.Files {
		if file.Path == "/usr/local/bin/fetch-verified.sh" {
			helper = file.Content
		}
	}
	if helper == "" {
		return errors.New("cloud-init installs no fetch-verified.sh")
	}
	body := "pinned artifact\n"
	url := "https://github.com/example/tool/releases/download/v1.0.0/tool"
	var errs []error
	for _, mode := range []string{"ok", "http"} {
		h := newDownloadHarness(t, nil)
		script := filepath.Join(h.root, "fetch-verified.sh")
		if err := os.WriteFile(script, []byte(helper), 0o600); err != nil {
			t.Fatal(err)
		}
		dest := filepath.Join(h.root, "artifact")
		out, args, err := h.run(t, h.root, []string{"CURL_BODY=" + body, "CURL_FAIL=" + mode}, "bash", script, url, fmt.Sprintf("%x", sha256.Sum256([]byte(body))), dest)
		if mode == "http" {
			// curl's own exit 22, not the checksum's 1: -f kept the error page from being saved.
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 22 {
				errs = append(errs, fmt.Errorf("http: %v, want curl's exit 22\n%s", err, out))
			}
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("ok: %w\n%s", err, out))
			continue
		}
		if err := ciCurlError(args); err != nil {
			errs = append(errs, fmt.Errorf("ok: %w", err))
		}
		if !slices.Contains(args, url) {
			errs = append(errs, fmt.Errorf("ok: curl args %q do not fetch %s", args, url))
		}
		if data, err := os.ReadFile(dest); err != nil || string(data) != body {
			errs = append(errs, fmt.Errorf("ok: fetched artifact = %q, want %q: %v", data, body, err))
		}
	}
	return errors.Join(errs...)
}

type starlarkCall struct {
	fn      string
	line    int
	kwargs  map[string]bool
	strings []string // every string literal inside the call, nested ones included
}

func isIdentByte(c byte) bool {
	return c == '_' || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9')
}

// starlarkCalls lists every call in src with its keyword arguments, skipping comments and
// string literals, which in MODULE.bazel hold whole BUILD files.
func starlarkCalls(src string) ([]*starlarkCall, error) {
	type token struct {
		text string
		str  bool
		line int
	}
	var toks []token
	for i, line := 0, 1; i < len(src); {
		c := src[i]
		switch {
		case c == '\n':
			line++
			i++
		case c == ' ' || c == '\t' || c == '\r' || c == '\\':
			i++
		case c == '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '"' || c == '\'':
			quote := src[i : i+1]
			if strings.HasPrefix(src[i:], strings.Repeat(quote, 3)) {
				quote = strings.Repeat(quote, 3)
			}
			j := i + len(quote)
			for {
				if j >= len(src) || (len(quote) == 1 && src[j] == '\n') {
					return nil, fmt.Errorf("line %d: unterminated string", line)
				}
				if strings.HasPrefix(src[j:], quote) {
					break
				}
				if src[j] == '\\' {
					j++
				}
				j++
			}
			toks = append(toks, token{src[i+len(quote) : j], true, line})
			line += strings.Count(src[i:j], "\n")
			i = j + len(quote)
		case isIdentByte(c):
			j := i
			for j < len(src) && (isIdentByte(src[j]) || src[j] == '.') {
				j++
			}
			toks = append(toks, token{src[i:j], false, line})
			i = j
		default:
			n := 1
			if i+1 < len(src) && src[i+1] == '=' && strings.IndexByte("=!<>+-*/%|&^", c) >= 0 {
				n = 2
			}
			toks = append(toks, token{src[i : i+n], false, line})
			i += n
		}
	}
	var calls, open []*starlarkCall
	var depths []int // bracket depth of each open call's arguments
	depth := 0
	for i, tok := range toks {
		if tok.str {
			for _, c := range open {
				c.strings = append(c.strings, tok.text)
			}
			continue
		}
		prev := token{}
		if i > 0 {
			prev = toks[i-1]
		}
		switch tok.text {
		case "(", "[", "{":
			depth++
			if tok.text == "(" && !prev.str && prev.text != "" && isIdentByte(prev.text[0]) {
				c := &starlarkCall{fn: prev.text, line: prev.line, kwargs: map[string]bool{}}
				calls, open, depths = append(calls, c), append(open, c), append(depths, depth)
			}
		case ")", "]", "}":
			if len(open) > 0 && depths[len(depths)-1] == depth {
				open, depths = open[:len(open)-1], depths[:len(depths)-1]
			}
			if depth--; depth < 0 {
				return nil, fmt.Errorf("line %d: unbalanced %s", tok.line, tok.text)
			}
		case "=":
			if len(open) > 0 && depths[len(depths)-1] == depth && !prev.str {
				open[len(open)-1].kwargs[prev.text] = true
			}
		}
	}
	if depth != 0 {
		return nil, errors.New("unbalanced brackets at end of file")
	}
	return calls, nil
}

type remoteDownload struct {
	file, fn string
	line     int
	checked  bool // carries sha256 or integrity
}

func (d remoteDownload) String() string { return fmt.Sprintf("%s:%d %s", d.file, d.line, d.fn) }

// remoteDownloads lists the remote fetches MODULE.bazel declares: its url/urls calls and the
// downloads in local repository rules it loads; read takes a repo-relative path.
func remoteDownloads(module string, read func(string) (string, error)) ([]remoteDownload, error) {
	var found []remoteDownload
	seen := map[string]bool{}
	var scanRules func(from, label string) error
	scanRules = func(from, label string) error {
		if strings.HasPrefix(label, ":") {
			label = "//" + strings.TrimPrefix(filepath.Dir(from), ".") + label
		}
		rest, ok := strings.CutPrefix(label, "//")
		if !ok {
			return nil
		}
		pkg, name, _ := strings.Cut(rest, ":")
		path := filepath.Join(pkg, name)
		if seen[path] {
			return nil
		}
		seen[path] = true
		src, err := read(path)
		if err != nil {
			return err
		}
		calls, err := starlarkCalls(src)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		for _, c := range calls {
			switch {
			case c.fn == "load" && len(c.strings) > 0:
				if err := scanRules(path, c.strings[0]); err != nil {
					return err
				}
			case strings.HasSuffix(c.fn, ".download") || strings.HasSuffix(c.fn, ".download_and_extract"):
				found = append(found, remoteDownload{path, c.fn, c.line, c.kwargs["sha256"] || c.kwargs["integrity"]})
			case strings.HasSuffix(c.fn, ".execute") && (slices.Contains(c.strings, "curl") || slices.Contains(c.strings, "wget")):
				found = append(found, remoteDownload{path, c.fn, c.line, false})
			}
		}
		return nil
	}
	calls, err := starlarkCalls(module)
	if err != nil {
		return nil, fmt.Errorf("MODULE.bazel: %w", err)
	}
	for _, c := range calls {
		switch {
		case c.kwargs["url"] || c.kwargs["urls"]:
			found = append(found, remoteDownload{"MODULE.bazel", c.fn, c.line, c.kwargs["sha256"] || c.kwargs["integrity"]})
		case (c.fn == "use_repo_rule" || c.fn == "use_extension") && len(c.strings) > 0:
			if err := scanRules("MODULE.bazel", c.strings[0]); err != nil {
				return nil, err
			}
		}
	}
	return found, nil
}

func TestRemoteDownloadScanner(t *testing.T) {
	t.Parallel()
	module := `http_archive = use_repo_rule("@bazel_tools//tools/build_defs/repo:http.bzl", "http_archive")
http_archive(
    name = "embedded",
    build_file_content = """
filegroup(
    name = "x",
    urls = ["https://not.a.call/"],
)
""",
    sha256 = "abc",
    urls = ["https://example.invalid/a.tgz"],
)
http_file(name = "bare", urls = ["https://example.invalid/f"])  # sha256 = "in a comment"
archive_override(module_name = "m", integrity = "sha256-x", urls = ['https://example.invalid/m.tgz'])
checked = use_repo_rule("//rules:checked.bzl", "checked")
curl = use_extension("//rules:curl.bzl", "curl")
`
	files := map[string]string{
		"rules/checked.bzl": `load(":helper.bzl", "helper")
def _impl(ctx):
    ctx.download(url = ctx.attr.url, output = "f", sha256 = ctx.attr.sha256)
`,
		"rules/helper.bzl": `def helper(ctx):
    ctx.download_and_extract(url = "https://example.invalid/a.tgz")  # sha256 = "in a comment"
`,
		"rules/curl.bzl": `def _impl(ctx):
    ctx.execute(["curl", "-fsSL", "-o", "f", "ftp://example.invalid/f"])
`,
	}
	read := func(path string) (string, error) {
		src, ok := files[path]
		if !ok {
			return "", fmt.Errorf("%s is not a fixture", path)
		}
		return src, nil
	}
	found, err := remoteDownloads(module, read)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, d := range found {
		got = append(got, fmt.Sprintf("%s checked=%t", d, d.checked))
	}
	want := []string{
		"MODULE.bazel:2 http_archive checked=true",
		"MODULE.bazel:13 http_file checked=false",
		"MODULE.bazel:14 archive_override checked=true",
		"rules/helper.bzl:2 ctx.download_and_extract checked=false",
		"rules/checked.bzl:3 ctx.download checked=true",
		"rules/curl.bzl:2 ctx.execute checked=false",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("remote downloads:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, tc := range []struct{ name, module, want string }{
		{"unreadable-rule", `x = use_repo_rule("//missing:x.bzl", "x")`, "missing/x.bzl is not a fixture"},
		{"unterminated-string", `x = "open`, "unterminated string"},
		{"unbalanced", `f(`, "unbalanced"},
	} {
		if _, err := remoteDownloads(tc.module, read); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
}

// uncheckedDownloads are fetches no committed checksum covers yet, keyed "file fn", with why.
var uncheckedDownloads = map[string]string{
	"sift_repo.bzl ctx.execute": "SIFT-1M is served only over FTP, which Bazel's downloader lacks; only a manual benchmark fetches it and no digest is recorded",
}

// TestRemoteDownloadsAreChecksummed keeps every remote fetch in MODULE.bazel verified and
// servable from the repository cache offline.
func TestRemoteDownloadsAreChecksummed(t *testing.T) {
	t.Parallel()
	read := func(path string) (string, error) {
		data, err := os.ReadFile(filepath.Join("..", path))
		return string(data), err
	}
	check := func(module string) error {
		found, err := remoteDownloads(module, read)
		if err != nil {
			return err
		}
		var errs []error
		allowed := map[string]bool{}
		for _, d := range found {
			key := d.file + " " + d.fn
			if _, ok := uncheckedDownloads[key]; ok && !d.checked {
				allowed[key] = true
			} else if !d.checked {
				errs = append(errs, fmt.Errorf("%s fetches a remote file without sha256 or integrity", d))
			}
		}
		for key := range uncheckedDownloads {
			if !allowed[key] {
				errs = append(errs, fmt.Errorf("%s is listed in uncheckedDownloads but is no unchecked download", key))
			}
		}
		return errors.Join(errs...)
	}
	module := readRepoFile(t, "MODULE.bazel")
	found, err := remoteDownloads(module, read)
	if err != nil {
		t.Fatal(err)
	}
	fdb := slices.ContainsFunc(found, func(d remoteDownload) bool { return d.fn == "archive_override" && d.checked })
	if !fdb || len(found) < 2 {
		t.Fatalf("scan missed the FoundationDB archive_override: %v", found)
	}
	if err := check(module); err != nil {
		t.Fatal(err)
	}
	integrity := regexp.MustCompile(`(?m)^ +integrity = "sha256-[^"]+",\n`).FindAllString(module, -1)
	sift := "sift_dataset = use_repo_rule(\"//:sift_repo.bzl\", \"sift_dataset\")\n"
	if len(integrity) != 1 || strings.Count(module, sift) != 1 {
		t.Fatalf("mutations do not apply once: %d integrity lines, %d sift rules", len(integrity), strings.Count(module, sift))
	}
	for _, m := range []struct{ name, old, want string }{
		{"no-fdb-integrity", integrity[0], "archive_override fetches a remote file without sha256 or integrity"},
		{"stale-exemption", sift, "sift_repo.bzl ctx.execute is listed in uncheckedDownloads but is no unchecked download"},
	} {
		if err := check(strings.Replace(module, m.old, "", 1)); err == nil || !strings.Contains(err.Error(), m.want) {
			t.Errorf("%s: %v, want %q", m.name, err, m.want)
		}
	}
}
