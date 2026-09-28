package sqldriver_test

// The carry rule (RFC-257 ws-j-design.md section 4, step 3) end to end: a new
// version of a stored template saved through fleet.SaveTemplate is carried
// from the latest stored version, each tenant is rebound through fleet.Migrate,
// and the rows written before the rebind read back by table after it. Section
// 4's tests 2, 3, 4, 6, 9, 10 and 11 are here; the ones whose v1 the target
// writes (1, 5, 10's target half and F2's population) are JVM specs
// (conformance, "WS-J a new version carried from the target's template").

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/ddl"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/core/fleet"
	"fdb.dev/pkg/relational/core/metadata"
)

// carryTemplate is the template Go's DDL builds from body, at template version
// `version`.
func carryTemplate(t *testing.T, name string, version int, body string) *metadata.RecordLayerSchemaTemplate {
	t.Helper()
	built, err := embedded.BuildSchemaTemplateFromDDLNamed(body, name)
	if err != nil {
		t.Fatalf("build %s@%d: %v", name, version, err)
	}
	tmpl, err := metadata.NewRecordLayerSchemaTemplateWithVersion(name, built.Underlying(), version)
	if err != nil {
		t.Fatal(err)
	}
	return tmpl
}

// carrySetup stores v1 of `name` from body, binds each schema of dbPath to it,
// and writes rows(schema) into each (statements run in order).
func carrySetup(t *testing.T, h *fleetHarness, dbPath, name, body string, schemas []string, rows func(schema string) []string) {
	t.Helper()
	h.mustRun(t, "bootstrap", func(txn api.Transaction) error {
		if err := h.cat.Initialize(txn); err != nil {
			return err
		}
		if err := ddl.NewCreateDatabaseConstantAction(dbPath, h.cat).Execute(txn); err != nil {
			return err
		}
		if err := ddl.NewSaveSchemaTemplateConstantAction(carryTemplate(t, name, 1, body), h.cat.SchemaTemplateCatalog()).Execute(txn); err != nil {
			return err
		}
		for _, s := range schemas {
			if err := ddl.NewCreateSchemaConstantAction(dbPath, s, name, h.cat, h.ks).Execute(txn); err != nil {
				return err
			}
		}
		return nil
	})
	for _, s := range schemas {
		db := fleetOpen(t, dbPath, s)
		for _, stmt := range rows(s) {
			mwjoMustExec(t, db, context.Background(), stmt)
		}
	}
}

// carrySave saves a new version through fleet.SaveTemplate and returns the
// stored proto of it.
func carrySave(t *testing.T, h *fleetHarness, tmpl api.SchemaTemplate) (*gen.MetaData, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	stored, err := fleet.SaveTemplate(ctx, h.db, h.cat, tmpl)
	if err != nil {
		return nil, err
	}
	if stored.Version() != tmpl.Version() {
		t.Fatalf("SaveTemplate returned version %d, want %d", stored.Version(), tmpl.Version())
	}
	var p *gen.MetaData
	h.mustRun(t, "load stored", func(txn api.Transaction) error {
		var lerr error
		p, lerr = h.cat.SchemaTemplateCatalog().LoadTemplateProto(txn, tmpl.MetadataName(), tmpl.Version())
		return lerr
	})
	return p, nil
}

// carryMigrate rebinds every schema of dbPath bound to `name` to `version`.
func carryMigrate(t *testing.T, h *fleetHarness, dbPath, name string, version int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	res, err := fleet.Migrate(ctx, h.db, h.cat, h.ks, fleet.FilterByTemplate(fleetTargets(t, h, dbPath), name), version, fleet.Options{})
	if err != nil {
		t.Fatalf("migrate to %d: %v", version, err)
	}
	for _, f := range res.Failures {
		t.Fatalf("migrate to %d: %v", version, f)
	}
	if res.Migrated == 0 {
		t.Fatalf("migrate to %d migrated no schema: %+v", version, res)
	}
}

func carryRecordType(p *gen.MetaData, name string) *gen.RecordType {
	for _, rt := range p.GetRecordTypes() {
		if rt.GetName() == name {
			return rt
		}
	}
	return nil
}

