// End-to-end tests for `frl stats` — the offline planner-statistics
// maintainer (RFC-236). Shares the process-wide FDB testcontainer with the
// other integration suites; each test bootstraps its OWN database so the
// order they run in cannot change what they observe. That matters more here
// than elsewhere: every assertion is about stored state that a sibling test
// collecting or clearing would silently rewrite.
package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"fdb.dev/pkg/relational/core/embedded"

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
	// Crossing the cap ABORTS and stores nothing, and the command must EXIT
	// NON-ZERO for it. Exiting 0 tells scheduled automation the refresh
	// succeeded while the previous statistics sit there going stale — and the
	// fleet path already treats the same report as a per-target failure, so a
	// zero exit here would make one outcome mean two things depending on which
	// flag was used.
	db := setupStatsDB(t, "capped", statsOneTableDDL,
		"INSERT INTO items VALUES (1, 'a'), (2, 'b'), (3, 'c'), (4, 'd'), (5, 'e');")

	out, err := runCmd(t, "stats", "collect", "--database", db, "--schema", "main",
		"--max-records-per-type", "2")
	if err == nil {
		t.Fatalf("a capped collection exited 0 while storing nothing; automation would "+
			"read that as a successful refresh\noutput: %s", out)
	}
	if !strings.Contains(err.Error(), "aborted") || !strings.Contains(err.Error(), "ITEMS") {
		t.Errorf("the error must say it aborted and name the table that blew the budget: %v", err)
	}
	// The report still prints, so an operator sees what happened rather than
	// only that something did.
	if !strings.Contains(out, "ITEMS") {
		t.Errorf("expected the report to name the capped table:\n%s", out)
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

// COLLECTION MUST REFUSE METADATA WHOSE STATISTICS COULD NEVER BE USED.
//
// The reader rejects metadata declaring synthetic (joined/unnested) record types
// outright, because RecordTypes() omits them and completeness is undecidable
// against a partial list. Collecting anyway reads every record in the store to
// produce a set the planner has already decided to reject — and then `stats
// show` recommends collection as the remedy, which is the one action guaranteed
// not to help.
//
// The relational SQL layer used here does not build synthetic types, so this
// asserts the SHAPE of the refusal on the JSON surface instead: `synthetic_types`
// must exist in the output contract, because it is what makes the verdict
// actionable and it reached the text renderer without reaching JSON.
func TestStats_ShowJSONCarriesSyntheticTypes(t *testing.T) {
	db := setupStatsDB(t, "synjson", statsOneTableDDL, "INSERT INTO items VALUES (1, 'a');")
	if out, err := runCmd(t, "stats", "collect", "--database", db, "--schema", "main"); err != nil {
		t.Fatalf("stats collect: %v\noutput: %s", err, out)
	}
	out, err := runCmd(t, "stats", "show", "--database", db, "--schema", "main", "-o", "json")
	if err != nil {
		t.Fatalf("stats show -o json: %v\noutput: %s", err, out)
	}

	// The field is omitempty, so a healthy schema does not carry it — what is
	// asserted is that the DECODER knows about it, i.e. the JSON shape and the
	// text renderer agree on what a refusal can report. Without this, a
	// synthetic-type refusal renders in text and vanishes from `-o json`.
	var got statsShowResult
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode JSON: %v\noutput: %s", err, out)
	}
	if got.SyntheticTypes != nil {
		t.Errorf("this schema declares no synthetic types, so the field must be absent: %v",
			got.SyntheticTypes)
	}
	// Through the RENDERER, not by marshalling statsShowResult directly.
	// Marshalling the struct proves the json tag exists; it does not prove
	// renderStatsStatus ever copies st.SyntheticTypes into it. Drop that one
	// assignment and the production path loses the names while a direct-marshal
	// test stays green — which is the whole failure mode being pinned.
	var buf bytes.Buffer
	render := &cobra.Command{}
	render.SetOut(&buf)
	if rErr := renderStatsStatus(render, "json", "/x/MAIN", embedded.StatisticsStatus{
		Refusal:        embedded.StatisticsSyntheticTypes,
		SyntheticTypes: []string{"JoinedAB"},
	}); rErr != nil {
		t.Fatalf("renderStatsStatus: %v", rErr)
	}
	// NOTE FOR ANYONE SIMPLIFYING THE AMBIGUOUS FIXTURE BELOW: its names are
	// DOUBLE-ESCAPED on purpose. AmbiguousTypes arrives already decoded, and
	// ToUserIdentifier is not idempotent, so wrapping it in userNames() renames
	// the table rather than failing -- and this test is what catches that, but
	// only while the fixture can tell one decode from two. Replace MY__01TABLE
	// with A and the invariant stops being observable here.
	if !strings.Contains(buf.String(), `"synthetic_types"`) ||
		!strings.Contains(buf.String(), "JoinedAB") {
		t.Errorf("the renderer drops the synthetic type names on the JSON path: %s\n"+
			"  They render in text and disappear from -o json, losing exactly the "+
			"detail added to make the verdict actionable.", buf.String())
	}

	// Same shape for the ambiguous-name pair, and for the same reason: it is a
	// SECOND field the renderer has to copy, so it fails the same way
	// independently. Both names must survive -- either table can be renamed to
	// break the collision, so naming only one is half an instruction.
	buf.Reset()
	if rErr := renderStatsStatus(render, "json", "/x/MAIN", embedded.StatisticsStatus{
		Refusal:        embedded.StatisticsAmbiguousNames,
		AmbiguousTypes: []string{"MY__1TABLE", "MY__01TABLE"},
	}); rErr != nil {
		t.Fatalf("renderStatsStatus (ambiguous): %v", rErr)
	}
	for _, want := range []string{`"ambiguous_types"`, "MY__1TABLE", "MY__01TABLE"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("the renderer drops %s on the JSON path: %s", want, buf.String())
		}
	}
}

