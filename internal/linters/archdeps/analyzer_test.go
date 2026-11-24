package archdeps

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path"
	"strings"
	"testing"

	"golang.org/x/tools/go/analysis"
)

// stubPkgSources provides minimal compilable Go sources for every package
// imported by the test cases so types.Config.Check performs real Go
// type-checking in-process without shelling out to the go toolchain (which is
// unavailable inside the Bazel sandbox).
var stubPkgSources = map[string]string{
	"fdb.dev/gen":                                   "package gen\ntype Value struct{}\n",
	"fdb.dev/pkg/relational/api":                    "package api\nfunc Open() {}\n",
	"fdb.dev/pkg/relational/core/functions":         "package functions\nfunc Call() {}\n",
	"fdb.dev/pkg/relational/core/rowstruct":         "package rowstruct\ntype Row struct{}\n",
	"fdb.dev/pkg/relational/sqldriver":              "package sqldriver\nfunc Register() {}\n",
	"fdb.dev/internal/relational/core":              "package core\nfunc Init() {}\n",
	"fdb.dev/internal/relational/sqldriver":         "package sqldriver\nfunc Register() {}\n",
	"fdb.dev/relational/sqldriver":                  "package sqldriver\nfunc Register() {}\n",
	"fdb.dev/pkg/recordlayer":                       "package recordlayer\ntype Store struct{}\n",
	"fdb.dev/pkg/recordlayer/keyspace":              "package keyspace\ntype Keyspace struct{}\n",
	"fdb.dev/pkg/recordlayer/javanum":               "package javanum\nfunc Parse() {}\n",
	"fdb.dev/internal/recordlayer/javanum":          "package javanum\nfunc Parse() {}\n",
	"fdb.dev/recordlayer/javanum":                   "package javanum\nfunc Parse() {}\n",
	"fdb.dev/pkg/recordlayer/protoname":             "package protoname\nfunc Escape(s string) string { return s }\n",
	"fdb.dev/pkg/recordlayer/query/plan/plans":      "package plans\ntype Plan struct{}\n",
	"fdb.dev/internal/recordlayer/query/plan/plans": "package plans\ntype Plan struct{}\n",
	"fdb.dev/recordlayer/query/plan/plans":          "package plans\ntype Plan struct{}\n",
	"fdb.dev/pkg/recordlayer/query/executor":        "package executor\nfunc Execute() {}\n",
	"fdb.dev/internal/recordlayer/query/executor":   "package executor\nfunc Execute() {}\n",
	"fdb.dev/recordlayer/query/executor":            "package executor\nfunc Execute() {}\n",
	"fdb.dev/recordlayer":                           "package recordlayer\ntype Store struct{}\n",
	"fdb.dev/pkg/fdbgo/fdb/tuple":                   "package tuple\ntype Tuple []any\n",
	"fdb.dev/pkg/fdbgo/client":                      "package client\ntype DB struct{}\n",
	"fdb.dev/fdb/client":                            "package client\ntype DB struct{}\n",
	"fdb.dev/pkg/simfdb":                            "package simfdb\nfunc Start() {}\n",
	"fdb.dev/pkg/testcontainers/foundationdb":       "package foundationdb\nfunc Run() {}\n",
	"fdb.dev/pkg/dst":                               "package dst\nfunc Check() {}\n",
	"fdb.dev/internal/linters/archdeps":             "package archdeps\nvar Name string\n",
	"github.com/docker/docker/client":               "package client\ntype Client struct{}\n",
	"github.com/stretchr/testify/require":           "package require\nfunc NoError(any, error) {}\n",
	"github.com/onsi/gomega":                        "package gomega\nfunc Expect(any) any { return nil }\n",
	"go.uber.org/mock/gomock":                       "package gomock\ntype Controller struct{}\n",
	"go.opentelemetry.io/otel":                      "package otel\nfunc Tracer(string) any { return nil }\n",
	"go.opentelemetry.io/otel/trace":                "package trace\ntype Span interface{}\n",
	"go.opentelemetry.io/otel/sdk/trace":            "package trace\ntype TracerProvider struct{}\n",
	"go.opentelemetry.io/proto/otlp/trace/v1":       "package v1\ntype Span struct{}\n",
}

