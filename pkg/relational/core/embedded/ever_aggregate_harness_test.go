package embedded

import (
	"strings"
	"testing"
)

// MIN_EVER and MAX_EVER over one column, repeated or mixed, intersect their
// aggregate indexes; the output names must not collide.
func TestPlanHarness_EverAggregatesIntersect(t *testing.T) {
	t.Parallel()
	ddl := `CREATE SCHEMA TEMPLATE x CREATE TABLE c(id bigint, country string, age integer, primary key(id))
		create index mn as select min_ever(age) from c group by country
		create index mx as select max_ever(age) from c group by country
		create index mn2 as select min_ever(age) from c group by country with attributes legacy_extremum_ever`
	for _, q := range []string{
		"SELECT country, min_ever(age), max_ever(age) FROM c WHERE country = 'USA' GROUP BY country",
		"SELECT country, min_ever(age), max_ever(age), min_ever(age) FROM c WHERE country = 'USA' GROUP BY country",
	} {
		plan, err := PlanQueryForTest(q, ddl, nil)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if !strings.Contains(plan, "MultiIntersection(AggregateIndex(MIN_EVER, MN") || !strings.Contains(plan, "AggregateIndex(MAX_EVER, MX") {
			t.Errorf("%s: %s", q, plan)
		}
	}
}

// (*) and (t.*) pack the star's columns into one record.
func TestPlanHarness_StarRecordConstructor(t *testing.T) {
	t.Parallel()
	ddl := `CREATE SCHEMA TEMPLATE x CREATE TABLE T3 ("id" BIGINT, "integers" INTEGER ARRAY, PRIMARY KEY ("id"))`
	for q, want := range map[string]string{
		`SELECT "id", SQ.* FROM T3, (SELECT (*) AS "w" FROM T3."integers") AS SQ`: "FlatMap(outer=Scan(T3), inner=Explode(field)) => {id: T3.id#0, w: {integers: Q$BOUND1}}",
		`SELECT (T3.*) AS w FROM T3`: "Map(Scan(T3), {W: {id: _current.id#0, integers: _current.integers#1}})",
	} {
		if got := explainWithResult(t, q, ddl); !strings.HasPrefix(got, want) {
			t.Errorf("%s: %s", q, got)
		}
	}
}