func carryIndex(p *gen.MetaData, name string) *gen.Index {
	for _, idx := range p.GetIndexes() {
		if idx.GetName() == name {
			return idx
		}
	}
	return nil
}

func carryUnionNumber(p *gen.MetaData, typeName string) int32 {
	for _, m := range p.GetRecords().GetMessageType() {
		if m.GetName() != "RecordTypeUnion" {
			continue
		}
		for _, f := range m.GetField() {
			if strings.HasSuffix(f.GetTypeName(), "."+typeName) || f.GetTypeName() == typeName {
				return f.GetNumber()
			}
		}
	}
	return 0
}

func carryQuery(t *testing.T, dbPath, schema, q string) string {
	t.Helper()
	db := evolReopen(t, dbPath, schema)
	rows, err := db.QueryContext(context.Background(), q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprint(vals...))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(out, ";")
}

func carryExplain(t *testing.T, dbPath, schema, q string) string {
	t.Helper()
	return carryQuery(t, dbPath, schema, "EXPLAIN "+q)
}

func carryInsert(table string, n int, row func(i int) string) []string {
	var out []string
	for base := 1; base <= n; base += 50 {
		var vals []string
		for i := base; i < base+50 && i <= n; i++ {
			vals = append(vals, row(i))
		}
		out = append(out, "INSERT INTO "+table+" VALUES "+strings.Join(vals, ","))
	}
	return out
}

// Test 2: v2 adds a table declared BEFORE the existing one, so Java's order of a
// fresh build would renumber: the stored table keeps its record-type key and
// union field, the new one takes the next of each and a since-version, the
// rebind is admitted, the old rows read back and the new table is writable.
func TestFDB_Carry_AddedTableKeepsTheStoredNumbering(t *testing.T) {
	t.Parallel()
	h := newFleetHarness(t)
	dbPath := "/carry_table_" + t.Name()[len(t.Name())-8:]
	v1 := "CREATE TABLE b(id BIGINT, x BIGINT, PRIMARY KEY(id))"
	carrySetup(t, h, dbPath, "CARRY_TABLE", v1, []string{"S"}, func(string) []string {
		return []string{"INSERT INTO b VALUES (1, 10), (2, 20)"}
	})
	var stored1 *gen.MetaData
	h.mustRun(t, "load v1", func(txn api.Transaction) error {
		var err error
		stored1, err = h.cat.SchemaTemplateCatalog().LoadTemplateProto(txn, "CARRY_TABLE", 1)
		return err
	})
	fresh := carryTemplate(t, "CARRY_TABLE", 2, "CREATE TABLE a(id BIGINT, y BIGINT, PRIMARY KEY(id)) "+v1)
	freshProto, err := fresh.Underlying().ToProto()
	if err != nil {
		t.Fatal(err)
	}
	if carryRecordType(freshProto, "B").GetExplicitKey().GetLongValue() == carryRecordType(stored1, "B").GetExplicitKey().GetLongValue() {
		t.Fatal("a fresh build keeps B's key; the test cannot tell a carry from a rebuild")
	}
	stored2, err := carrySave(t, h, fresh)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := carryRecordType(stored2, "B").GetExplicitKey().GetLongValue(), carryRecordType(stored1, "B").GetExplicitKey().GetLongValue(); got != want {
		t.Errorf("B's key %d, want the stored %d", got, want)
	}
	if got, want := carryUnionNumber(stored2, "B"), carryUnionNumber(stored1, "B"); got != want {
		t.Errorf("B's union field %d, want the stored %d", got, want)
	}
	a := carryRecordType(stored2, "A")
	if a.GetExplicitKey().GetLongValue() != carryRecordType(stored1, "B").GetExplicitKey().GetLongValue()+1 || a.GetSinceVersion() != stored2.GetVersion() {
		t.Errorf("A: key %v, since %d; want the next key and since the new metadata version %d", a.GetExplicitKey(), a.GetSinceVersion(), stored2.GetVersion())
	}
	if carryUnionNumber(stored2, "A") != carryUnionNumber(stored1, "B")+1 {
		t.Errorf("A's union field %d, want the next above %d", carryUnionNumber(stored2, "A"), carryUnionNumber(stored1, "B"))
	}
	carryMigrate(t, h, dbPath, "CARRY_TABLE", 2)
	if got := carryQuery(t, dbPath, "S", "SELECT id, x FROM b ORDER BY id"); got != "1 10;2 20" {
		t.Errorf("b after the rebind: %s", got)
	}
	mwjoMustExec(t, fleetOpen(t, dbPath, "S"), context.Background(), "INSERT INTO a VALUES (1, 7)")
	if got := carryQuery(t, dbPath, "S", "SELECT id, y FROM a"); got != "1 7" {
		t.Errorf("a: %s", got)
	}
}

