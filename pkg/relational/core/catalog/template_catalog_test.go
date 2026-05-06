package catalog

import (
	"errors"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/metadata"
)

// buildTemplateAtVersion builds a RecordLayerSchemaTemplate with an
// explicit catalog-level version, using the demo proto.
func buildTemplateAtVersion(t *testing.T, name string, version int) api.SchemaTemplate {
	t.Helper()
	b := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	md, err := b.Build()
	if err != nil {
		t.Fatalf("build metadata: %v", err)
	}
	tmpl, err := metadata.NewRecordLayerSchemaTemplateWithVersion(name, md, version)
	if err != nil {
		t.Fatalf("NewRecordLayerSchemaTemplateWithVersion: %v", err)
	}
	return tmpl
}

func TestTemplateCatalog_CreateAndLoad(t *testing.T) {
	t.Parallel()
	c := NewInMemorySchemaTemplateCatalog()
	tx := NewInMemoryTransaction()
	tmpl := buildTemplateAtVersion(t, "demo", 1)

	if ok, _ := c.DoesSchemaTemplateExist(tx, "demo"); ok {
		t.Error("fresh catalog: exists = true")
	}

	if err := c.CreateTemplate(tx, tmpl); err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}

	if ok, _ := c.DoesSchemaTemplateExist(tx, "demo"); !ok {
		t.Error("after create: exists = false")
	}

	if ok, _ := c.DoesSchemaTemplateExistAtVersion(tx, "demo", 1); !ok {
		t.Error("version 1 missing")
	}
	if ok, _ := c.DoesSchemaTemplateExistAtVersion(tx, "demo", 2); ok {
		t.Error("version 2 reported present")
	}

	got, err := c.LoadSchemaTemplate(tx, "demo")
	if err != nil {
		t.Fatalf("LoadSchemaTemplate: %v", err)
	}
	if got.MetadataName() != "demo" || got.Version() != 1 {
		t.Errorf("got name=%q version=%d, want demo/1", got.MetadataName(), got.Version())
	}
}

