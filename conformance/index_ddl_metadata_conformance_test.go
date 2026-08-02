//go:build bazelrunfiles

package conformance_test

// RFC-202 D11 — the cross-engine index-metadata check, at the STORED bytes.
//
// For every index shape the generator front ends produce (S1-S6), the real
// Java engine (fdb-relational 4.12.11.0 over JDBC) persists a schema template
// into the shared catalog, and Go reads the RAW stored
// RecordMetaDataProto.MetaData bytes back from the SAME subspace Java wrote
// (catalog.OpenRecordLayerStoreCatalog — the Java-wire-compatible
// (NULL, NULL, int64(0)) keyspace; ListTemplates' META_DATA column is the
// stored proto). Each stored index is compared against the index Go's own DDL
// front end builds for the IDENTICAL statement text — root_expression, type,
// **options** (where UNIQUE lives: without the options comparison a Go index
// that silently dropped UNIQUE compares equal to Java's unique index) and
// **predicate** (where a sparse index's WHERE lives).
//
// The comparison consumes the stored bytes directly (proto.Unmarshal of
// META_DATA), never Java's object model round-tripped through Go's — a Go
// deserialization bug cannot mask a divergence.
//
// Carve-out (RFC-202 §8(b1)): shapes over a NULLABLE ARRAY column are
// excluded — Go's descriptor has no `values` wrapper (RFC-143 §3a), so Java's
// key expression legitimately differs. No table below declares an array.

import (
	"context"
	"fmt"
	"sort"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/core/catalog"
	"fdb.dev/pkg/relational/core/embedded"
)

// rfc202MetadataShape is one cross-engine shape: the shared template BODY and
// the index names it declares (compared one by one).
type rfc202MetadataShape struct {
	name    string
	body    string
	indexes []string
}

var rfc202MetadataShapes = []rfc202MetadataShape{
	{
		name: "on_source_plain_and_desc",
		body: `CREATE TABLE T (id BIGINT, a BIGINT, b BIGINT, c STRING, PRIMARY KEY(id)) ` +
			`CREATE INDEX i_plain ON T(a, b) ` +
			`CREATE INDEX i_desc ON T(a DESC) ` +
			`CREATE INDEX i_nulls ON T(b ASC NULLS LAST, c DESC NULLS FIRST)`,
		indexes: []string{"I_PLAIN", "I_DESC", "I_NULLS"},
	},
	{
		name: "on_source_include_and_unique",
		body: `CREATE TABLE T (id BIGINT, a BIGINT, b BIGINT, c STRING, PRIMARY KEY(id)) ` +
			`CREATE INDEX i_cover ON T(a) INCLUDE (b, c) ` +
			`CREATE UNIQUE INDEX i_uniq ON T(b)`,
		indexes: []string{"I_COVER", "I_UNIQ"},
	},
	{
		name: "as_select_value_and_sparse",
		body: `CREATE TABLE T (id BIGINT, a BIGINT, b BIGINT, i INTEGER, d DOUBLE, PRIMARY KEY(id)) ` +
			`CREATE INDEX i_sel AS SELECT a, b FROM T ORDER BY a DESC, b ` +
			// The comparand's stored proto type follows the COMPARISON's
			// promoted type — the column's, not the literal's own width: a
			// BIGINT column stores long_value:10 where an INTEGER column
			// stores int_value:10 for the same literal (measured against the
			// live 4.12.11.0 JVM; the ParseHelpers.parseDecimal Integer
			// narrowing describes the RAW literal, which the comparison then
			// promotes to the column type).
			`CREATE INDEX i_sparse AS SELECT a FROM T WHERE a > 10 ORDER BY a ` +
			`CREATE INDEX i_sparse_int AS SELECT i FROM T WHERE i > 10 ORDER BY i ` +
			// The corpus's own filtered-index twin (index-ddl-values-only's
			// idx_mv_filtered_expensive) compares a DOUBLE column against an
			// integer literal — the promoted comparand type is the measurement.
			`CREATE INDEX i_sparse_dbl AS SELECT d FROM T WHERE d > 20 ORDER BY d`,
		indexes: []string{"I_SEL", "I_SPARSE", "I_SPARSE_INT", "I_SPARSE_DBL"},
	},
	{
		name: "as_select_aggregates",
		body: `CREATE TABLE T (id BIGINT, a BIGINT, b STRING, PRIMARY KEY(id)) ` +
			`CREATE INDEX i_sum AS SELECT SUM(a) FROM T GROUP BY b ` +
			`CREATE INDEX i_cnt AS SELECT COUNT(*) FROM T GROUP BY b ` +
			`CREATE INDEX i_min AS SELECT MIN(a) FROM T GROUP BY b`,
		indexes: []string{"I_SUM", "I_CNT", "I_MIN"},
	},
	{
		name: "version_index",
		body: `CREATE TABLE T (id BIGINT, a BIGINT, PRIMARY KEY(id)) ` +
			`CREATE INDEX i_ver AS SELECT a, "__ROW_VERSION" FROM T ORDER BY a, "__ROW_VERSION" ` +
			`WITH OPTIONS(STORE_ROW_VERSIONS=true)`,
		indexes: []string{"I_VER"},
	},
}