// Test 3: v2 CHANGES an index's key, (c) to (c, v): the carried index keeps its
// added version and subspace key and takes a last-modified version above the
// stored metadata version, so the rebind is admitted as a rebuild, which the
// store runs when it next opens (checkVersion's rebuild of every index modified
// since its header's version). A relational store has no record-count key, so
// a store holding any record counts as too large to rebuild inline
// (getRecordCountForRebuildIndexes, FDBRecordStore.java:4862-4884): an empty
// tenant rebuilds the index inline and plans it; a tenant with rows is left
// DISABLED, the index's old entries cleared and the planner not using it, until
// the online build makes it READABLE.
func TestFDB_Carry_ChangedIndexIsRebuiltOnOpen(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		rows int
	}{{"an empty tenant", 0}, {"a tenant with rows", 30}} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newFleetHarness(t)
			dbPath := fmt.Sprintf("/carry_changed_%d", c.rows)
			body := func(cols string) string {
				return "CREATE TABLE t(id BIGINT, c BIGINT, v BIGINT, PRIMARY KEY(id)) CREATE INDEX ix AS SELECT " + cols + " FROM t ORDER BY " + cols
			}
			name := fmt.Sprintf("CARRY_CHANGED_%d", c.rows)
			row := func(i int) string { return fmt.Sprintf("(%d, %d, %d)", i, i%7, i) }
			carrySetup(t, h, dbPath, name, body("c"), []string{"S"}, func(string) []string { return carryInsert("t", c.rows, row) })
			var stored1 *gen.MetaData
			h.mustRun(t, "load v1", func(txn api.Transaction) error {
				var err error
				stored1, err = h.cat.SchemaTemplateCatalog().LoadTemplateProto(txn, name, 1)
				return err
			})
			stored2, err := carrySave(t, h, carryTemplate(t, name, 2, body("c, v")))
			if err != nil {
				t.Fatal(err)
			}
			before, after := carryIndex(stored1, "IX"), carryIndex(stored2, "IX")
			if after.GetAddedVersion() != before.GetAddedVersion() || string(after.GetSubspaceKey()) != string(before.GetSubspaceKey()) ||
				after.GetLastModifiedVersion() <= stored1.GetVersion() {
				t.Fatalf("IX carried as added %d key %x modified %d; want the stored added %d and key %x, modified above %d",
					after.GetAddedVersion(), after.GetSubspaceKey(), after.GetLastModifiedVersion(),
					before.GetAddedVersion(), before.GetSubspaceKey(), stored1.GetVersion())
			}
			carryMigrate(t, h, dbPath, name, 2)
			q := "SELECT id FROM t WHERE c = 3 AND v = 10"
			if c.rows == 0 {
				// The first statement opens the store under v2: the inline rebuild.
				mwjoMustExec(t, fleetOpen(t, dbPath, "S"), context.Background(), strings.Join(carryInsert("t", 30, row), ""))
			} else {
				if got := carryQuery(t, dbPath, "S", q); got != "10" {
					t.Errorf("%s: %s", q, got)
				}
				if states := evolIndexStates(t, dbPath, "S"); states["IX"] != recordlayer.IndexStateDisabled {
					t.Fatalf("IX is %v on a tenant with rows, want DISABLED", states["IX"])
				}
				if plan := carryExplain(t, dbPath, "S", q); strings.Contains(plan, "IX") {
					t.Errorf("a DISABLED index is planned: %s", plan)
				}
				if n := carryIndexEntries(t, h, dbPath, "S", "IX"); n != 0 {
					t.Errorf("IX keeps %d entries of its old key while DISABLED", n)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer cancel()
				res, err := fleet.BuildAll(ctx, h.db, h.cat, h.ks, dbPath, fleet.BuildOptions{})
				if err != nil || res.Failed > 0 || res.Built == 0 {
					t.Fatalf("online build: %v %+v", err, res)
				}
			}
			if got := carryQuery(t, dbPath, "S", q); got != "10" {
				t.Errorf("%s: %s", q, got)
			}
			if states := evolIndexStates(t, dbPath, "S"); states["IX"] != recordlayer.IndexStateReadable {
				t.Errorf("IX is %v, want READABLE", states["IX"])
			}
			if plan := carryExplain(t, dbPath, "S", q); !strings.Contains(plan, "IX") {
				t.Errorf("the rebuilt index is not planned: %s", plan)
			}
			if n := carryIndexEntries(t, h, dbPath, "S", "IX"); n != 30 {
				t.Errorf("IX holds %d entries, want 30", n)
			}
		})
	}
}

