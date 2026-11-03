package sqltest

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/sqltest/testkit"
)

// Version zero indexes the first word; version one indexes the last word.
// Both QUERY and INDEX modes use the selected version, deliberately exposing
// a fallback/index mismatch if the planner overlooks version compatibility.
type sqlTextVersionTokenizer struct{ name string }

func (t *sqlTextVersionTokenizer) Name() string  { return t.name }
func (*sqlTextVersionTokenizer) MinVersion() int { return 0 }
func (*sqlTextVersionTokenizer) MaxVersion() int { return 1 }
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
			{"SELECT id FROM docs WHERE TEXT_CONTAINS_ALL(body,'red','" + name + "')", 0, version == "1"},
			{"SELECT id FROM docs WHERE TEXT_CONTAINS_ALL(body,'blue','" + name + "')", 1, version == "1"},
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
