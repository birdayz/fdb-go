//go:build bazelrunfiles

package conformance_test

// RFC-204 Phase 1 probe — dump the EXACT stored FileDescriptorProto Java's
// relational layer persists for representative schema templates (structs,
// arrays, nested PKs, name escaping). The output drives Go's descriptor
// emission rework; the shapes then graduate into byte-golden assertions.

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/core/catalog"
)

type rfc204Probe struct {
	name string
	body string
}

var rfc204Probes = []rfc204Probe{
	{
		name: "simple",
		body: `CREATE TABLE T (id BIGINT, name STRING, PRIMARY KEY(id))`,
	},
	{
		name: "struct_basic",
		body: `CREATE TYPE AS STRUCT S1 (a BIGINT, b BIGINT) ` +
			`CREATE TABLE T (id BIGINT, s S1, PRIMARY KEY(id))`,
	},
	{
		name: "struct_of_struct_shared",
		body: `CREATE TYPE AS STRUCT S1 (a BIGINT, b BIGINT) ` +
			`CREATE TYPE AS STRUCT S2 (c S1, d STRING) ` +
			`CREATE TABLE T1 (id BIGINT, s S2, PRIMARY KEY(id)) ` +
			`CREATE TABLE T2 (id BIGINT, u S1, PRIMARY KEY(id))`,
	},
	{
		name: "nullable_scalar_array",
		body: `CREATE TABLE T (id BIGINT, arr BIGINT ARRAY, PRIMARY KEY(id))`,
	},
	{
		name: "nullable_scalar_array_again",
		body: `CREATE TABLE T (id BIGINT, arr BIGINT ARRAY, PRIMARY KEY(id))`,
	},
	{
		name: "notnull_scalar_array",
		body: `CREATE TABLE T (id BIGINT, arr BIGINT ARRAY NOT NULL, PRIMARY KEY(id))`,
	},
	{
		name: "nullable_array_of_struct",
		body: `CREATE TYPE AS STRUCT P (x BIGINT, y BIGINT) ` +
			`CREATE TABLE T (id BIGINT, pts P ARRAY, PRIMARY KEY(id))`,
	},
	{
		name: "nullability_variants",
		body: `CREATE TYPE AS STRUCT ADDRESS (street STRING, city STRING, zipcode INTEGER) ` +
			`CREATE TABLE USERS (id INTEGER, name STRING, home_address ADDRESS, PRIMARY KEY(id)) ` +
			`CREATE TABLE BUSINESSES (id INTEGER, name STRING, headquarters ADDRESS, branch_offices ADDRESS ARRAY, PRIMARY KEY(id))`,
	},
	{
		name: "nested_pk",
		body: `CREATE TYPE AS STRUCT S1 (a BIGINT, b BIGINT) ` +
			`CREATE TABLE T1 (id S1, g BIGINT, PRIMARY KEY(id.a, id.b))`,
	},
	{
		name: "name_escaping",
		body: `CREATE TYPE AS STRUCT "x$$" (a BIGINT) ` +
			`CREATE TABLE "foo.tableA" (id BIGINT, s "x$$", PRIMARY KEY(id))`,
	},
	{
		name: "struct_uuid_vector",
		body: `CREATE TYPE AS STRUCT SV (u UUID, v VECTOR(3, FLOAT), n BIGINT) ` +
			`CREATE TABLE T (id BIGINT, sv SV, PRIMARY KEY(id))`,
	},
	{
		name: "deep_nesting",
		body: `CREATE TYPE AS STRUCT A (a BIGINT) ` +
			`CREATE TYPE AS STRUCT B (ast A, b BIGINT) ` +
			`CREATE TYPE AS STRUCT C (bst B, c BIGINT) ` +
			`CREATE TABLE T (id BIGINT, cst C, PRIMARY KEY(id))`,
	},
	{
		name: "forward_reference",
		body: `CREATE TABLE T (id BIGINT, s LATER, PRIMARY KEY(id)) ` +
			`CREATE TYPE AS STRUCT LATER (a BIGINT)`,
	},
	{
		name: "struct_nullable_field_variants",
		body: `CREATE TYPE AS STRUCT S1 (a BIGINT) ` +
			`CREATE TYPE AS STRUCT S3 (x S1, y S1 NULL, arr S1 ARRAY) ` +
			`CREATE TABLE T (id BIGINT, s S3, PRIMARY KEY(id))`,
	},
}

var _ = Describe("RFC-204 descriptor probe (Java stored bytes)", func() {
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

	loadStored := func(templateName string) *gen.MetaData {
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
		Expect(stored).NotTo(BeNil(), "template %s not found", templateName)
		return stored
	}

	for _, probe := range rfc204Probes {
		probe := probe
		It("dumps Java's stored descriptor for "+probe.name, func() {
			templateName := "RFC204P_" + uuid.New().String()[:8]
			var createResult struct {
				Created bool `json:"created"`
			}
			err := java.InvokeAs(ctx, "createSchemaTemplatePersistentJava", map[string]any{
				"clusterFile":        clusterFile,
				"templateName":       templateName,
				"schemaTemplateBody": probe.body,
			}, &createResult)
			Expect(err).NotTo(HaveOccurred(), "Java CREATE SCHEMA TEMPLATE for %s", probe.name)
			defer func() {
				var dropResult struct {
					Dropped bool `json:"dropped"`
				}
				_ = java.InvokeAs(context.Background(), "dropSchemaTemplatePersistentJava", map[string]any{
					"clusterFile":  clusterFile,
					"templateName": templateName,
				}, &dropResult)
			}()

			md := loadStored(templateName)
			txt, terr := prototext.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(md.GetRecords())
			Expect(terr).NotTo(HaveOccurred())
			fmt.Fprintf(GinkgoWriter, "==== RFC204 PROBE %s (template %s) ====\n%s\n==== END %s ====\n",
				probe.name, templateName, txt, probe.name)
			fmt.Fprintf(GinkgoWriter, "---- version=%d splitLongRecords=%v ----\n",
				md.GetVersion(), md.GetSplitLongRecords())
		})
	}
})
