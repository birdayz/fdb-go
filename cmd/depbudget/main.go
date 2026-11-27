// Command depbudget measures consumer Go dependency counts, compiled binary
// sizes (unstripped and stripped), and top package symbol-size attribution for
// tracked public entrypoints, and optionally enforces checked-in ceilings.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

const (
	modeReport = "report"
	modeCheck  = "check"

	formatBoth     = "both"
	formatJSON     = "json"
	formatMarkdown = "markdown"

	kindLibrary = "library"
	kindBinary  = "binary"

	repoModulePath = "fdb.dev"
	defaultTopN    = 15

	exitOK              = 0
	exitError           = 1
	exitBudgetViolation = 2
)

var defaultEntrypoints = []string{
	"fdb.dev/pkg/fdbgo/fdb",
	"fdb.dev/pkg/recordlayer",
	"fdb.dev/pkg/relational/sqldriver",
	"fdb.dev/cmd/fdb",
}

// ConfigError reports invalid CLI flags or malformed budget configuration.
type ConfigError struct {
	Field  string
	Detail string
	Err    error
}

func (e *ConfigError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("depbudget config (%s): %s: %v", e.Field, e.Detail, e.Err)
	}
	return fmt.Sprintf("depbudget config (%s): %s", e.Field, e.Detail)
}

func (e *ConfigError) Unwrap() error { return e.Err }

// CommandError reports a failed invocation of the Go toolchain or nm.
type CommandError struct {
	Op         string
	Entrypoint string
	Args       []string
	Stderr     string
	Err        error
}

func (e *CommandError) Error() string {
	msg := fmt.Sprintf("depbudget %s for %s (%s): %v", e.Op, e.Entrypoint, strings.Join(e.Args, " "), e.Err)
	if stderr := strings.TrimSpace(e.Stderr); stderr != "" {
		return msg + "\n" + stderr
	}
	return msg
}

func (e *CommandError) Unwrap() error { return e.Err }

// ParseError reports malformed output from go list or go tool nm.
type ParseError struct {
	Source string
	Detail string
	Err    error
}

func (e *ParseError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("depbudget parse %s: %s: %v", e.Source, e.Detail, e.Err)
	}
	return fmt.Sprintf("depbudget parse %s: %s", e.Source, e.Detail)
}

func (e *ParseError) Unwrap() error { return e.Err }

// BudgetViolation describes a single ceiling breach or entrypoint mismatch.
type BudgetViolation struct {
	Entrypoint string `json:"entrypoint"`
	Metric     string `json:"metric"`
	Actual     int64  `json:"actual"`
	Limit      int64  `json:"limit"`
	Reason     string `json:"reason"`
}

// BudgetViolationError reports one or more budget check failures.
type BudgetViolationError struct {
	Violations []BudgetViolation
}

func (e *BudgetViolationError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "depbudget: %d budget check failure(s):", len(e.Violations))
	for _, v := range e.Violations {
		if v.Limit > 0 || v.Actual > 0 {
			delta := v.Actual - v.Limit
			fmt.Fprintf(&b, "\n  - %s %s: actual %d exceeds max %d (+%d)", v.Entrypoint, v.Metric, v.Actual, v.Limit, delta)
			continue
		}
		fmt.Fprintf(&b, "\n  - %s %s: %s", v.Entrypoint, v.Metric, v.Reason)
	}
	return b.String()
}

// PackageSize holds symbol-size attribution aggregated by Go package prefix.
type PackageSize struct {
	Package string `json:"package"`
	Bytes   int64  `json:"bytes"`
	Symbols int    `json:"symbols"`
}

// EntrypointReport holds measured dependency and binary-size metrics for one entrypoint.
type EntrypointReport struct {
	ImportPath      string        `json:"import_path"`
	Kind            string        `json:"kind"`
	Packages        int           `json:"packages"`
	StdPackages     int           `json:"std_packages"`
	RepoPackages    int           `json:"repo_packages"`
	ExtPackages     int           `json:"ext_packages"`
	ExtModules      int           `json:"ext_modules"`
	ExtModuleList   []string      `json:"ext_module_list"`
	UnstrippedBytes int64         `json:"unstripped_bytes"`
	StrippedBytes   int64         `json:"stripped_bytes"`
	TopPackages     []PackageSize `json:"top_packages"`
}

