package conformance_test

// Track A2 — SchemaTemplateCatalog wire format Go↔Java cross-language
// round-trip. Builds on the FDBMetaDataStore wire-format pins from
// nightshift-53 (PR #119) by exercising the relational/SQL layer's
// catalog instead of the raw record-layer-core metadata store.
//
// **dayshift-54 closes A2** — peeled the onion four layers, all fixed:
//
//   Layer 1 (FIXED): keyspace prerequisite. Java's
//   `RecordLayerStoreCatalog` and Go's
//   `catalog.OpenRecordLayerStoreCatalog()` both target the
//   (NULL, NULL, int64(0)) FDB subspace prefix (3 bytes: 00 00 14).
//   Confirmed via direct subspace byte inspection.
//
//   Layer 2 (FIXED): catalog metadata-version. Java's
//   `RecordLayerSchemaTemplate.toRecordMetadata()` calls `setVersion(1)`
//   then `addIndex×3` → v4. Go's `BuildCatalogMetaData()` was starting
//   at 0 → v3 → `StaleMetaDataVersionError{Local:3, Stored:4}` on
//   cross-engine read. Fix in `BuildCatalogMetaData()`: explicit
//   `SetVersion(1)` before the addIndex calls.
//
//   Layer 3 (FIXED): proto FileDescriptor rebuild. Java's
//   `FileDescriptorSerializer` emits relative type-name references
//   like `setTypeName("T")` (no leading dot, no package) for a
//   `RecordTypeUnion` field whose type is the user's top-level
//   message `T`. Go's `protodesc.NewFile` resolves the reference
//   relative to `RecordTypeUnion`'s scope first (looking for
//   `RecordTypeUnion.T`) and doesn't fall back to file-scope; result:
//   `descriptor not found: RecordTypeUnion.T`. The
//   `protodesc.FileOptions{AllowUnresolvable:true}` option doesn't
//   help. Fix: pre-process the FileDescriptorProto to rewrite each
//   field's relative type-name to absolute (`"T"` → `".T"`) before
//   passing to `protodesc.NewFile`. Implementation:
//   `absolutizeFieldTypeNames` in `metadata_proto.go`.
//
//   Layer 4 (FIXED): union-descriptor field naming. Go's record-layer
//   union-descriptor parser expected fields named with a leading
//   underscore (`_TypeName`) per RecordLayer's UnionDescriptor
//   convention; Java's fdb-relational `FileDescriptorSerializer`
//   emits fields named `TypeName_N` (type name + counter) per its
//   line 107 (`typeDescriptor + "_" + fieldCounter`). Go dropped all
//   record types from such metadata, raising "no record types
//   defined in meta-data". Fix: `setRecordsWithUnionName` now also
//   accepts message-typed fields and derives the record type name
//   from the field's TYPE reference rather than only the field
//   name. AND `findUnionDescriptorName` recognises both
//   `UnionDescriptor` (RecordLayer-core default) and
//   `RecordTypeUnion` (fdb-relational default).
//
// **What this test pins**: end-to-end cross-language round-trip —
// Java JDBC `CREATE SCHEMA TEMPLATE` writes a template at the shared
// catalog subspace; Go's `OpenRecordLayerStoreCatalog` opens at the
// same subspace, `DoesSchemaTemplateExist` finds the template, and
// `LoadSchemaTemplate` decodes the embedded metadata to recover the
// user's table definition.
//
// **What's NOT yet pinned** (next-shift work):
//   - Reverse direction (Go writes via OpenRecordLayerStoreCatalog,
//     Java's standard JDBC reads).
//   - The Go sqldriver's three-string subspace remains divergent.
//     This is a sqldriver-specific concern, not a catalog-package
//     concern.

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/catalog"
)

var _ = Describe("SchemaTemplateCatalog cross-language round-trip (A2)", func() {
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

	It("Go reads SchemaTemplate persisted by Java JDBC at the shared catalog subspace", func() {
		// Use a uuid-suffixed template name so concurrent / parallel
		// runs don't collide on the shared (NULL, NULL, int64(0))
		// catalog subspace.
		templateName := "X2_" + uuid.New().String()[:8]
		schemaBody := `CREATE TABLE T (id BIGINT, name STRING, PRIMARY KEY (id))`

		// Java JDBC: persistently CREATE SCHEMA TEMPLATE.
		var createResult struct {
			Created      bool   `json:"created"`
			TemplateName string `json:"templateName"`
		}
		params := map[string]any{
			"clusterFile":        clusterFile,
			"templateName":       templateName,
			"schemaTemplateBody": schemaBody,
		}
		err := java.InvokeAs(ctx, "createSchemaTemplatePersistentJava", params, &createResult)
		Expect(err).NotTo(HaveOccurred(), "Java should create the template")
		Expect(createResult.Created).To(BeTrue())
		Expect(createResult.TemplateName).To(Equal(templateName))

		// Cleanup hook (runs even on test failure).
		defer func() {
			dropParams := map[string]any{
				"clusterFile":  clusterFile,
				"templateName": templateName,
			}
			var dropResult struct {
				Dropped bool `json:"dropped"`
			}
			_ = java.InvokeAs(context.Background(), "dropSchemaTemplatePersistentJava", dropParams, &dropResult)
		}()

		// Go: open RecordLayerStoreCatalog at the shared subspace
		// (DefaultCatalogSubspace = (NULL, NULL, int64(0))). Same place
		// Java's CREATE SCHEMA TEMPLATE wrote to. With dayshift-54's
		// version-alignment fix in BuildCatalogMetaData, the
		// FDBRecordStore opens cleanly past the metadata-version
		// check (Go=4, Java=4 — both ending at version 4 after
		// SetVersion(1) + 3 addIndex bumps).
		cat, openErr := catalog.OpenRecordLayerStoreCatalog()
		Expect(openErr).NotTo(HaveOccurred(), "Go catalog struct constructs cleanly")

		// Read the template back via the Go catalog API. Wrap the
		// recordlayer.FDBRecordContext via NewFDBTransaction (the
		// catalog API takes api.Transaction).
		var (
			doesExist bool
			loaded    api.SchemaTemplate
		)
		_, runErr := goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			tx := catalog.NewFDBTransaction(rtx)
			tc := cat.SchemaTemplateCatalog()
			exists, doesErr := tc.DoesSchemaTemplateExist(tx, templateName)
			if doesErr != nil {
				return nil, fmt.Errorf("DoesSchemaTemplateExist: %w", doesErr)
			}
			doesExist = exists
			if !exists {
				return nil, nil
			}
			tmpl, loadErr := tc.LoadSchemaTemplate(tx, templateName)
			if loadErr != nil {
				return nil, fmt.Errorf("LoadSchemaTemplate: %w", loadErr)
			}
			loaded = tmpl
			return nil, nil
		})
		Expect(runErr).NotTo(HaveOccurred())
		Expect(doesExist).To(BeTrue(), "Go catalog should see the Java-written template")
		Expect(loaded).NotTo(BeNil())
		Expect(loaded.MetadataName()).To(Equal(templateName))

		// Verify the template's table is round-tripped (one CREATE
		// TABLE in the body → one Table in the loaded template).
		tables, tablesErr := loaded.Tables()
		Expect(tablesErr).NotTo(HaveOccurred())
		Expect(tables).To(HaveLen(1), "expected exactly one table T")
		Expect(tables[0].MetadataName()).To(Equal("T"))
	})
})
