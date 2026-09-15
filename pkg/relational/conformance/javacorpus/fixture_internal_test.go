package javacorpus

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/conformance/javayamsql"
	"fdb.dev/pkg/relational/sqldriver"
	"fdb.dev/pkg/simfdb"
)

func fixtureFile(t *testing.T) *javayamsql.File {
	t.Helper()
	file, err := javayamsql.Parse("fixture.yamsql", []byte(`---
schema_template: CREATE TABLE t (id BIGINT, a BIGINT, PRIMARY KEY (id)) CREATE TABLE u (id BIGINT, PRIMARY KEY (id))
---
setup:
  connect: 1
  steps:
    - query: INSERT INTO t VALUES (1, 100)
---
test_block:
  connect: 1
  preset: single_repetition_ordered
  tests:
    - - query: SELECT COUNT(*) AS N FROM t
      - result: [{N: 1}]
`))
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func TestPrivateFixtureValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		edit func(*javayamsql.File)
		want bool
	}{
		{"local", func(*javayamsql.File) {}, true},
		{"closed_cast", func(f *javayamsql.File) {
			f.Blocks[1].Setup.Steps[0].Query = "INSERT INTO t VALUES (1, CAST(-0.0 AS BIGINT))"
		}, true},
		{"qualified", func(f *javayamsql.File) {
			f.Blocks[1].Setup.Steps[0].Query = "INSERT INTO YAML_PIN_1_SCHEMA.t VALUES (1, 100)"
		}, true},
		{"quoted_dotted", func(f *javayamsql.File) {
			f.Blocks[0].SchemaTemplate.Variants[0].Definition = `CREATE TABLE "a.b" (id BIGINT, PRIMARY KEY (id))`
			f.Blocks[1].Setup.Steps[0].Query = `INSERT INTO "a.b" VALUES (1)`
		}, true},
		{"include", func(f *javayamsql.File) {
			f.Blocks = append(f.Blocks, &javayamsql.Block{Kind: javayamsql.BlockInclude})
		}, false},
		{"wrong_order", func(f *javayamsql.File) { f.Blocks[0], f.Blocks[1] = f.Blocks[1], f.Blocks[0] }, false},
		{"external_connect", func(f *javayamsql.File) {
			f.Blocks[1].Setup.Connect = &javayamsql.Value{Kind: javayamsql.KindString, Str: "jdbc:embed:/other?schema=S"}
		}, false},
		{"system_connect", func(f *javayamsql.File) { f.Blocks[1].Setup.Connect.Int = 0 }, false},
		{"test_external_connect", func(f *javayamsql.File) { f.Blocks[2].Test.Connect.Int = 2 }, false},
		{"empty_setup", func(f *javayamsql.File) { f.Blocks[1].Setup.Steps = nil }, false},
		{"empty_tests", func(f *javayamsql.File) { f.Blocks[2].Test.Tests = nil }, false},
		{"assertion_in_setup", func(f *javayamsql.File) {
			f.Blocks[1].Setup.Steps[0].Configs = []*javayamsql.Config{{Kind: javayamsql.ConfigCount}}
		}, false},
		{"template_variant", func(f *javayamsql.File) { f.Blocks[0].SchemaTemplate.ListForm = true }, false},
		{"duplicate_table", func(f *javayamsql.File) {
			f.Blocks[0].SchemaTemplate.Variants[0].Definition += " CREATE TABLE T (id BIGINT, PRIMARY KEY (id))"
		}, false},
		{"no_tables", func(f *javayamsql.File) {
			f.Blocks[0].SchemaTemplate.Variants[0].Definition = "CREATE TYPE AS STRUCT x (id BIGINT)"
		}, false},
	}
	for _, sql := range []string{
		"INSERT INTO other.t VALUES (1, 100)", "INSERT INTO missing VALUES (1, 100)",
		"INSERT INTO t SELECT 1, 100", "INSERT INTO t VALUES (1, (SELECT 100))",
		"INSERT INTO t VALUES (1, CURRENT_TIMESTAMP)", "INSERT INTO t VALUES (1, ?)",
		"INSERT INTO t VALUES (1, ABS(100))", "INSERT INTO t VALUES (1, a)",
		"INSERT INTO t VALUES (1, 100) OPTIONS (DRY RUN)",
		"INSERT INTO t VALUES (1, 100); DELETE FROM u", "COMMIT", "DELETE FROM t", "CREATE TABLE x (id BIGINT, PRIMARY KEY (id))",
	} {
		cases = append(cases, struct {
			name string
			edit func(*javayamsql.File)
			want bool
		}{sql, func(f *javayamsql.File) { f.Blocks[1].Setup.Steps[0].Query = sql }, false})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			file := fixtureFile(t)
			tc.edit(file)
			fixture, err := validatePrivateFixture(file, "PIN")
			if (err == nil) != tc.want {
				t.Fatalf("valid=%v, want %v: %v", err == nil, tc.want, err)
			}
			if err == nil {
				if fixture.target != (connTarget{Path: "/YAML_PIN_1_DB", Schema: "YAML_PIN_1_SCHEMA"}) || len(fixture.tables) == 0 {
					t.Fatalf("invalid private target/table population: %+v", fixture)
				}
				if tc.name == "quoted_dotted" && (!reflect.DeepEqual(fixture.tables, []string{"a.b"}) || quoteFixtureIdentifier(fixture.tables[0]) != `"a.b"`) {
					t.Fatalf("quoted identifier lost its identity: %v", fixture.tables)
				}
			}
		})
	}
}