// AN ABORTED COLLECTION MUST NOT ANNOUNCE SUCCESS.
//
// RunE prints the report and THEN returns its non-zero error, so this renderer
// is reached on the aborted path too. An unconditional "collected statistics
// for X" headline announces success for a run that stored nothing — and the
// same output then lists the types as NOT collected, so the two halves of one
// message disagree. A reader takes the headline.
func TestStats_AbortedCollectionIsNotRenderedAsSuccess(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	render := &cobra.Command{}
	render.SetOut(&buf)

	// The aborted shape: nothing collected, and a reason per type.
	aborted := &recordlayer.CollectionReport{
		Collected: map[string]recordlayer.RecordTypeStatistic{},
		Skipped: map[string]string{
			"Order": "exceeds MaxRecordsPerType (50); collection aborted and stored nothing",
		},
		RecordsScanned: 51,
	}
	if err := renderCollectReport(render, "text", "/x/MAIN", aborted, time.Second); err != nil {
		t.Fatalf("renderCollectReport: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "collected statistics for") {
		t.Errorf("an aborted collection was announced as a success:\n%s", out)
	}
	if !strings.Contains(out, "ABORTED") {
		t.Errorf("an aborted collection must say so in its headline:\n%s", out)
	}

	// The success shape must still say so, or the fix above is just a rename.
	buf.Reset()
	ok := &recordlayer.CollectionReport{
		Collected:      map[string]recordlayer.RecordTypeStatistic{"Order": {Count: 7}},
		Skipped:        map[string]string{},
		RecordsScanned: 7,
	}
	if err := renderCollectReport(render, "text", "/x/MAIN", ok, time.Second); err != nil {
		t.Fatalf("renderCollectReport (success): %v", err)
	}
	if !strings.Contains(buf.String(), "collected statistics for") {
		t.Errorf("a successful collection stopped saying so:\n%s", buf.String())
	}
}

// A FAILED READ IS UNKNOWN, NOT EMPTY.
//
// decideStatistics returns StatisticsReadFailed with Found==false because
// existence is UNKNOWN — the read itself failed. Reporting "nothing is stored"
// turns a transient, permission or cluster fault into a confident statement of
// absence, and an operator who believes it runs collect, which does not
// diagnose the fault either. This is the same absent-versus-failed conflation
// the read side spent this feature removing, surfacing in the UI instead.
func TestStats_FailedReadIsReportedAsUnknownNotAbsent(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	render := &cobra.Command{}
	render.SetOut(&buf)
	if err := renderStatsStatus(render, "text", "/x/MAIN", embedded.StatisticsStatus{
		Refusal: embedded.StatisticsReadFailed,
		ReadErr: errors.New("operation_failed on GetRange"),
	}); err != nil {
		t.Fatalf("renderStatsStatus: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "nothing is stored") {
		t.Errorf("a FAILED read was reported as absence:\n%s", out)
	}
	if !strings.Contains(out, "UNKNOWN") {
		t.Errorf("a failed read must say existence is unknown:\n%s", out)
	}
	if !strings.Contains(out, "operation_failed on GetRange") {
		t.Errorf("the underlying error is what an operator needs and it is missing:\n%s", out)
	}
}

// A TORN SET IS STORED AND REPAIRABLE — THE DEFAULT ARM SAYS THE OPPOSITE OF BOTH.
//
// StatisticsTorn was added to stop the gate reporting torn sets as absent. It
// then landed in this renderer's default arm, which says "nothing is stored, and
// collection will not help" — wrong twice over: something IS stored, and a
// collect is exactly the repair, since it ClearRanges the range and rewrites
// header and entries in one transaction.
//
// Adding a refusal to a gate and forgetting the layer that renders it is the
// same gap this feature has now hit three times, in three different layers.
func TestStats_TornSetIsRenderedAsStoredAndRepairable(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	render := &cobra.Command{}
	render.SetOut(&buf)
	if err := renderStatsStatus(render, "text", "/x/MAIN", embedded.StatisticsStatus{
		Refusal:     embedded.StatisticsTorn,
		ReadRefusal: recordlayer.StatisticsReadCountMismatch,
	}); err != nil {
		t.Fatalf("renderStatsStatus: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "nothing is stored") {
		t.Errorf("a TORN set was reported as absent:\n%s", out)
	}
	if strings.Contains(out, "collection will not help") {
		t.Errorf("a torn set is repaired by collecting; the output says it is not:\n%s", out)
	}
	if !strings.Contains(out, "ARE stored") {
		t.Errorf("a torn set must say something is stored:\n%s", out)
	}
	// The specific way it is broken is what an operator acts on.
	if !strings.Contains(out, string(recordlayer.StatisticsReadCountMismatch)) {
		t.Errorf("the read's own reason is missing, so the operator cannot tell WHICH "+
			"way the set is torn:\n%s", out)
	}
}

// EVERY REFUSAL THAT LEAVES Found FALSE MUST BE CLASSIFIED HERE.
//
// renderStatsStatus's hint block runs only when Found is false, and its default
// arm says "nothing is stored, and collection will not help". That is right for
// exactly one refusal today and wrong for anything else that lands there — which
// is not hypothetical: StatisticsTorn was added to the gate and fell straight
// into it, producing "nothing is stored, and collection will not help: stored
// statistics are torn or unreadable", a line that contradicts itself twice.
//
// So the classification is enforced rather than remembered. Adding a refusal
// that leaves Found false means adding it here, and a wrong classification fails
// rather than shipping a self-contradicting sentence to an operator.
//
// The list is hand-maintained and carries the same acknowledged hole as its
// siblings: a constant absent from both this list and the switch cannot be
// caught, because Go cannot enumerate constants at runtime. What it does catch
// is the case that actually happened — a refusal added to the gate and rendered
// by a default arm that describes something else.
func TestStats_EveryNotFoundRefusalIsClassified(t *testing.T) {
	t.Parallel()

	// stored: does the refusal mean statistics EXIST in the store?
	// repairable: would `frl stats collect` fix it?
	// claimsAbsence: may the output state that nothing is stored? ONLY a
	// refusal that actually READ an empty range may. Torn read entries;
	// ReadFailed could not read; SyntheticTypes deliberately did not read at
	// all -- and a schema collected before its metadata was rebound to declare a
	// joined type still has its old entries sitting there.
	// repairable: would `frl stats collect` fix it?
	cases := []struct {
		refusal       embedded.StatisticsRefusal
		claimsAbsence bool
		repairable    bool
	}{
		{embedded.StatisticsNotCollected, true, true},
		{embedded.StatisticsTorn, false, true},
		{embedded.StatisticsReadFailed, false, false},
		{embedded.StatisticsSyntheticTypes, false, false},
		// Same as synthetic: decided from metadata without a read, so it may not
		// claim absence, and collection cannot repair it either.
		{embedded.StatisticsAmbiguousNames, false, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.refusal), func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			render := &cobra.Command{}
			render.SetOut(&buf)
			if err := renderStatsStatus(render, "text", "/x/MAIN", embedded.StatisticsStatus{
				Refusal: tc.refusal,
			}); err != nil {
				t.Fatalf("renderStatsStatus: %v", err)
			}
			out := buf.String()

			// Only a refusal that READ an empty range may claim absence. Every
			// other one either saw entries or never looked, and asserting absence
			// from a read that did not happen is the conflation this feature has
			// now had to remove at four separate layers.
			if !tc.claimsAbsence && strings.Contains(out, "nothing is stored") {
				t.Errorf("%q does not establish absence, rendered as absent:\n%s",
					tc.refusal, out)
			}
			// A refusal a collect would repair must never say collection is futile.
			if tc.repairable && strings.Contains(out, "collection will not help") {
				t.Errorf("%q is repaired by collecting, rendered as permanent:\n%s",
					tc.refusal, out)
			}
			// And the converse, or the assertions above are satisfiable by an
			// arm that says nothing at all.
			if !tc.repairable && strings.Contains(out, "run `frl stats collect`") {
				t.Errorf("%q cannot be fixed by collecting, yet the output recommends it:\n%s",
					tc.refusal, out)
			}
			if strings.TrimSpace(out) == "" {
				t.Errorf("%q rendered nothing at all", tc.refusal)
			}
		})
	}
}

// A DIAGNOSIS FIELD MUST REACH BOTH RENDER PATHS.
//
// ReadRefusal and ReadError were added so an operator could act: WHICH way a
// stored set is torn, and WHAT failed when a read failed. Both were populated
// on the text path only, so `stats show -o json` reported that statistics were
// unusable and never why — the exact asymmetry the synthetic and ambiguous
// names are on both paths to avoid.
//
// Driven through renderStatsStatus, not by marshalling statsShowResult: the
// struct having a json tag proves nothing about the renderer copying the field
// into it, which is the half that broke.
func TestStats_ShowJSONCarriesTheReadDiagnosis(t *testing.T) {
	t.Parallel()

	decode := func(t *testing.T, st embedded.StatisticsStatus) map[string]any {
		t.Helper()
		var buf bytes.Buffer
		render := &cobra.Command{}
		render.SetOut(&buf)
		if err := renderStatsStatus(render, "json", "/x/MAIN", st); err != nil {
			t.Fatalf("renderStatsStatus: %v", err)
		}
		var got map[string]any
		if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
			t.Fatalf("decode %s: %v", buf.String(), err)
		}
		return got
	}

	torn := decode(t, embedded.StatisticsStatus{
		Refusal:     embedded.StatisticsTorn,
		ReadRefusal: recordlayer.StatisticsReadCountMismatch,
	})
	if got, _ := torn["read_refusal"].(string); got != string(recordlayer.StatisticsReadCountMismatch) {
		t.Errorf("read_refusal = %q, want %q — JSON says the set is unusable and not "+
			"which way, while the text path names it",
			got, recordlayer.StatisticsReadCountMismatch)
	}

	failed := decode(t, embedded.StatisticsStatus{
		Refusal: embedded.StatisticsReadFailed,
		ReadErr: errors.New("operation_failed on GetRange"),
	})
	if got, _ := failed["read_error"].(string); !strings.Contains(got, "operation_failed on GetRange") {
		t.Errorf("read_error = %q, want the underlying failure — an automated caller "+
			"cannot distinguish a transient fault from a permanent one without it", got)
	}

	// And absent when there is nothing to report, or omitempty is decoration:
	// a field that is always present carries no signal.
	clean := decode(t, embedded.StatisticsStatus{Usable: true, Found: true})
	if _, present := clean["read_refusal"]; present {
		t.Errorf("read_refusal present on a usable set: %v", clean)
	}
	if _, present := clean["read_error"]; present {
		t.Errorf("read_error present on a usable set: %v", clean)
	}
}