// Report is the complete deterministic output of a depbudget measurement run.
type Report struct {
	Entrypoints []EntrypointReport `json:"entrypoints"`
}

// EntrypointBudget defines ceiling constraints for a single tracked entrypoint.
type EntrypointBudget struct {
	ImportPath       string `json:"import_path"`
	MaxPackages      int    `json:"max_packages"`
	MaxExtModules    int    `json:"max_ext_modules"`
	MaxStrippedBytes int64  `json:"max_stripped_bytes"`
}

// BudgetConfig is the schema for cmd/depbudget/budget.json.
type BudgetConfig struct {
	Entrypoints []EntrypointBudget `json:"entrypoints"`
}

type options struct {
	mode        string
	budgetPath  string
	goBin       string
	repoRoot    string
	entrypoints string
	format      string
	topN        int
}

func registerFlags(fs *flag.FlagSet, opts *options) {
	fs.StringVar(&opts.mode, "mode", modeReport, "run mode: report or check")
	fs.StringVar(&opts.budgetPath, "budget", "", "path to budget JSON file (required for -mode=check)")
	fs.StringVar(&opts.goBin, "go", defaultGoBin(), "path to the go binary to invoke (defaults to DEPBUDGET_GO via Bazel runfiles, or go on PATH)")
	fs.StringVar(&opts.repoRoot, "repo", defaultRepoRoot(), "path to repository root containing go.mod")
	fs.StringVar(&opts.entrypoints, "entrypoints", "", "comma-separated import paths to measure (defaults to tracked public entrypoints)")
	fs.StringVar(&opts.format, "format", formatMarkdown, "report output format: markdown, json, or both")
	fs.IntVar(&opts.topN, "top", defaultTopN, "number of top packages to attribute from nm -size")
}

func exitCodeForError(err error) int {
	if err == nil {
		return exitOK
	}
	var violErr *BudgetViolationError
	if errors.As(err, &violErr) {
		return exitBudgetViolation
	}
	return exitError
}

func main() {
	var opts options
	registerFlags(flag.CommandLine, &opts)
	flag.Parse()

	if err := run(opts, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(exitCodeForError(err))
	}
}

func defaultGoBin() string {
	exePath, _ := os.Executable()
	arg0 := ""
	if len(os.Args) > 0 {
		arg0 = os.Args[0]
	}
	return resolveDepbudgetGo(
		os.Getenv("DEPBUDGET_GO"),
		os.Getenv("RUNFILES_DIR"),
		os.Getenv("TEST_SRCDIR"),
		os.Getenv("TEST_WORKSPACE"),
		os.Getenv("RUNFILES_MANIFEST_FILE"),
		exePath,
		arg0,
	)
}

// resolveDepbudgetGo resolves DEPBUDGET_GO (typically an rlocationpath from Bazel)
// against RUNFILES_DIR, TEST_SRCDIR, <exe>.runfiles, or runfiles manifests.
func resolveDepbudgetGo(envGo, runfilesDir, testSrcDir, testWorkspace, manifestFile, exePath, arg0 string) string {
	if envGo == "" {
		return "go"
	}
	if filepath.IsAbs(envGo) {
		return envGo
	}
	rel := filepath.FromSlash(envGo)
	var roots []string
	if runfilesDir != "" {
		roots = append(roots, runfilesDir)
	}
	if testSrcDir != "" {
		roots = append(roots, testSrcDir)
		if testWorkspace != "" {
			roots = append(roots, filepath.Join(testSrcDir, testWorkspace))
		}
	}
	if exePath != "" {
		roots = append(roots, exePath+".runfiles")
	}
	if arg0 != "" && filepath.Base(arg0) != arg0 {
		roots = append(roots, arg0+".runfiles")
	}
	for _, root := range roots {
		candidate := filepath.Join(root, rel)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	var manifests []string
	if manifestFile != "" {
		manifests = append(manifests, manifestFile)
	}
	if exePath != "" {
		manifests = append(manifests, exePath+".runfiles_manifest")
	}
	if arg0 != "" && filepath.Base(arg0) != arg0 {
		manifests = append(manifests, arg0+".runfiles_manifest")
	}
	for _, m := range manifests {
		if target, ok := lookupRunfilesManifest(m, envGo); ok {
			return target
		}
	}
	return envGo
}

func lookupRunfilesManifest(manifestPath, key string) (string, bool) {
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		k, target, ok := strings.Cut(line, " ")
		if !ok || k != key {
			continue
		}
		target = strings.TrimSpace(target)
		if target == "" {
			continue
		}
		if info, err := os.Stat(target); err == nil && !info.IsDir() {
			return target, true
		}
	}
	return "", false
}