func TestFixtureCommitUnknownClassification(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"state_without_cause", api.NewError(api.ErrCodeStatementCompletionUnknown, "unknown"), false},
		{"bare_cause", fdb.Error{Code: 1021}, false},
		{"wrong_cause", api.WrapError(api.ErrCodeStatementCompletionUnknown, "unknown", fdb.Error{Code: 1020}), false},
		{"value", api.WrapError(api.ErrCodeStatementCompletionUnknown, "unknown", fdb.Error{Code: 1021}), true},
		{"pointer", fmt.Errorf("commit: %w", api.WrapError(api.ErrCodeStatementCompletionUnknown, "unknown", &fdb.Error{Code: 1021})), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := fixtureCommitUnknown(tc.err); got != tc.want {
				t.Fatalf("classified=%v, want %v for %v", got, tc.want, tc.err)
			}
		})
	}
}

func TestPrivateFixtureLoadCommitFaults(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                                    string
		faults                                  []int
		duplicate                               bool
		canceled                                bool
		wantAttempts, wantAmbiguities, wantRows int
		wantCode                                api.ErrorCode
	}{
		{name: "success", wantAttempts: 1, wantRows: 1},
		{name: "applied", faults: []int{simfdb.CommitUnknownApplied}, wantAttempts: 2, wantAmbiguities: 1, wantRows: 1},
		{name: "discarded", faults: []int{simfdb.CommitUnknownDiscarded}, wantAttempts: 2, wantAmbiguities: 1, wantRows: 1},
		{name: "definite_conflict", faults: []int{1020}, wantAttempts: 1, wantCode: api.ErrCodeSerializationFailure},
		{name: "duplicate", duplicate: true, wantAttempts: 1, wantCode: api.ErrCodeUniqueConstraintViolation},
		{name: "exhausted_applied", faults: []int{simfdb.CommitUnknownApplied, simfdb.CommitUnknownApplied, simfdb.CommitUnknownApplied}, wantAttempts: 3, wantAmbiguities: 3, wantRows: 1, wantCode: api.ErrCodeStatementCompletionUnknown},
		{name: "exhausted_discarded", faults: []int{simfdb.CommitUnknownDiscarded, simfdb.CommitUnknownDiscarded, simfdb.CommitUnknownDiscarded}, wantAttempts: 3, wantAmbiguities: 3, wantCode: api.ErrCodeStatementCompletionUnknown},
		{name: "canceled", canceled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := dst.NewSim(745)
			env.Buggify = dst.DisabledBuggifier()
			sim := simfdb.New(env)
			backend := recordlayer.NewFDBDatabaseWithBackend(sim).SetEnv(env)
			key := "fixture/" + t.Name()
			t.Cleanup(sqldriver.RegisterBackend(key, backend))
			file := fixtureFile(t)
			fixture, err := validatePrivateFixture(file, "PIN")
			if err != nil {
				t.Fatal(err)
			}
			res := FileResult{}
			r := &runner{cfg: Config{ClusterFile: key, IDPrefix: "PIN", FactoryResetLoad: true}, result: &res, fixture: fixture, connsByResource: map[string][]connTarget{}, dbs: map[connTarget]*sql.DB{}}
			defer r.teardown()
			ctx := context.Background()
			if err := r.executeSchemaTemplate(ctx, file.Path, file.Blocks[0]); err != nil {
				t.Fatal(err)
			}
			db, err := r.open(fixture.target)
			if err != nil {
				t.Fatal(err)
			}
			steps := file.Blocks[1].Setup.Steps
			if tc.duplicate {
				steps = append(append([]*javayamsql.Command(nil), steps...), steps[0])
			}
			sim.InjectSequence(tc.faults...)
			loadCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			if tc.canceled {
				cancel()
			}
			err = fixture.load(loadCtx, db, steps, &res)
			if tc.canceled {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation=%v", err)
				}
			} else if tc.wantCode == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var apiErr *api.Error
				if !errors.As(err, &apiErr) || apiErr.Code != tc.wantCode {
					t.Fatalf("load error=%v, want %s", err, tc.wantCode)
				}
			}
			if res.FixtureLoadAttempts != tc.wantAttempts || len(res.FixtureCommitAmbiguities) != tc.wantAmbiguities || res.QueriesRun != 0 {
				t.Fatalf("attempts=%d ambiguities=%d assertions=%d, want %d/%d/0", res.FixtureLoadAttempts, len(res.FixtureCommitAmbiguities), res.QueriesRun, tc.wantAttempts, tc.wantAmbiguities)
			}
			var n int
			if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t").Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != tc.wantRows {
				t.Fatalf("durable rows=%d, want %d", n, tc.wantRows)
			}
			t.Logf("fixture attempts=%d ambiguities=%d durable=%d", res.FixtureLoadAttempts, len(res.FixtureCommitAmbiguities), n)
		})
	}
}

