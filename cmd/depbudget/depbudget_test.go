package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParseNMOutput_AggregationAndEdgeCases(t *testing.T) {
	t.Parallel()

	sample := strings.Join([]string{
		// Normal package functions, methods, and %2e versioned module paths.
		"  53b600        139 T fdb.dev/internal/fdbgo/client.(*ClientMetrics).observeReadLatency",
		"  53b760       2255 T fdb.dev/internal/fdbgo/client.(*ClusterFile).String",
		" 1167c60       1000 T gopkg.in/yaml%2ev3.(*Encoder).Encode",
		" 1167d80        500 t gopkg.in/yaml%2ev3.(*Node).Encode.deferwrap1",
		// Generic symbols containing spaces and nested import paths inside [...].
		"  59e0a0        786 T crypto/internal/fips140/hkdf.Expand[go.shape.interface { BlockSize() int; Reset() }]",
		"  5b2450        200 R errors..dict.AsType[*fdb.dev/internal/fdbwire.FDBError]",
		// Compiler/linker prefixes and constants.
		"  6d6138       3000 r go:func.*",
		"  5026e0        400 T type:.eq.EES",
		"  5b1358         16 r $f64.3ff0000000000000",
		"  5b2880         32 r BSWAP_SHUFB_CTL",
		"  789100        150 D runtime.dataVar",
		"  789200         50 d runtime.smallData",
		// Must be excluded: BSS (B/b), undefined (U), zero-size markers, malformed lines.
		"  8055c0   33554432 B crypto/internal/fips140/drbg.memory",
		"  8055d0        128 b runtime.bssZero",
		"                  U runtime.throw",
		"  715518          0 R type:*",
		"not-a-valid-line",
		"",
	}, "\n")

	got, err := parseNMOutput(strings.NewReader(sample), 15)
	if err != nil {
		t.Fatalf("parseNMOutput: %v", err)
	}

	want := []PackageSize{
		{Package: "go:", Bytes: 3000, Symbols: 1},
		{Package: "fdb.dev/internal/fdbgo/client", Bytes: 2394, Symbols: 2},
		{Package: "gopkg.in/yaml.v3", Bytes: 1500, Symbols: 2},
		{Package: "crypto/internal/fips140/hkdf", Bytes: 786, Symbols: 1},
		{Package: "type:", Bytes: 400, Symbols: 1},
		{Package: "errors", Bytes: 200, Symbols: 1},
		{Package: "runtime", Bytes: 200, Symbols: 2},
		{Package: "<runtime/asm>", Bytes: 32, Symbols: 1},
		{Package: "$const", Bytes: 16, Symbols: 1},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("parseNMOutput mismatch:\ngot:  %+v\nwant: %+v", got, want)
	}

	// Verify topN truncation and deterministic tie-breaking by package name.
	tied := strings.Join([]string{
		"  1000 100 T pkg/b.Fn",
		"  2000 100 T pkg/a.Fn",
		"  3000 200 T pkg/c.Fn",
	}, "\n")
	top2, err := parseNMOutput(strings.NewReader(tied), 2)
	if err != nil {
		t.Fatalf("parseNMOutput top2: %v", err)
	}
	wantTop2 := []PackageSize{
		{Package: "pkg/c", Bytes: 200, Symbols: 1},
		{Package: "pkg/a", Bytes: 100, Symbols: 1},
	}
	if !slices.Equal(top2, wantTop2) {
		t.Fatalf("top2 = %+v, want %+v", top2, wantTop2)
	}
}

