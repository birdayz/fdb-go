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
	// values holds each keyword argument's value tokens; a repeated keyword maps to nil.
	values map[string][]starlarkToken
	stmt   int // index of the statement holding the call
}

type starlarkToken struct {
	text      string
	str       bool
	line, end int // first and last line; they differ only for multi-line strings
	col       int // byte column of the token's first character
}

func isIdentByte(c byte) bool {
	return c == '_' || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9')
}

func isName(s string) bool { return s != "" && isIdentByte(s[0]) && (s[0] < '0' || s[0] > '9') }

// starlarkTokens splits src into string literals, identifiers (dotted names included) and
// operators, skipping comments; continued marks the lines a backslash continues.
func starlarkTokens(src string) (toks []starlarkToken, continued map[int]bool, err error) {
	continued = map[int]bool{}
	for i, line, lineStart := 0, 1, 0; i < len(src); {
		c := src[i]
		switch {
		case c == '\n':
			line++
			i++
			lineStart = i
		case c == '\\':
			continued[line] = true
			i++
		case c == ' ' || c == '\t' || c == '\r':
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
					return nil, nil, fmt.Errorf("line %d: unterminated string", line)
				}
				if strings.HasPrefix(src[j:], quote) {
					break
				}
				if src[j] == '\\' {
					j++
				}
				j++
			}
			tok := starlarkToken{text: src[i+len(quote) : j], str: true, line: line, col: i - lineStart}
			if n := strings.Count(src[i:j], "\n"); n > 0 {
				line += n
				lineStart = i + strings.LastIndexByte(src[i:j], '\n') + 1
			}
			tok.end = line
			toks = append(toks, tok)
			i = j + len(quote)
		case isIdentByte(c):
			j := i
			for j < len(src) && (isIdentByte(src[j]) || src[j] == '.') {
				j++
			}
			toks = append(toks, starlarkToken{text: src[i:j], line: line, end: line, col: i - lineStart})
			i = j
		default:
			n := 1
			if i+1 < len(src) && src[i+1] == '=' && strings.IndexByte("=!<>+-*/%|&^", c) >= 0 {
				n = 2
			}
			toks = append(toks, starlarkToken{text: src[i : i+n], line: line, end: line, col: i - lineStart})
			i += n
		}
	}
	return toks, continued, nil
}

// starlarkStatements returns the first token index of each statement: one starts after a
// ';' or a newline at bracket depth 0 that no backslash continues.
func starlarkStatements(toks []starlarkToken, continued map[int]bool) []int {
	var starts []int
	depth := 0
	for k, tok := range toks {
		if k == 0 {
			starts = append(starts, 0)
		} else if prev := toks[k-1]; depth == 0 && ((!prev.str && prev.text == ";") || newLogicalLine(prev, tok, continued)) {
			starts = append(starts, k)
		}
		if !tok.str {
			switch tok.text {
			case "(", "[", "{":
				depth++
			case ")", "]", "}":
				depth--
			}
		}
	}
	return starts
}

func newLogicalLine(prev, tok starlarkToken, continued map[int]bool) bool {
	if tok.line <= prev.end {
		return false
	}
	for l := prev.end; l < tok.line; l++ {
		if continued[l] {
			return false
		}
	}
	return true
}

// starlarkCalls lists every call in src with its keyword arguments, skipping comments and
// string literals, which in MODULE.bazel hold whole BUILD files.
func starlarkCalls(src string) ([]*starlarkCall, error) {
	calls, _, err := starlarkParse(src)
	return calls, err
}

