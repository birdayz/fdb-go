// End-to-end tests for `frl stats` — the offline planner-statistics
// maintainer (RFC-236). Shares the process-wide FDB testcontainer with the
// other integration suites; each test bootstraps its OWN database so the
// order they run in cannot change what they observe. That matters more here
// than elsewhere: every assertion is about stored state that a sibling test
// collecting or clearing would silently rewrite.
package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
)

// setupStatsDB creates database /frlstats_<name> with one schema built from
// ddl, seeded by dml, through the real `frl sql` path. Returns the database
// URI.
func setupStatsDB(t *testing.T, name, ddl, dml string) string {
	t.Helper()
	bindConfig(t)
	dbURI := "/frlstats_" + name
	script := fmt.Sprintf(`
CREATE DATABASE %s;

CREATE SCHEMA TEMPLATE frlstats_%s_tpl
%s;

CREATE SCHEMA %s/main WITH TEMPLATE frlstats_%s_tpl;

%s
`, dbURI, name, ddl, dbURI, name, dml)
	path := filepath.Join(os.TempDir(), fmt.Sprintf("frlstats-%s-%d.sql", name, os.Getpid()))
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatalf("write schema.sql: %v", err)
	}
	defer os.Remove(path)
	if out, err := runCmd(t, "sql", "--database", dbURI, "--schema", "main", "-f", path); err != nil {
		t.Fatalf("bootstrap %s: %v\noutput: %s", dbURI, err, out)
	}
	return dbURI
}

const statsOneTableDDL = `CREATE TABLE items (
  id   BIGINT,
  name STRING,
  PRIMARY KEY (id)
)`

func TestIntegration_Stats_ShowBeforeCollectSaysNotCollected(t *testing.T) {
	db := setupStatsDB(t, "never", statsOneTableDDL, "INSERT INTO items VALUES (1, 'a');")
	out, err := runCmd(t, "stats", "show", "--database", db, "--schema", "main")
	if err != nil {
		t.Fatalf("stats show: %v\noutput: %s", err, out)
	}
	// "never collected" and "collected but unusable" are different operator
	// problems with different fixes, so the verdict must name which one.
	if !strings.Contains(out, "not collected") {
		t.Errorf("expected the verdict to say the statistics were never collected:\n%s", out)
	}
	if !strings.Contains(out, "frl stats collect") {
		t.Errorf("expected the output to name the command that fixes it:\n%s", out)
	}
}