func TestParseGoList_ClassificationAndErrors(t *testing.T) {
	t.Parallel()

	stream := strings.Join([]string{
		`{"ImportPath":"runtime","Name":"runtime","Standard":true}`,
		`{"ImportPath":"fdb.dev/pkg/fdbgo/fdb","Name":"fdb","Standard":false,"Module":{"Path":"fdb.dev","Main":true}}`,
		`{"ImportPath":"fdb.dev/internal/fdbgo/client","Name":"client","Standard":false,"Module":{"Path":"fdb.dev","Main":true}}`,
		`{"ImportPath":"go.opentelemetry.io/otel","Name":"otel","Standard":false,"Module":{"Path":"go.opentelemetry.io/otel","Main":false}}`,
		`{"ImportPath":"go.opentelemetry.io/otel/trace","Name":"trace","Standard":false,"Module":{"Path":"go.opentelemetry.io/otel","Main":false}}`,
		`{"ImportPath":"github.com/zeebo/xxh3","Name":"xxh3","Standard":false,"Module":{"Path":"github.com/zeebo/xxh3","Main":false}}`,
	}, "\n")

	res, err := parseGoList(strings.NewReader(stream), "fdb.dev/pkg/fdbgo/fdb")
	if err != nil {
		t.Fatalf("parseGoList: %v", err)
	}
	if res.Kind != kindLibrary || res.Packages != 6 || res.StdPackages != 1 || res.RepoPackages != 2 || res.ExtPackages != 3 || res.ExtModules != 2 {
		t.Fatalf("unexpected depListResult: %+v", res)
	}
	wantMods := []string{"github.com/zeebo/xxh3", "go.opentelemetry.io/otel"}
	if !slices.Equal(res.ExtModuleList, wantMods) {
		t.Fatalf("ExtModuleList = %v, want %v", res.ExtModuleList, wantMods)
	}

	// Binary entrypoint sets Kind = binary.
	binStream := `{"ImportPath":"fdb.dev/cmd/fdb","Name":"main","Standard":false,"Module":{"Path":"fdb.dev","Main":true}}`
	binRes, err := parseGoList(strings.NewReader(binStream), "fdb.dev/cmd/fdb")
	if err != nil {
		t.Fatalf("parseGoList binary: %v", err)
	}
	if binRes.Kind != kindBinary {
		t.Fatalf("Kind = %q, want %q", binRes.Kind, kindBinary)
	}

	// Missing target entrypoint must fail rather than pass silently.
	if _, err := parseGoList(strings.NewReader(stream), "fdb.dev/pkg/missing"); err == nil {
		t.Fatal("expected error when target entrypoint is absent from go list stream")
	}
}

func TestCheckBudget(t *testing.T) {
	t.Parallel()

	budget := BudgetConfig{
		Entrypoints: []EntrypointBudget{
			{ImportPath: "fdb.dev/pkg/fdbgo/fdb", MaxPackages: 210, MaxExtModules: 6, MaxStrippedBytes: 4000000},
			{ImportPath: "fdb.dev/cmd/fdb", MaxPackages: 450, MaxExtModules: 50, MaxStrippedBytes: 55000000},
		},
	}

	exactReport := Report{
		Entrypoints: []EntrypointReport{
			{ImportPath: "fdb.dev/pkg/fdbgo/fdb", Packages: 210, ExtModules: 6, StrippedBytes: 4000000},
			{ImportPath: "fdb.dev/cmd/fdb", Packages: 440, ExtModules: 47, StrippedBytes: 52000000},
		},
	}
	if err := checkBudget(exactReport, budget); err != nil {
		t.Fatalf("expected equal/lower metrics to pass, got: %v", err)
	}

	tests := []struct {
		name       string
		report     Report
		budget     BudgetConfig
		wantSubstr string
		wantConfig bool
	}{
		{
			name: "exceeded packages",
			report: Report{
				Entrypoints: []EntrypointReport{
					{ImportPath: "fdb.dev/pkg/fdbgo/fdb", Packages: 211, ExtModules: 6, StrippedBytes: 4000000},
					{ImportPath: "fdb.dev/cmd/fdb", Packages: 440, ExtModules: 47, StrippedBytes: 52000000},
				},
			},
			budget:     budget,
			wantSubstr: "fdb.dev/pkg/fdbgo/fdb packages: actual 211 exceeds max 210 (+1)",
		},
		{
			name: "exceeded ext modules",
			report: Report{
				Entrypoints: []EntrypointReport{
					{ImportPath: "fdb.dev/pkg/fdbgo/fdb", Packages: 205, ExtModules: 7, StrippedBytes: 4000000},
					{ImportPath: "fdb.dev/cmd/fdb", Packages: 440, ExtModules: 47, StrippedBytes: 52000000},
				},
			},
			budget:     budget,
			wantSubstr: "fdb.dev/pkg/fdbgo/fdb ext_modules: actual 7 exceeds max 6 (+1)",
		},
		{
			name: "exceeded stripped bytes",
			report: Report{
				Entrypoints: []EntrypointReport{
					{ImportPath: "fdb.dev/pkg/fdbgo/fdb", Packages: 205, ExtModules: 6, StrippedBytes: 4000001},
					{ImportPath: "fdb.dev/cmd/fdb", Packages: 440, ExtModules: 47, StrippedBytes: 52000000},
				},
			},
			budget:     budget,
			wantSubstr: "fdb.dev/pkg/fdbgo/fdb stripped_bytes: actual 4000001 exceeds max 4000000 (+1)",
		},
		{
			name: "missing measured entrypoint",
			report: Report{
				Entrypoints: []EntrypointReport{
					{ImportPath: "fdb.dev/pkg/fdbgo/fdb", Packages: 205, ExtModules: 6, StrippedBytes: 3900000},
				},
			},
			budget:     budget,
			wantSubstr: "fdb.dev/cmd/fdb entrypoint: missing from measured report",
		},
		{
			name: "unmatched measured entrypoint",
			report: Report{
				Entrypoints: []EntrypointReport{
					{ImportPath: "fdb.dev/pkg/fdbgo/fdb", Packages: 205, ExtModules: 6, StrippedBytes: 3900000},
					{ImportPath: "fdb.dev/cmd/fdb", Packages: 440, ExtModules: 47, StrippedBytes: 52000000},
					{ImportPath: "fdb.dev/pkg/extra", Packages: 10, ExtModules: 0, StrippedBytes: 1000000},
				},
			},
			budget:     budget,
			wantSubstr: "fdb.dev/pkg/extra entrypoint: unmatched measured entrypoint has no budget entry",
		},
		{
			name:       "empty budget refused",
			report:     exactReport,
			budget:     BudgetConfig{},
			wantSubstr: "zero entrypoints",
			wantConfig: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := checkBudget(tc.report, tc.budget)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSubstr)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.wantSubstr)
			}
			if tc.wantConfig {
				var cfgErr *ConfigError
				if !errors.As(err, &cfgErr) {
					t.Fatalf("expected *ConfigError, got %T", err)
				}
			} else {
				var violErr *BudgetViolationError
				if !errors.As(err, &violErr) {
					t.Fatalf("expected *BudgetViolationError, got %T", err)
				}
			}
		})
	}
}

