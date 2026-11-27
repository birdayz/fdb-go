package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

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