// EVERY DIAGNOSIS FIELD REACHES BOTH RENDER PATHS — AS A PROPERTY, NOT A CHECK.
//
// This asymmetry has now been found twice, and the second time only because the
// first was a spot-check. AmbiguousTypes was verified on both paths when it was
// added; that verification was never turned into a property, so ReadRefusal and
// ReadError were added later and reached only the text path — an operator using
// -o json saw THAT statistics were unusable and never WHY.
//
// So the property is enforced over every field carrying a diagnosis. A field
// listed here must appear in BOTH renderings when populated, and in neither when
// not. Adding a diagnosis field means adding a row; the hand-maintained list
// carries the same acknowledged hole as its siblings — a field absent from both
// this list and the renderer cannot be caught — but the case that actually
// happened, a field wired to one path, cannot pass.
func TestStats_EveryDiagnosisFieldReachesBothRenderPaths(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		st       embedded.StatisticsStatus
		jsonKey  string
		wantText string
	}{
		{
			name:     "synthetic types",
			st:       embedded.StatisticsStatus{Refusal: embedded.StatisticsSyntheticTypes, SyntheticTypes: []string{"JoinedAB"}},
			jsonKey:  "synthetic_types",
			wantText: "JoinedAB",
		},
		{
			name:     "ambiguous types",
			st:       embedded.StatisticsStatus{Refusal: embedded.StatisticsAmbiguousNames, AmbiguousTypes: []string{"MY__1T", "MY__01T"}},
			jsonKey:  "ambiguous_types",
			wantText: "MY__01T",
		},
		{
			name:     "read refusal",
			st:       embedded.StatisticsStatus{Refusal: embedded.StatisticsTorn, ReadRefusal: recordlayer.StatisticsReadCountMismatch},
			jsonKey:  "read_refusal",
			wantText: string(recordlayer.StatisticsReadCountMismatch),
		},
		{
			name:     "read error",
			st:       embedded.StatisticsStatus{Refusal: embedded.StatisticsReadFailed, ReadErr: errors.New("boom on GetRange")},
			jsonKey:  "read_error",
			wantText: "boom on GetRange",
		},
		{
			name:     "missing types",
			st:       embedded.StatisticsStatus{Refusal: embedded.StatisticsIncomplete, Found: true, MissingTypes: []string{"Gone"}},
			jsonKey:  "missing_types",
			wantText: "Gone",
		},
		{
			name:     "extra types",
			st:       embedded.StatisticsStatus{Usable: true, Found: true, ExtraTypes: []string{"Orphan"}},
			jsonKey:  "extra_types",
			wantText: "Orphan",
		},
	}

	render := func(t *testing.T, format string, st embedded.StatisticsStatus) string {
		t.Helper()
		var buf bytes.Buffer
		c := &cobra.Command{}
		c.SetOut(&buf)
		if err := renderStatsStatus(c, format, "/x/MAIN", st); err != nil {
			t.Fatalf("renderStatsStatus(%s): %v", format, err)
		}
		return buf.String()
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if text := render(t, "text", tc.st); !strings.Contains(text, tc.wantText) {
				t.Errorf("the TEXT path drops %s (%q missing):\n%s", tc.name, tc.wantText, text)
			}

			var got map[string]any
			raw := render(t, "json", tc.st)
			if err := json.Unmarshal([]byte(raw), &got); err != nil {
				t.Fatalf("decode %s: %v", raw, err)
			}
			if _, present := got[tc.jsonKey]; !present {
				t.Errorf("the JSON path drops %s (key %q absent) — a diagnosis on one "+
					"render path is half a diagnosis:\n%s", tc.name, tc.jsonKey, raw)
			}
		})
	}

	// The converse, over the SAME key set: a healthy verdict must carry none of
	// them. Without this, a renderer emitting every key unconditionally would
	// satisfy every assertion above while telling a reader nothing.
	var healthy map[string]any
	raw := render(t, "json", embedded.StatisticsStatus{Usable: true, Found: true})
	if err := json.Unmarshal([]byte(raw), &healthy); err != nil {
		t.Fatalf("decode healthy: %v", err)
	}
	for _, tc := range cases {
		if _, present := healthy[tc.jsonKey]; present {
			t.Errorf("%q is present on a healthy verdict, so its presence carries no "+
				"signal:\n%s", tc.jsonKey, raw)
		}
	}
}

