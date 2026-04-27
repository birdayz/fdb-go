package conformance_test

// Track A2 — cross-language SchemaTemplateCatalog round-trip. Java JDBC
// CREATE SCHEMA TEMPLATE writes; Go's OpenRecordLayerStoreCatalog reads.
// Pins keyspace, metadata-version, FileDescriptor rebuild, and union-
// descriptor naming compatibility. CLAUDE.md gotchas section + PR #120
// have the layer-by-layer detail.

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
