package cmd

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"fdb.dev/cmd/fdb/internal/config"
)

func TestIntegration_SQLAndStats_RejectMissingSelectedContext(t *testing.T) {
	database := setupStatsDB(t, "missing_selection", statsOneTableDDL,
		"INSERT INTO items VALUES (1, 'alpha'), (2, 'beta');")
	t.Setenv("FDB_CLUSTER_FILE", fixture.clusterFilePath)
	address := []string{"--database", database, "--schema", "main"}
	if out, _, err := runSQLWorkflow(t, "", append([]string{"rl", "stats", "collect"}, address...)...); err != nil {
		t.Fatalf("seed statistics: %v\n%s", err, out)
	}
	storeBefore := snapshotStoreSubspace(t, database, "MAIN")
	if len(storeBefore) == 0 {
		t.Fatal("expected a populated fallback store")
	}
	readStats := func() statsShowResult {
		t.Helper()
		args := append([]string{"rl", "stats", "show", "--cluster-file", fixture.clusterFilePath, "-o", "json"}, address...)
		out, _, err := runSQLWorkflow(t, "", args...)
		if err != nil {
			t.Fatalf("read fallback statistics: %v\n%s", err, out)
		}
		var result statsShowResult
		if err := json.Unmarshal([]byte(out), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	statsBefore := readStats()
	if statsBefore.CollectedAtVersion == 0 || statsBefore.PerType["ITEMS"] != 2 {
		t.Fatalf("expected persisted two-record statistics: %+v", statsBefore)
	}
	path := filepath.Join(t.TempDir(), "broken.yaml")
	if err := os.WriteFile(path, []byte("current_context: deleted\ncontexts: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FDB_CONFIG", path)
	for _, selection := range []string{"implicit", "explicit"} {
		for _, command := range [][]string{
			{"sql", "-c", "DELETE FROM items"},
			{"rl", "stats", "show"},
			{"rl", "stats", "collect"},
			{"rl", "stats", "collect", "--all-schemas"},
			{"rl", "stats", "clear", "--yes"},
		} {
			t.Run(selection+"/"+strings.Join(command, " "), func(t *testing.T) {
				args := append([]string{}, command...)
				if !strings.Contains(strings.Join(command, " "), "--all-schemas") {
					args = append(args, address...)
				} else {
					args = append(args, "--database", database)
				}
				if selection == "explicit" {
					args = append(args, "--context", "deleted")
				}
				out, errOut, err := runSQLWorkflow(t, "", args...)
				var missing *config.ContextNotFoundError
				if !errors.Is(err, config.ErrNoContext) || !errors.As(err, &missing) || missing.Name != "deleted" || out != "" || errOut != "" {
					t.Errorf("missing selection reached fallback: stdout=%q stderr=%q error=%v", out, errOut, err)
				}
			})
		}
	}
	if after := snapshotStoreSubspace(t, database, "MAIN"); !reflect.DeepEqual(storeBefore, after) {
		t.Error("refused commands changed fallback record-store bytes")
	}
	statsAfter := readStats()
	if statsAfter.CollectedAtVersion != statsBefore.CollectedAtVersion || !reflect.DeepEqual(statsAfter.PerType, statsBefore.PerType) {
		t.Errorf("refused commands changed fallback statistics: before=%+v after=%+v", statsBefore, statsAfter)
	}
	if err := os.WriteFile(path, []byte("contexts: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, err := runSQLWorkflow(t, "", append([]string{"sql", "-o", "json", "-c", "SELECT id FROM items ORDER BY id"}, address...)...)
	var rows []map[string]any
	if err != nil || json.Unmarshal([]byte(out), &rows) != nil || len(rows) != 2 || rows[0]["ID"] != float64(1) || rows[1]["ID"] != float64(2) {
		t.Fatalf("unselected default discovery control: error=%v rows=%s", err, out)
	}
}