// EXISTENCE IS TRI-STATE AND THE JSON MUST SAY SO.
//
// `found` was a plain bool, so three distinct facts serialised identically as
// false: the store is empty, the store holds a TORN set whose entries were
// read, and existence was never established (a failed read, or the synthetic
// preflight which deliberately does not look). The second is known-wrong and the
// third is unknown — telling a machine consumer "false" for either is the same
// absent-versus-unknown conflation this feature removed from the reader, at the
// last layer that still had it.
func TestStats_ShowJSONFoundIsTriState(t *testing.T) {
	t.Parallel()

	decode := func(t *testing.T, st embedded.StatisticsStatus) map[string]any {
		t.Helper()
		var buf bytes.Buffer
		c := &cobra.Command{}
		c.SetOut(&buf)
		if err := renderStatsStatus(c, "json", "/x/MAIN", st); err != nil {
			t.Fatalf("renderStatsStatus: %v", err)
		}
		var got map[string]any
		if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
			t.Fatalf("decode %s: %v", buf.String(), err)
		}
		return got
	}

	cases := []struct {
		name string
		st   embedded.StatisticsStatus
		want any // true, false, or nil for unknown
	}{
		{"usable", embedded.StatisticsStatus{Usable: true, Found: true}, true},
		{"absent", embedded.StatisticsStatus{Refusal: embedded.StatisticsNotCollected}, false},
		{"torn: entries WERE read", embedded.StatisticsStatus{Refusal: embedded.StatisticsTorn}, true},
		{"read failed: unknown", embedded.StatisticsStatus{Refusal: embedded.StatisticsReadFailed}, nil},
		// Ambiguity joined the unknown set when its verdict stopped paying a read.
		// Without this case, dropping it from the nil arm restores `found: false` --
		// the known-absence lie -- with the whole package still green.
		{"ambiguous: never looked", embedded.StatisticsStatus{Refusal: embedded.StatisticsAmbiguousNames}, nil},
		{"synthetic preflight: never looked", embedded.StatisticsStatus{Refusal: embedded.StatisticsSyntheticTypes}, nil},
		// Expired/incomplete DID read a set, so existence is known true.
		{"expired", embedded.StatisticsStatus{Refusal: embedded.StatisticsExpired, Found: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := decode(t, tc.st)
			v, present := got["found"]
			if !present {
				t.Fatalf("`found` is absent entirely; it must be present and nullable: %v", got)
			}
			if v != tc.want {
				t.Errorf("found = %v (%T), want %v — %q", v, v, tc.want, tc.name)
			}
		})
	}
}