// normalizedIndexOptions renders an index's stored options as sorted
// "key=value" strings for order-insensitive comparison.
func normalizedIndexOptions(idx *gen.Index) []string {
	out := make([]string, 0, len(idx.GetOptions()))
	for _, o := range idx.GetOptions() {
		out = append(out, o.GetKey()+"="+o.GetValue())
	}
	sort.Strings(out)
	return out
}

// normalizedProto clones m and clears proto2 optionals explicitly set to
// their default — Java materializes defaults Go leaves unset; the bytes are
// otherwise identical.
func normalizedProto[M proto.Message](m M) M {
	cp := proto.Clone(m).(M)
	clearProto2Defaults(cp.ProtoReflect())
	return cp
}

var _ = Describe("RFC-202 D11 index-DDL metadata cross-engine (stored bytes)", func() {
	var (
		ctx         context.Context
		java        *JavaInvoker
		clusterFile string
		goRecordDB  *recordlayer.FDBDatabase
	)

	BeforeEach(func() {
		ctx = context.Background()
		java = NewJavaInvoker()
		goRecordDB = recordlayer.NewFDBDatabase(sharedDB)
		var err error
		clusterFile, err = sharedContainer.ClusterFile(ctx)
		Expect(err).NotTo(HaveOccurred())
	})

	// loadStoredJavaMetaData reads the RAW stored MetaData bytes for
	// templateName from the shared catalog subspace Java wrote.
	loadStoredJavaMetaData := func(templateName string) *gen.MetaData {
		cat, openErr := catalog.OpenRecordLayerStoreCatalog()
		Expect(openErr).NotTo(HaveOccurred())
		var stored *gen.MetaData
		_, runErr := goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			tx := catalog.NewFDBTransaction(rtx)
			rs, listErr := cat.SchemaTemplateCatalog().ListTemplates(tx)
			if listErr != nil {
				return nil, listErr
			}
			for rs.Next() {
				name, nameErr := rs.String(1)
				if nameErr != nil {
					return nil, nameErr
				}
				if name != templateName {
					continue
				}
				raw, rawErr := rs.Bytes(3)
				if rawErr != nil {
					return nil, rawErr
				}
				md := &gen.MetaData{}
				if err := proto.Unmarshal(raw, md); err != nil {
					return nil, fmt.Errorf("unmarshal stored MetaData: %w", err)
				}
				stored = md
			}
			return nil, rs.Err()
		})
		Expect(runErr).NotTo(HaveOccurred())
		Expect(stored).NotTo(BeNil(), "template %s not found in the shared catalog", templateName)
		return stored
	}

	// Both engines REJECT a sparse predicate whose COLUMN side needs the
	// comparison promotion: `WHERE int_col < 5000000000` plans as
	// promote(I AS LONG) < 5000000000, a PromoteValue that
	// IndexPredicate.isSupported refuses (IndexPredicate.java:227), so Java's
	// CREATE SCHEMA TEMPLATE fails with "Unsupported predicate '…'"
	// (MaterializedViewIndexGenerator.java:676) — measured against the live
	// JVM. Go rejects the same lattice shape in its predicate arm.
	It("rejects a promoted-column sparse predicate in both engines", func() {
		templateName := "RFC202_" + uuid.New().String()[:8]
		body := `CREATE TABLE T (id BIGINT, i INTEGER, PRIMARY KEY(id)) ` +
			`CREATE INDEX i_sparse_int_big AS SELECT i FROM T WHERE i < 5000000000 ORDER BY i`
		var createResult struct {
			Created bool `json:"created"`
		}
		err := java.InvokeAs(ctx, "createSchemaTemplatePersistentJava", map[string]any{
			"clusterFile":        clusterFile,
			"templateName":       templateName,
			"schemaTemplateBody": body,
		}, &createResult)
		Expect(err).To(HaveOccurred(), "Java must reject the promoted-column sparse predicate")
		Expect(err.Error()).To(ContainSubstring("Unsupported predicate"))

		_, goErr := embedded.BuildSchemaTemplateFromDDL(body)
		Expect(goErr).To(HaveOccurred(),
			"Go must reject the same shape — Java's promote(col) fails IndexPredicate.isSupported")
		Expect(goErr.Error()).To(ContainSubstring("Unsupported predicate"))
	})

	for _, shape := range rfc202MetadataShapes {
		shape := shape
		It("matches Java's stored index bytes for "+shape.name, func() {
			templateName := "RFC202_" + uuid.New().String()[:8]

			var createResult struct {
				Created bool `json:"created"`
			}
			err := java.InvokeAs(ctx, "createSchemaTemplatePersistentJava", map[string]any{
				"clusterFile":        clusterFile,
				"templateName":       templateName,
				"schemaTemplateBody": shape.body,
			}, &createResult)
			Expect(err).NotTo(HaveOccurred(), "Java CREATE SCHEMA TEMPLATE for %s", shape.name)
			Expect(createResult.Created).To(BeTrue())
			defer func() {
				var dropResult struct {
					Dropped bool `json:"dropped"`
				}
				_ = java.InvokeAs(context.Background(), "dropSchemaTemplatePersistentJava", map[string]any{
					"clusterFile":  clusterFile,
					"templateName": templateName,
				}, &dropResult)
			}()

			javaMD := loadStoredJavaMetaData(templateName)

			goTmpl, buildErr := embedded.BuildSchemaTemplateFromDDL(shape.body)
			Expect(buildErr).NotTo(HaveOccurred(), "Go DDL front end for %s", shape.name)
			goProto, protoErr := goTmpl.Underlying().ToProto()
			Expect(protoErr).NotTo(HaveOccurred())

			javaIdx := make(map[string]*gen.Index, len(javaMD.GetIndexes()))
			for _, idx := range javaMD.GetIndexes() {
				javaIdx[idx.GetName()] = idx
			}
			goIdx := make(map[string]*gen.Index, len(goProto.GetIndexes()))
			for _, idx := range goProto.GetIndexes() {
				goIdx[idx.GetName()] = idx
			}

			for _, name := range shape.indexes {
				j, jOK := javaIdx[name]
				g, gOK := goIdx[name]
				Expect(jOK).To(BeTrue(), "Java stored no index %s (has %v)", name, indexNames(javaMD.GetIndexes()))
				Expect(gOK).To(BeTrue(), "Go built no index %s (has %v)", name, indexNames(goProto.GetIndexes()))

				jRoot := normalizedProto(j.GetRootExpression())
				gRoot := normalizedProto(g.GetRootExpression())
				Expect(proto.Equal(gRoot, jRoot)).To(BeTrue(),
					"%s root_expression diverges:\n  go:   %v\n  java: %v", name, gRoot, jRoot)

				Expect(g.GetType()).To(Equal(j.GetType()), "%s index type diverges", name)

				Expect(normalizedIndexOptions(g)).To(Equal(normalizedIndexOptions(j)),
					"%s options diverge (UNIQUE lives here — a dropped option is a wire divergence)", name)

				jPred := j.GetPredicate()
				gPred := g.GetPredicate()
				if jPred == nil || gPred == nil {
					Expect(gPred == nil).To(Equal(jPred == nil),
						"%s predicate presence diverges (go=%v java=%v)", name, gPred, jPred)
				} else {
					Expect(proto.Equal(normalizedProto(gPred), normalizedProto(jPred))).To(BeTrue(),
						"%s stored predicate diverges:\n  go:   %v\n  java: %v", name, gPred, jPred)
				}
			}
		})
	}
})

func indexNames(indexes []*gen.Index) []string {
	out := make([]string, 0, len(indexes))
	for _, idx := range indexes {
		out = append(out, idx.GetName())
	}
	sort.Strings(out)
	return out
}
