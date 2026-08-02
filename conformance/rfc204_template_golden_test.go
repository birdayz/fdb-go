//go:build bazelrunfiles

package conformance_test

// RFC-204 Phase 1 byte-goldens — Go's schema-template metadata against the
// LIVE Java engine's stored bytes.
//
// For every representative template (each §1.2 construct: structs,
// struct-of-struct sharing, forward references, arrays flat and wrapped,
// nested primary keys, name escaping, UUID/VECTOR struct fields, deep
// nesting, nullability variants — and explicitly the NULLABLE
// ARRAY-OF-STRUCT column, the NullableArrayWrapper emission shape), Java
// (fdb-relational 4.12.11.0 over JDBC) persists the template into the shared
// catalog; Go builds the IDENTICAL DDL through its own front end and must
// produce the SAME RecordMetaData proto — descriptor bytes included, because
// the catalog persists RecordMetaData.toProto().toByteArray() and a Java
// client must open a Go-created template byte-for-byte.
//
// The ONE token compared under normalization is the NullableArrayWrapper
// message name: Java names it ProtoUtils.uniqueTypeName() = "__type__" +
// UUID.randomUUID() — RANDOM per serialization (two identical templates get
// different names; every reader is structural,
// NullableArrayUtils.isWrappedArrayDescriptor). Literal equality on that
// token is therefore unpinnable against Java's own output; both sides'
// wrapper names are canonicalized in encounter order before the byte
// comparison, and everything else — placement, order, labels, numbers,
// type_name references, union shape, options — is compared exactly.

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/core/catalog"
	"fdb.dev/pkg/relational/core/embedded"
)

type rfc204Golden struct {
	name string
	body string
}

var rfc204Goldens = []rfc204Golden{
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
		name: "notnull_scalar_array",
		body: `CREATE TABLE T (id BIGINT, arr BIGINT ARRAY NOT NULL, PRIMARY KEY(id))`,
	},
	{
		// The C2 shape: a NULLABLE array-of-struct column — the
		// NullableArrayWrapper referencing a top-level struct element.
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

// normalizeWrapperNames canonicalizes every "__type__*" message name (and the
// field type_name references to it) in encounter order. Mutates fdp.
func normalizeWrapperNames(fdp *descriptorpb.FileDescriptorProto) {
	rename := map[string]string{}
	for _, m := range fdp.GetMessageType() {
		if strings.HasPrefix(m.GetName(), "__type__") {
			if _, ok := rename[m.GetName()]; !ok {
				rename[m.GetName()] = fmt.Sprintf("__wrapper_%d__", len(rename))
			}
		}
	}
	for _, m := range fdp.GetMessageType() {
		if to, ok := rename[m.GetName()]; ok {
			m.Name = proto.String(to)
		}
		for _, f := range m.GetField() {
			if to, ok := rename[f.GetTypeName()]; ok {
				f.TypeName = proto.String(to)
			}
		}
	}
}

var _ = Describe("RFC-204 schema-template byte-goldens (Go vs live Java)", func() {
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

	for _, g := range rfc204Goldens {
		g := g
		It("matches Java's stored template bytes for "+g.name, func() {
			templateName := "RFC204G_" + uuid.New().String()[:8]
			var createResult struct {
				Created bool `json:"created"`
			}
			err := java.InvokeAs(ctx, "createSchemaTemplatePersistentJava", map[string]any{
				"clusterFile":        clusterFile,
				"templateName":       templateName,
				"schemaTemplateBody": g.body,
			}, &createResult)
			Expect(err).NotTo(HaveOccurred(), "Java CREATE SCHEMA TEMPLATE for %s", g.name)
			defer func() {
				var dropResult struct {
					Dropped bool `json:"dropped"`
				}
				_ = java.InvokeAs(context.Background(), "dropSchemaTemplatePersistentJava", map[string]any{
					"clusterFile":  clusterFile,
					"templateName": templateName,
				}, &dropResult)
			}()

			javaMD := loadStored(templateName)

			// Go: the SAME DDL through the production front end. The template
			// name must match Java's because it is the descriptor FILE name.
			goTmpl, buildErr := embedded.BuildSchemaTemplateFromDDLNamed(g.body, templateName)
			Expect(buildErr).NotTo(HaveOccurred(), "Go DDL front end for %s", g.name)
			goMD, protoErr := goTmpl.Underlying().ToProto()
			Expect(protoErr).NotTo(HaveOccurred())

			// Descriptor bytes: exact after wrapper-name canonicalization.
			javaRecords := proto.Clone(javaMD.GetRecords()).(*descriptorpb.FileDescriptorProto)
			goRecords := proto.Clone(goMD.GetRecords()).(*descriptorpb.FileDescriptorProto)
			normalizeWrapperNames(javaRecords)
			normalizeWrapperNames(goRecords)
			Expect(proto.Equal(goRecords, javaRecords)).To(BeTrue(),
				"%s: descriptor diverges\n--- go ---\n%s\n--- java ---\n%s", g.name,
				prototext.Format(goRecords), prototext.Format(javaRecords))
			jb, jerr := proto.MarshalOptions{Deterministic: true}.Marshal(javaRecords)
			Expect(jerr).NotTo(HaveOccurred())
			gb, gerr := proto.MarshalOptions{Deterministic: true}.Marshal(goRecords)
			Expect(gerr).NotTo(HaveOccurred())
			Expect(gb).To(Equal(jb),
				"%s: descriptor BYTES diverge after structural equality — field presence drift", g.name)

			// The rest of the stored MetaData: record types (primary keys!),
			// flags, version — compared with records detached (already
			// compared above under normalization).
			javaRest := proto.Clone(javaMD).(*gen.MetaData)
			goRest := proto.Clone(goMD).(*gen.MetaData)
			javaRest.Records, goRest.Records = nil, nil
			Expect(proto.Equal(normalizedProto(goRest), normalizedProto(javaRest))).To(BeTrue(),
				"%s: MetaData (minus records) diverges\n--- go ---\n%s\n--- java ---\n%s", g.name,
				prototext.Format(goRest), prototext.Format(javaRest))
		})
	}
})
