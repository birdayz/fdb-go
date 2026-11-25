// Package archdeps is a nogo analyzer that enforces the repository's layered
// dependency DAG (L0-L7) and architectural import rules (R1-R6) across pkg/,
// internal/, and future top-level packages.
//
// Layer hierarchy (L0-L7, lower layers must not depend on higher layers):
//   - L0 (r2AllowedLeaves): pure non-storage leaf primitives shared between
//     storage and planner (indexfetch, indexpredicate, javanum, protoname,
//     protoscope, protovalue, tupleordering, vectorcodec).
//   - L1/L2 (fdbClientRoots): FDB client, wire, transport, and low-level
//     libraries (pkg/fdbgo, internal/fdbgo, internal/fdbwire,
//     internal/fdbclient, fdb).
//   - L3: Record Layer storage (recordLayerRoots excluding L0 leaves, L4
//     plannerRoots, and L5 executorRoots).
//   - L4 (plannerRoots): Record Layer pure query planner (recordlayer/query/plan).
//   - L5 (executorRoots): Record Layer query executor (recordlayer/query/executor).
//   - L6 (relationalCoreAPIRoots): Relational engine core and API
//     (relational/api, relational/core).
//   - L7 (sqlDriverRoots): Relational database/sql driver (relational/sqldriver).
//
// Enforced rules:
//   - R1 (Record Layer _|_ Relational): all files (prod and _test.go) under
//     recordLayerRoots must not import relationalRoots (baseline: r1Allow).
//   - R2 (Pure Planner _|_ Storage & FDB): all files (prod and _test.go) under
//     plannerRoots (L4) must not import fdbClientRoots (L1/L2) or
//     recordLayerRoots outside plannerRoots and r2AllowedLeaves, blocking L4->L3
//     and L4->L5 edges (baseline: r2Allow).
//   - R3 (FDB Client _|_ Upper Layers): all files (prod and _test.go) under
//     fdbClientRoots (L1/L2) must not import recordLayerRoots or relationalRoots
//     (baseline: r3Allow).
//   - R4 (Core Runtime Prod _|_ Test/Harness/Sim/Linter): production files
//     (!isTest) under coreRuntimeRoots (minus coreRuntimeExemptions: chaos,
//     keystoretest) must not import r4ForbiddenRoots (baseline: r4Allow).
//   - R5 (Test-Framework Sprawl Freeze): all files under fdb.dev outside the
//     frozen package set r5Allow must not import r5TestFrameworkRoots (testify,
//     ginkgo, gomega, gomock).
//   - R6 (FDB Client Prod _|_ OpenTelemetry): production files (!isTest) under
//     fdbClientRoots must not import go.opentelemetry.io/otel or subpackages
//     (baseline: r6Allow).
//   - L0 (L0 Leaves _|_ Upward Layers): all files (prod and _test.go) under
//     r2AllowedLeaves (L0) must not import upward into storage (recordLayerRoots
//     excluding r2AllowedLeaves), planner (L4), executor (L5), relational
//     (L6/L7), or fdbClientRoots (L1/L2) (baseline: l0Allow).
//   - L3 (L3 Storage _|_ L4/L5 Planner & Executor in Prod): production files
//     (!isTest) in L3 must not import plannerRoots (L4) or executorRoots (L5)
//     (empty baseline: l3Allow).
//   - L6 (L6 Relational Core/API _|_ L7 SQL Driver in Prod): production files
//     (!isTest) under relationalCoreAPIRoots (L6) must not import sqlDriverRoots
//     (L7) (empty baseline: l6Allow).
package archdeps