// Test 4: v2 DROPS an index: a former index removed at the new metadata
// version, the rebind admitted, the index's data cleared on open.
func TestFDB_Carry_DroppedIndexBecomesAFormerIndex(t *testing.T) {
	t.Parallel()
	h := newFleetHarness(t)
	dbPath := "/carry_dropped"
	table := "CREATE TABLE t(id BIGINT, c BIGINT, PRIMARY KEY(id))"
	carrySetup(t, h, dbPath, "CARRY_DROPPED", table+" CREATE INDEX ix AS SELECT c FROM t ORDER BY c", []string{"S"}, func(string) []string {
		return carryInsert("t", 20, func(i int) string { return fmt.Sprintf("(%d, %d)", i, i) })
	})
	if n := carryIndexEntries(t, h, dbPath, "S", "IX"); n != 20 {
		t.Fatalf("IX holds %d entries before the drop, want 20", n)
	}
	stored2, err := carrySave(t, h, carryTemplate(t, "CARRY_DROPPED", 2, table))
	if err != nil {
		t.Fatal(err)
	}
	if len(stored2.GetFormerIndexes()) != 1 || stored2.GetFormerIndexes()[0].GetFormerName() != "IX" ||
		stored2.GetFormerIndexes()[0].GetRemovedVersion() != stored2.GetVersion() {
		t.Fatalf("former indexes %v, want IX removed at %d", stored2.GetFormerIndexes(), stored2.GetVersion())
	}
	carryMigrate(t, h, dbPath, "CARRY_DROPPED", 2)
	if got := carryQuery(t, dbPath, "S", "SELECT count(*) FROM t"); got != "20" {
		t.Errorf("rows: %s", got)
	}
	if n := carryIndexEntries(t, h, dbPath, "S", "IX"); n != 0 {
		t.Errorf("IX holds %d entries after the store opened under v2, want its data cleared", n)
	}
}

// carryIndexEntries counts the entries of index `index` in the schema's store,
// under the subspace key v1 stored for it (its name).
func carryIndexEntries(t *testing.T, h *fleetHarness, dbPath, schema, index string) int {
	t.Helper()
	ss, err := h.ks.SchemaSubspace(dbPath, schema)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	n, err := h.db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		begin, end := ss.Sub(int64(recordlayer.IndexKey), index).FDBRangeKeys()
		kvs, err := rtx.Transaction().GetRange(fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{}).GetSliceWithError()
		return len(kvs), err
	})
	if err != nil {
		t.Fatal(err)
	}
	return n.(int)
}