func TestTemplateCatalog_CreateDuplicateErrors(t *testing.T) {
	t.Parallel()
	c := NewInMemorySchemaTemplateCatalog()
	tx := NewInMemoryTransaction()
	tmpl := buildTemplateAtVersion(t, "demo", 1)

	if err := c.CreateTemplate(tx, tmpl); err != nil {
		t.Fatal(err)
	}
	err := c.CreateTemplate(tx, tmpl)
	if err == nil {
		t.Fatal("duplicate CreateTemplate succeeded")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeDuplicateSchemaTemplate {
		t.Errorf("Code = %q, want %q", apiErr.Code, api.ErrCodeDuplicateSchemaTemplate)
	}
}

func TestTemplateCatalog_LoadLatestVersion(t *testing.T) {
	t.Parallel()
	c := NewInMemorySchemaTemplateCatalog()
	tx := NewInMemoryTransaction()
	for _, v := range []int{1, 3, 2} {
		if err := c.CreateTemplate(tx, buildTemplateAtVersion(t, "demo", v)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := c.LoadSchemaTemplate(tx, "demo")
	if err != nil {
		t.Fatalf("LoadSchemaTemplate: %v", err)
	}
	if got.Version() != 3 {
		t.Errorf("LoadSchemaTemplate returned version %d, want 3 (latest)", got.Version())
	}
}

func TestTemplateCatalog_LoadSpecificVersion(t *testing.T) {
	t.Parallel()
	c := NewInMemorySchemaTemplateCatalog()
	tx := NewInMemoryTransaction()
	for _, v := range []int{1, 2} {
		if err := c.CreateTemplate(tx, buildTemplateAtVersion(t, "demo", v)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := c.LoadSchemaTemplateAtVersion(tx, "demo", 1)
	if err != nil {
		t.Fatalf("LoadSchemaTemplateAtVersion: %v", err)
	}
	if got.Version() != 1 {
		t.Errorf("LoadSchemaTemplateAtVersion returned version %d, want 1", got.Version())
	}

	// Missing version errors with UnknownSchemaTemplate.
	_, err = c.LoadSchemaTemplateAtVersion(tx, "demo", 42)
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeUnknownSchemaTemplate {
		t.Errorf("Code = %q, want %q", apiErr.Code, api.ErrCodeUnknownSchemaTemplate)
	}
}

func TestTemplateCatalog_LoadMissingErrors(t *testing.T) {
	t.Parallel()
	c := NewInMemorySchemaTemplateCatalog()
	tx := NewInMemoryTransaction()

	_, err := c.LoadSchemaTemplate(tx, "nope")
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeUnknownSchemaTemplate {
		t.Errorf("Code = %q, want %q", apiErr.Code, api.ErrCodeUnknownSchemaTemplate)
	}
}

func TestTemplateCatalog_DeleteAllVersions(t *testing.T) {
	t.Parallel()
	c := NewInMemorySchemaTemplateCatalog()
	tx := NewInMemoryTransaction()
	for _, v := range []int{1, 2, 3} {
		if err := c.CreateTemplate(tx, buildTemplateAtVersion(t, "demo", v)); err != nil {
			t.Fatal(err)
		}
	}

	if err := c.DeleteTemplate(tx, "demo", true); err != nil {
		t.Fatalf("DeleteTemplate: %v", err)
	}
	if ok, _ := c.DoesSchemaTemplateExist(tx, "demo"); ok {
		t.Error("exists = true after DeleteTemplate")
	}
	// Delete of missing with throw errors.
	err := c.DeleteTemplate(tx, "demo", true)
	if err == nil {
		t.Error("DeleteTemplate(missing, throw) didn't error")
	}
	// Without throw: no error.
	if err := c.DeleteTemplate(tx, "demo", false); err != nil {
		t.Errorf("DeleteTemplate(missing, no-throw): %v", err)
	}
}

func TestTemplateCatalog_DeleteOneVersion(t *testing.T) {
	t.Parallel()
	c := NewInMemorySchemaTemplateCatalog()
	tx := NewInMemoryTransaction()
	for _, v := range []int{1, 2} {
		if err := c.CreateTemplate(tx, buildTemplateAtVersion(t, "demo", v)); err != nil {
			t.Fatal(err)
		}
	}

	if err := c.DeleteTemplateVersion(tx, "demo", 1, true); err != nil {
		t.Fatalf("DeleteTemplateVersion(1): %v", err)
	}
	if ok, _ := c.DoesSchemaTemplateExistAtVersion(tx, "demo", 1); ok {
		t.Error("version 1 still exists after delete")
	}
	if ok, _ := c.DoesSchemaTemplateExistAtVersion(tx, "demo", 2); !ok {
		t.Error("version 2 lost on a v1 delete")
	}

	// Deleting the last remaining version should also remove the parent
	// entry so DoesSchemaTemplateExist returns false.
	if err := c.DeleteTemplateVersion(tx, "demo", 2, true); err != nil {
		t.Fatal(err)
	}
	if ok, _ := c.DoesSchemaTemplateExist(tx, "demo"); ok {
		t.Error("last-version delete didn't remove parent template entry")
	}
}

func TestTemplateCatalog_ListTemplates(t *testing.T) {
	t.Parallel()
	c := NewInMemorySchemaTemplateCatalog()
	tx := NewInMemoryTransaction()
	// Insert out of order; the listing must sort by (name, version).
	_ = c.CreateTemplate(tx, buildTemplateAtVersion(t, "zulu", 1))
	_ = c.CreateTemplate(tx, buildTemplateAtVersion(t, "alpha", 2))
	_ = c.CreateTemplate(tx, buildTemplateAtVersion(t, "alpha", 1))

	rs, err := c.ListTemplates(tx)
	if err != nil {
		t.Fatalf("ListTemplates: %v", err)
	}
	defer rs.Close()

	var rows []struct {
		name    string
		version int64
	}
	for rs.Next() {
		n, err := rs.String(1)
		if err != nil {
			t.Fatalf("String(1): %v", err)
		}
		v, err := rs.Long(2)
		if err != nil {
			t.Fatalf("Long(2): %v", err)
		}
		rows = append(rows, struct {
			name    string
			version int64
		}{n, v})
	}
	want := []struct {
		name    string
		version int64
	}{
		{"alpha", 1}, {"alpha", 2}, {"zulu", 1},
	}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows, want %d: %v", len(rows), len(want), rows)
	}
	for i := range rows {
		if rows[i] != want[i] {
			t.Errorf("row %d: got %+v, want %+v", i, rows[i], want[i])
		}
	}
}
