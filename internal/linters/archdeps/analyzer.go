// Package archdeps is a nogo analyzer that enforces the repository's layered
// dependency DAG (R1-R5) across pkg/, internal/, and future top-level packages.
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
	Doc:  "enforces layered architecture import boundaries (R1-R5)",
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
	plannerRoots = []string{
		"fdb.dev/pkg/recordlayer/query/plan",
		"fdb.dev/internal/recordlayer/query/plan",
		"fdb.dev/recordlayer/query/plan",
	}
	fdbClientRoots = []string{
		"fdb.dev/pkg/fdbgo",
		"fdb.dev/internal/fdbgo",
		"fdb.dev/internal/fdbwire",
		"fdb.dev/internal/fdbclient",
		"fdb.dev/pkg/internal/fdbclient",
		"fdb.dev/fdb",
	}
	coreRuntimeRoots = []string{
		"fdb.dev/pkg/fdbgo/fdb",
		"fdb.dev/pkg/fdbgo/client",
		"fdb.dev/pkg/fdbgo/transport",
		"fdb.dev/pkg/fdbgo/wire",
		"fdb.dev/pkg/fdbgo/fdbmetrics",
		"fdb.dev/internal/fdbgo/fdb",
		"fdb.dev/internal/fdbgo/client",
		"fdb.dev/internal/fdbgo/transport",
		"fdb.dev/internal/fdbgo/wire",
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
	}
	r5TestFrameworkRoots = []string{
		"github.com/stretchr/testify",
		"github.com/onsi/ginkgo",
		"github.com/onsi/gomega",
		"go.uber.org/mock",
	}
	repoTopDirs = []string{
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
		"tools/",
	}
)

// r2AllowedLeaves lists pure, non-storage leaf packages under pkg/recordlayer
// intentionally extracted so the planner and record layer can share them.
var r2AllowedLeaves = map[string]bool{
	"fdb.dev/pkg/recordlayer/indexfetch":          true,
	"fdb.dev/pkg/recordlayer/indexpredicate":      true,
	"fdb.dev/pkg/recordlayer/internal/protovalue": true,
	"fdb.dev/pkg/recordlayer/javanum":             true,
	"fdb.dev/pkg/recordlayer/protoname":           true,
	"fdb.dev/pkg/recordlayer/protoscope":          true,
	"fdb.dev/pkg/recordlayer/tupleordering":       true,
	"fdb.dev/pkg/recordlayer/vectorcodec":         true,
}

// Baseline debt allowlists: every entry is an existing dependency to be
// retired rather than expanded. Keys are repo-relative file paths.
var (
	r1Allow = map[string]map[string]bool{
		"pkg/recordlayer/query/executor/executor_test.go":            {"fdb.dev/pkg/relational/core/functions": true},
		"pkg/recordlayer/query/executor/read_path_agreement_test.go": {"fdb.dev/pkg/relational/core/rowstruct": true},
	}
	r2Allow = map[string]map[string]bool{
		"pkg/recordlayer/query/plan/cascades/aggregate_index_entry_reader_test.go":    {"fdb.dev/pkg/fdbgo/fdb/tuple": true},
		"pkg/recordlayer/query/plan/cascades/predicates/text_comparison.go":           {"fdb.dev/pkg/recordlayer": true},
		"pkg/recordlayer/query/plan/cascades/predicates/text_comparison_test.go":      {"fdb.dev/pkg/recordlayer": true},
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
		"pkg/fdbgo/libfdbc/differential_test.go": {"fdb.dev/pkg/recordlayer": true},
	}
	r4Allow = map[string]map[string]bool{
		"pkg/recordlayer/attempt_loop.go":                    {"fdb.dev/pkg/dst": true},
		"pkg/recordlayer/database.go":                        {"fdb.dev/pkg/dst": true},
		"pkg/recordlayer/index_maintainer.go":                {"fdb.dev/pkg/dst": true},
		"pkg/recordlayer/indexing_heartbeat.go":              {"fdb.dev/pkg/dst": true},
		"pkg/recordlayer/indexing_mutual.go":                 {"fdb.dev/pkg/dst": true},
		"pkg/recordlayer/online_indexer.go":                  {"fdb.dev/pkg/dst": true},
		"pkg/recordlayer/ranked_set.go":                      {"fdb.dev/pkg/dst": true},
		"pkg/recordlayer/retry_predicate.go":                 {"fdb.dev/pkg/dst": true},
		"pkg/recordlayer/scan_limiter_state.go":              {"fdb.dev/pkg/dst": true},
		"pkg/recordlayer/scan_properties.go":                 {"fdb.dev/pkg/dst": true},
		"pkg/recordlayer/store.go":                           {"fdb.dev/pkg/dst": true},
		"pkg/recordlayer/store_builder.go":                   {"fdb.dev/pkg/dst": true},
		"pkg/recordlayer/transformed_record_serializer.go":   {"fdb.dev/pkg/dst": true},
		"pkg/recordlayer/vectorindex/guardiann_storage.go":   {"fdb.dev/pkg/dst": true},
		"pkg/recordlayer/vectorindex/guardiann_types.go":     {"fdb.dev/pkg/dst": true},
		"pkg/recordlayer/vectorindex/hnsw.go":                {"fdb.dev/pkg/dst": true},
		"pkg/recordlayer/vectorindex/spfresh_rebalancer.go":  {"fdb.dev/pkg/dst": true},
		"pkg/relational/core/embedded/cascades_generator.go": {"fdb.dev/pkg/dst": true},
		"pkg/relational/core/embedded/connection.go":         {"fdb.dev/pkg/dst": true},
		"pkg/relational/core/session/session.go":             {"fdb.dev/pkg/dst": true},
	}
	// r5Allow freezes the exact set of normalized packages allowed to import
	// testify, ginkgo, gomega, or gomock (sorted alphabetically).
	r5Allow = map[string]bool{
		"fdb.dev/cmd/bench-report":                           true,
		"fdb.dev/conformance":                                true,
		"fdb.dev/pkg/async/rtree":                            true,
		"fdb.dev/pkg/fdbgo/client":                           true,
		"fdb.dev/pkg/fdbgo/fdb":                              true,
		"fdb.dev/pkg/fdbgo/fdb/subspace":                     true,
		"fdb.dev/pkg/fdbgo/fdb/tuple":                        true,
		"fdb.dev/pkg/fdbgo/transport":                        true,
		"fdb.dev/pkg/recordlayer":                            true,
		"fdb.dev/pkg/recordlayer/keyspace":                   true,
		"fdb.dev/pkg/recordlayer/query/executor":             true,
		"fdb.dev/pkg/recordlayer/query/plan/cascades/values": true,
		"fdb.dev/pkg/recordlayer/vectorindex":                true,
		"fdb.dev/pkg/relational/core/catalog":                true,
		"fdb.dev/pkg/relational/core/ddl":                    true,
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
		if withinAny(imp, plannerRoots) || r2AllowedLeaves[imp] {
			return false
		}
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
