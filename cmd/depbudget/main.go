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
	"strconv"
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

type goListPackage struct {
	ImportPath string `json:"ImportPath"`
	Name       string `json:"Name"`
	Standard   bool   `json:"Standard"`
	Incomplete bool   `json:"Incomplete"`
	Module     *struct {
		Path string `json:"Path"`
		Main bool   `json:"Main"`
	} `json:"Module"`
	Error *struct {
		Err string `json:"Err"`
	} `json:"Error"`
}

type depListResult struct {
	Kind          string
	Packages      int
	StdPackages   int
	RepoPackages  int
	ExtPackages   int
	ExtModules    int
	ExtModuleList []string
}

func parseGoList(r io.Reader, entrypoint string) (depListResult, error) {
	dec := json.NewDecoder(r)
	seen := make(map[string]struct{})
	extMods := make(map[string]struct{})
	var res depListResult
	foundEntrypoint := false

	for {
		var pkg goListPackage
		err := dec.Decode(&pkg)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return depListResult{}, &ParseError{Source: "go list", Detail: "decode package json", Err: err}
		}
		if pkg.Error != nil && pkg.Error.Err != "" {
			return depListResult{}, &ParseError{Source: "go list", Detail: fmt.Sprintf("%s: %s", pkg.ImportPath, pkg.Error.Err)}
		}
		if pkg.Incomplete {
			return depListResult{}, &ParseError{Source: "go list", Detail: fmt.Sprintf("%s reported incomplete package metadata", pkg.ImportPath)}
		}
		if pkg.ImportPath == "" {
			continue
		}
		if _, dup := seen[pkg.ImportPath]; dup {
			continue
		}
		seen[pkg.ImportPath] = struct{}{}

		if pkg.ImportPath == entrypoint {
			foundEntrypoint = true
			if pkg.Name == "main" {
				res.Kind = kindBinary
			} else {
				res.Kind = kindLibrary
			}
		}

		res.Packages++
		switch {
		case pkg.Standard:
			res.StdPackages++
		case pkg.Module == nil || pkg.Module.Main || pkg.Module.Path == repoModulePath:
			res.RepoPackages++
		default:
			res.ExtPackages++
			extMods[pkg.Module.Path] = struct{}{}
		}
	}

	if !foundEntrypoint {
		return depListResult{}, &ParseError{Source: "go list", Detail: fmt.Sprintf("entrypoint %q missing from go list output", entrypoint)}
	}
	res.ExtModuleList = make([]string, 0, len(extMods))
	for m := range extMods {
		res.ExtModuleList = append(res.ExtModuleList, m)
	}
	slices.Sort(res.ExtModuleList)
	res.ExtModules = len(res.ExtModuleList)
	return res, nil
}