// OPERATOR-FACING NAMES ARE SQL IDENTIFIERS, NOT STORAGE NAMES.
//
// Metadata and the collector key by ESCAPED names — a table quoted as
// "MY$TABLE" is stored as MY__1TABLE — and every rendering surface copied those
// keys verbatim. That names a table the operator does not have. `per_type` is a
// documented interface, so a script keying by table name silently misses rather
// than failing, which is the worse direction.
//
// Only the AMBIGUOUS pair was decoded when that gate was added: the copy in
// front of me, not the class. This covers the SHOW path; the collect path has
// its own test, because when this one claimed to "cover the class" the collect
// decode could be reverted entirely with the suite still green.
//
// SYNTHETIC types are the documented EXCEPTION and are covered by their own
// test below: their names are not known to be escaped, so decoding them would
// invent a declaration. See SyntheticRecordTypesNotModeledError.Error().
func TestStats_OperatorFacingNamesAreDecoded(t *testing.T) {
	t.Parallel()

	const storage, sql = "MY__1TABLE", "MY$TABLE"
	if got := recordlayer.ToUserIdentifier(storage); got != sql {
		t.Fatalf("fixture is wrong: %q decodes to %q, not %q — this test would then "+
			"assert nothing about decoding", storage, got, sql)
	}

	render := func(t *testing.T, format string, st embedded.StatisticsStatus) string {
		t.Helper()
		var buf bytes.Buffer
		c := &cobra.Command{}
		c.SetOut(&buf)
		if err := renderStatsStatus(c, format, "/x/MAIN", st); err != nil {
			t.Fatalf("renderStatsStatus(%s): %v", format, err)
		}
		return buf.String()
	}

	cases := []struct {
		name string
		st   embedded.StatisticsStatus
	}{
		{"per_type", embedded.StatisticsStatus{
			Usable: true, Found: true,
			Stats: recordlayer.StoreStatistics{
				PerType: map[string]recordlayer.RecordTypeStatistic{storage: {Count: 7}},
			},
		}},
		{"missing_types", embedded.StatisticsStatus{
			Refusal: embedded.StatisticsIncomplete, Found: true, MissingTypes: []string{storage},
		}},
		{"extra_types", embedded.StatisticsStatus{
			Usable: true, Found: true, ExtraTypes: []string{storage},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st := tc.st
			for _, format := range []string{"text", "json"} {
				out := render(t, format, st)
				if !strings.Contains(out, sql) {
					t.Errorf("%s/%s does not name the table by its SQL identifier %q:\n%s",
						tc.name, format, sql, out)
				}
				// And the raw storage name must NOT leak alongside it, or the
				// assertion above is satisfied by printing both.
				if strings.Contains(out, storage) {
					t.Errorf("%s/%s leaks the storage name %q:\n%s",
						tc.name, format, storage, out)
				}
			}
		})
	}
}