type pkgImporter struct {
	fset *token.FileSet
	pkgs map[string]*types.Package
}

func (im *pkgImporter) Import(importPath string) (*types.Package, error) {
	if pkg, ok := im.pkgs[importPath]; ok {
		return pkg, nil
	}
	src, ok := stubPkgSources[importPath]
	if !ok {
		src = "package " + path.Base(importPath) + "\n"
	}
	f, err := parser.ParseFile(im.fset, importPath+".go", src, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	conf := &types.Config{
		Importer: im,
		Sizes:    types.SizesFor("gc", "amd64"),
	}
	pkg, err := conf.Check(importPath, im.fset, []*ast.File{f}, nil)
	if err != nil {
		return nil, err
	}
	im.pkgs[importPath] = pkg
	return pkg, nil
}

type sourceFile struct {
	filename string
	src      string
}

func analyzeFiles(t *testing.T, pkgPath string, inputs []sourceFile) []analysis.Diagnostic {
	t.Helper()
	fset := token.NewFileSet()
	files := make([]*ast.File, 0, len(inputs))
	for _, in := range inputs {
		file, err := parser.ParseFile(fset, in.filename, in.src, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", in.filename, err)
		}
		files = append(files, file)
	}
	info := &types.Info{
		Types: map[ast.Expr]types.TypeAndValue{},
		Defs:  map[*ast.Ident]types.Object{},
		Uses:  map[*ast.Ident]types.Object{},
	}
	sizes := types.SizesFor("gc", "amd64")
	conf := &types.Config{
		Importer: &pkgImporter{fset: fset, pkgs: map[string]*types.Package{}},
		Sizes:    sizes,
	}
	pkg, err := conf.Check(pkgPath, fset, files, info)
	if err != nil {
		t.Fatalf("typecheck %s: %v", pkgPath, err)
	}
	var diags []analysis.Diagnostic
	pass := &analysis.Pass{
		Analyzer:   Analyzer,
		Fset:       fset,
		Files:      files,
		Pkg:        pkg,
		TypesInfo:  info,
		TypesSizes: sizes,
		ResultOf:   map[*analysis.Analyzer]any{},
		Report:     func(d analysis.Diagnostic) { diags = append(diags, d) },
	}
	if _, err := Analyzer.Run(pass); err != nil {
		t.Fatalf("Analyzer.Run: %v", err)
	}
	return diags
}

func analyze(t *testing.T, pkgPath, filename, src string) []analysis.Diagnostic {
	t.Helper()
	return analyzeFiles(t, pkgPath, []sourceFile{{filename: filename, src: src}})
}

func srcImporting(pkgName, imp string) string {
	return "package " + pkgName + "\n\nimport _ \"" + imp + "\"\n"
}

func TestAnalyzerRegistration(t *testing.T) {
	t.Parallel()
	if err := analysis.Validate([]*analysis.Analyzer{Analyzer}); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if Analyzer.Name != "archdeps" {
		t.Fatalf("Analyzer.Name = %q, want archdeps", Analyzer.Name)
	}
	if Analyzer.Doc == "" || Analyzer.Run == nil {
		t.Fatalf("Analyzer missing Doc or Run")
	}
}

func TestRules(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		pkgPath  string
		filename string
		src      string
		want     int
	}{
		// R1: Record Layer _|_ Relational
		{
			name:     "R1 prod recordlayer importing relational",
			pkgPath:  "fdb.dev/pkg/recordlayer/query/executor",
			filename: "pkg/recordlayer/query/executor/executor.go",
			src:      srcImporting("executor", "fdb.dev/pkg/relational/api"),
			want:     1,
		},
		{
			name:     "R1 top-level recordlayer importing internal relational",
			pkgPath:  "fdb.dev/recordlayer/store",
			filename: "recordlayer/store/store.go",
			src:      srcImporting("store", "fdb.dev/internal/relational/core"),
			want:     1,
		},
		{
			name:     "R1 unallowlisted test file importing relational fails",
			pkgPath:  "fdb.dev/pkg/recordlayer/query/executor",
			filename: "pkg/recordlayer/query/executor/other_test.go",
			src:      srcImporting("executor", "fdb.dev/pkg/relational/api"),
			want:     1,
		},
		{
			name:     "R1 retired executor_test importing relational fails",
			pkgPath:  "fdb.dev/pkg/recordlayer/query/executor",
			filename: "pkg/recordlayer/query/executor/executor_test.go",
			src:      srcImporting("executor", "fdb.dev/pkg/relational/core/functions"),
			want:     1,
		},
		{
			name:     "R1 retired read_path_agreement_test with Bazel prefix fails",
			pkgPath:  "fdb.dev/pkg/recordlayer/query/executor",
			filename: "/b/f/w/execroot/_main/pkg/recordlayer/query/executor/read_path_agreement_test.go",
			src:      srcImporting("executor", "fdb.dev/pkg/relational/core/rowstruct"),
			want:     1,
		},
		{
			name:     "R1 path boundary check prevents false suffix match",
			pkgPath:  "fdb.dev/pkg/recordlayer/query/executor",
			filename: "other_pkg/recordlayer/query/executor/executor_test.go",
			src:      srcImporting("executor", "fdb.dev/pkg/relational/core/functions"),
			want:     1,
		},
		{
			name:     "R1 allowlisted file importing non-allowlisted relational fails",
			pkgPath:  "fdb.dev/pkg/recordlayer/query/executor",
			filename: "pkg/recordlayer/query/executor/executor_test.go",
			src:      srcImporting("executor", "fdb.dev/pkg/relational/api"),
			want:     1,
		},
		{
			name:     "R1 sibling prefix is ignored",
			pkgPath:  "fdb.dev/pkg/recordlayerx",
			filename: "pkg/recordlayerx/x.go",
			src:      srcImporting("recordlayerx", "fdb.dev/pkg/relational/api"),
			want:     0,
		},

		// R2: Pure Planner _|_ Storage & FDB
		{
			name:     "R2 planner importing recordlayer root fails",
			pkgPath:  "fdb.dev/pkg/recordlayer/query/plan/cascades",
			filename: "pkg/recordlayer/query/plan/cascades/x.go",
			src:      srcImporting("cascades", "fdb.dev/pkg/recordlayer"),
			want:     1,
		},
		{
			name:     "R2 planner test importing non-plan storage fails",
			pkgPath:  "fdb.dev/pkg/recordlayer/query/plan/cascades",
			filename: "pkg/recordlayer/query/plan/cascades/x_test.go",
			src:      srcImporting("cascades", "fdb.dev/pkg/recordlayer/keyspace"),
			want:     1,
		},
		{
			name:     "R2 planner importing executor fails",
			pkgPath:  "fdb.dev/pkg/recordlayer/query/plan/cascades",
			filename: "pkg/recordlayer/query/plan/cascades/x.go",
			src:      srcImporting("cascades", "fdb.dev/pkg/recordlayer/query/executor"),
			want:     1,
		},
		{
			name:     "R2 planner importing unallowlisted tuple fails",
			pkgPath:  "fdb.dev/pkg/recordlayer/query/plan/plans",
			filename: "pkg/recordlayer/query/plan/plans/other.go",
			src:      srcImporting("plans", "fdb.dev/pkg/fdbgo/fdb/tuple"),
			want:     1,
		},
		{
			name:     "R2 internal planner importing fdb fails",
			pkgPath:  "fdb.dev/internal/recordlayer/query/plan",
			filename: "internal/recordlayer/query/plan/x.go",
			src:      srcImporting("plan", "fdb.dev/fdb/client"),
			want:     1,
		},
		{
			name:     "R2 planner importing allowed leaves across pkg/internal/top-level and subplan passes",
			pkgPath:  "fdb.dev/pkg/recordlayer/query/plan/cascades",
			filename: "pkg/recordlayer/query/plan/cascades/x.go",
			src:      "package cascades\n\nimport (\n\t_ \"fdb.dev/pkg/recordlayer/javanum\"\n\t_ \"fdb.dev/internal/recordlayer/javanum\"\n\t_ \"fdb.dev/recordlayer/javanum\"\n\t_ \"fdb.dev/pkg/recordlayer/query/plan/plans\"\n)\n",
			want:     0,
		},
		{
			name:     "R2 retired text_comparison importing recordlayer fails",
			pkgPath:  "fdb.dev/pkg/recordlayer/query/plan/cascades/predicates",
			filename: "/sandbox/execroot/_main/pkg/recordlayer/query/plan/cascades/predicates/text_comparison.go",
			src:      srcImporting("predicates", "fdb.dev/pkg/recordlayer"),
			want:     1,
		},
		{
			name:     "R2 text_comparison importing L0 textcmp leaf passes",
			pkgPath:  "fdb.dev/pkg/recordlayer/query/plan/cascades/predicates",
			filename: "/sandbox/execroot/_main/pkg/recordlayer/query/plan/cascades/predicates/text_comparison.go",
			src:      srcImporting("predicates", "fdb.dev/internal/recordlayer/textcmp"),
			want:     0,
		},
		{
			name:     "R2 allowlisted load_by_keys tuple passes",
			pkgPath:  "fdb.dev/pkg/recordlayer/query/plan/plans",
			filename: "pkg/recordlayer/query/plan/plans/load_by_keys.go",
			src:      srcImporting("plans", "fdb.dev/pkg/fdbgo/fdb/tuple"),
			want:     0,
		},

		// R3: FDB Client _|_ Upper Layers
		{
			name:     "R3 fdbgo prod importing recordlayer fails",
			pkgPath:  "fdb.dev/pkg/fdbgo/client",
			filename: "pkg/fdbgo/client/x.go",
			src:      srcImporting("client", "fdb.dev/pkg/recordlayer"),
			want:     1,
		},
		{
			name:     "R3 fdb external test package importing relational fails",
			pkgPath:  "fdb.dev/pkg/fdbgo/fdb_test",
			filename: "pkg/fdbgo/fdb/x_test.go",
			src:      srcImporting("fdb_test", "fdb.dev/pkg/relational/api"),
			want:     1,
		},
		{
			name:     "R3 internal fdbwire importing top-level recordlayer fails",
			pkgPath:  "fdb.dev/internal/fdbwire",
			filename: "internal/fdbwire/x.go",
			src:      srcImporting("fdbwire", "fdb.dev/recordlayer"),
			want:     1,
		},
		{
			name:     "R3 allowlisted libfdbc differential_test external pkg passes",
			pkgPath:  "fdb.dev/pkg/fdbgo/libfdbc_test",
			filename: "/execroot/_main/pkg/fdbgo/libfdbc/differential_test.go",
			src:      srcImporting("libfdbc_test", "fdb.dev/pkg/recordlayer"),
			want:     0,
		},
		{
			name:     "R3 allowlisted libfdbc differential_test importing relational fails",
			pkgPath:  "fdb.dev/pkg/fdbgo/libfdbc_test",
			filename: "pkg/fdbgo/libfdbc/differential_test.go",
			src:      srcImporting("libfdbc_test", "fdb.dev/pkg/relational/api"),
			want:     1,
		},

		// R4: Core Runtime Production Libraries
		{
			name:     "R4 prod fdbgo importing simfdb fails",
			pkgPath:  "fdb.dev/pkg/fdbgo/transport",
			filename: "pkg/fdbgo/transport/x.go",
			src:      srcImporting("transport", "fdb.dev/pkg/simfdb"),
			want:     1,
		},
		{
			name:     "R4 prod recordlayer importing testcontainers fails",
			pkgPath:  "fdb.dev/pkg/recordlayer",
			filename: "pkg/recordlayer/x.go",
			src:      srcImporting("recordlayer", "fdb.dev/pkg/testcontainers/foundationdb"),
			want:     1,
		},
		{
			name:     "R4 prod relational core importing unallowlisted dst fails",
			pkgPath:  "fdb.dev/pkg/relational/core/embedded",
			filename: "pkg/relational/core/embedded/other.go",
			src:      srcImporting("embedded", "fdb.dev/pkg/dst"),
			want:     1,
		},
		{
			name:     "R4 prod relational sqldriver importing docker fails",
			pkgPath:  "fdb.dev/pkg/relational/sqldriver",
			filename: "pkg/relational/sqldriver/x.go",
			src:      srcImporting("sqldriver", "github.com/docker/docker/client"),
			want:     1,
		},
		{
			name:     "R4 prod recordlayer importing linters fails",
			pkgPath:  "fdb.dev/pkg/recordlayer",
			filename: "pkg/recordlayer/x.go",
			src:      srcImporting("recordlayer", "fdb.dev/internal/linters/archdeps"),
			want:     1,
		},
		{
			name:     "R4 test file in core runtime importing dst and testcontainers passes",
			pkgPath:  "fdb.dev/pkg/relational/sqldriver",
			filename: "pkg/relational/sqldriver/x_test.go",
			src:      "package sqldriver\n\nimport (\n\t_ \"fdb.dev/pkg/dst\"\n\t_ \"fdb.dev/pkg/testcontainers/foundationdb\"\n)\n",
			want:     0,
		},
		{
			name:     "R4 chaos package exempt in prod",
			pkgPath:  "fdb.dev/pkg/recordlayer/chaos",
			filename: "pkg/recordlayer/chaos/sim.go",
			src:      srcImporting("chaos", "fdb.dev/pkg/simfdb"),
			want:     0,
		},
		{
			name:     "R4 keystoretest package exempt in prod",
			pkgPath:  "fdb.dev/pkg/recordlayer/keystoretest",
			filename: "pkg/recordlayer/keystoretest/ks.go",
			src:      srcImporting("keystoretest", "fdb.dev/pkg/dst"),
			want:     0,
		},
		{
			name:     "R4 retired dst prod import with Bazel prefix fails",
			pkgPath:  "fdb.dev/pkg/recordlayer",
			filename: "/b/f/w/execroot/_main/pkg/recordlayer/store.go",
			src:      srcImporting("recordlayer", "fdb.dev/pkg/dst"),
			want:     1,
		},
		{
			name:     "R4+R5 prod relational importing gomock fails",
			pkgPath:  "fdb.dev/pkg/relational/api",
			filename: "pkg/relational/api/mocks.go",
			src:      srcImporting("api", "go.uber.org/mock/gomock"),
			want:     2,
		},

		// R5: Test-Framework Sprawl Freeze
		{
			name:     "R5 unallowlisted package importing testify fails",
			pkgPath:  "fdb.dev/pkg/recordlayer/rlmetrics",
			filename: "pkg/recordlayer/rlmetrics/rlmetrics_test.go",
			src:      srcImporting("rlmetrics", "github.com/stretchr/testify/require"),
			want:     1,
		},
		{
			name:     "R5 unallowlisted external test package importing gomega fails",
			pkgPath:  "fdb.dev/pkg/relational/core/embedded_test",
			filename: "pkg/relational/core/embedded/x_test.go",
			src:      srcImporting("embedded_test", "github.com/onsi/gomega"),
			want:     1,
		},
		{
			name:     "R5 allowlisted external test package normalized passes",
			pkgPath:  "fdb.dev/pkg/fdbgo/fdb_test",
			filename: "pkg/fdbgo/fdb/error_test.go",
			src:      srcImporting("fdb_test", "github.com/onsi/gomega"),
			want:     0,
		},
		{
			name:     "R5 unembedded Bazel test package allowlisted passes",
			pkgPath:  "pkg/relational/sqltest/agg/agg_test",
			filename: "pkg/relational/sqltest/agg/agg_test.go",
			src:      srcImporting("sqltest", "github.com/stretchr/testify/require"),
			want:     0,
		},
		{
			name:     "R5 unembedded Bazel test package unallowlisted fails",
			pkgPath:  "pkg/relational/sqltest/dml/dml_test",
			filename: "pkg/relational/sqltest/dml/dml_test.go",
			src:      srcImporting("sqltest", "github.com/onsi/gomega"),
			want:     1,
		},

		// R6: FDB Client Production _|_ OpenTelemetry
		{
			name:     "R6 unallowlisted fdbgo prod file importing otel fails",
			pkgPath:  "fdb.dev/pkg/fdbgo/client",
			filename: "pkg/fdbgo/client/other.go",
			src:      srcImporting("client", "go.opentelemetry.io/otel"),
			want:     1,
		},
		{
			name:     "R6 unallowlisted fdbgo prod file importing otel/trace fails",
			pkgPath:  "fdb.dev/pkg/fdbgo/transport",
			filename: "pkg/fdbgo/transport/conn.go",
			src:      srcImporting("transport", "go.opentelemetry.io/otel/trace"),
			want:     1,
		},
		{
			name:     "R6 internal fdbwire prod file importing otel/sdk/trace fails",
			pkgPath:  "fdb.dev/internal/fdbwire",
			filename: "internal/fdbwire/wire.go",
			src:      srcImporting("fdbwire", "go.opentelemetry.io/otel/sdk/trace"),
			want:     1,
		},
		{
			name:     "R6 allowlisted fdbgo client span.go importing otel/trace passes",
			pkgPath:  "fdb.dev/pkg/fdbgo/client",
			filename: "/execroot/_main/pkg/fdbgo/client/span.go",
			src:      srcImporting("client", "go.opentelemetry.io/otel/trace"),
			want:     0,
		},
		{
			name:     "R6 allowlisted fdbgo client database.go importing otel/sdk/trace fails",
			pkgPath:  "fdb.dev/pkg/fdbgo/client",
			filename: "pkg/fdbgo/client/database.go",
			src:      srcImporting("client", "go.opentelemetry.io/otel/sdk/trace"),
			want:     1,
		},
		{
			name:     "R6 fdbgo test file importing otel/sdk/trace passes",
			pkgPath:  "fdb.dev/pkg/fdbgo/client",
			filename: "pkg/fdbgo/client/span_otel_test.go",
			src:      srcImporting("client", "go.opentelemetry.io/otel/sdk/trace"),
			want:     0,
		},
		{
			name:     "R6 proto otlp is not go.opentelemetry.io/otel",
			pkgPath:  "fdb.dev/pkg/fdbgo/client",
			filename: "pkg/fdbgo/client/other.go",
			src:      srcImporting("client", "go.opentelemetry.io/proto/otlp/trace/v1"),
			want:     0,
		},

		// L0: Leaf Primitives _|_ Upward Layers
		{
			name:     "L0 leaf importing recordlayer storage fails",
			pkgPath:  "fdb.dev/pkg/recordlayer/javanum",
			filename: "pkg/recordlayer/javanum/x.go",
			src:      srcImporting("javanum", "fdb.dev/pkg/recordlayer"),
			want:     1,
		},
		{
			name:     "L0 leaf importing planner fails",
			pkgPath:  "fdb.dev/pkg/recordlayer/protoname",
			filename: "pkg/recordlayer/protoname/x.go",
			src:      srcImporting("protoname", "fdb.dev/pkg/recordlayer/query/plan/plans"),
			want:     1,
		},
		{
			name:     "L0 leaf importing executor fails",
			pkgPath:  "fdb.dev/pkg/recordlayer/vectorcodec",
			filename: "pkg/recordlayer/vectorcodec/x.go",
			src:      srcImporting("vectorcodec", "fdb.dev/pkg/recordlayer/query/executor"),
			want:     1,
		},
		{
			name:     "L0 leaf test importing fdbgo tuple fails",
			pkgPath:  "fdb.dev/pkg/recordlayer/javanum",
			filename: "pkg/recordlayer/javanum/x_test.go",
			src:      srcImporting("javanum", "fdb.dev/pkg/fdbgo/fdb/tuple"),
			want:     1,
		},
		{
			name:     "L0 allowlisted tupleordering importing fdbgo tuple passes",
			pkgPath:  "fdb.dev/pkg/recordlayer/tupleordering",
			filename: "/execroot/_main/pkg/recordlayer/tupleordering/tupleordering.go",
			src:      srcImporting("tupleordering", "fdb.dev/pkg/fdbgo/fdb/tuple"),
			want:     0,
		},
		{
			name:     "L0 internal leaf importing top-level fdb client fails",
			pkgPath:  "fdb.dev/internal/recordlayer/javanum",
			filename: "internal/recordlayer/javanum/x.go",
			src:      srcImporting("javanum", "fdb.dev/fdb/client"),
			want:     1,
		},
		{
			name:     "L0 leaf importing gen and sibling leaf passes",
			pkgPath:  "fdb.dev/pkg/recordlayer/indexpredicate",
			filename: "pkg/recordlayer/indexpredicate/normalize.go",
			src:      "package indexpredicate\n\nimport (\n\t_ \"fdb.dev/gen\"\n\t_ \"fdb.dev/pkg/recordlayer/protoname\"\n)\n",
			want:     0,
		},

		// L3: Storage _|_ Planner & Executor in Production
		{
			name:     "L3 prod recordlayer importing planner fails",
			pkgPath:  "fdb.dev/pkg/recordlayer",
			filename: "pkg/recordlayer/store.go",
			src:      srcImporting("recordlayer", "fdb.dev/pkg/recordlayer/query/plan/plans"),
			want:     1,
		},
		{
			name:     "L3 prod recordlayer subpackage importing executor fails",
			pkgPath:  "fdb.dev/pkg/recordlayer/keyspace",
			filename: "pkg/recordlayer/keyspace/k.go",
			src:      srcImporting("keyspace", "fdb.dev/pkg/recordlayer/query/executor"),
			want:     1,
		},
		{
			name:     "L3 internal prod storage importing top-level planner fails",
			pkgPath:  "fdb.dev/internal/recordlayer/storage",
			filename: "internal/recordlayer/storage/s.go",
			src:      srcImporting("storage", "fdb.dev/recordlayer/query/plan/plans"),
			want:     1,
		},
		{
			name:     "L3 test file importing planner passes",
			pkgPath:  "fdb.dev/pkg/recordlayer",
			filename: "pkg/recordlayer/index_entry_reader_fdb_test.go",
			src:      srcImporting("recordlayer", "fdb.dev/pkg/recordlayer/query/plan/plans"),
			want:     0,
		},
		{
			name:     "L5 executor prod importing planner passes",
			pkgPath:  "fdb.dev/pkg/recordlayer/query/executor",
			filename: "pkg/recordlayer/query/executor/executor.go",
			src:      srcImporting("executor", "fdb.dev/pkg/recordlayer/query/plan/plans"),
			want:     0,
		},

		// L6: Relational Core/API _|_ SQL Driver in Production
		{
			name:     "L6 prod relational api importing sqldriver fails",
			pkgPath:  "fdb.dev/pkg/relational/api",
			filename: "pkg/relational/api/conn.go",
			src:      srcImporting("api", "fdb.dev/pkg/relational/sqldriver"),
			want:     1,
		},
		{
			name:     "L6 prod relational core importing sqldriver fails",
			pkgPath:  "fdb.dev/pkg/relational/core/embedded",
			filename: "pkg/relational/core/embedded/conn.go",
			src:      srcImporting("embedded", "fdb.dev/pkg/relational/sqldriver"),
			want:     1,
		},
		{
			name:     "L6 internal relational core importing top-level sqldriver fails",
			pkgPath:  "fdb.dev/internal/relational/core",
			filename: "internal/relational/core/x.go",
			src:      srcImporting("core", "fdb.dev/relational/sqldriver"),
			want:     1,
		},
		{
			name:     "L6 relational core test importing sqldriver passes",
			pkgPath:  "fdb.dev/pkg/relational/core/embedded",
			filename: "pkg/relational/core/embedded/x_test.go",
			src:      srcImporting("embedded", "fdb.dev/pkg/relational/sqldriver"),
			want:     0,
		},

		// External files skipped
		{
			name:     "external dependency file skipped",
			pkgPath:  "fdb.dev/pkg/recordlayer",
			filename: "external/some_repo/foo.go",
			src:      srcImporting("recordlayer", "fdb.dev/pkg/relational/api"),
			want:     0,
		},
		{
			name:     "non-fdb.dev package skipped",
			pkgPath:  "example.com/thirdparty/pkg",
			filename: "thirdparty/foo.go",
			src:      srcImporting("pkg", "fdb.dev/pkg/relational/api"),
			want:     0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			diags := analyze(t, tc.pkgPath, tc.filename, tc.src)
			if len(diags) != tc.want {
				t.Fatalf("diagnostics = %d (%v), want %d", len(diags), diags, tc.want)
			}
		})
	}
}