func defaultRepoRoot() string {
	if ws := os.Getenv("BUILD_WORKSPACE_DIRECTORY"); ws != "" {
		return ws
	}
	return "."
}

func resolveInputPath(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	// Under bazelisk run, cwd is runfiles; prefer the live workspace copy if present.
	if ws := os.Getenv("BUILD_WORKSPACE_DIRECTORY"); ws != "" {
		candidate := filepath.Join(ws, p)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return p
}

func run(opts options, out io.Writer) error {
	if opts.mode != modeReport && opts.mode != modeCheck {
		return &ConfigError{Field: "mode", Detail: fmt.Sprintf("unknown mode %q (want %q or %q)", opts.mode, modeReport, modeCheck)}
	}
	if opts.format != formatBoth && opts.format != formatJSON && opts.format != formatMarkdown {
		return &ConfigError{Field: "format", Detail: fmt.Sprintf("unknown format %q (want both, json, or markdown)", opts.format)}
	}
	if opts.topN < 1 {
		return &ConfigError{Field: "top", Detail: fmt.Sprintf("must be >= 1, got %d", opts.topN)}
	}
	if opts.goBin == "" {
		return &ConfigError{Field: "go", Detail: "go binary path must not be empty"}
	}

	var budget BudgetConfig
	hasBudget := opts.budgetPath != ""
	if opts.mode == modeCheck && !hasBudget {
		return &ConfigError{Field: "budget", Detail: "-budget is required when -mode=check"}
	}
	if hasBudget {
		var err error
		budget, err = loadBudget(resolveInputPath(opts.budgetPath))
		if err != nil {
			return err
		}
	}

	eps, err := resolveEntrypoints(opts.entrypoints)
	if err != nil {
		return err
	}

	absRepo, err := filepath.Abs(opts.repoRoot)
	if err != nil {
		return &ConfigError{Field: "repo", Detail: "resolve repo root", Err: err}
	}
	if _, err := os.Stat(filepath.Join(absRepo, "go.mod")); err != nil {
		return &ConfigError{Field: "repo", Detail: fmt.Sprintf("no go.mod at %s", absRepo), Err: err}
	}

	goBinPath, goEnv, err := buildGoEnv(opts.goBin)
	if err != nil {
		return err
	}

	report := Report{Entrypoints: make([]EntrypointReport, 0, len(eps))}
	for _, ep := range eps {
		er, err := measureEntrypoint(goBinPath, goEnv, absRepo, ep, opts.topN)
		if err != nil {
			return err
		}
		report.Entrypoints = append(report.Entrypoints, er)
	}

	switch opts.mode {
	case modeReport:
		return writeReport(out, report, opts.format)
	case modeCheck:
		err := checkBudget(report, budget)
		if _, wErr := io.WriteString(out, RenderCheckMarkdown(report, budget)); wErr != nil {
			return wErr
		}
		return err
	default:
		return nil
	}
}

func resolveEntrypoints(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return slices.Clone(defaultEntrypoints), nil
	}
	var out []string
	seen := make(map[string]struct{})
	for _, part := range strings.Split(raw, ",") {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		if _, dup := seen[p]; dup {
			return nil, &ConfigError{Field: "entrypoints", Detail: fmt.Sprintf("duplicate entrypoint %q", p)}
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, &ConfigError{Field: "entrypoints", Detail: "no valid entrypoints provided"}
	}
	return out, nil
}

func loadBudget(path string) (BudgetConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return BudgetConfig{}, &ConfigError{Field: "budget", Detail: fmt.Sprintf("read %s", path), Err: err}
	}
	var cfg BudgetConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return BudgetConfig{}, &ConfigError{Field: "budget", Detail: fmt.Sprintf("parse %s", path), Err: err}
	}
	if err := validateBudget(cfg); err != nil {
		return BudgetConfig{}, err
	}
	return cfg, nil
}

