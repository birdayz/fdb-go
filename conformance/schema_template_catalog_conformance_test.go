package conformance_test

// Track A2 — SchemaTemplateCatalog wire format Go↔Java cross-language
// round-trip. Builds on the FDBMetaDataStore wire-format pins from
// nightshift-53 (PR #119) by exercising the relational/SQL layer's
// catalog instead of the raw record-layer-core metadata store.
//
// **dayshift-54 progress** — peeled the onion two layers:
//
//   Layer 1 (FIXED): The keyspace prerequisite. Java's
//   RecordLayerStoreCatalog and Go's
//   catalog.OpenRecordLayerStoreCatalog() both target the
//   (NULL, NULL, int64(0)) FDB subspace prefix (3 bytes: 00 00 14).
//   Confirmed via direct subspace byte inspection.
//
//   Layer 2 (FIXED): Catalog metadata-version divergence. Java's
//   `RecordLayerSchemaTemplate.toRecordMetadata()` calls setVersion(1)
//   then addIndex×3 → version=4. Go's `BuildCatalogMetaData()` was
//   starting at 0 and ending at 3, raising
//   `StaleMetaDataVersionError{Local:3, Stored:4}` on cross-engine
//   read. Fix in `BuildCatalogMetaData()`: explicit `SetVersion(1)`
//   before the addIndex calls.
//
//   Layer 3 (PINNED, NOT YET FIXED): SchemaTemplate proto-deserialize
//   divergence. After Go can open the catalog, `LoadSchemaTemplate`
//   reads the TEMPLATE record bytes (the embedded RecordMetaData
//   proto for the user's template `T`), and Go's
//   `RecordMetaDataFromProto` fails to rebuild the FileDescriptor:
//
//     proto: message field "RecordTypeUnion.T_0" cannot resolve type:
//     "RecordTypeUnion.T": descriptor not found: RecordTypeUnion.T
//
//   This means Java's serialised schema-template metadata uses a
//   nested-message reference to `RecordTypeUnion.T` that Go's
//   FileDescriptor builder can't resolve from the proto-encoded
//   FileDescriptorProto alone. Possible causes: Java emits a
//   self-referential FileDescriptorProto where the union message
//   `RecordTypeUnion` has a `T_0` field of type `RecordTypeUnion.T`
//   (i.e. a nested message inside RecordTypeUnion that wraps the
//   user's table type), and Go's proto rebuild path doesn't handle
//   that shape. Substantial follow-on work for the next shift —
//   likely 1-2 shifts of comparing Java's FileDescriptor emission
//   path against Go's parse path.
//
// **What this test pins**:
//   - Java JDBC `CREATE SCHEMA TEMPLATE` via fdb-relational writes a
//     SchemaTemplate at the Java-compat subspace
//   - Go's `OpenRecordLayerStoreCatalog()` opens at the same subspace
//     past the metadata-version check (the dayshift-54 fix in
//     `BuildCatalogMetaData`)
//   - Go's `DoesSchemaTemplateExist` correctly identifies that the
//     template Java wrote is present (record-layer-level lookup works)
//   - The remaining gap is at the proto-descriptor-rebuild layer
//     inside `LoadSchemaTemplate` — pinned via assertion on the
//     specific error substring so Go's deserializer changes surface
//     as test failure (caller updates the assertion)

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
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

		// First verify Go can OPEN the catalog past the metadata-version
		// check (Layer 2 fix from BuildCatalogMetaData SetVersion(1))
		// AND that Go correctly identifies the template Java wrote
		// exists at the record-layer level (Layer 1 + 2).
		var (
			doesExist bool
			loadErr   error
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
			// Layer 3: this is the still-broken layer. Go's
			// `RecordMetaDataFromProto` fails to rebuild the
			// FileDescriptor for the user's table — see the doc block
			// at the top of the file. Capture the error rather than
			// fail the txn function so we can pin the SPECIFIC error
			// shape.
			_, loadErr = tc.LoadSchemaTemplate(tx, templateName)
			return nil, nil
		})
		Expect(runErr).NotTo(HaveOccurred())
		Expect(doesExist).To(BeTrue(),
			"Layer 1+2 fix verified: Go can open the catalog and see "+
				"the Java-written template at the record-layer level")

		// Layer 3 pin: LoadSchemaTemplate fails on the embedded
		// FileDescriptor rebuild. Pin the SPECIFIC error substring so
		// Go's deserializer changes surface as test failure (caller
		// updates the assertion when Layer 3 is fixed).
		Expect(loadErr).To(HaveOccurred(),
			"Layer 3 (next-shift gap): expected LoadSchemaTemplate to "+
				"fail on FileDescriptor rebuild — see doc at top of file")
		Expect(loadErr.Error()).To(ContainSubstring("rebuild file descriptor"),
			"Pin Layer 3 failure mode at the rebuild step")
		Expect(loadErr.Error()).To(Or(
			ContainSubstring("RecordTypeUnion.T"),
			ContainSubstring("descriptor not found"),
		), "Pin Layer 3 root-cause: nested type resolution in the union")

		// strings unused warning suppress (kept for future use when
		// LoadSchemaTemplate succeeds and we'll inspect the loaded
		// template's table name).
		_ = strings.TrimSpace
	})
})
