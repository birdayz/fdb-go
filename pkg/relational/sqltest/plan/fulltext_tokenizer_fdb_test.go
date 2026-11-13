package sqltest

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/ddl"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/sqltest/testkit"
)

// Version 0 indexes the first word and version 1 the last, in both modes, so
// a planner that ignores the version returns different records.
type sqlTextVersionTokenizer struct {
	name      string
	singleton bool
}

func (t *sqlTextVersionTokenizer) Name() string  { return t.name }
func (*sqlTextVersionTokenizer) MinVersion() int { return 0 }
func (t *sqlTextVersionTokenizer) MaxVersion() int {
	if t.singleton {
		return 0
	}
	return 1
}

func (t *sqlTextVersionTokenizer) TokenizeToList(text string, version int, mode recordlayer.TokenizerMode) ([]string, error) {
	if err := recordlayer.ValidateTokenizerVersion(t, version); err != nil {
		return nil, err
	}
	tokens, err := recordlayer.DefaultTextTokenizerInstance().TokenizeToList(text, 0, mode)
	if err != nil || len(tokens) == 0 {
		return tokens, err
	}
	if version == 0 {
		return tokens[:1], nil
	}
	return tokens[len(tokens)-1:], nil
}

func (t *sqlTextVersionTokenizer) TokenizeToMap(text string, version int, mode recordlayer.TokenizerMode) (map[string][]int, error) {
	tokens, err := t.TokenizeToList(text, version, mode)
	if err != nil {
		return nil, err
	}
	out := map[string][]int{}
	for i, token := range tokens {
		if token != "" {
			out[token] = append(out[token], i)
		}
	}
	return out, nil
}

func (t *sqlTextVersionTokenizer) Tokenize(text string, version int, mode recordlayer.TokenizerMode) (recordlayer.TokenIterator, error) {
	tokens, err := t.TokenizeToList(text, version, mode)
	if err != nil {
		return nil, err
	}
	return &sqlTextTokens{tokens: tokens}, nil
}

type sqlTextTokens struct {
	tokens []string
	next   int
}

func (t *sqlTextTokens) HasNext() bool { return t.next < len(t.tokens) }
func (t *sqlTextTokens) Next() string  { token := t.tokens[t.next]; t.next++; return token }

type sqlTextTokenizerFactory struct{ tokenizer *sqlTextVersionTokenizer }

func (f *sqlTextTokenizerFactory) Name() string                            { return f.tokenizer.Name() }
func (f *sqlTextTokenizerFactory) GetTokenizer() recordlayer.TextTokenizer { return f.tokenizer }

var fulltextSQLVersionFactory = &sqlTextTokenizerFactory{tokenizer: &sqlTextVersionTokenizer{name: "sql_fulltext_version_regression"}}

var fulltextSQLBoundFactory = &sqlTextTokenizerFactory{tokenizer: &sqlTextVersionTokenizer{name: "sql_fulltext_bound_regression", singleton: true}}

func TestFDBFullTextMatchingCustomTokenizer(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	factory := fulltextSQLBoundFactory
	if err := recordlayer.GlobalTextTokenizerRegistry().Register(factory); err != nil {
		t.Fatal(err)
	}
	name := strings.ToUpper(t.Name())
	template, err := embedded.BuildSchemaTemplateFromDDLNamed("CREATE TABLE docs(id BIGINT, body STRING, PRIMARY KEY(id)) CREATE FULLTEXT INDEX body_text ON docs(body) CREATE TABLE plain_docs(id BIGINT, body STRING, PRIMARY KEY(id))", name)
	if err != nil {
		t.Fatal(err)
	}
	template.Underlying().GetIndex("BODY_TEXT").SetOption(recordlayer.IndexOptionTextTokenizerName, factory.Name()).SetOption(recordlayer.IndexOptionTextTokenizerVersion, "0")
	h := testkit.NewEvolHarness(t)
	path := "/FRL/" + name
	h.MustRun(t, "create custom tokenizer schema", func(txn api.Transaction) error {
		if err := h.Cat.Initialize(txn); err != nil {
			return err
		}
		if err := ddl.NewCreateDatabaseConstantAction(path, h.Cat).Execute(txn); err != nil {
			return err
		}
		if err := ddl.NewSaveSchemaTemplateConstantAction(template, h.Cat.SchemaTemplateCatalog()).Execute(txn); err != nil {
			return err
		}
		return ddl.NewCreateSchemaConstantAction(path, "S", name, h.Cat, h.Ks).Execute(txn)
	})
	db := testkit.EvolReopen(t, path, "S")
	ctx := context.Background()
	for _, table := range []string{"docs", "plain_docs"} {
		if _, err := db.ExecContext(ctx, "INSERT INTO "+table+" VALUES (1,'red blue'),(2,'blue red'),(3,'red')"); err != nil {
			t.Fatal(err)
		}
	}
	conn := testkit.PinEmbeddedConn(t, db, func(*embedded.EmbeddedConnection) {})
	for _, table := range []string{"docs", "plain_docs"} {
		for _, tokenizer := range []string{factory.Name(), "default", factory.Name(), "default"} {
			want := []int64{2}
			if tokenizer == "default" {
				want = []int64{1, 2}
			}
			for _, bound := range []bool{false, true} {
				query := "SELECT id FROM " + table + " WHERE TEXT_CONTAINS_ALL(body,'blue red','" + tokenizer + "') ORDER BY id"
				var args []any
				if bound {
					query = "SELECT id FROM " + table + " WHERE TEXT_CONTAINS_ALL(body,?,?) ORDER BY id"
					args = []any{"blue red", tokenizer}
				}
				var plan string
				if err := conn.QueryRowContext(ctx, "EXPLAIN "+query, args...).Scan(&plan); err != nil {
					t.Fatal(err)
				}
				if indexed := table == "docs" && tokenizer == factory.Name(); strings.Contains(plan, "TextIndexScan(BODY_TEXT") != indexed {
					t.Fatalf("table=%s tokenizer=%s bound=%v: %s", table, tokenizer, bound, plan)
				}
				if got := testkit.QueryIDs(t, conn, query, args...); !reflect.DeepEqual(got, want) {
					t.Fatalf("table=%s tokenizer=%s bound=%v: got %v want %v", table, tokenizer, bound, got, want)
				}
			}
		}
	}
}

