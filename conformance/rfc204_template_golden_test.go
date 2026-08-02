//go:build bazelrunfiles

package conformance_test

// RFC-204 Phase 1 byte-goldens — the STORED schema-template bytes, three
// ways, all equal:
//
//	live Java catalog bytes == committed golden bytes == Go catalog bytes
//
// For every representative template (each §1.2 construct: structs,
// struct-of-struct sharing, forward references, arrays flat and wrapped,
// nested primary keys, name escaping, UUID/VECTOR struct fields, deep
// nesting, nullability variants — and explicitly the NULLABLE
// ARRAY-OF-STRUCT column, the NullableArrayWrapper emission shape), Java
// (fdb-relational 4.12.11.0 over JDBC) persists the template into the shared
// catalog and the raw META_DATA blob is read back; Go builds the IDENTICAL
// DDL through its own front end, persists it through its own template
// catalog, and ITS raw stored blob is read back. Both are compared — as
// BYTES — against each other and against the committed golden under
// testdata/rfc204/, so the pin survives (and diffs meaningfully) without a
// live JVM in the loop. Deliberately no clearProto2Defaults-style
// normalization: explicitly-set defaults (nullInterpretation NOT_UNIQUE,
// explicit_key 0) ARE the stored bytes and stay under test.
//
// The ONE token canonicalized before comparison is the NullableArrayWrapper
// message name: Java names it ProtoUtils.uniqueTypeName() = "__type__" +
// UUID.randomUUID() — RANDOM per serialization (two identical templates get
// different names; every reader is structural,
// NullableArrayUtils.isWrappedArrayDescriptor). Both sides' wrapper names
// are rewritten in encounter order; everything else — placement, order,
// labels, numbers, type_name references, union shape, options, record
// types, primary keys, flags — is compared exactly.
//
// Re-blessing (only with a Java-verified behaviour change): run with
// --test_env=RFC204_GOLDEN_OUT=<abs path to conformance/testdata/rfc204>
// --sandbox_writable_path=<same path>, then commit the diff.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bazelbuild/rules_go/go/runfiles"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
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
		// The NULLABLE array-of-struct column — the NullableArrayWrapper
		// referencing a top-level struct element.
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

// isWrapperShape reports the NullableArrayWrapper SHAPE — exactly one
// field, named "values", number 1, LABEL_REPEATED — the same structural
// test every runtime reader applies
// (NullableArrayUtils.isWrappedArrayDescriptor). Gating the canonicalizer
// on the shape rather than a "__type__" name prefix means a user-declared
// struct that happens to be named "__type__…" is not silently rewritten,
// and a wrapper is rewritten no matter what its (random) name looks like.
func isWrapperShape(m *descriptorpb.DescriptorProto) bool {
	if len(m.GetField()) != 1 {
		return false
	}
	f := m.GetField()[0]
	return f.GetName() == "values" && f.GetNumber() == 1 &&
		f.GetLabel() == descriptorpb.FieldDescriptorProto_LABEL_REPEATED
}

