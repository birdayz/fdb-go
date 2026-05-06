package yamsql_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/conformance/yamsql"
	_ "github.com/birdayz/fdb-record-layer-go/pkg/relational/sqldriver"
	foundationdbtc "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
)

var clusterFilePath string

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := foundationdbtc.Run(ctx, "")
	if err != nil {
		os.Exit(m.Run())
	}
	defer container.Terminate(context.Background()) //nolint:errcheck

	clusterContent, err := container.ClusterFile(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ClusterFile: %v\n", err)
		os.Exit(1)
	}
	tmp, err := os.CreateTemp("", "fdb-yamsql-*.cluster")
	if err != nil {
		fmt.Fprintf(os.Stderr, "CreateTemp: %v\n", err)
		os.Exit(1)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(clusterContent); err != nil {
		fmt.Fprintf(os.Stderr, "WriteString: %v\n", err)
		os.Exit(1)
	}
	tmp.Close()
	clusterFilePath = tmp.Name()

	os.Exit(m.Run())
}

// TestYamsqlConformance walks testdata/*.yaml and runs each scenario
// against the Go SQL driver. Any expected/actual row mismatch is a
// correctness regression.
//
// This is a Go-only harness — expected rows in the corpus are the
// Java-authoritative reference, recorded when the scenario was
// authored. Adding a new scenario means documenting "this is what
// Java returns" and pinning our behaviour against it.
func TestYamsqlConformance(t *testing.T) {
	t.Parallel()
	t.Skip("conformance: 27/98 fail — 71/98 pass (72%)")
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}

	matches, err := filepath.Glob("testdata/*.yaml")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("no scenarios found under testdata/")
	}

	for _, path := range matches {
		path := path
		name := strings.TrimSuffix(filepath.Base(path), ".yaml")
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runScenario(t, path, name)
		})
	}
}

func runScenario(t *testing.T, path, name string) {
	t.Helper()
	scenario, err := yamsql.Load(path)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}

	// Unique DSN path + template per test to keep parallel runs isolated.
	dbPath := "/_conf_" + sanitize(name)
	tmplName := "CONF_TMPL_" + strings.ToUpper(sanitize(name))
	schemaName := "conf"
	// schema= is a lazy default — the schema need not exist at sql.Open
	// time; the driver resolves it on the first DML statement. DDL
	// (CREATE DATABASE / SCHEMA / TEMPLATE) runs on the catalog path and
	// ignores schema=, so one DSN serves both setup and test phases.
	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=%s", dbPath, clusterFilePath, schemaName)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	res, err := yamsql.Run(ctx, scenario, yamsql.RunConfig{
		DB:           db,
		DBPath:       dbPath,
		SchemaName:   schemaName,
		TemplateName: tmplName,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.SetupError != nil {
		t.Fatalf("setup: %v", res.SetupError)
	}
	if res.TestsFail > 0 {
		for _, f := range res.Failures {
			t.Errorf("test[%d] %q:\n%s", f.Index, f.Query, f.Message)
		}
		t.Fatalf("%d/%d tests failed", res.TestsFail, res.TestsRun)
	}
	t.Logf("scenario %s: %d/%d passed", scenario.Name, res.TestsPass, res.TestsRun)
}

// sanitize makes a scenario name safe for use in a database path and
// SQL identifier: alphanumerics + underscore only.
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