import (
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// Analyzer is the nogo entry point.
var Analyzer = &analysis.Analyzer{
	Name: "archdeps",
	Doc:  "enforces layered architecture import boundaries (R1-R6, L0-L7)",
	Run:  run,
}

var (
	recordLayerRoots = []string{
		"fdb.dev/pkg/recordlayer",
		"fdb.dev/internal/recordlayer",
		"fdb.dev/recordlayer",
	}
	relationalRoots = []string{
		"fdb.dev/pkg/relational",
		"fdb.dev/internal/relational",
		"fdb.dev/relational",
	}
	relationalCoreAPIRoots = []string{
		"fdb.dev/pkg/relational/api",
		"fdb.dev/pkg/relational/core",
		"fdb.dev/internal/relational/api",
		"fdb.dev/internal/relational/core",
		"fdb.dev/internal/relational/relapi",
		"fdb.dev/internal/relational",
		"fdb.dev/relational/api",
		"fdb.dev/relational/core",
	}
	sqlDriverRoots = []string{
		"fdb.dev/pkg/relational/sqldriver",
		"fdb.dev/internal/relational/sqldriver",
		"fdb.dev/relational/sqldriver",
	}
	plannerRoots = []string{
		"fdb.dev/pkg/recordlayer/query/plan",
		"fdb.dev/internal/recordlayer/query/plan",
		"fdb.dev/recordlayer/query/plan",
	}
	executorRoots = []string{
		"fdb.dev/pkg/recordlayer/query/executor",
		"fdb.dev/internal/recordlayer/query/executor",
		"fdb.dev/recordlayer/query/executor",
	}
	fdbClientRoots = []string{
		"fdb.dev/pkg/fdbgo",
		"fdb.dev/internal/fdbgo",
		"fdb.dev/internal/fdbwire",
		"fdb.dev/internal/fdbclient",
		"fdb.dev/fdb",
	}
	coreRuntimeRoots = []string{
		"fdb.dev/pkg/fdbgo/fdb",
		"fdb.dev/pkg/fdbgo/fdbmetrics",
		"fdb.dev/internal/fdbgo/fdb",
		"fdb.dev/internal/fdbgo/client",
		"fdb.dev/internal/fdbgo/transport",
		"fdb.dev/internal/fdbgo/diag",
		"fdb.dev/internal/fdbgo/hrtimer",
		"fdb.dev/internal/fdbgo/sendqueue",
		"fdb.dev/internal/fdbwire",
		"fdb.dev/internal/fdbclient",
		"fdb.dev/internal/fdbgo/fdbmetrics",
		"fdb.dev/fdb/fdb",
		"fdb.dev/fdb/client",
		"fdb.dev/fdb/transport",
		"fdb.dev/fdb/wire",
		"fdb.dev/fdb/fdbmetrics",
		"fdb.dev/pkg/recordlayer",
		"fdb.dev/internal/recordlayer",
		"fdb.dev/recordlayer",
		"fdb.dev/pkg/relational/api",
		"fdb.dev/pkg/relational/core",
		"fdb.dev/pkg/relational/sqldriver",
		"fdb.dev/internal/relational/api",
		"fdb.dev/internal/relational/core",
		"fdb.dev/internal/relational/relapi",
		"fdb.dev/internal/relational",
		"fdb.dev/internal/relational/sqldriver",
		"fdb.dev/relational/api",
		"fdb.dev/relational/core",
		"fdb.dev/relational/sqldriver",
	}
	coreRuntimeExemptions = []string{
		"fdb.dev/pkg/recordlayer/chaos",
		"fdb.dev/pkg/recordlayer/keystoretest",
		"fdb.dev/internal/recordlayer/chaos",
		"fdb.dev/internal/recordlayer/keystoretest",
		"fdb.dev/recordlayer/chaos",
		"fdb.dev/recordlayer/keystoretest",
		"fdb.dev/internal/relational/simgate",
	}
	r4ForbiddenRoots = []string{
		"fdb.dev/pkg/testcontainers",
		"github.com/docker/docker",
		"github.com/onsi/ginkgo",
		"github.com/onsi/gomega",
		"github.com/stretchr/testify",
		"go.uber.org/mock",
		"fdb.dev/pkg/simfdb",
		"fdb.dev/internal/simfdb",
		"fdb.dev/internal/linters",
		"fdb.dev/pkg/dst",
		"fdb.dev/internal/dst",
	}
	r5TestFrameworkRoots = []string{
		"github.com/stretchr/testify",
		"github.com/onsi/ginkgo",
		"github.com/onsi/gomega",
		"go.uber.org/mock",
	}
	otelRoot = "go.opentelemetry.io/otel"
	// r2AllowedLeaves lists pure, non-storage L0 leaf packages under
	// recordLayerRoots intentionally extracted so the planner and record layer
	// can share them (covering pkg/, internal/, and top-level paths).
	r2AllowedLeaves = []string{
		"fdb.dev/pkg/recordlayer/indexfetch",
		"fdb.dev/pkg/recordlayer/indexpredicate",
		"fdb.dev/pkg/recordlayer/internal/protovalue",
		"fdb.dev/pkg/recordlayer/javanum",
		"fdb.dev/pkg/recordlayer/protoname",
		"fdb.dev/pkg/recordlayer/protoscope",
		"fdb.dev/pkg/recordlayer/tupleordering",
		"fdb.dev/pkg/recordlayer/metadatapb",
		"fdb.dev/pkg/recordlayer/vectorcodec",
		"fdb.dev/internal/recordlayer/indexfetch",
		"fdb.dev/internal/recordlayer/indexpredicate",
		"fdb.dev/internal/recordlayer/javanum",
		"fdb.dev/internal/recordlayer/protoname",
		"fdb.dev/internal/recordlayer/protoscope",
		"fdb.dev/internal/recordlayer/protovalue",
		"fdb.dev/internal/recordlayer/textcmp",
		"fdb.dev/internal/recordlayer/tupleordering",
		"fdb.dev/internal/recordlayer/vectorcodec",
		"fdb.dev/recordlayer/indexfetch",
		"fdb.dev/recordlayer/indexpredicate",
		"fdb.dev/recordlayer/internal/protovalue",
		"fdb.dev/recordlayer/javanum",
		"fdb.dev/recordlayer/protoname",
		"fdb.dev/recordlayer/protoscope",
		"fdb.dev/recordlayer/tupleordering",
		"fdb.dev/recordlayer/metadatapb",
		"fdb.dev/recordlayer/vectorcodec",
	}
	repoTopDirs = []string{
		"bench/",
		"cmd/",
		"conformance/",
		"example/",
		"fdb/",
		"gen/",
		"infra/",
		"internal/",
		"layers/",
		"pkg/",
		"recordlayer/",
		"relational/",
		"tests/",
		"tools/",
	}
)

// Baseline debt allowlists: every entry is an existing dependency to be
// retired rather than expanded. Keys are repo-relative file paths.
var (
	r1Allow = map[string]map[string]bool{}
	r2Allow = map[string]map[string]bool{
		"pkg/recordlayer/query/plan/cascades/aggregate_index_entry_reader_test.go":    {"fdb.dev/pkg/fdbgo/fdb/tuple": true},
		"pkg/recordlayer/query/plan/cascades/values/index_entry_extraction_test.go":   {"fdb.dev/pkg/fdbgo/fdb/tuple": true},
		"pkg/recordlayer/query/plan/cascades/values/ordering_claim_test.go":           {"fdb.dev/pkg/fdbgo/fdb/tuple": true},
		"pkg/recordlayer/query/plan/cascades/values/value_index_entry_object.go":      {"fdb.dev/pkg/fdbgo/fdb/tuple": true},
		"pkg/recordlayer/query/plan/cascades/values/value_index_entry_object_test.go": {"fdb.dev/pkg/fdbgo/fdb/tuple": true},
		"pkg/recordlayer/query/plan/cascades/values/value_ordered_bytes.go":           {"fdb.dev/pkg/fdbgo/fdb/tuple": true},
		"pkg/recordlayer/query/plan/cascades/values/value_ordered_bytes_test.go":      {"fdb.dev/pkg/fdbgo/fdb/tuple": true},
		"pkg/recordlayer/query/plan/plans/load_by_keys.go":                            {"fdb.dev/pkg/fdbgo/fdb/tuple": true},
		"pkg/recordlayer/query/plan/plans/new_plans_test.go":                          {"fdb.dev/pkg/fdbgo/fdb/tuple": true},
	}
	r3Allow = map[string]map[string]bool{
		"internal/fdbclient/libfdbc/differential_test.go": {"fdb.dev/pkg/recordlayer": true},
	}
	r4Allow = map[string]map[string]bool{}
	// r5Allow freezes the exact set of normalized packages allowed to import
	// testify, ginkgo, gomega, or gomock (sorted alphabetically).
	r5Allow = map[string]bool{
		"fdb.dev/cmd/bench-report":                           true,
		"fdb.dev/conformance":                                true,
		"fdb.dev/internal/fdbgo/client":                      true,
		"fdb.dev/internal/fdbgo/transport":                   true,
		"fdb.dev/internal/recordlayer/query/executor":        true,
		"fdb.dev/internal/recordlayer/rtree":                 true,
		"fdb.dev/internal/relational/catalog":                true,
		"fdb.dev/internal/relational/ddl":                    true,
		"fdb.dev/pkg/fdbgo/fdb":                              true,
		"fdb.dev/pkg/fdbgo/fdb/subspace":                     true,
		"fdb.dev/pkg/fdbgo/fdb/tuple":                        true,
		"fdb.dev/pkg/recordlayer":                            true,
		"fdb.dev/pkg/recordlayer/keyspace":                   true,
		"fdb.dev/pkg/recordlayer/query/plan/cascades/values": true,
		"fdb.dev/pkg/recordlayer/vectorindex":                true,
		"fdb.dev/pkg/relational/core/query/expr":             true,
		"fdb.dev/pkg/relational/sqltest/agg":                 true,
		"fdb.dev/pkg/relational/sqltest/core":                true,
		"fdb.dev/pkg/relational/sqltest/cte":                 true,
		"fdb.dev/pkg/relational/sqltest/join":                true,
		"fdb.dev/pkg/relational/sqltest/select":              true,
		"fdb.dev/pkg/relational/sqltest/subquery":            true,
		"fdb.dev/pkg/relational/sqltest/types":               true,
		"fdb.dev/pkg/testcontainers/foundationdb":            true,
	}
	// r6Allow freezes the 5 existing fdbClientRoots production files that
	// import go.opentelemetry.io/otel/trace.
	r6Allow = map[string]map[string]bool{
		"internal/fdbgo/client/database.go":    {"go.opentelemetry.io/otel/trace": true},
		"internal/fdbgo/client/options.go":     {"go.opentelemetry.io/otel/trace": true},
		"internal/fdbgo/client/span.go":        {"go.opentelemetry.io/otel/trace": true},
		"internal/fdbgo/client/transaction.go": {"go.opentelemetry.io/otel/trace": true},
		"pkg/fdbgo/fdb/database.go":            {"go.opentelemetry.io/otel/trace": true},
	}
	// l0Allow freezes the single existing upward dependency from an L0 leaf.
	l0Allow = map[string]map[string]bool{
		"internal/recordlayer/tupleordering/tupleordering.go": {"fdb.dev/pkg/fdbgo/fdb/tuple": true},
	}
	l3Allow = map[string]map[string]bool{}
	l6Allow = map[string]map[string]bool{}
)

func run(pass *analysis.Pass) (any, error) {
	if pass.Pkg == nil {
		return nil, nil
	}
	pkgPath := normalizePkgPath(pass.Pkg.Path())
	if !within(pkgPath, "fdb.dev") {
		return nil, nil
	}

	inR1 := withinAny(pkgPath, recordLayerRoots)
	inR2 := withinAny(pkgPath, plannerRoots)
	inR3 := withinAny(pkgPath, fdbClientRoots)
	inR4 := withinAny(pkgPath, coreRuntimeRoots) && !withinAny(pkgPath, coreRuntimeExemptions)
	inL0 := withinAny(pkgPath, r2AllowedLeaves)
	inL3 := inR1 && !inL0 && !inR2 && !withinAny(pkgPath, executorRoots)
	inL6 := withinAny(pkgPath, relationalCoreAPIRoots) && !withinAny(pkgPath, sqlDriverRoots)

	for _, file := range pass.Files {
		filename := filepath.ToSlash(pass.Fset.Position(file.Package).Filename)
		if strings.HasPrefix(filename, "external/") || strings.Contains(filename, "/external/") {
			continue
		}
		isTest := strings.HasSuffix(filename, "_test.go")

		for _, spec := range file.Imports {
			imp, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				continue
			}

			if inR1 && withinAny(imp, relationalRoots) && !isFileImportAllowed(r1Allow, filename, imp) {
				pass.Report(analysis.Diagnostic{
					Pos:     spec.Pos(),
					End:     spec.End(),
					Message: "archdeps (R1): record layer package " + pkgPath + " must not import relational package " + imp,
				})
			}

			if inR2 && isR2Forbidden(imp) && !isFileImportAllowed(r2Allow, filename, imp) {
				pass.Report(analysis.Diagnostic{
					Pos:     spec.Pos(),
					End:     spec.End(),
					Message: "archdeps (R2): pure planner package " + pkgPath + " must not import storage/FDB package " + imp,
				})
			}

			if inR3 && (withinAny(imp, recordLayerRoots) || withinAny(imp, relationalRoots)) && !isFileImportAllowed(r3Allow, filename, imp) {
				pass.Report(analysis.Diagnostic{
					Pos:     spec.Pos(),
					End:     spec.End(),
					Message: "archdeps (R3): FDB client package " + pkgPath + " must not import upper-layer package " + imp,
				})
			}

			if inR4 && !isTest && withinAny(imp, r4ForbiddenRoots) && !isFileImportAllowed(r4Allow, filename, imp) {
				pass.Report(analysis.Diagnostic{
					Pos:     spec.Pos(),
					End:     spec.End(),
					Message: "archdeps (R4): core production package " + pkgPath + " must not import test/harness/sim/linter package " + imp,
				})
			}

			if withinAny(imp, r5TestFrameworkRoots) && !r5Allow[pkgPath] {
				pass.Report(analysis.Diagnostic{
					Pos:     spec.Pos(),
					End:     spec.End(),
					Message: "archdeps (R5): package " + pkgPath + " is not allowlisted to import test framework " + imp + " (use stdlib testing)",
				})
			}

			if inR3 && !isTest && within(imp, otelRoot) && !isFileImportAllowed(r6Allow, filename, imp) {
				pass.Report(analysis.Diagnostic{
					Pos:     spec.Pos(),
					End:     spec.End(),
					Message: "archdeps (R6): FDB client production package " + pkgPath + " must not import OpenTelemetry package " + imp,
				})
			}

			if inL0 && isL0Forbidden(imp) && !isFileImportAllowed(l0Allow, filename, imp) {
				pass.Report(analysis.Diagnostic{
					Pos:     spec.Pos(),
					End:     spec.End(),
					Message: "archdeps (L0): leaf package " + pkgPath + " must not import upward package " + imp,
				})
			}

			if inL3 && !isTest && (withinAny(imp, plannerRoots) || withinAny(imp, executorRoots)) && !isFileImportAllowed(l3Allow, filename, imp) {
				pass.Report(analysis.Diagnostic{
					Pos:     spec.Pos(),
					End:     spec.End(),
					Message: "archdeps (L3): storage production package " + pkgPath + " must not import planner/executor package " + imp,
				})
			}

			if inL6 && !isTest && withinAny(imp, sqlDriverRoots) && !isFileImportAllowed(l6Allow, filename, imp) {
				pass.Report(analysis.Diagnostic{
					Pos:     spec.Pos(),
					End:     spec.End(),
					Message: "archdeps (L6): relational core/api production package " + pkgPath + " must not import sqldriver package " + imp,
				})
			}
		}
	}
	return nil, nil
}

