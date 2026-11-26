// Package coveringleaf holds the schema and queries shared by the two tests
// that probe a COVERING index leaf from opposite sides — the planner-side shape
// pin in package embedded_test and the column-metadata pin in package
// sqldriver_test.
//
// It exists because those two tests are one test cut in half by a package
// boundary. The metadata test asserts that a projection over a covering leaf
// reports the right column TYPES; that assertion is only meaningful while the
// queries actually plan with the projection sitting directly over a covering
// scan, which is what the shape pin checks. They therefore have to agree on the
// schema and on the queries, exactly — and until this package they agreed by
// two hand-maintained copies in two `_test.go` files that cannot import each
// other.
//
// Two copies of a fixture do not stay equal; they stay equal until someone
// edits one. The failure when they diverge is the silent kind: the metadata
// test keeps passing against a schema whose queries no longer plan as covering,
// so it reports green while testing nothing it claims to test. That is the same
// evaporating-coverage failure the shape pin was written to prevent, so leaving
// the fixture duplicated left a hole underneath the very guard against it.
package coveringleaf

// DDL is the table and index the probes share, as whitespace-separated
// statements. Both consumers embed it: package embedded_test hands it to the
// plan harness directly, and package sqldriver_test splices it into a
// CREATE SCHEMA TEMPLATE. Statement separation is whitespace in both, so the
// single spelling serves both without either reformatting it.
//
// The shape the probes need: CATEGORY is indexed, ID is the primary key (so it
// rides along in every index entry), and PRICE is neither — which is what makes
// a projection of PRICE the non-covering control.
const DDL = `CREATE TABLE products (id BIGINT, category INTEGER, price INTEGER, name STRING, PRIMARY KEY (id)) ` +
	`CREATE INDEX idx_cat ON products (category)`

// Probe is one query plus what both sides expect of it: the plan shape the
// planner-side pin asserts, and the column metadata the driver-side pin
// asserts. Keeping the two expectations on ONE record is the point — a query
// whose coveringness changes has to have its metadata expectation revisited in
// the same edit, because they are the same field set.
type Probe struct {
	// Name is the subtest name on both sides.
	Name string
	// Query is the SQL, identical on both sides.
	Query string
	// Covering is whether the query plans with the projection directly over a
	// covering index scan. When false the query is a CONTROL: it plans a
	// fetching shape and would pass the metadata assertion even with the leaf
	// walk broken, which is what makes it a control rather than a case.
	Covering bool
	// ColumnMeta is the expected `NAME|DATABASE_TYPE` per output column.
	ColumnMeta []string
}

// Probes are the three queries, in the order both tests run them.
var Probes = []Probe{
	{
		// The covering shape: the projected column IS the indexed one.
		Name:       "projected_indexed_column_over_covering_leaf",
		Query:      "SELECT category FROM products WHERE category = 2",
		Covering:   true,
		ColumnMeta: []string{"CATEGORY|INTEGER"},
	},
	{
		// Still covering — the primary key rides along in the index entry.
		Name:       "projected_pk_over_covering_leaf",
		Query:      "SELECT id FROM products WHERE category = 2",
		Covering:   true,
		ColumnMeta: []string{"ID|BIGINT"},
	},
	{
		// Control: a non-covered column forces the fetching shape.
		Name:       "projected_noncovered_column_is_the_control",
		Query:      "SELECT category, price FROM products WHERE category = 2",
		Covering:   false,
		ColumnMeta: []string{"CATEGORY|INTEGER", "PRICE|INTEGER"},
	},
}
