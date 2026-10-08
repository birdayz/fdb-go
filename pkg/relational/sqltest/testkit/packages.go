package testkit

import (
	"fmt"
	"os"
	"slices"
	"strings"
)

// Packages are the packages of the end-to-end suite. Each is a member of the
// whole-corpus census (//pkg/relational/sqltest:corpus); the sqltest guard holds
// the two lists equal, and Main refuses to run a package missing here, so a new
// package cannot silently drop out of the census.
var Packages = []string{
	"pkg/relational/sqltest/agg",
	"pkg/relational/sqltest/agg/distinctelision",
	"pkg/relational/sqltest/catalog",
	"pkg/relational/sqltest/core",
	"pkg/relational/sqltest/cte",
	"pkg/relational/sqltest/dml",
	"pkg/relational/sqltest/join",
	"pkg/relational/sqltest/metamorphic",
	"pkg/relational/sqltest/metamorphic/aggregates",
	"pkg/relational/sqltest/metamorphic/commute",
	"pkg/relational/sqltest/metamorphic/compositepk",
	"pkg/relational/sqltest/metamorphic/direction",
	"pkg/relational/sqltest/metamorphic/exprequiv",
	"pkg/relational/sqltest/metamorphic/joins",
	"pkg/relational/sqltest/metamorphic/minmaxnulls",
	"pkg/relational/sqltest/metamorphic/norec",
	"pkg/relational/sqltest/metamorphic/paging",
	"pkg/relational/sqltest/metamorphic/parens",
	"pkg/relational/sqltest/metamorphic/predicates",
	"pkg/relational/sqltest/metamorphic/rewrites",
	"pkg/relational/sqltest/metamorphic/rowdiff",
	"pkg/relational/sqltest/metamorphic/shapeequiv",
	"pkg/relational/sqltest/plan",
	"pkg/relational/sqltest/plan/caphit",
	"pkg/relational/sqltest/plan/options",
	"pkg/relational/sqltest/planshape",
	"pkg/relational/sqltest/select",
	"pkg/relational/sqltest/subquery",
	"pkg/relational/sqltest/types",
}

// CensusTarget compiles every package of Packages into one binary; only there do
// the census floors (claims about the whole corpus) hold.
const CensusTarget = "//pkg/relational/sqltest/census:census_test"

// driverTarget shares Main but is not part of the corpus.
const driverTarget = "//pkg/relational/sqldriver:sqldriver_test"

func checkRegistered() {
	t := os.Getenv("TEST_TARGET")
	if t == "" || t == CensusTarget || t == driverTarget {
		return
	}
	pkg := strings.TrimPrefix(strings.SplitN(t, ":", 2)[0], "//")
	if !slices.Contains(Packages, pkg) {
		fmt.Fprintf(os.Stderr, "%s is not registered: add %q to testkit.Packages and "+
			"its :corpus filegroup to //pkg/relational/sqltest:corpus\n", t, pkg)
		os.Exit(1)
	}
}