// THE COLLECT PATH DECODES TOO — REPORT BODY AND ERROR BANNER ALIKE.
//
// Reverting every decode in renderCollectReport left the suite green, because
// the name test drove only renderStatsStatus while its comment claimed to cover
// the class. Two surfaces, one of them tested, is the shape this whole PR exists
// to fix.
//
// The banner matters as much as the body: on an aborted run they print three
// lines apart, so leaving one raw showed MY$TABLE in the not-collected block and
// MY__1TABLE in the error beneath it — one table, two spellings, same output.
func TestStats_CollectPathDecodesNames(t *testing.T) {
	t.Parallel()

	const storage, sql = "MY__1TABLE", "MY$TABLE"
	if recordlayer.ToUserIdentifier(storage) != sql {
		t.Fatalf("fixture is wrong: %q does not decode to %q", storage, sql)
	}

	render := func(t *testing.T, format string, r *recordlayer.CollectionReport) string {
		t.Helper()
		var buf bytes.Buffer
		c := &cobra.Command{}
		c.SetOut(&buf)
		if err := renderCollectReport(c, format, "/x/MAIN", r, time.Second); err != nil {
			t.Fatalf("renderCollectReport(%s): %v", format, err)
		}
		return buf.String()
	}

	t.Run("collected body", func(t *testing.T) {
		t.Parallel()
		r := &recordlayer.CollectionReport{
			Collected:      map[string]recordlayer.RecordTypeStatistic{storage: {Count: 7}},
			Skipped:        map[string]string{},
			RecordsScanned: 7,
		}
		for _, format := range []string{"text", "json"} {
			out := render(t, format, r)
			if !strings.Contains(out, sql) {
				t.Errorf("collect/%s does not name the table by its SQL identifier:\n%s", format, out)
			}
			if strings.Contains(out, storage) {
				t.Errorf("collect/%s leaks the storage name:\n%s", format, out)
			}
		}
	})

	t.Run("skipped body", func(t *testing.T) {
		t.Parallel()
		r := &recordlayer.CollectionReport{
			Collected:      map[string]recordlayer.RecordTypeStatistic{},
			Skipped:        map[string]string{storage: "exceeds MaxRecordsPerType"},
			RecordsScanned: 51,
		}
		for _, format := range []string{"text", "json"} {
			out := render(t, format, r)
			if !strings.Contains(out, sql) {
				t.Errorf("collect-skipped/%s does not name the table by its SQL "+
					"identifier:\n%s", format, out)
			}
			if strings.Contains(out, storage) {
				t.Errorf("collect-skipped/%s leaks the storage name:\n%s", format, out)
			}
		}
	})

	t.Run("abort error banner", func(t *testing.T) {
		t.Parallel()
		// The banner is built by describeSkippedTypes and printed BELOW an
		// already-decoded body, so a raw name here contradicts the lines above it.
		got := describeSkippedTypes(userName, map[string]string{storage: "exceeds MaxRecordsPerType"})
		if !strings.Contains(got, sql) {
			t.Errorf("the abort banner does not name the table by its SQL identifier: %s", got)
		}
		if strings.Contains(got, storage) {
			t.Errorf("the abort banner leaks the storage name: %s", got)
		}
	})

	t.Run("abort banner orders by the decoded name", func(t *testing.T) {
		t.Parallel()
		// One entry cannot observe a sort, so the arm above passes whether the
		// helper sorts in storage space or user space. A__0B decodes to A__B and
		// A__1B decodes to A$B, so the two orders DISAGREE: storage-sorted prints
		// A__B first, user-sorted prints A$B first.
		got := describeSkippedTypes(userName, map[string]string{
			"A__0B": "storage-first",
			"A__1B": "user-first",
		})
		if !strings.HasPrefix(got, "A$B") {
			t.Errorf("the abort banner is not ordered by the name it prints: %s", got)
		}
		// Guard the fixture: if decoding ever stopped changing the order, the
		// assertion above would hold for a helper that never sorts by user name.
		// Storage order is A__0B < A__1B; decoded it is A$B < A__B, because $ is
		// 0x24 and _ is 0x5F. The two orders must DISAGREE or the assertion above
		// holds for a helper that never sorts by the user name at all.
		if userName("A__0B") <= userName("A__1B") {
			t.Fatalf("fixture is vacuous: decoding no longer reverses %s/%s",
				userName("A__0B"), userName("A__1B"))
		}
	})
}

