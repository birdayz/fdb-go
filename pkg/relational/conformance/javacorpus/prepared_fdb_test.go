package javacorpus_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"fdb.dev/pkg/relational/conformance/javacorpus"
	"fdb.dev/pkg/relational/conformance/javayamsql"
)

func TestPreparedCorpusExecution(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	file, err := javayamsql.Parse("prepared-regression.yamsql", []byte(`schema_template: CREATE TABLE t(id bigint, name string, embedding vector(3, half), PRIMARY KEY(id))
---
test_block:
  preset: single_repetition_ordered
  options:
    statement_type: prepared
  tests:
    -
      - query: INSERT INTO t VALUES (!! 1 !!, !! "O'Brien" !!, !! !v16 [1.0, 2.0, 3.0] !!)
      - count: 1
    -
      - query: SELECT name FROM t WHERE id = !! 1 !!
      - result: [{NAME: "O'Brien"}]
    -
      - query: SELECT euclidean_distance(embedding, !! !v16 [1.0, 2.0, 3.0] !!) AS d FROM t
      - result: [{D: 0.0}]
`))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	result := javacorpus.RunParsed(ctx, file, javacorpus.Config{ClusterFile: clusterFilePath, IDPrefix: "PREPARED_REGRESSION"})
	if result.Status != javacorpus.StatusPass || result.QueriesRun != 3 {
		t.Fatalf("prepared execution: %+v", result)
	}
	for _, skip := range result.Skips {
		if skip.Class == javacorpus.SkipClass("unsupported:prepared") {
			t.Fatalf("prepared execution omitted: %+v", skip)
		}
	}
}

func TestPreparedVectorCorpus(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	corpus, err := javayamsql.OpenCorpus()
	if err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"guardiann-semantic-search.yamsql", "vector-engine-preference.yamsql", "vector-mixed-version-metadata.yamsql", "vector.yamsql", "documentation-queries/vector-documentation-queries.yamsql", "documentation-queries/window-function-documentation-queries.yamsql", "semantic-search.yamsql", "semantic-search-advanced-metrics.yamsql"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			result := javacorpus.Run(ctx, corpus, name, javacorpus.Config{ClusterFile: clusterFilePath, IDPrefix: fmt.Sprintf("PREPARED%d", i)})
			if result.Status != javacorpus.StatusPass || result.QueriesRun == 0 {
				t.Fatalf("%s: %+v", name, result)
			}
			for _, skip := range result.Skips {
				if skip.Class == javacorpus.SkipClass("unsupported:prepared") {
					t.Fatalf("prepared execution omitted: %+v", skip)
				}
			}
		})
	}
}

func TestFilteredIndexCorpus(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	corpus, err := javayamsql.OpenCorpus()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	result := javacorpus.Run(ctx, corpus, "index-ddl-values-only.yamsql", javacorpus.Config{ClusterFile: clusterFilePath, IDPrefix: "FILTEREDPROOF"})
	if result.Status != javacorpus.StatusPass || result.QueriesRun == 0 {
		t.Fatalf("filtered index execution: %+v", result)
	}
}

func TestRecordInExecution(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	file, err := javayamsql.Parse("record-in-regression.yamsql", []byte(`schema_template: CREATE TYPE AS STRUCT pair(x bigint, y bigint) CREATE TABLE t(id bigint, a bigint, b bigint, f pair, PRIMARY KEY(id)) CREATE INDEX f1 AS SELECT f.x, f.y FROM t ORDER BY f.x, f.y
---
test_block:
  preset: single_repetition_ordered
  tests:
    -
      - query: INSERT INTO t VALUES (1, 90, 9, (90, 9)), (2, 81, 18, (81, 18)), (3, 80, 19, (80, 19))
      - count: 3
    -
      - query: SELECT id FROM t WHERE f IN ((90L, 9L), (81L, 18L))
      - unorderedResult: [{ID: 1}, {ID: 2}]
    -
      - query: SELECT id FROM t WHERE (a, b) IN ((90L, 9L), (81L, 18L)) ORDER BY id
      - unorderedResult: [{ID: 1}, {ID: 2}]
    -
      - query: SELECT id FROM t WHERE f NOT IN ((90L, 9L), (81L, 18L)) ORDER BY id
      - result: [{ID: 3}]
    -
      - query: SELECT id FROM t WHERE f IN ((a, b), (90L, 9L)) ORDER BY id
      - result: [{ID: 1}, {ID: 2}, {ID: 3}]
`))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	result := javacorpus.RunParsed(ctx, file, javacorpus.Config{ClusterFile: clusterFilePath, IDPrefix: "RECORDIN"})
	if result.Status != javacorpus.StatusPass || result.QueriesRun != 5 {
		t.Fatalf("record IN execution: %+v", result)
	}
}

