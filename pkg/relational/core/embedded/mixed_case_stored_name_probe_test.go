package embedded

import (
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/metadata"
)

// What a record type whose STORED name is not upper-case does to access paths,
// ON THE SELECT PATH ONLY. The DML answer is DIFFERENT and lives in the second
// test below; reading this one as the whole story is how RFC-238 §7c briefly
// bounded itself to escaped names. SELECT and DML do not resolve a table name
// the same way, so a measurement of one is not a measurement of the other.
//
// Relational DDL upper-cases every unquoted identifier before storing it, so
// for DDL-sourced metadata the SQL spelling and the stored spelling coincide
// and the question does not arise. Metadata supplied by an application --
// SetRecords over a .proto, or the programmatic builder used here -- carries
// the proto message name verbatim, and idiomatic proto names are CamelCase.
//
// The arms differ only in the case of the stored table name and in whether the
// query quotes it. Columns, primary key, index and query shape are identical.
func TestMixedCaseStoredNameAccessPaths(t *testing.T) {
	t.Parallel()

	plan := func(stored, ref string) (string, string) {
		t.Helper()
		b := metadata.NewSchemaTemplateBuilder().SetName("probe").
			AddTable(stored, []metadata.ColumnSpec{
				metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
				metadata.NewColumnSpec("NAME", api.NewStringType(true), 2),
			}, []string{"ID"}).
			AddIndex(stored, "IDX_NAME", []string{"NAME"}, false)
		tmpl, err := b.Build()
		if err != nil {
			t.Fatalf("stored=%s: build: %v", stored, err)
		}
		render := func(sql string) string {
			p, err := PlanQueryWithMetadata(sql, tmpl.Underlying(), nil)
			if err != nil {
				return "ERROR: " + err.Error()
			}
			return p
		}
		return render("SELECT name FROM " + ref + " WHERE id = 1"),
			render("SELECT id FROM " + ref + " WHERE name = 'x'")
	}

	upperPK, upperIdx := plan("CUSTOMER", "customer")
	barePK, bareIdx := plan("Customer", "customer")
	quotedPK, quotedIdx := plan("Customer", `"Customer"`)

	t.Logf("PROBE stored=CUSTOMER ref=customer     pk:  %s", upperPK)
	t.Logf("PROBE stored=CUSTOMER ref=customer     idx: %s", upperIdx)
	t.Logf("PROBE stored=Customer ref=customer     pk:  %s", barePK)
	t.Logf("PROBE stored=Customer ref=customer     idx: %s", bareIdx)
	t.Logf(`PROBE stored=Customer ref="Customer"   pk:  %s`, quotedPK)
	t.Logf(`PROBE stored=Customer ref="Customer"   idx: %s`, quotedIdx)

	// Control: the upper-case arm must show both access paths, or the probe is
	// measuring nothing.
	if !strings.Contains(upperPK, "Scan(CUSTOMER, [=])") {
		t.Errorf("control lost PK pushdown: %s", upperPK)
	}
	if !strings.Contains(upperIdx, "IndexScan(IDX_NAME") {
		t.Errorf("control lost index access: %s", upperIdx)
	}

	// A mixed-case stored name referenced UNQUOTED is rejected outright: the
	// SQL identifier normalizes to upper case and no such table exists. It is
	// not a silent degradation to a full scan.
	if !strings.Contains(barePK, "42F01") {
		t.Errorf("unquoted reference to a mixed-case table planned instead of 42F01: %s", barePK)
	}
	// The index-seeking query too, so the rejection is a property of the TABLE
	// reference rather than of the one predicate shape. Logged-but-unasserted is
	// how a probe arm becomes decoration.
	if !strings.Contains(bareIdx, "42F01") {
		t.Errorf("unquoted mixed-case reference planned on the index query instead of 42F01: %s", bareIdx)
	}

	// Quoted, the SQL spelling and the stored spelling coincide, so both access
	// paths are available exactly as in the upper-case arm.
	if !strings.Contains(quotedPK, "Scan(Customer, [=])") {
		t.Errorf("quoted mixed-case lost PK pushdown: %s", quotedPK)
	}
	if !strings.Contains(quotedIdx, "IndexScan(IDX_NAME") {
		t.Errorf("quoted mixed-case lost index access: %s", quotedIdx)
	}
}