// starlarkParse is starlarkCalls plus what src's statements bind (see starlarkBindings).
func starlarkParse(src string) ([]*starlarkCall, map[string]*starlarkBinding, error) {
	toks, continued, err := starlarkTokens(src)
	if err != nil {
		return nil, nil, err
	}
	starts := starlarkStatements(toks, continued)
	stmtOf := make([]int, len(toks))
	for s, start := range starts {
		for k := start; k < len(toks) && (s+1 == len(starts) || k < starts[s+1]); k++ {
			stmtOf[k] = s
		}
	}
	type kwarg struct {
		name  string
		start int
	}
	var calls, open []*starlarkCall
	var depths []int       // bracket depth of each open call's arguments
	var pending []kwarg    // the keyword argument each open call is reading, if any
	refs := map[int]bool{} // token index of each whole `sha256s = name` argument's name
	closeKwarg := func(i int) {
		p := &pending[len(pending)-1]
		if p.name == "" {
			return
		}
		if p.name == "sha256s" && i == p.start+1 {
			refs[p.start] = true
		}
		c := open[len(open)-1]
		if _, dup := c.values[p.name]; dup {
			c.values[p.name] = nil
		} else {
			c.values[p.name] = toks[p.start:i]
		}
		p.name = ""
	}
	depth := 0
	for i, tok := range toks {
		if tok.str {
			for _, c := range open {
				c.strings = append(c.strings, tok.text)
			}
			continue
		}
		prev := starlarkToken{}
		if i > 0 {
			prev = toks[i-1]
		}
		switch tok.text {
		case "(", "[", "{":
			depth++
			if tok.text == "(" && !prev.str && prev.text != "" && isIdentByte(prev.text[0]) {
				s := stmtOf[i]
				c := &starlarkCall{fn: prev.text, line: prev.line, kwargs: map[string]bool{}, values: map[string][]starlarkToken{}, stmt: s}
				calls, open, depths, pending = append(calls, c), append(open, c), append(depths, depth), append(pending, kwarg{})
			}
		case ")", "]", "}":
			if len(open) > 0 && depths[len(depths)-1] == depth {
				closeKwarg(i)
				open, depths, pending = open[:len(open)-1], depths[:len(depths)-1], pending[:len(pending)-1]
			}
			if depth--; depth < 0 {
				return nil, nil, fmt.Errorf("line %d: unbalanced %s", tok.line, tok.text)
			}
		case ",":
			if len(open) > 0 && depths[len(depths)-1] == depth {
				closeKwarg(i)
			}
		case "=":
			if len(open) > 0 && depths[len(depths)-1] == depth && !prev.str {
				open[len(open)-1].kwargs[prev.text] = true
				pending[len(pending)-1] = kwarg{prev.text, i + 1}
			}
		}
	}
	if depth != 0 {
		return nil, nil, errors.New("unbalanced brackets at end of file")
	}
	return calls, starlarkBindings(toks, starts, refs), nil
}

// starlarkBinding is what a file does with one name.
type starlarkBinding struct {
	uses  int             // its bindings and references, other than whole `sha256s = name` arguments
	value []starlarkToken // value of a plain `name = value` statement when that is the name's first use
	stmt  int             // that statement's index
	top   bool            // it is the first statement of a line that starts in column 0
}

// starlarkBindings counts, per name, its bindings and references. Only a plain `name = value`
// statement records a value. Every other occurrence of the name counts against it: a second
// binding, an alias (`other = name`), any argument but a whole `sha256s = name` (refs), a load()
// import or alias, a def, for or lambda name or parameter, a subscript, tuple or augmented
// target, and an attribute use such as name.update(...). So a name that is rebound, mutated or
// escapes into another value or call never resolves.
func starlarkBindings(toks []starlarkToken, starts []int, refs map[int]bool) map[string]*starlarkBinding {
	out := map[string]*starlarkBinding{}
	use := func(name string) *starlarkBinding {
		name, _, _ = strings.Cut(name, ".")
		b := out[name]
		if b == nil {
			b = &starlarkBinding{}
			out[name] = b
		}
		b.uses++
		return b
	}
	targets := map[int]bool{} // token index of each plain binding's name
	for s, start := range starts {
		end := len(toks)
		if s+1 < len(starts) {
			end = starts[s+1]
		}
		st := toks[start:end]
		if n := len(st); !st[n-1].str && st[n-1].text == ";" {
			st = st[:n-1]
		}
		if len(st) == 0 {
			continue
		}
		if !st[0].str && st[0].text == "load" && len(st) > 1 && st[1].text == "(" {
			for k := 3; k < len(st); k++ { // load(label, "name") imports name; aliases are counted below
				if st[k].str && !st[k-1].str && st[k-1].text == "," {
					use(st[k].text)
				}
			}
			continue
		}
		// A plain binding is `name = value` with that `=` the statement's only one at depth 0;
		// chained, tuple, subscript, attribute and augmented forms never record a value.
		var eqs []int
		depth := 0
		for k, t := range st {
			if t.str {
				continue
			}
			switch t.text {
			case "(", "[", "{":
				depth++
			case ")", "]", "}":
				depth--
			case "=":
				if depth == 0 {
					eqs = append(eqs, k)
				}
			}
		}
		if len(eqs) == 1 && eqs[0] == 1 && !st[0].str && isName(st[0].text) && !strings.Contains(st[0].text, ".") {
			targets[start] = true
			if b := use(st[0].text); b.uses == 1 {
				b.value, b.stmt, b.top = st[2:], s, st[0].col == 0
			}
		}
	}
	for k, t := range toks {
		if !t.str && isName(t.text) && !refs[k] && !targets[k] {
			use(t.text) // any other occurrence may rebind, mutate or alias the name
		}
	}
	return out
}

