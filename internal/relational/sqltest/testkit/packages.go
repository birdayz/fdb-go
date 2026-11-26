package testkit

import (
	"fmt"
	"os"
	"slices"
	"strings"
)

// Packages are the packages of the end-to-end suite. Each is a member of the
// whole-corpus census (//tests/sql:corpus); the sqltest guard holds
// the two lists equal, and Main refuses to run a package missing here, so a new
// package cannot silently drop out of the census.
var Packages = []string{
	"tests/sql/agg",
	"tests/sql/agg/distinctelision",
	"tests/sql/catalog",
	"tests/sql/core",
	"tests/sql/cte",
	"tests/sql/dml",
	"tests/sql/join",
	"tests/sql/metamorphic",
	"tests/sql/metamorphic/aggregates",
	"tests/sql/metamorphic/commute",
	"tests/sql/metamorphic/compositepk",
	"tests/sql/metamorphic/direction",
	"tests/sql/metamorphic/exprequiv",
	"tests/sql/metamorphic/joins",
	"tests/sql/metamorphic/minmaxnulls",
	"tests/sql/metamorphic/norec",
	"tests/sql/metamorphic/paging",
	"tests/sql/metamorphic/parens",
	"tests/sql/metamorphic/predicates",
	"tests/sql/metamorphic/rewrites",
	"tests/sql/metamorphic/rowdiff",
	"tests/sql/metamorphic/shapeequiv",
	"tests/sql/otel",
	"tests/sql/plan",
	"tests/sql/plan/caphit",
	"tests/sql/plan/options",
	"tests/sql/planshape",
	"tests/sql/select",
	"tests/sql/subquery",
	"tests/sql/types",
}

// driverTarget shares Main but is not part of the corpus.
const driverTarget = "//pkg/relational/sqldriver:sqldriver_test"

func checkRegistered() {
	t := os.Getenv("TEST_TARGET")
	if t == "" || t == driverTarget {
		return
	}
	pkg := strings.TrimPrefix(strings.SplitN(t, ":", 2)[0], "//")
	if !slices.Contains(Packages, pkg) {
		fmt.Fprintf(os.Stderr, "%s is not registered: add %q to testkit.Packages and "+
			"its :corpus filegroup to //tests/sql:corpus\n", t, pkg)
		os.Exit(1)
	}
}