// THE SAME QUESTION ON THE DML PATH. The answer used to differ from SELECT's
// and no longer does, which is the point of keeping the table.
//
// Production now rejects an unquoted DML target that does not resolve strictly
// -- `DELETE FROM customer` against a table declared `"Customer"` is 42F01,
// matching Java and this engine's SELECT path. INSERT is covered on both its
// forms, and the SELECT form had been the worst of the three: its target was
// resolved lazily per row in the executor, so a source producing rows raised a
// raw SQLSTATE-less error while a source producing NONE never reached the
// lookup and reported SUCCESS against a table that does not exist. Both forms
// now go through the same planning-time guard. The rejection is pinned
// end-to-end in
// yamsql/testdata/unquoted_dml_against_a_quoted_table.yaml, and it is the
// user-visible contract.
//
// This harness once planned from metadata with none of that validation, and the
// plans it produced are the record of what the defect looked like: a target
// reading `CUSTOMER`, which names no record type in this metadata, over a scan
// that matched nothing -- reachable from SQL, and the DELETE reported success
// having removed no rows. The harness now mirrors production's target guard AND
// its source sweep, in that order, so those two rows read 42F01 with
// production's exact wording. A harness that validates LESS than production is
// a harness whose golden cannot see production's behaviour, which is how the
// DML path came to be missing the SELECT path's validation in the first place.
func TestMixedCaseStoredNameDMLAccessPaths(t *testing.T) {
	t.Parallel()

	plan := func(stored, ref string) (string, string) {
		t.Helper()
		b := metadata.NewSchemaTemplateBuilder().SetName("probe").
			AddTable(stored, []metadata.ColumnSpec{
				metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
				metadata.NewColumnSpec("NAME", api.NewStringType(true), 2),
			}, []string{"ID"}).
			AddIndex(stored, "IDX_NAME", []string{"NAME"}, false)
		tmpl, err := b.Build()
		if err != nil {
			t.Fatalf("stored=%s: build: %v", stored, err)
		}
		render := func(sql string) string {
			p, err := PlanPhysicalDMLWithMetadata(sql, tmpl.Underlying(), nil)
			if err != nil {
				return "ERROR: " + err.Error()
			}
			return p.Explain()
		}
		return render("UPDATE " + ref + " SET name = 'y' WHERE id = 1"),
			render("DELETE FROM " + ref + " WHERE id = 1")
	}

	upperUpd, upperDel := plan("CUSTOMER", "customer")
	bareUpd, bareDel := plan("Customer", "customer")
	quotedUpd, quotedDel := plan("Customer", `"Customer"`)

	t.Logf("PROBE stored=CUSTOMER ref=customer     upd: %s", upperUpd)
	t.Logf("PROBE stored=CUSTOMER ref=customer     del: %s", upperDel)
	t.Logf("PROBE stored=Customer ref=customer     upd: %s", bareUpd)
	t.Logf("PROBE stored=Customer ref=customer     del: %s", bareDel)
	t.Logf(`PROBE stored=Customer ref="Customer"   upd: %s`, quotedUpd)
	t.Logf(`PROBE stored=Customer ref="Customer"   del: %s`, quotedDel)
	// ASSERT THE WHOLE STRING, not two bits of it. An earlier version checked
	// only "no ERROR" and "no [=]", and a mutation that made the DELETE target
	// carry the RESOLVED name -- falsifying the first of the two defects §7c
	// names -- left every assertion green. Two negatives constrain two bits;
	// neither of them was the target spelling. Three of the six arms were not
	// asserted at all.
	for _, tc := range []struct{ name, got, want string }{
		{
			"upper/unquoted UPDATE", upperUpd,
			"Update(CUSTOMER, [1 transforms], UnorderedPrimaryKeyDistinct(Scan(CUSTOMER, [=])))",
		},
		{
			"upper/unquoted DELETE", upperDel,
			"Delete(CUSTOMER, Scan(CUSTOMER, [=]))",
		},

		// THE FINDING, both halves visible in one string: the target reads
		// CUSTOMER, which names no record type in this metadata, AND the scan
		// under it lost the PK range the quoted arm gets. Either half moving is
		// a change to RFC-238 section 7c's DML population -- read it before
		// editing these.
		{
			"mixed/unquoted UPDATE", bareUpd,
			"ERROR: 42F01: Unknown table CUSTOMER",
		},
		{
			"mixed/unquoted DELETE", bareDel,
			"ERROR: 42F01: Unknown table CUSTOMER",
		},

		// Quoted, the spellings coincide and everything is right.
		{
			"mixed/quoted UPDATE", quotedUpd,
			"Update(Customer, [1 transforms], UnorderedPrimaryKeyDistinct(Scan(Customer, [=])))",
		},
		{
			"mixed/quoted DELETE", quotedDel,
			"Delete(Customer, Scan(Customer, [=]))",
		},
	} {
		if tc.got != tc.want {
			t.Errorf("%s plan changed.\n got: %s\nwant: %s\n"+
				"RFC-238 section 7c's DML population is derived from these six strings. If\n"+
				"the target spelling moved, the first defect it names is closed; if the\n"+
				"scan gained [=], the second is. Say which in the RFC before updating this.",
				tc.name, tc.got, tc.want)
		}
	}
}
