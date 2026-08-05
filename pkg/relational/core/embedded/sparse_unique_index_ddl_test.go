package embedded

import (
	"testing"
)

// RFC-210 §5.1 clause 4 refuses a SPARSE (WHERE-filtered) UNIQUE index as an
// elision proof, and that clause is the one whose failure is WRONG ROWS rather
// than a missed optimization: a sparse index omits every record its stored
// predicate rejects, so its UNIQUE declaration constrains only the rows the
// predicate ADMITS and says nothing whatever about the ones it excludes — which
// may hold arbitrarily many duplicates of an admitted value.
//
// §7.2 requires that row end-to-end with real data, and flags a prerequisite
// that has to be settled before the e2e can be written at all: UNIQUE and a
// stored predicate had never been combined anywhere in this repo — not in a
// test, not in a yamsql scenario. So the first question is whether the DDL even
// ACCEPTS the combination. If it does not, clause 4 is unreachable by
// construction and the e2e must be replaced by a pin on THAT fact rather than
// quietly weakened.
//
// This is that smoke check, and it settles the question in the direction that
// keeps clause 4 reachable: the combination is accepted, and the resulting index
// carries BOTH unique=true and a non-nil predicate. The e2e with real duplicates
// outside the predicate therefore exists (sqldriver's sparse-unique test) rather
// than being substituted for.
//
// It is kept as its own test rather than folded into the e2e because the two
// fail for different reasons and want different failure messages: this one says
// "the DDL surface changed underneath clause 4", the e2e says "clause 4 stopped
// refusing".
func TestSparseUniqueIndexDDL_AcceptsUniqueWithAStoredPredicate(t *testing.T) {
	t.Parallel()

	const ddl = "CREATE TABLE SP (ID BIGINT, EMAIL STRING, KEEP BIGINT, PRIMARY KEY (ID))\n" +
		"CREATE UNIQUE INDEX SPARSE_U AS SELECT EMAIL FROM SP WHERE KEEP > 0 ORDER BY EMAIL"

	tmpl, err := buildSchemaTemplateFromDDL(ddl)
	if err != nil {
		t.Fatalf("the DDL REJECTED a UNIQUE index carrying a stored predicate: %v\n"+
			"That inverts RFC-210 §7.2's sparse row. Clause 4 becomes unreachable by "+
			"construction, the end-to-end test that exercises it is testing nothing, "+
			"and what belongs here instead is a pin on the rejection — with the "+
			"reachability re-armed the moment the DDL accepts it again. Do not "+
			"weaken the e2e to match; change which test exists.", err)
	}

	var found bool
	for _, idx := range tmpl.Underlying().GetAllIndexes() {
		if idx.Name != "SPARSE_U" {
			continue
		}
		found = true
		if !idx.IsUnique() {
			t.Error("SPARSE_U was built WITHOUT unique=true. The UNIQUE keyword was " +
				"accepted by the parser and dropped by the generator, so clause 4 " +
				"never sees a unique candidate and the shape it refuses cannot occur")
		}
		if idx.GetPredicateProto() == nil {
			t.Error("SPARSE_U was built with NO stored predicate. The WHERE was " +
				"dropped, which makes this an ordinary FULL unique index — and a full " +
				"unique index is a shape clause 4 correctly ADMITS, so an e2e built on " +
				"this DDL would assert the opposite of what it claims while passing")
		}
	}
	if !found {
		t.Fatal("no index named SPARSE_U was built at all")
	}
}