func TestPrivateFixtureRunParsedAndNamespaceOwnership(t *testing.T) {
	t.Parallel()
	for _, factoryMode := range []bool{false, true} {
		t.Run(fmt.Sprint(factoryMode), func(t *testing.T) {
			t.Parallel()
			env := dst.NewSim(746)
			env.Buggify = dst.DisabledBuggifier()
			sim := simfdb.New(env)
			key := "fixture/" + t.Name()
			t.Cleanup(sqldriver.RegisterBackend(key, recordlayer.NewFDBDatabaseWithBackend(sim).SetEnv(env)))
			file := fixtureFile(t)
			cfg := Config{ClusterFile: key, IDPrefix: "PIN", FactoryResetLoad: factoryMode}
			res := RunParsed(context.Background(), file, cfg)
			wantAttempts := 0
			if factoryMode {
				wantAttempts = 1
			}
			if res.Status != StatusPass || res.QueriesRun != 1 || res.FixtureLoadAttempts != wantAttempts {
				t.Fatalf("mode=%v status=%s assertions=%d attempts=%d error=%v", factoryMode, res.Status, res.QueriesRun, res.FixtureLoadAttempts, res.Err)
			}
			if !factoryMode {
				return
			}
			fixture, err := validatePrivateFixture(file, "PIN")
			if err != nil {
				t.Fatal(err)
			}
			r := &runner{cfg: cfg, result: &FileResult{}, fixture: fixture, connsByResource: map[string][]connTarget{}, dbs: map[connTarget]*sql.DB{}}
			defer r.teardown()
			ctx := context.Background()
			if err := r.executeSchemaTemplate(ctx, file.Path, file.Blocks[0]); err != nil {
				t.Fatal(err)
			}
			db, err := r.open(fixture.target)
			if err != nil {
				t.Fatal(err)
			}
			for _, query := range []string{"INSERT INTO t VALUES (99, 999)", "INSERT INTO u VALUES (99)"} {
				if _, err := db.ExecContext(ctx, query); err != nil {
					t.Fatal(err)
				}
			}
			// A second invocation must not drop/reset this already-owned namespace.
			res = RunParsed(ctx, file, cfg)
			if res.Status != StatusFail || res.FixtureLoadAttempts != 0 || res.QueriesRun != 0 {
				t.Fatalf("existing namespace was not refused: %+v", res)
			}
			var id int64
			if err := db.QueryRowContext(ctx, "SELECT id FROM u").Scan(&id); err != nil || id != 99 {
				t.Fatalf("existing namespace changed: id=%d error=%v", id, err)
			}
			sim.InjectOnce(simfdb.CommitUnknownApplied)
			res = FileResult{}
			if err := fixture.load(ctx, db, file.Blocks[1].Setup.Steps, &res); err != nil {
				t.Fatal(err)
			}
			if res.FixtureLoadAttempts != 2 || len(res.FixtureCommitAmbiguities) != 1 {
				t.Fatalf("missing replay witness: %+v", res)
			}
			if err := db.QueryRowContext(ctx, "SELECT id FROM t").Scan(&id); err != nil || id != 1 {
				t.Fatalf("full setup not restored: id=%d error=%v", id, err)
			}
			var n int64
			if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM u").Scan(&n); err != nil || n != 0 {
				t.Fatalf("declared table absent from setup was not reset: count=%d error=%v", n, err)
			}
		})
	}
}