func TestMarkdownRendering(t *testing.T) {
	t.Parallel()

	report := Report{
		Entrypoints: []EntrypointReport{
			{
				ImportPath:      "fdb.dev/pkg/fdbgo/fdb",
				Kind:            kindLibrary,
				Packages:        205,
				StdPackages:     184,
				RepoPackages:    9,
				ExtPackages:     12,
				ExtModules:      6,
				ExtModuleList:   []string{"github.com/zeebo/xxh3", "go.opentelemetry.io/otel"},
				UnstrippedBytes: 5970649,
				StrippedBytes:   3952800,
				TopPackages: []PackageSize{
					{Package: "go:", Bytes: 255000, Symbols: 20},
					{Package: "fdb.dev/internal/fdbgo/client", Bytes: 120000, Symbols: 85},
				},
			},
		},
	}

	md := RenderMarkdown(report)
	for _, want := range []string{
		"| Entrypoint | Kind | Packages | Std | Repo | Ext Pkgs | Ext Mods | Unstripped (B) | Stripped (B) |",
		"| `fdb.dev/pkg/fdbgo/fdb` | library | 205 | 184 | 9 | 12 | 6 | 5970649 | 3952800 |",
		"### Top 2 packages by symbol size: `fdb.dev/pkg/fdbgo/fdb`",
		"| 1 | `go:` | 255000 | 20 |",
		"| 2 | `fdb.dev/internal/fdbgo/client` | 120000 | 85 |",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q in:\n%s", want, md)
		}
	}
}

func TestCheckedInBudgetValid(t *testing.T) {
	t.Parallel()

	cfg, err := loadBudget("budget.json")
	if err != nil {
		t.Fatalf("loadBudget(budget.json): %v", err)
	}
	got := make([]string, 0, len(cfg.Entrypoints))
	for _, ep := range cfg.Entrypoints {
		got = append(got, ep.ImportPath)
	}
	if !slices.Equal(got, defaultEntrypoints) {
		t.Fatalf("budget.json entrypoints = %v, want %v", got, defaultEntrypoints)
	}
}