// LISTS ARE ORDERED BY THE NAME ACTUALLY PRINTED.
//
// The gate sorts these lists in STORAGE space, correctly, since that is the
// namespace it holds them in. The renderer decodes them — and the two orders
// differ: storage-sorted [A__0B, A__1B] prints as [A__B, A$B], while a reader
// scanning the output expects [A$B, A__B].
//
// Decoding is funnelled through a closed set of helpers (userName/userNames/
// userNamesFor/userNameFor/userFieldNames/userKeyed, plus fleet's
// describeSkipped) rather than scattered at call sites -- see stats.go, where
// the call-site list was wrong three times before being replaced by the helper
// set. This test covers userNames; the others carry their own arms.
//
// SyntheticRecordTypesNotModeledError.Error() decodes nothing and preserves
// input order, pinned by TestSyntheticRefusalErrorNamesTypesVerbatim.
func TestStats_ListsAreSortedByTheDecodedName(t *testing.T) {
	t.Parallel()

	// Storage order and user order DISAGREE for this pair, which is what makes
	// the assertion meaningful: A__0B < A__1B as stored, A$B < A__B as printed.
	storage := []string{"A__0B", "A__1B"}
	if !sort.StringsAreSorted(storage) {
		t.Fatalf("fixture is not storage-sorted: %v", storage)
	}
	wantUser := []string{"A$B", "A__B"}
	got := userNames(storage)
	if len(got) != 2 || got[0] != wantUser[0] || got[1] != wantUser[1] {
		t.Fatalf("userNames(%v) = %v, want %v — decoded names must be re-sorted, or a "+
			"storage-space order is printed in user space", storage, got, wantUser)
	}
	// Guard the fixture: if these ever decode to the same relative order, the
	// test passes without exercising the re-sort.
	decodedInPlace := []string{userName(storage[0]), userName(storage[1])}
	if sort.StringsAreSorted(decodedInPlace) {
		t.Fatalf("decoding %v preserves order (%v), so this test cannot observe the "+
			"re-sort it exists to pin", storage, decodedInPlace)
	}
}

// SYNTHETIC TYPE NAMES ARE THE EXCEPTION: RENDERED VERBATIM.
//
// Every other operator-facing name here is decoded, because it provably came
// from escaping a SQL identifier. A joined/unnested type is named by an
// arbitrary string handed to Java's addJoinedRecordType and stored verbatim,
// and this port never creates one — so MY__1JOINED is ambiguous between the
// escaping of MY$JOINED and a literal MY__1JOINED, and the decoded reading
// names something the operator cannot find in their metadata.
//
// This test is the counterweight to TestStats_OperatorFacingNamesAreDecoded: if
// someone "completes the class" by decoding synthetic names too, it fails.
func TestStats_SyntheticTypeNamesAreRenderedVerbatim(t *testing.T) {
	t.Parallel()

	const stored, ifDecoded = "MY__1JOINED", "MY$JOINED"
	if recordlayer.ToUserIdentifier(stored) != ifDecoded {
		t.Fatalf("fixture is vacuous: %q does not decode to %q, so a decoding "+
			"regression would be invisible here", stored, ifDecoded)
	}

	st := embedded.StatisticsStatus{
		Refusal:        embedded.StatisticsSyntheticTypes,
		SyntheticTypes: []string{stored},
	}
	for _, format := range []string{"text", "json"} {
		var buf bytes.Buffer
		c := &cobra.Command{}
		c.SetOut(&buf)
		if err := renderStatsStatus(c, format, "/x/MAIN", st); err != nil {
			t.Fatalf("renderStatsStatus(%s): %v", format, err)
		}
		out := buf.String()
		if !strings.Contains(out, stored) {
			t.Errorf("%s does not name the declaration as stored (%q):\n%s", format, stored, out)
		}
		if strings.Contains(out, ifDecoded) {
			t.Errorf("%s DECODED a synthetic type name to %q:\n%s\nJava stores these "+
				"verbatim and this port never creates one, so the escaped reading is a "+
				"guess", format, ifDecoded, out)
		}
	}
}