func TestAnalyzerEndToEndMultiFileAndPositions(t *testing.T) {
	t.Parallel()

	t.Run("multi-file package with real symbol references", func(t *testing.T) {
		t.Parallel()
		diags := analyzeFiles(t, "fdb.dev/pkg/recordlayer/storage", []sourceFile{
			{
				filename: "pkg/recordlayer/storage/good.go",
				src:      "package storage\n\nimport \"fdb.dev/pkg/recordlayer/protoname\"\n\nfunc Clean(s string) string {\n\treturn protoname.Escape(s)\n}\n",
			},
			{
				filename: "pkg/recordlayer/storage/bad_rel.go",
				src:      "package storage\n\nimport \"fdb.dev/pkg/relational/api\"\n\nfunc BadRel() {\n\tapi.Open()\n}\n",
			},
			{
				filename: "pkg/recordlayer/storage/bad_plan.go",
				src:      "package storage\n\nimport \"fdb.dev/pkg/recordlayer/query/plan/plans\"\n\nfunc BadPlan() plans.Plan {\n\treturn plans.Plan{}\n}\n",
			},
			{
				filename: "pkg/recordlayer/storage/ok_test.go",
				src:      "package storage\n\nimport \"fdb.dev/pkg/recordlayer/query/plan/plans\"\n\nfunc TestHelper() plans.Plan {\n\treturn plans.Plan{}\n}\n",
			},
		})
		if len(diags) != 2 {
			t.Fatalf("diagnostics = %d (%v), want 2", len(diags), diags)
		}
		var messages []string
		for _, d := range diags {
			if !d.Pos.IsValid() || !d.End.IsValid() || d.End <= d.Pos {
				t.Errorf("invalid diagnostic span: %+v", d)
			}
			messages = append(messages, d.Message)
		}
		joined := strings.Join(messages, "\n")
		if !strings.Contains(joined, "archdeps (R1):") {
			t.Errorf("expected R1 diagnostic, got:\n%s", joined)
		}
		if !strings.Contains(joined, "archdeps (L3):") {
			t.Errorf("expected L3 diagnostic, got:\n%s", joined)
		}
	})

	t.Run("L6 and R6 exact diagnostic tags and spans", func(t *testing.T) {
		t.Parallel()
		l6Diags := analyze(t, "fdb.dev/pkg/relational/core/embedded", "pkg/relational/core/embedded/bad.go",
			"package embedded\n\nimport \"fdb.dev/pkg/relational/sqldriver\"\n\nfunc f() { sqldriver.Register() }\n")
		if len(l6Diags) != 1 || !strings.Contains(l6Diags[0].Message, "archdeps (L6):") {
			t.Fatalf("L6 diagnostics = %+v, want 1 L6 diagnostic", l6Diags)
		}
		if !l6Diags[0].Pos.IsValid() || !l6Diags[0].End.IsValid() {
			t.Fatalf("L6 diagnostic missing valid Pos/End: %+v", l6Diags[0])
		}

		r6Diags := analyze(t, "fdb.dev/pkg/fdbgo/client", "pkg/fdbgo/client/bad.go",
			"package client\n\nimport \"go.opentelemetry.io/otel/trace\"\n\nvar _ trace.Span\n")
		if len(r6Diags) != 1 || !strings.Contains(r6Diags[0].Message, "archdeps (R6):") {
			t.Fatalf("R6 diagnostics = %+v, want 1 R6 diagnostic", r6Diags)
		}
	})

	t.Run("nil pass Pkg is handled safely", func(t *testing.T) {
		t.Parallel()
		var diags []analysis.Diagnostic
		pass := &analysis.Pass{
			Analyzer: Analyzer,
			Fset:     token.NewFileSet(),
			Report:   func(d analysis.Diagnostic) { diags = append(diags, d) },
		}
		if _, err := Analyzer.Run(pass); err != nil {
			t.Fatalf("Analyzer.Run(nil Pkg): %v", err)
		}
		if len(diags) != 0 {
			t.Fatalf("diagnostics = %v, want 0", diags)
		}
	})
}