// normalizeWrapperNames canonicalizes every wrapper-SHAPED message name
// (and the field type_name references to it) in encounter order. Mutates
// fdp.
func normalizeWrapperNames(fdp *descriptorpb.FileDescriptorProto) {
	rename := map[string]string{}
	for _, m := range fdp.GetMessageType() {
		if isWrapperShape(m) {
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

// normalizeStoredTemplate parses raw stored MetaData bytes, canonicalizes
// wrapper names, and re-marshals deterministically — the byte form all
// three sides (Java catalog, committed golden, Go catalog) compare in.
func normalizeStoredTemplate(raw []byte) ([]byte, *gen.MetaData, error) {
	md := &gen.MetaData{}
	if err := proto.Unmarshal(raw, md); err != nil {
		return nil, nil, fmt.Errorf("unmarshal stored MetaData: %w", err)
	}
	if md.GetRecords() != nil {
		normalizeWrapperNames(md.GetRecords())
	}
	out, err := proto.MarshalOptions{Deterministic: true}.Marshal(md)
	if err != nil {
		return nil, nil, err
	}
	return out, md, nil
}

var _ = Describe("RFC-204 schema-template byte-goldens (Java catalog == committed == Go catalog)", func() {
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

	// rawStoredTemplate reads the raw META_DATA blob for templateName back
	// out of the given catalog — the bytes as PERSISTED, not an in-memory
	// re-serialization.
	rawStoredTemplate := func(cat *catalog.RecordLayerStoreCatalog, templateName string) []byte {
		var raw []byte
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
				b, bErr := rs.Bytes(3)
				if bErr != nil {
					return nil, bErr
				}
				raw = b
			}
			return nil, rs.Err()
		})
		Expect(runErr).NotTo(HaveOccurred())
		Expect(raw).NotTo(BeNil(), "template %s not found in catalog", templateName)
		return raw
	}

	goldenPath := func(name string) (string, bool) {
		r, err := runfiles.New()
		if err != nil {
			return "", false
		}
		p, err := r.Rlocation("_main/conformance/testdata/rfc204/" + name + ".binpb")
		if err != nil {
			return "", false
		}
		if _, statErr := os.Stat(p); statErr != nil {
			return "", false
		}
		return p, true
	}

	for _, g := range rfc204Goldens {
		g := g
		It("matches stored template bytes for "+g.name, func() {
			templateName := "RFC204G_" + g.name

			// --- Java side: create in the shared catalog, read raw bytes back.
			var dropResult struct {
				Dropped bool `json:"dropped"`
			}
			// Fixed names: drop a leftover from a failed earlier run first.
			_ = java.InvokeAs(ctx, "dropSchemaTemplatePersistentJava", map[string]any{
				"clusterFile":  clusterFile,
				"templateName": templateName,
			}, &dropResult)
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
				_ = java.InvokeAs(context.Background(), "dropSchemaTemplatePersistentJava", map[string]any{
					"clusterFile":  clusterFile,
					"templateName": templateName,
				}, &dropResult)
			}()
			sharedCat, err := catalog.OpenRecordLayerStoreCatalog()
			Expect(err).NotTo(HaveOccurred())
			javaNorm, _, err := normalizeStoredTemplate(rawStoredTemplate(sharedCat, templateName))
			Expect(err).NotTo(HaveOccurred())

			// --- Go side: same DDL through the production front end,
			// PERSISTED through Go's template catalog (in a private
			// subspace — the shared one already holds Java's row under this
			// name), and read back as stored bytes.
			goTmpl, buildErr := embedded.BuildSchemaTemplateFromDDLNamed(g.body, templateName)
			Expect(buildErr).NotTo(HaveOccurred(), "Go DDL front end for %s", g.name)
			goCat, err := catalog.NewRecordLayerStoreCatalog(
				subspace.Sub("rfc204golden", g.name, uuid.NewString()))
			Expect(err).NotTo(HaveOccurred())
			_, runErr := goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				tx := catalog.NewFDBTransaction(rtx)
				return nil, goCat.SchemaTemplateCatalog().CreateTemplate(tx, goTmpl)
			})
			Expect(runErr).NotTo(HaveOccurred())
			goNorm, goMD, err := normalizeStoredTemplate(rawStoredTemplate(goCat, templateName))
			Expect(err).NotTo(HaveOccurred())

			// --- Committed golden: written on RFC204_GOLDEN_OUT re-bless
			// runs, asserted on every normal run so the pin holds (and
			// diffs meaningfully) beyond the live-JVM differential.
			if outDir := os.Getenv("RFC204_GOLDEN_OUT"); outDir != "" {
				Expect(os.MkdirAll(outDir, 0o755)).To(Succeed())
				Expect(os.WriteFile(filepath.Join(outDir, g.name+".binpb"), javaNorm, 0o644)).To(Succeed())
			}
			committed := javaNorm
			if p, ok := goldenPath(g.name); ok {
				b, readErr := os.ReadFile(p)
				Expect(readErr).NotTo(HaveOccurred())
				committed = b
				Expect(javaNorm).To(Equal(committed),
					"%s: LIVE Java bytes drifted from the committed golden — either the pinned Java "+
						"version changed behaviour or the golden is stale; re-bless with RFC204_GOLDEN_OUT "+
						"only after verifying which", g.name)
			} else {
				AddReportEntry("rfc204-golden-missing",
					fmt.Sprintf("%s: no committed golden staged; compared against live Java only", g.name))
			}

			if !bytes.Equal(goNorm, committed) {
				// Structural rendering first for a readable diff; the
				// authoritative assertion is on bytes.
				committedMD := &gen.MetaData{}
				Expect(proto.Unmarshal(committed, committedMD)).To(Succeed())
				Expect(prototext.Format(goMD)).To(Equal(prototext.Format(committedMD)),
					"%s: Go stored bytes diverge (structural rendering of the byte diff)", g.name)
				Expect(goNorm).To(Equal(committed),
					"%s: Go stored bytes diverge with identical structure — field presence/order drift", g.name)
			}
		})
	}
})