func TestIntegration_Stats_CollectThenShowIsUsable(t *testing.T) {
	db := setupStatsDB(t, "usable", statsOneTableDDL,
		"INSERT INTO items VALUES (1, 'a'), (2, 'b'), (3, 'c');")

	out, err := runCmd(t, "stats", "collect", "--database", db, "--schema", "main")
	if err != nil {
		t.Fatalf("stats collect: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "records scanned: 3") {
		t.Errorf("collect did not report the 3 records it scanned:\n%s", out)
	}

	out, err = runCmd(t, "stats", "show", "--database", db, "--schema", "main")
	if err != nil {
		t.Fatalf("stats show: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "USABLE") || strings.Contains(out, "NOT USABLE") {
		t.Errorf("expected the planner verdict to be USABLE:\n%s", out)
	}
	// The count is the whole product. A verdict of USABLE over a wrong number
	// is worse than a refusal, because the planner acts on it.
	if !strings.Contains(out, "ITEMS") || !strings.Contains(out, "3") {
		t.Errorf("expected the exact per-type count for ITEMS:\n%s", out)
	}
	if !strings.Contains(out, "planner_statistics") {
		t.Errorf("expected the output to name the opt-in the planner needs:\n%s", out)
	}
}

func TestIntegration_Stats_ShowJSONIsTyped(t *testing.T) {
	db := setupStatsDB(t, "json", statsOneTableDDL,
		"INSERT INTO items VALUES (1, 'a'), (2, 'b');")
	if out, err := runCmd(t, "stats", "collect", "--database", db, "--schema", "main"); err != nil {
		t.Fatalf("stats collect: %v\noutput: %s", err, out)
	}
	out, err := runCmd(t, "stats", "show", "--database", db, "--schema", "main", "-o", "json")
	if err != nil {
		t.Fatalf("stats show -o json: %v\noutput: %s", err, out)
	}
	var got statsShowResult
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode JSON: %v\noutput: %s", err, out)
	}
	if !got.Usable {
		t.Errorf("usable = false, refusal = %q", got.Refusal)
	}
	if got.PerType["ITEMS"] != 2 {
		t.Errorf("per_type[ITEMS] = %d, want 2 (got %v)", got.PerType["ITEMS"], got.PerType)
	}
	if got.CollectedAtVersion == 0 {
		t.Errorf("collected_at_version is 0 — the freshness gate has nothing to compare")
	}
	if got.MaxAgeVersions == 0 {
		t.Errorf("max_age_versions is 0 — the bound is not reported")
	}
	// Age must be non-negative and inside the bound, or the entry this test
	// just wrote would already be refused.
	if got.AgeVersions < 0 || got.AgeVersions > got.MaxAgeVersions {
		t.Errorf("age_versions = %d, outside [0, %d]", got.AgeVersions, got.MaxAgeVersions)
	}
}

func TestIntegration_Stats_CappedTypeIsAbsentAndRefuses(t *testing.T) {
	// The cap is what bounds work on a huge table, and its contract is that an
	// EXCEEDED type is recorded as ABSENT rather than as a partial count. This
	// pins both halves: the collector says it skipped, and the planner's own
	// verdict then refuses the whole schema for incompleteness — the stated
	// cost of schema-wide completeness, exercised rather than asserted.
	db := setupStatsDB(t, "capped", statsOneTableDDL,
		"INSERT INTO items VALUES (1, 'a'), (2, 'b'), (3, 'c'), (4, 'd'), (5, 'e');")

	out, err := runCmd(t, "stats", "collect", "--database", db, "--schema", "main",
		"--max-records-per-type", "2")
	if err != nil {
		t.Fatalf("stats collect: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "not collected") || !strings.Contains(out, "ITEMS") {
		t.Errorf("expected the capped type to be reported as not collected:\n%s", out)
	}

	out, err = runCmd(t, "stats", "show", "--database", db, "--schema", "main")
	if err != nil {
		t.Fatalf("stats show: %v\noutput: %s", err, out)
	}
	// A capped type leaves NO entry at all, so the store has nothing — the
	// verdict is "not collected", not "incomplete". Pinned as the actual
	// behaviour rather than the one the cap's name suggests.
	if !strings.Contains(out, "NOT USABLE") {
		t.Errorf("expected the planner to refuse after a capped collection:\n%s", out)
	}
}

func TestIntegration_Stats_ClearRefusesWithoutYes(t *testing.T) {
	db := setupStatsDB(t, "noyes", statsOneTableDDL, "INSERT INTO items VALUES (1, 'a');")
	if out, err := runCmd(t, "stats", "collect", "--database", db, "--schema", "main"); err != nil {
		t.Fatalf("stats collect: %v\noutput: %s", err, out)
	}
	// Non-interactive stdin without --yes must be a hard error, never a
	// silent proceed and never a hang waiting for a TTY that is not there.
	out, err := runCmd(t, "stats", "clear", "--database", db, "--schema", "main")
	if err == nil {
		t.Fatalf("clear without --yes succeeded; want a refusal\noutput: %s", out)
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Errorf("refusal does not name the flag that resolves it: %v", err)
	}
	// And it must not have cleared anything on its way out.
	out, showErr := runCmd(t, "stats", "show", "--database", db, "--schema", "main")
	if showErr != nil {
		t.Fatalf("stats show: %v\noutput: %s", showErr, out)
	}
	if strings.Contains(out, "NOT USABLE") {
		t.Errorf("the refused clear removed the statistics anyway:\n%s", out)
	}
}

func TestIntegration_Stats_ClearRemovesThem(t *testing.T) {
	db := setupStatsDB(t, "clear", statsOneTableDDL,
		"INSERT INTO items VALUES (1, 'a'), (2, 'b');")
	if out, err := runCmd(t, "stats", "collect", "--database", db, "--schema", "main"); err != nil {
		t.Fatalf("stats collect: %v\noutput: %s", err, out)
	}
	if out, err := runCmd(t, "stats", "clear", "--database", db, "--schema", "main", "--yes"); err != nil {
		t.Fatalf("stats clear: %v\noutput: %s", err, out)
	}
	out, err := runCmd(t, "stats", "show", "--database", db, "--schema", "main")
	if err != nil {
		t.Fatalf("stats show: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "not collected") {
		t.Errorf("statistics survived the clear:\n%s", out)
	}
	// Clearing must not touch the records. The whole safety argument for
	// storing statistics outside the store's subspace is that nothing here can
	// reach record or index data.
	rows, err := runCmd(t, "sql", "--database", db, "--schema", "main", "-c", "SELECT id FROM items")
	if err != nil {
		t.Fatalf("select after clear: %v\noutput: %s", err, rows)
	}
	if !strings.Contains(rows, "2 rows") && !strings.Contains(rows, "2 row") {
		t.Errorf("expected the 2 seeded rows to survive the clear:\n%s", rows)
	}
}

func TestStats_RequiresRelationalAddressing(t *testing.T) {
	// Statistics are located through the relational keyspace, so a store
	// addressed any other way would get bytes nothing reads. Refusing is the
	// only honest answer; succeeding would look like it worked.
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no database", []string{"stats", "show", "--schema", "main"}, "--database"},
		{"no schema", []string{"stats", "show", "--database", "/x"}, "--schema"},
		{"collect no database", []string{"stats", "collect", "--schema", "main"}, "--database"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runCmd(t, tc.args...)
			if err == nil {
				t.Fatalf("expected an error naming %s", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %s", err, tc.want)
			}
		})
	}
}

func TestStats_RejectsUnknownOutputFormat(t *testing.T) {
	_, err := runCmd(t, "stats", "show", "--database", "/x", "--schema", "main", "-o", "yaml")
	if err == nil {
		t.Fatal("expected an error for an unsupported output format")
	}
	// The format is validated BEFORE any connection is attempted, so a typo
	// fails in milliseconds instead of after an FDB dial timeout.
	if !strings.Contains(err.Error(), "yaml") {
		t.Errorf("error %q does not name the rejected format", err)
	}
}

// THE TWO DERIVATIONS MUST AGREE. This is the most important test in this file.
//
// Statistics live at a location derived from the relational keyspace root and
// the schema's store subspace. Three pieces of code now compute it: the planner
// (through EmbeddedConnection), `stats collect --database --schema` (through the
// same connection, deliberately), and `stats collect --all-schemas` (through
// fleet.CollectStatistics, which cannot use a connection because it has no
// single schema to bind one to).
//
// That third path is a second derivation, and a second derivation is exactly
// what fails silently: the fan-out writes somewhere the planner never reads,
// every command reports success, the summary says `collected=N`, and the only
// symptom is that plans never change. Nothing in either path errors.
//
// So the fan-out WRITES and the connection READS. A disagreement in the
// database-path convention, the schema case folding, or the keyspace root shows
// up here as "not collected" and nowhere else.
func TestIntegration_Stats_FleetCollectIsReadableByTheConnection(t *testing.T) {
	db := setupStatsDB(t, "fleet", statsOneTableDDL,
		"INSERT INTO items VALUES (1, 'a'), (2, 'b'), (3, 'c'), (4, 'd');")

	out, err := runCmd(t, "stats", "collect", "--database", db, "--all-schemas")
	if err != nil {
		t.Fatalf("stats collect --all-schemas: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "collected=1") {
		t.Errorf("expected the fan-out summary to report one collected schema:\n%s", out)
	}

	// Read back through the CONNECTION path — the one the planner uses.
	out, err = runCmd(t, "stats", "show", "--database", db, "--schema", "main", "-o", "json")
	if err != nil {
		t.Fatalf("stats show: %v\noutput: %s", err, out)
	}
	var got statsShowResult
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode JSON: %v\noutput: %s", err, out)
	}
	if !got.Usable {
		t.Fatalf("the fan-out reported success but the connection cannot read what it wrote\n"+
			"  refusal: %q, found: %v\n"+
			"  The two paths derive the statistics location independently. This is what a\n"+
			"  disagreement between them looks like, and it is otherwise invisible: the\n"+
			"  collector succeeds, the summary says collected=1, and plans silently never\n"+
			"  change.", got.Refusal, got.Found)
	}
	if got.PerType["ITEMS"] != 4 {
		t.Errorf("per_type[ITEMS] = %d, want 4 — the paths agree on the location but not on\n"+
			"  the contents (got %v)", got.PerType["ITEMS"], got.PerType)
	}
}

func TestStats_AllSchemasRejectsASingleSchema(t *testing.T) {
	// Two conflicting targets. Silently preferring one would collect a
	// different set of schemas than the operator named.
	_, err := runCmd(t, "stats", "collect", "--database", "/x", "--schema", "main", "--all-schemas")
	if err == nil {
		t.Fatal("expected --all-schemas + --schema to be rejected")
	}
	if !strings.Contains(err.Error(), "--all-schemas") || !strings.Contains(err.Error(), "--schema") {
		t.Errorf("error %q does not name both conflicting flags", err)
	}
}

// COLLECTION WRITES NOTHING INSIDE THE STORE'S OWN SUBSPACE.
//
// This is the wire-compat pin. Java owns record-store keyspaces 0-10 and marks
// the enum UNSTABLE, so a byte written inside one is a hazard for a feature
// Java does not have. Statistics live outside the store subspace entirely, and
// a snapshot taken across a collection is what says so: nothing rewritten,
// nothing added.
//
// WHAT IT DOES NOT CATCH, stated because the obvious reading is wrong and was
// measured to be wrong. Opening a record store runs checkPossiblyRebuild, which
// WRITES — a header bump, index clears, rebuild marks — when the metadata handed
// to it is NEWER than the store header. The collector guards that with
// SetSkipPossiblyRebuild + Open (connection.go, fleet/statistics.go), and this
// test cannot see the guard: the fixture's schema is created and immediately
// collected, so its header already matches its metadata and the rebuild path has
// nothing to do. Reverting the guard to CreateOrOpen leaves this test GREEN
// (verified by mutation). The guard's own pin is
// TestCollectStatisticsDoesNotMigrateAStaleStore in pkg/recordlayer, which
// builds the version skew this fixture cannot.
func TestIntegration_Stats_CollectDoesNotTouchTheStore(t *testing.T) {
	db := setupStatsDB(t, "readonly", statsOneTableDDL,
		"INSERT INTO items VALUES (1, 'a'), (2, 'b'), (3, 'c');")

	before := snapshotStoreSubspace(t, db, "MAIN")
	if len(before) == 0 {
		t.Fatal("the store subspace is empty before collection — a comparison over " +
			"nothing would pass whatever the collector did")
	}

	if out, err := runCmd(t, "stats", "collect", "--database", db, "--schema", "main"); err != nil {
		t.Fatalf("stats collect: %v\noutput: %s", err, out)
	}

	after := snapshotStoreSubspace(t, db, "MAIN")
	if !reflect.DeepEqual(before, after) {
		t.Errorf("collection wrote inside the store's own subspace: %d keys before, %d after\n"+
			"  Statistics must live OUTSIDE every record store's subspace. Java owns\n"+
			"  record-store keyspaces 0-10 and marks the enum UNSTABLE, so a byte written\n"+
			"  inside one is a wire-compat hazard for a feature Java does not have.",
			len(before), len(after))
		for k := range after {
			if _, existed := before[k]; !existed {
				t.Errorf("  key added inside the store subspace: %x", k)
			}
		}
	}

	// Non-vacuity: a collector that did nothing at all would pass the above.
	out, err := runCmd(t, "stats", "show", "--database", db, "--schema", "main")
	if err != nil {
		t.Fatalf("stats show: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "USABLE") || strings.Contains(out, "NOT USABLE") {
		t.Fatalf("the collection did not produce usable statistics, so the "+
			"no-mutation assertion above proved nothing:\n%s", out)
	}
}

// snapshotStoreSubspace reads every key/value under one schema's record store,
// keyed by the raw key bytes. Uses the SAME subspace derivation the planner and
// the collector use, so it cannot drift from what it is asserting about.
func snapshotStoreSubspace(t *testing.T, database, schema string) map[string]string {
	t.Helper()
	ss, err := relationalStoreSubspace(database, schema)
	if err != nil {
		t.Fatalf("resolve store subspace: %v", err)
	}
	fdbDB, err := openDatabase(fixture.clusterFilePath)
	if err != nil {
		t.Fatalf("open FDB: %v", err)
	}
	rec := recordlayer.NewFDBDatabase(fdbDB)
	out := map[string]string{}
	_, err = rec.Run(context.Background(), func(rtx *recordlayer.FDBRecordContext) (any, error) {
		begin, end := ss.FDBRangeKeys()
		kvs, rErr := rtx.ReadTransaction(true).GetRange(
			fdb.KeyRange{Begin: fdb.Key(begin.FDBKey()), End: fdb.Key(end.FDBKey())},
			fdb.RangeOptions{}).GetSliceWithError()
		if rErr != nil {
			return nil, rErr
		}
		for _, kv := range kvs {
			out[string(kv.Key)] = string(kv.Value)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("snapshot store subspace: %v", err)
	}
	return out
}

// THE FAN-OUT REJECTS AN OUTPUT FORMAT IT CANNOT HONOUR.
//
// `--all-schemas` emits per-schema progress and a tally, not a single report, so
// there is nothing for `-o json` to render. Accepting the flag and printing text
// anyway hands a script unparseable output while exiting 0 — the worst of the
// three possible behaviours, because it reports success.
//
// This test exists because the fix for it was once reported as landed and had
// not been applied at all: the batch edit that was meant to add it died on an
// earlier anchor, the failure was not read, and the whole batch was written up as
// done. A claim that something is fixed is worth exactly as much as the test
// that runs it. No FDB needed — the flag is rejected before any connection.
func TestStats_AllSchemasRejectsAnUnrenderableOutputFormat(t *testing.T) {
	// json is the interesting format: it PASSES validateOutputFormat, so only
	// the fan-out's own gate can reject it. (yaml is rejected earlier, by the
	// format validator, with a different message — testing it here would be
	// asserting the wrong gate.)
	// No --database, for the same reason the control below omits it: if this gate
	// ever breaks, the command must fail on the NEXT check rather than dial a
	// cluster. With --database a broken gate costs 60 seconds of connection
	// timeout per run before failing.
	_, err := runCmd(t, "stats", "collect", "--all-schemas", "-o", "json")
	if err == nil {
		t.Fatal("--all-schemas -o json was accepted; want a rejection")
	}
	if !strings.Contains(err.Error(), "--all-schemas") {
		t.Errorf("error %q does not name the flag that makes the format unrenderable", err)
	}
	// The banner is title-cased by fang, so an error may not LEAD with a flag or
	// the operator reads "--Output". The repo gates this globally (pkg/docscheck);
	// asserted here too because this specific message was rewritten for that.
	if strings.HasPrefix(err.Error(), "-") {
		t.Errorf("error leads with a flag and will render title-cased: %q", err)
	}

	// CONTROL: -o text must get PAST the output gate. Proven without dialling
	// FDB by omitting --database, so the next check in the same branch is the
	// one that fires. An earlier version of this control passed --database and
	// spent 60s in a connection timeout to learn nothing.
	_, textErr := runCmd(t, "stats", "collect", "--all-schemas", "-o", "text")
	if textErr == nil {
		t.Fatal("expected --all-schemas without --database to be rejected")
	}
	if strings.Contains(textErr.Error(), "has nothing to render") {
		t.Errorf("-o text was rejected by the OUTPUT gate; it must reach the next check: %v", textErr)
	}
	if !strings.Contains(textErr.Error(), "--database") {
		t.Errorf("expected the missing-database error, which is what proves -o text passed "+
			"the output gate; got: %v", textErr)
	}
}