func measureEntrypoint(goBin string, env []string, absRepo, entrypoint string, topN int) (EntrypointReport, error) {
	listOut, err := runGoCmd(goBin, env, absRepo, "list", entrypoint, "list", "-deps", "-json", entrypoint)
	if err != nil {
		return EntrypointReport{}, err
	}
	deps, err := parseGoList(bytes.NewReader(listOut), entrypoint)
	if err != nil {
		return EntrypointReport{}, err
	}

	tmpDir, err := os.MkdirTemp("", "depbudget-*")
	if err != nil {
		return EntrypointReport{}, fmt.Errorf("depbudget create temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	target := entrypoint
	var buildPrefixArgs []string
	if deps.Kind == kindLibrary {
		// Use -overlay to compile a synthetic main inside the repo module without
		// modifying the working tree.
		mainSrc := fmt.Sprintf("package main\n\nimport _ %q\n\nfunc main() {}\n", entrypoint)
		mainPath := filepath.Join(tmpDir, "main.go")
		if err := os.WriteFile(mainPath, []byte(mainSrc), 0o600); err != nil {
			return EntrypointReport{}, fmt.Errorf("depbudget write synthetic main: %w", err)
		}
		overlayPath := filepath.Join(tmpDir, "overlay.json")
		virtualMain := filepath.Join(absRepo, "__depbudget_probe__", "main.go")
		overlayBytes, err := json.Marshal(struct {
			Replace map[string]string `json:"Replace"`
		}{
			Replace: map[string]string{virtualMain: mainPath},
		})
		if err != nil {
			return EntrypointReport{}, fmt.Errorf("depbudget marshal overlay: %w", err)
		}
		if err := os.WriteFile(overlayPath, overlayBytes, 0o600); err != nil {
			return EntrypointReport{}, fmt.Errorf("depbudget write overlay: %w", err)
		}
		buildPrefixArgs = []string{"-overlay=" + overlayPath}
		target = "./__depbudget_probe__"
	}

	unstrippedBin := filepath.Join(tmpDir, "bin_unstripped")
	unstrippedArgs := append([]string{"build"}, buildPrefixArgs...)
	unstrippedArgs = append(unstrippedArgs, "-trimpath", "-o", unstrippedBin, target)
	if _, err := runGoCmd(goBin, env, absRepo, "build-unstripped", entrypoint, unstrippedArgs...); err != nil {
		return EntrypointReport{}, err
	}
	unstrippedInfo, err := os.Stat(unstrippedBin)
	if err != nil {
		return EntrypointReport{}, fmt.Errorf("depbudget stat unstripped binary: %w", err)
	}

	strippedBin := filepath.Join(tmpDir, "bin_stripped")
	strippedArgs := append([]string{"build"}, buildPrefixArgs...)
	strippedArgs = append(strippedArgs, "-trimpath", "-ldflags=-s -w", "-o", strippedBin, target)
	if _, err := runGoCmd(goBin, env, absRepo, "build-stripped", entrypoint, strippedArgs...); err != nil {
		return EntrypointReport{}, err
	}
	strippedInfo, err := os.Stat(strippedBin)
	if err != nil {
		return EntrypointReport{}, fmt.Errorf("depbudget stat stripped binary: %w", err)
	}

	nmOut, err := runGoCmd(goBin, env, absRepo, "nm", entrypoint, "tool", "nm", "-size", unstrippedBin)
	if err != nil {
		return EntrypointReport{}, err
	}
	topPkgs, err := parseNMOutput(bytes.NewReader(nmOut), topN)
	if err != nil {
		return EntrypointReport{}, err
	}

	return EntrypointReport{
		ImportPath:      entrypoint,
		Kind:            deps.Kind,
		Packages:        deps.Packages,
		StdPackages:     deps.StdPackages,
		RepoPackages:    deps.RepoPackages,
		ExtPackages:     deps.ExtPackages,
		ExtModules:      deps.ExtModules,
		ExtModuleList:   deps.ExtModuleList,
		UnstrippedBytes: unstrippedInfo.Size(),
		StrippedBytes:   strippedInfo.Size(),
		TopPackages:     topPkgs,
	}, nil
}

// parseNMOutput parses go tool nm -size output and aggregates binary-resident
// symbol sizes (T/t/R/r/D/d) by Go package prefix, returning the top N entries.
// BSS symbols (B/b) occupy zero bytes in the binary file and are excluded.
func parseNMOutput(r io.Reader, topN int) ([]PackageSize, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, &ParseError{Source: "go tool nm", Detail: "read output", Err: err}
	}

	type acc struct {
		bytes   int64
		symbols int
	}
	byPkg := make(map[string]*acc)
	lines := strings.Split(string(raw), "\n")
	for _, line := range lines {
		size, symType, name, ok := parseNMFields(line)
		if !ok || size <= 0 {
			continue
		}
		switch symType {
		case "T", "t", "R", "r", "D", "d":
		default:
			continue
		}
		pkg := symbolPackage(name)
		a := byPkg[pkg]
		if a == nil {
			a = &acc{}
			byPkg[pkg] = a
		}
		a.bytes += size
		a.symbols++
	}

	out := make([]PackageSize, 0, len(byPkg))
	for pkg, a := range byPkg {
		out = append(out, PackageSize{
			Package: pkg,
			Bytes:   a.bytes,
			Symbols: a.symbols,
		})
	}
	slices.SortFunc(out, func(a, b PackageSize) int {
		if a.Bytes != b.Bytes {
			if a.Bytes > b.Bytes {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Package, b.Package)
	})
	if topN > 0 && len(out) > topN {
		out = out[:topN]
	}
	return out, nil
}

func parseNMFields(line string) (size int64, symType, name string, ok bool) {
	s := strings.TrimSpace(line)
	if s == "" {
		return 0, "", "", false
	}
	addr, rest, found := cutField(s)
	if !found {
		return 0, "", "", false
	}
	if _, err := strconv.ParseUint(addr, 16, 64); err != nil {
		return 0, "", "", false
	}
	sizeStr, rest, found := cutField(rest)
	if !found {
		return 0, "", "", false
	}
	n, err := strconv.ParseInt(sizeStr, 10, 64)
	if err != nil || n < 0 {
		return 0, "", "", false
	}
	typ, rest, found := cutField(rest)
	if !found || len(typ) != 1 {
		return 0, "", "", false
	}
	sym := strings.TrimSpace(rest)
	if sym == "" {
		return 0, "", "", false
	}
	return n, typ, sym, true
}

func cutField(s string) (field, rest string, ok bool) {
	s = strings.TrimLeft(s, " \t")
	if s == "" {
		return "", "", false
	}
	idx := strings.IndexAny(s, " \t")
	if idx < 0 {
		return s, "", false
	}
	return s[:idx], strings.TrimLeft(s[idx:], " \t"), true
}

func symbolPackage(name string) string {
	switch {
	case strings.HasPrefix(name, "type:"):
		return "type:"
	case strings.HasPrefix(name, "go:"):
		return "go:"
	case strings.HasPrefix(name, "$"):
		return "$const"
	}

	// Strip generic shape/type-argument suffixes before finding the package boundary
	// so slashes or dots inside [...] do not distort the package path.
	head := name
	if idx := strings.IndexAny(head, "[ "); idx >= 0 {
		head = head[:idx]
	}

	lastSlash := strings.LastIndexByte(head, '/')
	tail := head[lastSlash+1:]
	dot := strings.IndexByte(tail, '.')
	if dot <= 0 {
		if lastSlash >= 0 {
			return strings.ReplaceAll(head, "%2e", ".")
		}
		return "<runtime/asm>"
	}
	pkg := head[:lastSlash+1+dot]
	return strings.ReplaceAll(pkg, "%2e", ".")
}

func checkBudget(report Report, budget BudgetConfig) error {
	if err := validateBudget(budget); err != nil {
		return err
	}

	reportByPath := make(map[string]EntrypointReport, len(report.Entrypoints))
	for _, r := range report.Entrypoints {
		if _, dup := reportByPath[r.ImportPath]; dup {
			return &ConfigError{Field: "entrypoints", Detail: fmt.Sprintf("duplicate entrypoint %q in report", r.ImportPath)}
		}
		reportByPath[r.ImportPath] = r
	}

	budgetByPath := make(map[string]EntrypointBudget, len(budget.Entrypoints))
	for _, b := range budget.Entrypoints {
		budgetByPath[b.ImportPath] = b
	}

	var violations []BudgetViolation
	for _, b := range budget.Entrypoints {
		r, ok := reportByPath[b.ImportPath]
		if !ok {
			violations = append(violations, BudgetViolation{
				Entrypoint: b.ImportPath,
				Metric:     "entrypoint",
				Reason:     "missing from measured report",
			})
			continue
		}
		if r.Packages > b.MaxPackages {
			violations = append(violations, BudgetViolation{
				Entrypoint: b.ImportPath,
				Metric:     "packages",
				Actual:     int64(r.Packages),
				Limit:      int64(b.MaxPackages),
				Reason:     "package count exceeds budget",
			})
		}
		if r.ExtModules > b.MaxExtModules {
			violations = append(violations, BudgetViolation{
				Entrypoint: b.ImportPath,
				Metric:     "ext_modules",
				Actual:     int64(r.ExtModules),
				Limit:      int64(b.MaxExtModules),
				Reason:     "external module count exceeds budget",
			})
		}
		if r.StrippedBytes > b.MaxStrippedBytes {
			violations = append(violations, BudgetViolation{
				Entrypoint: b.ImportPath,
				Metric:     "stripped_bytes",
				Actual:     r.StrippedBytes,
				Limit:      b.MaxStrippedBytes,
				Reason:     "stripped binary size exceeds budget",
			})
		}
	}

	for _, r := range report.Entrypoints {
		if _, ok := budgetByPath[r.ImportPath]; !ok {
			violations = append(violations, BudgetViolation{
				Entrypoint: r.ImportPath,
				Metric:     "entrypoint",
				Reason:     "unmatched measured entrypoint has no budget entry",
			})
		}
	}

	if len(violations) > 0 {
		return &BudgetViolationError{Violations: violations}
	}
	return nil
}

func writeReport(out io.Writer, report Report, format string) error {
	switch format {
	case formatJSON:
		raw, err := RenderJSON(report)
		if err != nil {
			return err
		}
		_, err = out.Write(raw)
		return err
	case formatMarkdown:
		_, err := io.WriteString(out, RenderMarkdown(report))
		return err
	case formatBoth:
		raw, err := RenderJSON(report)
		if err != nil {
			return err
		}
		if _, err := out.Write(raw); err != nil {
			return err
		}
		if _, err := io.WriteString(out, "\n"+RenderMarkdown(report)); err != nil {
			return err
		}
		return nil
	default:
		return &ConfigError{Field: "format", Detail: fmt.Sprintf("unknown format %q", format)}
	}
}

// RenderJSON serializes a Report as deterministic, indented JSON.
func RenderJSON(report Report) ([]byte, error) {
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("depbudget marshal json: %w", err)
	}
	return append(raw, '\n'), nil
}

// RenderMarkdown formats the entrypoint summary table and per-entrypoint top packages.
func RenderMarkdown(report Report) string {
	var b strings.Builder
	b.WriteString("| Entrypoint | Kind | Packages | Std | Repo | Ext Pkgs | Ext Mods | Unstripped (B) | Stripped (B) |\n")
	b.WriteString("| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |\n")
	for _, ep := range report.Entrypoints {
		fmt.Fprintf(&b, "| `%s` | %s | %d | %d | %d | %d | %d | %d | %d |\n",
			ep.ImportPath, ep.Kind, ep.Packages, ep.StdPackages, ep.RepoPackages,
			ep.ExtPackages, ep.ExtModules, ep.UnstrippedBytes, ep.StrippedBytes)
	}
	for _, ep := range report.Entrypoints {
		if len(ep.TopPackages) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n### Top %d packages by symbol size: `%s`\n\n", len(ep.TopPackages), ep.ImportPath)
		b.WriteString("| Rank | Package | Bytes | Symbols |\n")
		b.WriteString("| ---: | --- | ---: | ---: |\n")
		for i, p := range ep.TopPackages {
			fmt.Fprintf(&b, "| %d | `%s` | %d | %d |\n", i+1, p.Package, p.Bytes, p.Symbols)
		}
	}
	return b.String()
}

// RenderCheckMarkdown formats actual vs ceiling comparisons for -mode=check.
func RenderCheckMarkdown(report Report, budget BudgetConfig) string {
	reportByPath := make(map[string]EntrypointReport, len(report.Entrypoints))
	for _, r := range report.Entrypoints {
		reportByPath[r.ImportPath] = r
	}
	var b strings.Builder
	b.WriteString("| Entrypoint | Packages (actual/max) | Ext Mods (actual/max) | Stripped B (actual/max) | Status |\n")
	b.WriteString("| --- | ---: | ---: | ---: | --- |\n")
	for _, cfg := range budget.Entrypoints {
		r, ok := reportByPath[cfg.ImportPath]
		if !ok {
			fmt.Fprintf(&b, "| `%s` | -/%d | -/%d | -/%d | MISSING |\n",
				cfg.ImportPath, cfg.MaxPackages, cfg.MaxExtModules, cfg.MaxStrippedBytes)
			continue
		}
		status := "PASS"
		if r.Packages > cfg.MaxPackages || r.ExtModules > cfg.MaxExtModules || r.StrippedBytes > cfg.MaxStrippedBytes {
			status = "FAIL"
		}
		fmt.Fprintf(&b, "| `%s` | %d/%d | %d/%d | %d/%d | %s |\n",
			cfg.ImportPath,
			r.Packages, cfg.MaxPackages,
			r.ExtModules, cfg.MaxExtModules,
			r.StrippedBytes, cfg.MaxStrippedBytes,
			status)
	}
	return b.String()
}