func TestFDBFullTextExplicitTokenizerAndVersions(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	factory := fulltextSQLVersionFactory
	name := factory.Name()
	if err := recordlayer.GlobalTextTokenizerRegistry().Register(factory); err != nil {
		t.Fatal(err)
	}
	fdb.MustAPIVersion(730)
	raw, err := fdb.OpenDatabase(testkit.ClusterFile())
	if err != nil {
		t.Fatal(err)
	}
	database := recordlayer.NewFDBDatabase(raw)
	for _, version := range []string{"0", "1"} {
		template, err := embedded.BuildSchemaTemplateFromDDL("CREATE TABLE docs(id BIGINT, body STRING, PRIMARY KEY(id)) CREATE FULLTEXT INDEX body_text ON docs(body)")
		if err != nil {
			t.Fatal(err)
		}
		md := template.Underlying()
		index := md.GetIndex("BODY_TEXT")
		index.SetOption(recordlayer.IndexOptionTextTokenizerName, name).SetOption(recordlayer.IndexOptionTextTokenizerVersion, version)
		space := subspace.FromBytes(tuple.Tuple{t.Name(), version}.Pack())
		desc := md.GetRecordType("DOCS").Descriptor
		_, err = database.Run(context.Background(), func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, e := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(space).CreateOrOpen()
			if e != nil {
				return nil, e
			}
			message := dynamicpb.NewMessage(desc)
			message.Set(desc.Fields().ByName("ID"), protoreflect.ValueOfInt64(1))
			message.Set(desc.Fields().ByName("BODY"), protoreflect.ValueOfString("red blue"))
			_, e = store.SaveRecord(message)
			return nil, e
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			query string
			want  int
			index bool
		}{
			{"SELECT id FROM docs WHERE TEXT_CONTAINS_ALL(body,'red')", 1, false},
			{"SELECT id FROM docs WHERE TEXT_CONTAINS_ALL(body,'red','" + name + "')", 0, false},
			{"SELECT id FROM docs WHERE TEXT_CONTAINS_ALL(body,'blue','" + name + "')", 1, false},
		} {
			plan, err := embedded.PlanRecordQueryWithMetadata(tc.query, md, nil)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(plan.Explain(), "TextIndexScan") != tc.index {
				t.Fatalf("version=%s %s plan=%s expected indexed=%v", version, tc.query, plan.Explain(), tc.index)
			}
			_, err = database.Run(context.Background(), func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, e := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(space).Open()
				if e != nil {
					return nil, e
				}
				cursor, e := executor.ExecutePlan(context.Background(), plan, store, executor.EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
				if e != nil {
					return nil, e
				}
				defer cursor.Close()
				rows, e := executor.CollectAll(context.Background(), cursor)
				if e != nil {
					return nil, e
				}
				if len(rows) != tc.want {
					t.Fatalf("version=%s %s rows=%d want=%d", version, tc.query, len(rows), tc.want)
				}
				return nil, nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
}