var sha256Hex = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// sha256sChecked reports whether a call's sha256s argument is a checksum map: an inline dict
// literal, or a bare name the same file binds exactly once, at top level and before the call,
// to one. The dict must be non-empty, every key a string literal and every value a string
// literal of 64 hex digits; any other expression, an empty dict included, does not count.
func sha256sChecked(c *starlarkCall, bindings map[string]*starlarkBinding) bool {
	v := c.values["sha256s"]
	if len(v) == 1 && !v[0].str && isName(v[0].text) && !strings.Contains(v[0].text, ".") {
		b := bindings[v[0].text]
		if b == nil || b.uses != 1 || b.value == nil || !b.top || b.stmt >= c.stmt {
			return false
		}
		v = b.value
	}
	return sha256sMap(v)
}

// sha256sMap reports whether toks are exactly {"key": "<64 hex digits>", ...}: at least one
// entry, an optional trailing comma and nothing else.
func sha256sMap(toks []starlarkToken) bool {
	n := len(toks)
	if n < 5 || toks[0].str || toks[0].text != "{" || toks[n-1].str || toks[n-1].text != "}" {
		return false
	}
	for inner := toks[1 : n-1]; len(inner) > 0; {
		if len(inner) < 3 || !inner[0].str || inner[1].str || inner[1].text != ":" || !inner[2].str || !sha256Hex.MatchString(inner[2].text) {
			return false
		}
		if inner = inner[3:]; len(inner) > 0 {
			if inner[0].str || inner[0].text != "," {
				return false
			}
			inner = inner[1:]
		}
	}
	return true
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
	calls, bindings, err := starlarkParse(module)
	if err != nil {
		return nil, fmt.Errorf("MODULE.bazel: %w", err)
	}
	for _, c := range calls {
		switch {
		case c.kwargs["url"] || c.kwargs["urls"]:
			checked := c.kwargs["sha256"] || c.kwargs["integrity"] || sha256sChecked(c, bindings)
			found = append(found, remoteDownload{"MODULE.bazel", c.fn, c.line, checked})
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

// TestSha256sChecksumMaps drives every accept and reject arm of sha256sChecked through
// remoteDownloads: every url-bearing call a fixture declares must have the wanted verdict.
func TestSha256sChecksumMaps(t *testing.T) {
	t.Parallel()
	hex := strings.Repeat("0123456789abcdef", 4)
	pin := func(v string) string { return `{"a.tar.xz": "` + v + `"}` }
	fetch := func(arg string) string {
		return "rust.repository_set(\n    name = \"r\",\n    sha256s = " + arg + ",\n    urls = [\"https://example.invalid/{}.tar.xz\"],\n)\n"
	}
	bind := "pins = " + pin(hex) + "\n"
	indent := func(src string) string {
		return "    " + strings.ReplaceAll(strings.TrimSuffix(src, "\n"), "\n", "\n    ") + "\n"
	}
	read := func(path string) (string, error) { return "", fmt.Errorf("%s is not a fixture", path) }
	for _, tc := range []struct {
		name, module string
		checked      bool
	}{
		{"accept-inline-literal", fetch(pin(hex)), true},
		{"accept-inline-uppercase-hex", fetch(pin(strings.ToUpper(hex))), true},
		{"accept-inline-multiline-comments-trailing-comma", fetch("{\n        # pinned\n        \"a.tar.xz\": \"" + hex + "\",  # a\n        '''b.tar.xz''': '" + hex + "',\n    }"), true},
		{"accept-single-top-level-binding", bind + fetch("pins"), true},
		{"accept-binding-over-lines-with-comments", "pins = {\n    # pinned\n    \"a.tar.xz\": \"" + hex + "\",\n}\n\n" + fetch("pins"), true},
		{"accept-singular-sha256-unchanged", "http_archive(name = \"h\", sha256 = \"x\", urls = [\"https://example.invalid/h\"])\n", true},
		{"accept-semicolon-terminated-binding", "pins = " + pin(hex) + ";\n" + fetch("pins"), true},
		{"accept-binding-before-semicolon-statement", "pins = " + pin(hex) + "; x = 1\n" + fetch("pins"), true},
		{"accept-binding-continued-by-backslash", "pins = \\\n    " + pin(hex) + "\n" + fetch("pins"), true},
		{"accept-binding-between-multiline-string-and-comments", "doc = \"\"\"\npins = {}\npins.update(x)\n\"\"\"\n# pins = {}\n" + bind + "# pins.update({})\n" + fetch("pins"), true},
		{"accept-two-sha256s-arguments", bind + fetch("pins") + fetch("pins"), true},
		{"accept-name-in-def-body-call", bind + "def f():\n" + indent(fetch("pins")), true},
		{"reject-no-checksum", "http_file(name = \"f\", urls = [\"https://example.invalid/f\"])\n", false},
		{"reject-unbound-name", fetch("pins"), false},
		{"reject-bound-twice", bind + bind + fetch("pins"), false},
		{"reject-bound-after-use", fetch("pins") + bind, false},
		{"reject-rebound-after-use", bind + fetch("pins") + bind, false},
		{"reject-semicolon-second-statement-binding", "x = 1; " + bind + fetch("pins"), false},
		{"reject-compound-body-semicolon-binding", "if True: x = 1; " + bind + fetch("pins"), false},
		{"reject-destructuring-binding", "pins, other = " + pin(hex) + ", " + pin(hex) + "\n" + fetch("pins"), false},
		{"reject-def-name-shadow", bind + "def pins():\n    return 1\n" + fetch("pins"), false},
		{"reject-def-parameter-shadow", bind + "def f(pins):\n    return pins\n" + fetch("pins"), false},
		{"reject-for-variable-shadow", bind + "names = [pins for pins in [1]]\n" + fetch("pins"), false},
		{"reject-lambda-parameter-shadow", bind + "g = lambda pins: pins\n" + fetch("pins"), false},
		{"reject-augmented-shift", bind + "pins <<= 1\n" + fetch("pins"), false},
		{"reject-alias-escape-mutation", bind + "other = pins\nother.update(" + pin(hex) + ")\n" + fetch("pins"), false},
		{"reject-positional-reference", bind + "keep(pins)\n" + fetch("pins"), false},
		{"reject-other-keyword-reference", bind + "keep(m = pins)\n" + fetch("pins"), false},
		{"reject-augmented-union", bind + "pins |= " + pin(hex) + "\n" + fetch("pins"), false},
		{"reject-subscript-mutation", bind + "pins[\"b.tar.xz\"] = \"" + hex + "\"\n" + fetch("pins"), false},
		{"reject-method-mutation", bind + "pins.update(" + pin(hex) + ")\n" + fetch("pins"), false},
		{"reject-chained-binding", "pins = other = " + pin(hex) + "\n" + fetch("pins"), false},
		{"reject-load-imported", "load(\"//rules:pins.bzl\", \"pins\")\n" + fetch("pins"), false},
		{"reject-load-alias", "load(\"//rules:pins.bzl\", pins = \"rust_pins\")\n" + fetch("pins"), false},
		{"reject-nested-def-binding", "def f():\n" + indent(bind) + fetch("pins"), false},
		{"reject-nested-if-binding", "if True:\n" + indent(bind) + fetch("pins"), false},
		{"reject-binding-in-string", "doc = \"\"\"\n" + bind + "\"\"\"\n" + fetch("pins"), false},
		{"reject-binding-in-comment", "# " + bind + fetch("pins"), false},
		{"reject-bound-to-call", "pins = dict(a = \"" + hex + "\")\n" + fetch("pins"), false},
		{"reject-bound-to-comprehension", "pins = {k: \"" + hex + "\" for k in [\"a.tar.xz\"]}\n" + fetch("pins"), false},
		{"reject-bound-to-union", "pins = " + pin(hex) + " | " + pin(hex) + "\n" + fetch("pins"), false},
		{"reject-bound-to-concatenation-across-continuation", "pins = " + pin(hex) + " \\\n    + " + pin(hex) + "\n" + fetch("pins"), false},
		{"reject-bound-to-alias", "base = " + pin(hex) + "\npins = base\n" + fetch("pins"), false},
		{"reject-bound-to-empty", "pins = {}\n" + fetch("pins"), false},
		{"reject-inline-empty", fetch("{}"), false},
		{"reject-inline-call", fetch("dict(a = \"" + hex + "\")"), false},
		{"reject-inline-union", fetch(pin(hex) + " | " + pin(hex)), false},
		{"reject-non-string-key", fetch("{a: \"" + hex + "\"}"), false},
		{"reject-non-string-value", fetch("{\"a.tar.xz\": h}"), false},
		{"reject-missing-value", fetch("{\"a.tar.xz\": }"), false},
		{"reject-missing-colon", fetch("{\"a.tar.xz\" \"" + hex + "\"}"), false},
		{"reject-short-value", fetch(pin(hex[:63])), false},
		{"reject-long-value", fetch(pin(hex + "0")), false},
		{"reject-non-hex-value", fetch(pin(hex[:63] + "g")), false},
		{"reject-escaped-value", fetch(pin(`\x30` + hex[4:])), false},
		{"reject-repeated-keyword", strings.Replace(fetch(pin(hex)), "    urls", "    sha256s = "+pin(hex)+",\n    urls", 1), false},
	} {
		found, err := remoteDownloads(tc.module, read)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		var got []string
		bad := len(found) == 0
		for _, d := range found {
			got = append(got, fmt.Sprintf("%s checked=%t", d, d.checked))
			bad = bad || d.checked != tc.checked
		}
		if bad {
			t.Errorf("%s: found %q, want downloads with checked=%t", tc.name, got, tc.checked)
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
	// Positive control: the Rust toolchain fetches are in the scanned population and are
	// checksummed through the single top-level otel_rust_sha256s map; unbound, both fail.
	var rust []string
	for _, d := range found {
		if d.file == "MODULE.bazel" && (d.line == 109 || d.line == 129) {
			rust = append(rust, fmt.Sprintf("%s checked=%t", d, d.checked))
		}
	}
	wantRust := []string{"MODULE.bazel:109 rust.repository_set checked=true", "MODULE.bazel:129 otel_rust_host.host_tools checked=true"}
	if !slices.Equal(rust, wantRust) {
		t.Fatalf("Rust toolchain downloads:\n%s\nwant:\n%s", strings.Join(rust, "\n"), strings.Join(wantRust, "\n"))
	}
	binding := "otel_rust_sha256s = {"
	if strings.Count(module, binding) != 1 {
		t.Fatalf("%q is not bound exactly once in MODULE.bazel", binding)
	}
	unbound := strings.Replace(module, binding, "otel_rust_sha256s_moved = {", 1)
	for _, want := range []string{"MODULE.bazel:109 rust.repository_set fetches", "MODULE.bazel:129 otel_rust_host.host_tools fetches"} {
		if err := check(unbound); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("unbound-rust-sha256s: %v, want %q", err, want)
		}
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
