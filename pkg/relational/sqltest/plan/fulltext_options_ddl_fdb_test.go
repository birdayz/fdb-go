package sqltest

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/sqltest/testkit"
)

var (
	fulltextSQLOptionsFactory         = &sqlTextTokenizerFactory{tokenizer: &sqlTextVersionTokenizer{name: "sql_fulltext_ddl_options_regression", singleton: true}}
	fulltextSQLOptionsVersionsFactory = &sqlTextTokenizerFactory{tokenizer: &sqlTextVersionTokenizer{name: "sql_fulltext_ddl_options_versions_regression"}}
)

// FULLTEXT ... OPTIONS (TOKENIZER, TOKENIZER_VERSION) sets the TEXT index's
// tokenizer options from DDL; SQL text access then follows the stored
// options (matching name, version 0, a tokenizer of one version), and invalid
// options are refused.
func TestFDBFullTextDDLOptions(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	factory, versions := fulltextSQLOptionsFactory, fulltextSQLOptionsVersionsFactory
	for _, f := range []*sqlTextTokenizerFactory{factory, versions} {
		if err := recordlayer.GlobalTextTokenizerRegistry().Register(f); err != nil {
			t.Fatal(err)
		}
	}
	ddl := func(options string) string {
		return "CREATE TABLE docs(id BIGINT, body STRING, part BIGINT, PRIMARY KEY(id)) " +
			"CREATE FULLTEXT INDEX body_text ON docs(body) PARTITION BY (part) " + options
	}
	for _, tc := range []struct {
		options, name, version string
	}{
		{"OPTIONS (TOKENIZER = '" + factory.Name() + "', TOKENIZER_VERSION = 0)", factory.Name(), "0"},
		// Option names are case-insensitive and may come in any order.
		{"options (tokenizer_version = 1, Tokenizer = '" + versions.Name() + "')", versions.Name(), "1"},
		{"OPTIONS (TOKENIZER = '" + factory.Name() + "')", factory.Name(), ""},
	} {
		tmpl, err := embedded.BuildSchemaTemplateFromDDL(ddl(tc.options))
		if err != nil {
			t.Fatalf("%s: %v", tc.options, err)
		}
		opts := tmpl.Underlying().GetIndex("BODY_TEXT").Options
		version, hasVersion := opts[recordlayer.IndexOptionTextTokenizerVersion]
		if opts[recordlayer.IndexOptionTextTokenizerName] != tc.name || version != tc.version || hasVersion != (tc.version != "") {
			t.Fatalf("%s: index options %v", tc.options, opts)
		}
	}
	opts := mustTextIndexOptions(t, ddl(""))
	if _, ok := opts[recordlayer.IndexOptionTextTokenizerName]; ok {
		t.Fatalf("FULLTEXT without OPTIONS has tokenizer options %v", opts)
	}
	if _, ok := opts[recordlayer.IndexOptionTextTokenizerVersion]; ok {
		t.Fatalf("FULLTEXT without OPTIONS has tokenizer options %v", opts)
	}

	ctx := context.Background()
	// SQL tokenizes at the tokenizer's maximum version and uses a text index
	// only at version 0, so a tokenizer with a version 1 keeps record filtering.
	for _, tc := range []struct {
		tokenizer, version string
		indexed            bool
	}{{factory.Name(), "0", true}, {versions.Name(), "1", false}} {
		schema := "fulltext_ddl_options_v" + tc.version
		db := testkit.SetupErrorDB(t, "/FRL/FULLTEXT_DDL_OPTIONS_"+tc.version, schema,
			ddl("OPTIONS (TOKENIZER = '"+tc.tokenizer+"', TOKENIZER_VERSION = "+tc.version+")"))
		testkit.MustExecCtx(t, db, ctx, "INSERT INTO docs VALUES (1, 'red', 0), (2, 'blue', 0), (3, 'red', 1), (4, 'blue', 1)")
		conn := testkit.PinEmbeddedConn(t, db, func(*embedded.EmbeddedConnection) {})
		query := "SELECT id FROM docs WHERE TEXT_CONTAINS_ALL(body, 'red', '" + tc.tokenizer + "') AND part IN (0, 1) ORDER BY id"
		var plan string
		if err := conn.QueryRowContext(ctx, "EXPLAIN "+query).Scan(&plan); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(plan, "TextIndexScan(BODY_TEXT") != tc.indexed {
			t.Fatalf("version %s: indexed=%v plan %s", tc.version, tc.indexed, plan)
		}
		if got := testkit.QueryIDs(t, conn, query); !reflect.DeepEqual(got, []int64{1, 3}) {
			t.Fatalf("version %s: ids %v", tc.version, got)
		}
	}

	for _, tc := range []struct {
		options string
		code    api.ErrorCode
	}{
		{"OPTIONS (TOKENIZER = 'no_such_tokenizer')", api.ErrCodeInvalidSchemaTemplate},
		{"OPTIONS (TOKENIZER = '')", api.ErrCodeInvalidSchemaTemplate},
		{"OPTIONS (TOKENIZER = '" + versions.Name() + "', TOKENIZER_VERSION = 2)", api.ErrCodeInvalidSchemaTemplate},
		{"OPTIONS (TOKENIZER = '" + factory.Name() + "', TOKENIZER_VERSION = 1)", api.ErrCodeInvalidSchemaTemplate},
		{"OPTIONS (TOKENIZER_VERSION = 1)", api.ErrCodeInvalidSchemaTemplate}, // the default tokenizer has version 0 only
		{"OPTIONS (TOKENIZER = 'default', TOKENIZER = 'default')", api.ErrCodeSyntaxError},
		{"OPTIONS (TOKENIZER = 7)", api.ErrCodeSyntaxError},
		{"OPTIONS (TOKENIZER_VERSION = '0')", api.ErrCodeSyntaxError},
		{"OPTIONS (TOKENIZER_VERSION = 0L)", api.ErrCodeSyntaxError},
		{"OPTIONS (TOKENIZER_VERSION = 99999999999)", api.ErrCodeSyntaxError},
		{"OPTIONS (TOKENIZER_VERSION = -1)", api.ErrCodeSyntaxError},
		{"OPTIONS (STEMMER = 'x')", api.ErrCodeUnsupportedOperation},
		{"OPTIONS ()", api.ErrCodeSyntaxError},
	} {
		_, err := embedded.BuildSchemaTemplateFromDDL(ddl(tc.options))
		if apiErr := testkit.AsAPIError(err); apiErr == nil || apiErr.Code != tc.code {
			t.Errorf("%s: err %v, want %s", tc.options, err, tc.code)
		}
	}
}

func mustTextIndexOptions(t *testing.T, ddl string) map[string]string {
	t.Helper()
	tmpl, err := embedded.BuildSchemaTemplateFromDDL(ddl)
	if err != nil {
		t.Fatal(err)
	}
	return tmpl.Underlying().GetIndex("BODY_TEXT").Options
}