func TestRecordInCorpus(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	corpus, err := javayamsql.OpenCorpus()
	if err != nil {
		t.Fatal(err)
	}
	file, err := corpus.ParseFile("in-predicate.yamsql")
	if err != nil {
		t.Fatal(err)
	}
	selected := 0
	for _, block := range file.Blocks {
		if block.Test == nil {
			continue
		}
		var tests []*javayamsql.Test
		for _, test := range block.Test.Tests {
			switch test.Command.Query {
			case "select a, b from ta where (a, b) in ((0L, 9L), (1L, 8L))",
				"select a, b from ta where f in ((90L, 9L), (81L, 18L))",
				"select a, b from ta where f in ((90L, 9L), (81L, 18L), (81L, 18L), (90L, 9L))":
				tests = append(tests, test)
				selected++
			}
		}
		block.Test.Tests = tests
		block.Test.Preset = "single_repetition_ordered"
	}
	if selected != 3 {
		t.Fatalf("selected %d record membership cases, want 3", selected)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	result := javacorpus.RunParsed(ctx, file, javacorpus.Config{ClusterFile: clusterFilePath, IDPrefix: "RECORDINCORPUS"})
	if result.Status != javacorpus.StatusPass || result.QueriesRun != 3 {
		t.Fatalf("record IN corpus: %+v", result)
	}
}

func TestPermutedAggregateDerivedOrder(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	corpus, err := javayamsql.OpenCorpus()
	if err != nil {
		t.Fatal(err)
	}
	file, err := corpus.ParseFile("aggregate-index-tests.yamsql")
	if err != nil {
		t.Fatal(err)
	}
	selected := 0
	for _, block := range file.Blocks {
		if block.Test == nil {
			continue
		}
		var tests []*javayamsql.Test
		for _, test := range block.Test.Tests {
			if test.Command.Query == "select t.* from (select col3, max(col2) as m from t2 where col1 = 1 group by col1, col3) as t where m < 2 order by m desc;" || test.Command.Query == "select M as x1, min(b) as x2 from t3 group by a+b as M, b+10;" || test.Command.Query == "select a, ek.k, b, max(d) from t6, (select k from t6.c where k = 'q') as ek group by a, ek.k, b having a = 1 and max(d) > 100" {
				tests = append(tests, test)
				selected++
			}
		}
		block.Test.Tests = tests
		block.Test.Preset = "single_repetition_ordered"
	}
	if selected != 3 {
		t.Fatalf("selected %d cases, want 3", selected)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	result := javacorpus.RunParsed(ctx, file, javacorpus.Config{ClusterFile: clusterFilePath, IDPrefix: "PERMUTEDORDER"})
	if result.Status != javacorpus.StatusPass || result.QueriesRun != 3 {
		t.Fatalf("permuted aggregate execution: %+v", result)
	}
}

func TestBitmapAggregateCorpus(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	corpus, err := javayamsql.OpenCorpus()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	result := javacorpus.Run(ctx, corpus, "bitmap-aggregate-index.yamsql", javacorpus.Config{ClusterFile: clusterFilePath, IDPrefix: "BITMAPAGG"})
	if result.Status != javacorpus.StatusPass || result.QueriesRun == 0 {
		t.Fatalf("bitmap execution: %+v", result)
	}
}

// Java QueryVisitor rejects LIMIT outright; its Cascades planner also has no
// physical sort fallback. These are supported Go read-side extensions, not
// permission to accept arbitrary Java negatives without checking the result.
func TestCorpusReadSideExtensions(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	file, err := javayamsql.Parse("read-side-extensions.yamsql", []byte(`schema_template: CREATE TABLE ta(a bigint, b bigint, PRIMARY KEY(a)) CREATE TABLE t1(id bigint, a1 bigint, a2 bigint, b bigint, PRIMARY KEY(id)) CREATE TABLE t2(id bigint, b1 bigint, b3 string, PRIMARY KEY(id))
---
test_block:
  preset: single_repetition_ordered
  tests:
    -
      - query: INSERT INTO ta VALUES (1, 10), (2, 20)
      - count: 2
    -
      - query: INSERT INTO t1 VALUES (1, 1, 20, 2), (2, 1, 10, 1)
      - count: 2
    -
      - query: INSERT INTO t2 VALUES (1, 1, 'z'), (2, 1, 'a')
      - count: 2
    -
      - query: select p.* FROM ta as p where exists (select * from ta where ta.a = p.a limit 1);
      - unorderedResult: [{A: 1, B: 10}, {A: 2, B: 20}]
    -
      - query: select b from t1 where exists (select * from t1 order by b limit 1)
      - unorderedResult: [{B: 2}, {B: 1}]
    -
      - query: select p.* FROM ta as p where exists (select * from ta where ta.a = p.a limit 0);
      - result: []
    -
      - query: select (t1.*), (t2.*) from t1, t2 where t1.a1 = 1 and t2.b1 = 1 order by t1.a2, t2.b3
      - result: [{{ID: 2, A1: 1, A2: 10, B: 1}, {ID: 2, B1: 1, B3: 'a'}}, {{ID: 2, A1: 1, A2: 10, B: 1}, {ID: 1, B1: 1, B3: 'z'}}, {{ID: 1, A1: 1, A2: 20, B: 2}, {ID: 2, B1: 1, B3: 'a'}}, {{ID: 1, A1: 1, A2: 20, B: 2}, {ID: 1, B1: 1, B3: 'z'}}]
`))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	result := javacorpus.RunParsed(ctx, file, javacorpus.Config{ClusterFile: clusterFilePath, IDPrefix: "READSIDEEXT"})
	if result.Status != javacorpus.StatusPass || result.QueriesRun != 7 {
		t.Fatalf("read-side extensions: %+v", result)
	}
}