func TestExitCodeForError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want int
	}{
		{name: "nil", err: nil, want: exitOK},
		{name: "budget violation", err: &BudgetViolationError{Violations: []BudgetViolation{{Entrypoint: "a", Metric: "packages"}}}, want: exitBudgetViolation},
		{name: "wrapped budget violation", err: fmt.Errorf("failed: %w", &BudgetViolationError{}), want: exitBudgetViolation},
		{name: "config error", err: &ConfigError{Field: "budget", Detail: "missing"}, want: exitError},
		{name: "command error", err: &CommandError{Op: "list", Entrypoint: "a", Err: errors.New("exit 1")}, want: exitError},
		{name: "parse error", err: &ParseError{Source: "nm", Detail: "bad"}, want: exitError},
		{name: "generic error", err: errors.New("unexpected"), want: exitError},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := exitCodeForError(tc.err); got != tc.want {
				t.Fatalf("exitCodeForError(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

func TestDefaultFlagsAndRunfilesResolution(t *testing.T) {
	t.Parallel()

	var opts options
	fs := flag.NewFlagSet("depbudget", flag.ContinueOnError)
	registerFlags(fs, &opts)
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("fs.Parse: %v", err)
	}
	if opts.format != formatMarkdown {
		t.Fatalf("default -format = %q, want %q", opts.format, formatMarkdown)
	}
	if opts.mode != modeReport {
		t.Fatalf("default -mode = %q, want %q", opts.mode, modeReport)
	}

	tmp := t.TempDir()
	const rloc = "rules_go_sdk/bin/go"
	runfilesDir := filepath.Join(tmp, "rf")
	rfGo := filepath.Join(runfilesDir, filepath.FromSlash(rloc))
	writeTestFile(t, rfGo, "#!/bin/sh\n")

	testSrcDir := filepath.Join(tmp, "srcdir")
	srcGo := filepath.Join(testSrcDir, filepath.FromSlash(rloc))
	writeTestFile(t, srcGo, "#!/bin/sh\n")

	exePath := filepath.Join(tmp, "bin", "depbudget")
	exeGo := filepath.Join(exePath+".runfiles", filepath.FromSlash(rloc))
	writeTestFile(t, exeGo, "#!/bin/sh\n")

	manifestTarget := filepath.Join(tmp, "manifest-target", "go")
	writeTestFile(t, manifestTarget, "#!/bin/sh\n")
	manifestPath := filepath.Join(tmp, "MANIFEST")
	writeTestFile(t, manifestPath, rloc+" "+manifestTarget+"\n")

	if got := resolveDepbudgetGo("", runfilesDir, testSrcDir, "", "", exePath, ""); got != "go" {
		t.Fatalf("empty env = %q, want go", got)
	}
	if got := resolveDepbudgetGo("/abs/go", runfilesDir, testSrcDir, "", "", exePath, ""); got != "/abs/go" {
		t.Fatalf("abs env = %q, want /abs/go", got)
	}
	if got := resolveDepbudgetGo(rloc, runfilesDir, testSrcDir, "", "", exePath, ""); got != rfGo {
		t.Fatalf("RUNFILES_DIR = %q, want %q", got, rfGo)
	}
	if got := resolveDepbudgetGo(rloc, "", testSrcDir, "_main", "", exePath, ""); got != srcGo {
		t.Fatalf("TEST_SRCDIR = %q, want %q", got, srcGo)
	}
	if got := resolveDepbudgetGo(rloc, "", "", "", "", exePath, ""); got != exeGo {
		t.Fatalf("exe.runfiles = %q, want %q", got, exeGo)
	}
	if got := resolveDepbudgetGo(rloc, "", "", "", manifestPath, "", ""); got != manifestTarget {
		t.Fatalf("manifest = %q, want %q", got, manifestTarget)
	}
	if got := resolveDepbudgetGo("custom-go", runfilesDir, testSrcDir, "", "", exePath, ""); got != "custom-go" {
		t.Fatalf("unresolved fallback = %q, want custom-go", got)
	}
}

func TestMeasureAndCheckEndToEnd(t *testing.T) {
	t.Parallel()

	goBin := defaultGoBin()
	if envGo := os.Getenv("DEPBUDGET_GO"); envGo != "" {
		if goBin == "go" {
			t.Fatalf("DEPBUDGET_GO=%q was not resolved from Bazel runfiles", envGo)
		}
		if _, err := os.Stat(goBin); err != nil {
			t.Fatalf("resolved DEPBUDGET_GO %q stat failed: %v", goBin, err)
		}
	}

	goBinPath, goEnv, err := buildGoEnv(goBin)
	if err != nil {
		t.Fatalf("buildGoEnv(%q): %v", goBin, err)
	}
	for _, wantEnv := range []string{
		"CGO_ENABLED=0",
		"GOPROXY=off",
		"GOSUMDB=off",
		"GOTOOLCHAIN=local",
		"GOWORK=off",
		"GOENV=off",
	} {
		if !slices.Contains(goEnv, wantEnv) {
			t.Errorf("goEnv missing %q", wantEnv)
		}
	}
	if filepath.Base(filepath.Dir(goBinPath)) == "bin" {
		wantGOROOT := "GOROOT=" + filepath.Dir(filepath.Dir(goBinPath))
		if !slices.Contains(goEnv, wantGOROOT) {
			t.Errorf("goEnv missing %q", wantGOROOT)
		}
	}

	modDir := t.TempDir()
	writeTestFile(t, filepath.Join(modDir, "go.mod"), "module example.com/synth\n\ngo 1.21\n")
	writeTestFile(t, filepath.Join(modDir, "lib", "lib.go"), "package lib\n\nimport \"fmt\"\n\nfunc Greeting() string {\n\treturn fmt.Sprintf(\"hello %d\", 42)\n}\n")
	writeTestFile(t, filepath.Join(modDir, "cmd", "app", "main.go"), "package main\n\nimport (\n\t\"fmt\"\n\n\t\"example.com/synth/lib\"\n)\n\nfunc main() {\n\tfmt.Println(lib.Greeting())\n}\n")

	const (
		libPkg = "example.com/synth/lib"
		binPkg = "example.com/synth/cmd/app"
	)

	libReport, err := measureEntrypoint(goBinPath, goEnv, modDir, libPkg, 10)
	if err != nil {
		t.Fatalf("measureEntrypoint(lib): %v", err)
	}
	if _, err := os.Stat(filepath.Join(modDir, "__depbudget_probe__")); !os.IsNotExist(err) {
		t.Fatalf("library probe left __depbudget_probe__ in module dir (stat err=%v)", err)
	}
	if libReport.Kind != kindLibrary {
		t.Fatalf("libReport.Kind = %q, want %q", libReport.Kind, kindLibrary)
	}
	if libReport.Packages <= 0 || libReport.StdPackages <= 0 || libReport.RepoPackages != 1 {
		t.Fatalf("unexpected lib package counts: %+v", libReport)
	}
	if libReport.ExtModules != 0 || libReport.ExtPackages != 0 || len(libReport.ExtModuleList) != 0 {
		t.Fatalf("unexpected external deps on stdlib-only module: %+v", libReport)
	}
	if libReport.StrippedBytes <= 0 || libReport.UnstrippedBytes <= libReport.StrippedBytes {
		t.Fatalf("unexpected lib binary sizes: unstripped=%d stripped=%d", libReport.UnstrippedBytes, libReport.StrippedBytes)
	}
	if len(libReport.TopPackages) == 0 || libReport.TopPackages[0].Bytes <= 0 {
		t.Fatalf("expected non-empty TopPackages for lib, got %+v", libReport.TopPackages)
	}

	binReport, err := measureEntrypoint(goBinPath, goEnv, modDir, binPkg, 10)
	if err != nil {
		t.Fatalf("measureEntrypoint(bin): %v", err)
	}
	if binReport.Kind != kindBinary {
		t.Fatalf("binReport.Kind = %q, want %q", binReport.Kind, kindBinary)
	}
	if binReport.RepoPackages != 2 || binReport.ExtModules != 0 || binReport.ExtPackages != 0 {
		t.Fatalf("unexpected bin package counts: %+v", binReport)
	}
	if binReport.StrippedBytes <= 0 || binReport.UnstrippedBytes <= binReport.StrippedBytes {
		t.Fatalf("unexpected bin binary sizes: unstripped=%d stripped=%d", binReport.UnstrippedBytes, binReport.StrippedBytes)
	}

	measured := Report{Entrypoints: []EntrypointReport{libReport, binReport}}
	passingBudget := BudgetConfig{
		Entrypoints: []EntrypointBudget{
			{
				ImportPath:       libPkg,
				MaxPackages:      libReport.Packages + 5,
				MaxExtModules:    0,
				MaxStrippedBytes: libReport.StrippedBytes + 1024,
			},
			{
				ImportPath:       binPkg,
				MaxPackages:      binReport.Packages + 5,
				MaxExtModules:    0,
				MaxStrippedBytes: binReport.StrippedBytes + 1024,
			},
		},
	}
	if err := checkBudget(measured, passingBudget); err != nil {
		t.Fatalf("checkBudget(passing): %v", err)
	}

	passingBudgetPath := filepath.Join(t.TempDir(), "passing-budget.json")
	passingJSON := fmt.Sprintf(
		`{"entrypoints":[{"import_path":%q,"max_packages":%d,"max_ext_modules":0,"max_stripped_bytes":%d},{"import_path":%q,"max_packages":%d,"max_ext_modules":0,"max_stripped_bytes":%d}]}`+"\n",
		libPkg, libReport.Packages+5, libReport.StrippedBytes+1024,
		binPkg, binReport.Packages+5, binReport.StrippedBytes+1024,
	)
	writeTestFile(t, passingBudgetPath, passingJSON)

	var passOut bytes.Buffer
	passErr := run(options{
		mode:        modeCheck,
		budgetPath:  passingBudgetPath,
		goBin:       goBin,
		repoRoot:    modDir,
		entrypoints: libPkg + "," + binPkg,
		format:      formatMarkdown,
		topN:        10,
	}, &passOut)
	if passErr != nil {
		t.Fatalf("run(modeCheck passing): %v\noutput:\n%s", passErr, passOut.String())
	}
	if code := exitCodeForError(passErr); code != exitOK {
		t.Fatalf("passing exitCodeForError = %d, want %d", code, exitOK)
	}
	if !strings.Contains(passOut.String(), "| PASS |") {
		t.Fatalf("passing check output missing PASS:\n%s", passOut.String())
	}

	failingBudget := BudgetConfig{
		Entrypoints: []EntrypointBudget{
			{
				ImportPath:       libPkg,
				MaxPackages:      1,
				MaxExtModules:    0,
				MaxStrippedBytes: 1,
			},
			{
				ImportPath:       binPkg,
				MaxPackages:      binReport.Packages + 5,
				MaxExtModules:    0,
				MaxStrippedBytes: binReport.StrippedBytes + 1024,
			},
		},
	}
	if err := checkBudget(measured, failingBudget); err == nil {
		t.Fatal("expected checkBudget(failing) to return violation error, got nil")
	} else if code := exitCodeForError(err); code != exitBudgetViolation {
		t.Fatalf("checkBudget(failing) exitCodeForError = %d, want %d (err=%v)", code, exitBudgetViolation, err)
	}

	failingBudgetPath := filepath.Join(t.TempDir(), "failing-budget.json")
	failingJSON := fmt.Sprintf(
		`{"entrypoints":[{"import_path":%q,"max_packages":1,"max_ext_modules":0,"max_stripped_bytes":1},{"import_path":%q,"max_packages":%d,"max_ext_modules":0,"max_stripped_bytes":%d}]}`+"\n",
		libPkg, binPkg, binReport.Packages+5, binReport.StrippedBytes+1024,
	)
	writeTestFile(t, failingBudgetPath, failingJSON)

	var failOut bytes.Buffer
	failErr := run(options{
		mode:        modeCheck,
		budgetPath:  failingBudgetPath,
		goBin:       goBin,
		repoRoot:    modDir,
		entrypoints: libPkg + "," + binPkg,
		format:      formatMarkdown,
		topN:        10,
	}, &failOut)
	if failErr == nil {
		t.Fatal("expected run(modeCheck failing) to fail, got nil")
	}
	var violErr *BudgetViolationError
	if !errors.As(failErr, &violErr) {
		t.Fatalf("expected *BudgetViolationError, got %T (%v)", failErr, failErr)
	}
	if code := exitCodeForError(failErr); code != exitBudgetViolation {
		t.Fatalf("failing exitCodeForError = %d, want %d", code, exitBudgetViolation)
	}
	if !strings.Contains(failOut.String(), "| FAIL |") {
		t.Fatalf("failing check output missing FAIL:\n%s", failOut.String())
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
