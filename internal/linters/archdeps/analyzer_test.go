package archdeps

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"testing"

	"golang.org/x/tools/go/analysis"
)

func analyze(t *testing.T, pkgPath, filename, src string) []analysis.Diagnostic {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var diags []analysis.Diagnostic
	pass := &analysis.Pass{
		Fset:   fset,
		Files:  []*ast.File{file},
		Pkg:    types.NewPackage(pkgPath, file.Name.Name),
		Report: func(d analysis.Diagnostic) { diags = append(diags, d) },
	}
	if _, err := run(pass); err != nil {
		t.Fatalf("run: %v", err)
	}
	return diags
}

func srcImporting(pkgName, imp string) string {
	return "package " + pkgName + "\n\nimport \"" + imp + "\"\n"
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
			name:     "R1 allowlisted executor_test passes",
			pkgPath:  "fdb.dev/pkg/recordlayer/query/executor",
			filename: "pkg/recordlayer/query/executor/executor_test.go",
			src:      srcImporting("executor", "fdb.dev/pkg/relational/core/functions"),
			want:     0,
		},
		{
			name:     "R1 allowlisted read_path_agreement_test with Bazel prefix passes",
			pkgPath:  "fdb.dev/pkg/recordlayer/query/executor",
			filename: "/b/f/w/execroot/_main/pkg/recordlayer/query/executor/read_path_agreement_test.go",
			src:      srcImporting("executor", "fdb.dev/pkg/relational/core/rowstruct"),
			want:     0,
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
			name:     "R2 planner importing allowed leaf and subplan passes",
			pkgPath:  "fdb.dev/pkg/recordlayer/query/plan/cascades",
			filename: "pkg/recordlayer/query/plan/cascades/x.go",
			src:      "package cascades\n\nimport (\n\t_ \"fdb.dev/pkg/recordlayer/javanum\"\n\t_ \"fdb.dev/pkg/recordlayer/query/plan/plans\"\n)\n",
			want:     0,
		},
		{
			name:     "R2 allowlisted text_comparison with Bazel prefix passes",
			pkgPath:  "fdb.dev/pkg/recordlayer/query/plan/cascades/predicates",
			filename: "/sandbox/execroot/_main/pkg/recordlayer/query/plan/cascades/predicates/text_comparison.go",
			src:      srcImporting("predicates", "fdb.dev/pkg/recordlayer"),
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
			name:     "R4 allowlisted dst prod import with Bazel prefix passes",
			pkgPath:  "fdb.dev/pkg/recordlayer",
			filename: "/b/f/w/execroot/_main/pkg/recordlayer/store.go",
			src:      srcImporting("recordlayer", "fdb.dev/pkg/dst"),
			want:     0,
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
		{
			name:     "external dependency file skipped",
			pkgPath:  "fdb.dev/pkg/recordlayer",
			filename: "external/some_repo/foo.go",
			src:      srcImporting("recordlayer", "fdb.dev/pkg/relational/api"),
			want:     0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := len(analyze(t, tc.pkgPath, tc.filename, tc.src)); got != tc.want {
				t.Fatalf("diagnostics = %d, want %d", got, tc.want)
			}
		})
	}
}