func validateBudget(cfg BudgetConfig) error {
	if len(cfg.Entrypoints) == 0 {
		return &ConfigError{Field: "entrypoints", Detail: "budget defines zero entrypoints"}
	}
	seen := make(map[string]struct{}, len(cfg.Entrypoints))
	for _, b := range cfg.Entrypoints {
		if strings.TrimSpace(b.ImportPath) == "" {
			return &ConfigError{Field: "import_path", Detail: "budget entry has empty import_path"}
		}
		if _, dup := seen[b.ImportPath]; dup {
			return &ConfigError{Field: "import_path", Detail: fmt.Sprintf("duplicate entrypoint %q in budget", b.ImportPath)}
		}
		seen[b.ImportPath] = struct{}{}
		if b.MaxPackages <= 0 {
			return &ConfigError{Field: "max_packages", Detail: fmt.Sprintf("%s: max_packages must be > 0, got %d", b.ImportPath, b.MaxPackages)}
		}
		if b.MaxExtModules < 0 {
			return &ConfigError{Field: "max_ext_modules", Detail: fmt.Sprintf("%s: max_ext_modules must be >= 0, got %d", b.ImportPath, b.MaxExtModules)}
		}
		if b.MaxStrippedBytes <= 0 {
			return &ConfigError{Field: "max_stripped_bytes", Detail: fmt.Sprintf("%s: max_stripped_bytes must be > 0, got %d", b.ImportPath, b.MaxStrippedBytes)}
		}
	}
	return nil
}

// buildGoEnv constructs an offline, hermetic environment for invoking go.
// When -go points to a specific SDK binary (e.g. from Bazel test_go_sdk),
// PATH and GOROOT are pinned to that binary so go tool nm and stdlib match.
func buildGoEnv(goBin string) (string, []string, error) {
	resolved := goBin
	if strings.ContainsRune(goBin, filepath.Separator) {
		abs, err := filepath.Abs(goBin)
		if err != nil {
			return "", nil, &ConfigError{Field: "go", Detail: fmt.Sprintf("resolve %s", goBin), Err: err}
		}
		resolved = abs
	} else {
		lp, err := exec.LookPath(goBin)
		if err != nil {
			return "", nil, &ConfigError{Field: "go", Detail: fmt.Sprintf("locate %s on PATH", goBin), Err: err}
		}
		resolved = lp
	}
	if _, err := os.Stat(resolved); err != nil {
		return "", nil, &ConfigError{Field: "go", Detail: fmt.Sprintf("stat %s", resolved), Err: err}
	}

	binDir := filepath.Dir(resolved)
	overrides := map[string]string{
		"CGO_ENABLED": "0",
		"GOPROXY":     "off",
		"GOSUMDB":     "off",
		"GOTOOLCHAIN": "local",
		"GOWORK":      "off",
		"GOENV":       "off",
		"GOFLAGS":     "",
		"GOTELEMETRY": "off",
		"PATH":        binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}
	if filepath.Base(binDir) == "bin" {
		overrides["GOROOT"] = filepath.Dir(binDir)
	}
	if os.Getenv("GOCACHE") == "" {
		if testTmp := os.Getenv("TEST_TMPDIR"); testTmp != "" {
			overrides["GOCACHE"] = filepath.Join(testTmp, "depbudget-gocache")
		}
	}

	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, kv := range os.Environ() {
		k, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if _, replaced := overrides[k]; replaced {
			continue
		}
		// Never inherit an ambient GOROOT that could disagree with -go.
		if k == "GOROOT" {
			continue
		}
		env = append(env, kv)
	}
	keys := make([]string, 0, len(overrides))
	for k := range overrides {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		env = append(env, k+"="+overrides[k])
	}
	return resolved, env, nil
}

func runGoCmd(goBin string, env []string, dir, op, entrypoint string, args ...string) ([]byte, error) {
	cmd := exec.Command(goBin, args...)
	cmd.Dir = dir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, &CommandError{
			Op:         op,
			Entrypoint: entrypoint,
			Args:       args,
			Stderr:     stderr.String(),
			Err:        err,
		}
	}
	return stdout.Bytes(), nil
}
