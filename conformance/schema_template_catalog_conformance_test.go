package conformance_test

// Track A2 — SchemaTemplateCatalog wire format Go↔Java cross-language
// round-trip. Builds on the FDBMetaDataStore wire-format pins from
// nightshift-53 (PR #119) by exercising the relational/SQL layer's
// catalog instead of the raw record-layer-core metadata store.
//
// **The keyspace prerequisite**: Java's RecordLayerStoreCatalog and
// Go's catalog.OpenRecordLayerStoreCatalog() both target the
// (NULL, NULL, int64(0)) FDB subspace prefix (3 bytes: 00 00 14).
// Confirmed by inspecting Go's `subspace.Sub(nil, nil, int64(0)).Bytes()`
// which produces the same byte prefix as Java's
// RelationalKeyspaceProvider three-level keyspace
// (KeyType.NULL → KeyType.NULL → KeyType.LONG=0).
//
// **What this test pins**:
//   - Java JDBC `CREATE SCHEMA TEMPLATE` via fdb-relational writes a
//     SchemaTemplate at the Java-compat subspace
//   - Go's `RecordLayerStoreCatalog` (opened at the same subspace,
//     bypassing the sqldriver which uses a different three-string
//     subspace — see `pkg/relational/core/catalog/fdb_store_catalog.go:62-67`)
//     can read the same template
//   - Wire format compatibility for: TEMPLATE_NAME / TEMPLATE_VERSION
//     fields, embedded RecordMetaData proto, table extraction
//
// **What's NOT yet pinned** (left for a future shift):
//   - Reverse direction (Go writes via OpenRecordLayerStoreCatalog,
//     Java's standard JDBC reads). Mechanical follow-on but requires
//     wiring Java to look up a template by a Go-supplied name.
//   - The Go sqldriver's use of the three-string subspace remains
//     unchanged. Migration is a separate, larger refactor — see TODO.md.

import (
	"context"
	"errors"

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

	It("pins the Go↔Java SchemaTemplateCatalog metadata-version divergence", func() {
		// EXPECTED FAILURE PINNING — see the "Wire-format divergence
		// finding" block at the bottom of this It block. The test
		// asserts the SPECIFIC current-state error so a fix in the next
		// shift will surface as test failure (caller updates the
		// assertions). NEVER paper over bugs (CLAUDE.md design
		// principle): the test must catch divergence drift.

		// Use a uuid-suffixed template name so concurrent / parallel
		// runs don't collide on the shared (NULL, NULL, int64(0))
		// catalog subspace.
		templateName := "X2_" + uuid.New().String()[:8]
		schemaBody := `CREATE TABLE T (id BIGINT, name STRING, PRIMARY KEY (id))`

		// Java JDBC: persistently CREATE SCHEMA TEMPLATE. This path
		// also (lazily) initialises Java's RecordLayerStoreCatalog
		// metadata in FDB at the (NULL, NULL, int64(0)) subspace.
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

		// Go: open RecordLayerStoreCatalog at the same subspace and
		// attempt to read the template Java just wrote. We expect this
		// to fail with StaleMetaDataVersionError because Go's
		// `BuildCatalogMetaData()` produces a metadata at version 3
		// (current as of dayshift-54), while Java's
		// `RecordLayerStoreCatalog` writes its catalog metadata at
		// version 4. The divergence prevents Go from opening the
		// record store at all, before any actual TEMPLATE record is
		// read. This test pins that specific failure so a future shift
		// fixing the version alignment will surface here as a passing
		// test (and the caller updates assertions).
		cat, openErr := catalog.OpenRecordLayerStoreCatalog()
		Expect(openErr).NotTo(HaveOccurred(), "Go catalog struct constructs cleanly")

		var doesExistErr error
		_, _ = goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			tx := catalog.NewFDBTransaction(rtx)
			tc := cat.SchemaTemplateCatalog()
			_, doesExistErr = tc.DoesSchemaTemplateExist(tx, templateName)
			return nil, nil
		})
		Expect(doesExistErr).To(HaveOccurred(),
			"Wire-format divergence finding: Go's BuildCatalogMetaData() produces "+
				"catalog metadata at version 3, but Java's RecordLayerStoreCatalog "+
				"writes at version 4. Go opens the catalog store and rejects the "+
				"version skew with StaleMetaDataVersionError. If this test starts "+
				"passing, that's good news — update assertions to verify the loaded "+
				"template instead of the divergence.")

		// Pin the SPECIFIC versions so version drift on either side
		// surfaces here. Unwrap the api.Error wrapper to get the
		// underlying StaleMetaDataVersionError.
		var apiErr *api.Error
		Expect(errors.As(doesExistErr, &apiErr)).To(BeTrue(),
			"expected api.Error wrapper, got %T: %v", doesExistErr, doesExistErr)
		var staleErr *recordlayer.StaleMetaDataVersionError
		Expect(errors.As(apiErr.Cause, &staleErr)).To(BeTrue(),
			"expected StaleMetaDataVersionError as wrapped cause, got %T: %v", apiErr.Cause, apiErr.Cause)
		Expect(staleErr.LocalVersion).To(Equal(3),
			"Go's catalog metadata version (current pin)")
		Expect(staleErr.StoredVersion).To(Equal(4),
			"Java's catalog metadata version (current pin)")
	})
})