// normalizePkgPath strips external _test suffixes and maps unembedded Bazel
// go_test package paths (e.g. pkg/relational/sqltest/agg/agg_test) to fdb.dev/...
func normalizePkgPath(pkgPath string) string {
	if within(pkgPath, "fdb.dev") {
		return strings.TrimSuffix(pkgPath, "_test")
	}
	for _, prefix := range repoTopDirs {
		if strings.HasPrefix(pkgPath, prefix) {
			dir := path.Dir(pkgPath)
			if dir == "." {
				return "fdb.dev"
			}
			return "fdb.dev/" + dir
		}
	}
	return strings.TrimSuffix(pkgPath, "_test")
}

func isR2Forbidden(imp string) bool {
	if withinAny(imp, fdbClientRoots) {
		return true
	}
	if withinAny(imp, recordLayerRoots) {
		if withinAny(imp, plannerRoots) || withinAny(imp, r2AllowedLeaves) {
			return false
		}
		return true
	}
	return false
}

func isL0Forbidden(imp string) bool {
	if withinAny(imp, fdbClientRoots) || withinAny(imp, relationalRoots) {
		return true
	}
	if withinAny(imp, recordLayerRoots) && !withinAny(imp, r2AllowedLeaves) {
		return true
	}
	return false
}

func within(p, root string) bool {
	return p == root || strings.HasPrefix(p, root+"/")
}

func withinAny(p string, roots []string) bool {
	for _, root := range roots {
		if within(p, root) {
			return true
		}
	}
	return false
}

func hasPathSuffix(slashFilename, repoRel string) bool {
	return slashFilename == repoRel || strings.HasSuffix(slashFilename, "/"+repoRel)
}

func isFileImportAllowed(allow map[string]map[string]bool, slashFilename, imp string) bool {
	for relPath, imps := range allow {
		if hasPathSuffix(slashFilename, relPath) && imps[imp] {
			return true
		}
	}
	return false
}