// ONE OUTPUT NEVER PRINTS TWO TABLES UNDER ONE NAME, EVEN ACROSS ITS LISTS.
//
// Stored MY__1TABLE and MY__01TABLE decode to MY$TABLE and MY__1TABLE, so the
// second would print under the FIRST one's stored name and a reader could not
// tell which table it means.
//
// The decision has to range over the whole OUTPUT, not each map: harden
// `collected` and `skipped` separately and the pair straddles them — one name
// in each, neither map sees a collision, each is individually correct, and the
// printed report still shows the same label twice for two different
// stored types. That is the shape this drives.
func TestOneOutputNeverPrintsTwoTablesUnderOneName(t *testing.T) {
	t.Parallel()

	const a, b = "MY__1TABLE", "MY__01TABLE"
	if recordlayer.ToUserIdentifier(b) != a {
		t.Fatalf("fixture is vacuous: %q decodes to %q, not %q", b, recordlayer.ToUserIdentifier(b), a)
	}

	t.Run("straddling two maps", func(t *testing.T) {
		t.Parallel()
		// One name in each map — the case a per-map guard cannot see.
		decode := safeDecoderOver([]string{a, b})
		if got := decode(a); got != a {
			t.Errorf("decode(%q) = %q; under a collision every name must stay stored", a, got)
		}
		if got := decode(b); got != b {
			t.Errorf("decode(%q) = %q; under a collision every name must stay stored", b, got)
		}
	})

	t.Run("no collision still decodes", func(t *testing.T) {
		t.Parallel()
		// The fallback must not fire spuriously, or every operator reads storage
		// names forever.
		decode := safeDecoderOver([]string{a, "OTHER"})
		if got := decode(a); got != "MY$TABLE" {
			t.Errorf("decode(%q) = %q, want the SQL identifier MY$TABLE", a, got)
		}
	})

	t.Run("keyedBy applies one decision to a whole map", func(t *testing.T) {
		t.Parallel()
		decode := safeDecoderOver([]string{a, b})
		got := keyedBy(decode, map[string]int64{a: 9, b: 4000})
		if len(got) != 2 || got[a] != 9 || got[b] != 4000 {
			t.Errorf("rows are not keyed by their own stored names: %v", got)
		}
	})
}

// THE RENDERERS THEMSELVES FEED THE WHOLE UNION, not just one of their maps.
//
// The bug was never in the decision — it was in WHICH names get fed to it.
// Testing safeDecoderOver in isolation pins the decision and leaves the wiring
// free: narrow either call site back to one map and the helper still behaves
// perfectly while the rendered output shows one label for two stored types.
// So this drives the real renderers with the pair SPLIT across their two
// sources.
func TestRenderersFeedTheWholeUnionToTheDecoder(t *testing.T) {
	t.Parallel()

	// MY__01TABLE decodes to MY__1TABLE, which is the other name verbatim.
	const a, b = "MY__1TABLE", "MY__01TABLE"
	if recordlayer.ToUserIdentifier(b) != a {
		t.Fatalf("fixture is vacuous: %q decodes to %q", b, recordlayer.ToUserIdentifier(b))
	}

	render := func(t *testing.T, fn func(*cobra.Command) error) string {
		t.Helper()
		var buf bytes.Buffer
		c := &cobra.Command{}
		c.SetOut(&buf)
		if err := fn(c); err != nil {
			t.Fatalf("render: %v", err)
		}
		return buf.String()
	}

	t.Run("collect: pair split across collected and skipped", func(t *testing.T) {
		t.Parallel()
		rep := &recordlayer.CollectionReport{
			Collected: map[string]recordlayer.RecordTypeStatistic{a: {Count: 7}},
			Skipped:   map[string]string{b: "exceeds MaxRecordsPerType"},
		}
		out := render(t, func(c *cobra.Command) error {
			return renderCollectReport(c, "json", "/x/MAIN", rep, time.Second)
		})
		// Decoding b yields a's stored name, so under the split BOTH must print
		// stored. Seeing MY$TABLE means only `collected` was fed to the decision.
		if strings.Contains(out, "MY$TABLE") {
			t.Errorf("collected was decoded while skipped straddled the collision — "+
				"the decision saw only one map:\n%s", out)
		}
		for _, want := range []string{a, b} {
			if !strings.Contains(out, want) {
				t.Errorf("output does not name %q by its stored spelling:\n%s", want, out)
			}
		}
	})

	t.Run("show: pair split across per_type and missing_types", func(t *testing.T) {
		t.Parallel()
		st := embedded.StatisticsStatus{
			Usable: true, Found: true,
			Stats:        recordlayer.StoreStatistics{PerType: map[string]recordlayer.RecordTypeStatistic{a: {Count: 7}}},
			MissingTypes: []string{b},
		}
		out := render(t, func(c *cobra.Command) error {
			return renderStatsStatus(c, "json", "/x/MAIN", st)
		})
		if strings.Contains(out, "MY$TABLE") {
			t.Errorf("per_type was decoded while missing_types straddled the collision "+
				"— the decision saw only one list:\n%s", out)
		}
		for _, want := range []string{a, b} {
			if !strings.Contains(out, want) {
				t.Errorf("output does not name %q by its stored spelling:\n%s", want, out)
			}
		}
	})
}