// Test 6: a tenant bound to v1 while v3 is carried from v2 is admitted, and its
// rows read back.
func TestFDB_Carry_ATenantOnAnOlderVersionRebindsToTheLatest(t *testing.T) {
	t.Parallel()
	h := newFleetHarness(t)
	dbPath := "/carry_older"
	table := "CREATE TABLE t(id BIGINT, c BIGINT, v BIGINT, PRIMARY KEY(id))"
	carrySetup(t, h, dbPath, "CARRY_OLDER", table, []string{"A", "B"}, func(s string) []string {
		return carryInsert("t", 10, func(i int) string { return fmt.Sprintf("(%d, %d, %d)", i, i%3, i) })
	})
	if _, err := carrySave(t, h, carryTemplate(t, "CARRY_OLDER", 2, table+" CREATE INDEX ic AS SELECT c FROM t ORDER BY c")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	res, err := fleet.Migrate(ctx, h.db, h.cat, h.ks, fleet.FilterByTemplate(fleetTargetsNamed(t, h, dbPath, "A"), "CARRY_OLDER"), 2, fleet.Options{})
	if err != nil || res.Failed > 0 || res.Migrated != 1 {
		t.Fatalf("migrate A to 2: %v %+v", err, res)
	}
	if _, err := carrySave(t, h, carryTemplate(t, "CARRY_OLDER", 3,
		table+" CREATE INDEX ic AS SELECT c FROM t ORDER BY c CREATE INDEX iv AS SELECT v FROM t ORDER BY v")); err != nil {
		t.Fatal(err)
	}
	if got := fleetVersions(t, h, dbPath); got["A"] != 2 || got["B"] != 1 {
		t.Fatalf("bound versions %v, want A at 2 and B at 1", got)
	}
	carryMigrate(t, h, dbPath, "CARRY_OLDER", 3)
	for _, s := range []string{"A", "B"} {
		if got := carryQuery(t, dbPath, s, "SELECT count(*) FROM t WHERE v > 4"); got != "6" {
			t.Errorf("%s: %s", s, got)
		}
	}
}

func fleetTargetsNamed(t *testing.T, h *fleetHarness, dbPath, schema string) []fleet.Target {
	t.Helper()
	var out []fleet.Target
	for _, tg := range fleetTargets(t, h, dbPath) {
		if tg.SchemaName == schema {
			out = append(out, tg)
		}
	}
	return out
}

// Test 9: v2 changes ONLY an index's predicate (WHERE c > 1 to WHERE c > 2): a
// CHANGED index (neither validator compares predicates, so the carry rule
// must), rebuilt on open, and holding exactly the rows the new predicate admits.
func TestFDB_Carry_PredicateChangeIsAChangedIndex(t *testing.T) {
	t.Parallel()
	h := newFleetHarness(t)
	dbPath := "/carry_predicate"
	body := func(bound int) string {
		return fmt.Sprintf("CREATE TABLE t(id BIGINT, c BIGINT, PRIMARY KEY(id)) CREATE INDEX ix AS SELECT c FROM t WHERE c > %d ORDER BY c", bound)
	}
	carrySetup(t, h, dbPath, "CARRY_PREDICATE", body(1), []string{"S"}, func(string) []string {
		return carryInsert("t", 10, func(i int) string { return fmt.Sprintf("(%d, %d)", i, i) })
	})
	if n := carryIndexEntries(t, h, dbPath, "S", "IX"); n != 9 {
		t.Fatalf("IX under WHERE c > 1 holds %d entries, want 9", n)
	}
	var stored1 *gen.MetaData
	h.mustRun(t, "load v1", func(txn api.Transaction) error {
		var err error
		stored1, err = h.cat.SchemaTemplateCatalog().LoadTemplateProto(txn, "CARRY_PREDICATE", 1)
		return err
	})
	stored2, err := carrySave(t, h, carryTemplate(t, "CARRY_PREDICATE", 2, body(2)))
	if err != nil {
		t.Fatal(err)
	}
	if carryIndex(stored2, "IX").GetLastModifiedVersion() <= stored1.GetVersion() {
		t.Fatal("a predicate change was carried as EQUIVALENT")
	}
	carryMigrate(t, h, dbPath, "CARRY_PREDICATE", 2)
	// The first read opens the store under v2, which leaves the changed index
	// DISABLED with its entries cleared; the online build fills it under the
	// new predicate.
	if got := carryQuery(t, dbPath, "S", "SELECT count(*) FROM t"); got != "10" {
		t.Errorf("rows: %s", got)
	}
	if states := evolIndexStates(t, dbPath, "S"); states["IX"] != recordlayer.IndexStateDisabled {
		t.Fatalf("IX is %v after the rebind, want DISABLED (CHANGED)", states["IX"])
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if res, err := fleet.BuildAll(ctx, h.db, h.cat, h.ks, dbPath, fleet.BuildOptions{}); err != nil || res.Failed > 0 || res.Built == 0 {
		t.Fatalf("online build: %v %+v", err, res)
	}
	if n := carryIndexEntries(t, h, dbPath, "S", "IX"); n != 8 {
		t.Errorf("IX under WHERE c > 2 holds %d entries, want 8", n)
	}
}

// Test 10 (Go's half): v2 is the same DDL: every index EQUIVALENT, carried as
// the stored Index message, no rebuild: above the 200-record inline threshold
// the index stays READABLE (it would be left DISABLED if it were rebuilt).
func TestFDB_Carry_EquivalentIndexIsNotRebuilt(t *testing.T) {
	t.Parallel()
	h := newFleetHarness(t)
	dbPath := "/carry_equivalent"
	body := "CREATE TABLE t(id BIGINT, c BIGINT, PRIMARY KEY(id)) CREATE INDEX ix AS SELECT c FROM t ORDER BY c"
	carrySetup(t, h, dbPath, "CARRY_EQUIVALENT", body, []string{"S"}, func(string) []string {
		return carryInsert("t", 260, func(i int) string { return fmt.Sprintf("(%d, %d)", i, i%5) })
	})
	var stored1 *gen.MetaData
	h.mustRun(t, "load v1", func(txn api.Transaction) error {
		var err error
		stored1, err = h.cat.SchemaTemplateCatalog().LoadTemplateProto(txn, "CARRY_EQUIVALENT", 1)
		return err
	})
	stored2, err := carrySave(t, h, carryTemplate(t, "CARRY_EQUIVALENT", 2, body))
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(carryIndex(stored2, "IX"), carryIndex(stored1, "IX")) {
		t.Fatalf("IX not carried as stored: %v against %v", carryIndex(stored2, "IX"), carryIndex(stored1, "IX"))
	}
	carryMigrate(t, h, dbPath, "CARRY_EQUIVALENT", 2)
	q := "SELECT count(*) FROM t WHERE c = 3"
	if got := carryQuery(t, dbPath, "S", q); got != "52" {
		t.Errorf("%s: %s", q, got)
	}
	if states := evolIndexStates(t, dbPath, "S"); states["IX"] != recordlayer.IndexStateReadable {
		t.Errorf("IX is %v after the rebind, want READABLE (not rebuilt)", states["IX"])
	}
}

// Test 11: v2 drops an index, v3 re-adds one under its name: the name is the
// former index's subspace key, so the save is refused, naming both.
func TestFDB_Carry_ReAddingADroppedNameIsRefused(t *testing.T) {
	t.Parallel()
	h := newFleetHarness(t)
	dbPath := "/carry_readd"
	table := "CREATE TABLE t(id BIGINT, c BIGINT, v BIGINT, PRIMARY KEY(id))"
	carrySetup(t, h, dbPath, "CARRY_READD", table+" CREATE INDEX ix AS SELECT c FROM t ORDER BY c", []string{"S"}, func(string) []string { return nil })
	stored2, err := carrySave(t, h, carryTemplate(t, "CARRY_READD", 2, table))
	if err != nil {
		t.Fatal(err)
	}
	_, err = carrySave(t, h, carryTemplate(t, "CARRY_READD", 3, table+" CREATE INDEX ix AS SELECT v FROM t ORDER BY v"))
	want := fmt.Sprintf("index IX cannot be added: its name is the subspace key of index IX dropped at version %d; add it under another name", stored2.GetVersion())
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeInvalidSchemaTemplate || apiErr.Message != want {
		t.Fatalf("err = %v, want %s %q", err, api.ErrCodeInvalidSchemaTemplate, want)
	}
}
