package main

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestParseNMOutput_AggregationAndEdgeCases(t *testing.T) {
	t.Parallel()

	sample := strings.Join([]string{
		// Normal package functions, methods, and %2e versioned module paths.
		"  53b600        139 T fdb.dev/pkg/fdbgo/client.(*ClientMetrics).observeReadLatency",
		"  53b760       2255 T fdb.dev/pkg/fdbgo/client.(*ClusterFile).String",
		" 1167c60       1000 T gopkg.in/yaml%2ev3.(*Encoder).Encode",
		" 1167d80        500 t gopkg.in/yaml%2ev3.(*Node).Encode.deferwrap1",
		// Generic symbols containing spaces and nested import paths inside [...].
		"  59e0a0        786 T crypto/internal/fips140/hkdf.Expand[go.shape.interface { BlockSize() int; Reset() }]",
		"  5b2450        200 R errors..dict.AsType[*fdb.dev/pkg/fdbgo/wire.FDBError]",
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
		{Package: "fdb.dev/pkg/fdbgo/client", Bytes: 2394, Symbols: 2},
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
		`{"ImportPath":"fdb.dev/pkg/fdbgo/client","Name":"client","Standard":false,"Module":{"Path":"fdb.dev","Main":true}}`,
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

func TestJSONRoundTripAndMarkdownRendering(t *testing.T) {
	t.Parallel()

	orig := Report{
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
					{Package: "fdb.dev/pkg/fdbgo/client", Bytes: 120000, Symbols: 85},
				},
			},
		},
	}

	raw, err := RenderJSON(orig)
	if err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	var decoded Report
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	raw2, err := RenderJSON(decoded)
	if err != nil {
		t.Fatalf("RenderJSON round-trip: %v", err)
	}
	if string(raw) != string(raw2) {
		t.Fatalf("JSON round-trip not deterministic:\n%s\nvs\n%s", raw, raw2)
	}

	md := RenderMarkdown(orig)
	for _, want := range []string{
		"| Entrypoint | Kind | Packages | Std | Repo | Ext Pkgs | Ext Mods | Unstripped (B) | Stripped (B) |",
		"| `fdb.dev/pkg/fdbgo/fdb` | library | 205 | 184 | 9 | 12 | 6 | 5970649 | 3952800 |",
		"### Top 2 packages by symbol size: `fdb.dev/pkg/fdbgo/fdb`",
		"| 1 | `go:` | 255000 | 20 |",
		"| 2 | `fdb.dev/pkg/fdbgo/client` | 120000 | 85 |",
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
