package catalog

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/metadata"
)

// cachedTemplateRow is a template row of name at version holding its own copy
// of md, so equal rows share no backing array.
func cachedTemplateRow(name string, version int32, md []byte) *gen.Templates {
	return &gen.Templates{
		TEMPLATE_NAME: proto.String(name), TEMPLATE_VERSION: proto.Int32(version), META_DATA: append([]byte(nil), md...),
	}
}

// templateCached is templateCache's entry for the row (name, version, md).
func templateCached(name string, version int32, md []byte) (api.SchemaTemplate, bool) {
	templateCache.mu.Lock()
	defer templateCache.mu.Unlock()
	t, ok := templateCache.entries[templateCacheKey{name: name, version: version, metaData: sha256.Sum256(md)}]
	return t, ok
}

func mustLoadTemplate(t *testing.T, row *gen.Templates) api.SchemaTemplate {
	t.Helper()
	tmpl, err := loadTemplate(row)
	if err != nil {
		t.Fatalf("loadTemplate(%s, %d): %v", row.GetTEMPLATE_NAME(), row.GetTEMPLATE_VERSION(), err)
	}
	return tmpl
}

// metaDataVersion is the version of the RecordMetaData tmpl was built from.
func metaDataVersion(t *testing.T, tmpl api.SchemaTemplate) int {
	t.Helper()
	rl, ok := tmpl.(*metadata.RecordLayerSchemaTemplate)
	if !ok {
		t.Fatalf("template is %T, want *metadata.RecordLayerSchemaTemplate", tmpl)
	}
	return rl.Underlying().Version()
}

func templateHasIndex(t *testing.T, tmpl api.SchemaTemplate, name string) bool {
	t.Helper()
	indexes, err := tmpl.Indexes()
	if err != nil {
		t.Fatal(err)
	}
	return slices.Contains(indexes, name)
}

// Two loads of byte-identical rows return one template object; deserializing
// the same row directly (the control) builds a new one.
func TestLoadTemplate_ByteIdenticalRowsShareOneTemplate(t *testing.T) {
	t.Parallel()
	name := t.Name()
	md := demoMetaData(t, 3, nil)

	first := mustLoadTemplate(t, cachedTemplateRow(name, 1, md))
	second := mustLoadTemplate(t, cachedTemplateRow(name, 1, md))
	if first != second {
		t.Fatal("two loads of byte-identical rows built two templates")
	}
	fresh, err := deserializeTemplate(cachedTemplateRow(name, 1, md))
	if err != nil {
		t.Fatal(err)
	}
	if fresh == first {
		t.Fatal("deserializeTemplate returned the cached object; the identity above proves nothing")
	}
	if cached, ok := templateCached(name, 1, md); !ok || cached != first {
		t.Fatalf("cache entry = (%v, %t), want the loaded template", cached, ok)
	}
	if first.MetadataName() != name || first.Version() != 1 || metaDataVersion(t, first) != 3 {
		t.Fatalf("template (%s, %d, metadata %d), want (%s, 1, metadata 3)",
			first.MetadataName(), first.Version(), metaDataVersion(t, first), name)
	}
}

// A row rewritten in place (same name and version, other META_DATA bytes) is
// a miss that reads the new bytes, and each row's bytes keep their own entry.
func TestLoadTemplate_RewrittenMetaDataIsAMiss(t *testing.T) {
	t.Parallel()
	name := t.Name()
	before := demoMetaData(t, 3, nil)
	after := demoMetaData(t, 4, withPriceIndex(4, 4, recordlayer.Field("price"), nil))

	old := mustLoadTemplate(t, cachedTemplateRow(name, 2, before))
	rewritten := mustLoadTemplate(t, cachedTemplateRow(name, 2, after))
	if rewritten == old {
		t.Fatal("a row with other META_DATA bytes returned the template cached for the old bytes")
	}
	if got := metaDataVersion(t, old); got != 3 {
		t.Errorf("old template metadata version %d, want 3", got)
	}
	if got := metaDataVersion(t, rewritten); got != 4 {
		t.Errorf("rewritten template metadata version %d, want 4", got)
	}
	if templateHasIndex(t, old, "Order$price") {
		t.Error("old template has the index only the rewritten bytes declare")
	}
	if !templateHasIndex(t, rewritten, "Order$price") {
		t.Error("rewritten template lacks the index its bytes declare")
	}
	if rewritten.MetadataName() != name || rewritten.Version() != 2 {
		t.Errorf("rewritten template (%s, %d), want (%s, 2)", rewritten.MetadataName(), rewritten.Version(), name)
	}
	if again := mustLoadTemplate(t, cachedTemplateRow(name, 2, before)); again != old {
		t.Error("the old bytes no longer load their cached template")
	}
	if again := mustLoadTemplate(t, cachedTemplateRow(name, 2, after)); again != rewritten {
		t.Error("the rewritten bytes no longer load their cached template")
	}
}

// A row that fails to deserialize returns its error on every load and leaves
// no entry, and a valid row of the same name and version then loads.
func TestLoadTemplate_ErrorsAreNotCached(t *testing.T) {
	t.Parallel()
	name := t.Name()
	unknownType := demoMetaData(t, 3, func(p *gen.MetaData) {
		p.Indexes = append(p.Indexes, &gen.Index{
			Name:           proto.String("Nope$x"),
			RecordType:     []string{"Nope"},
			RootExpression: recordlayer.Field("x").ToKeyExpression(),
			Type:           proto.String(recordlayer.IndexTypeValue),
		})
	})
	for _, c := range []struct {
		name string
		md   []byte
	}{
		{"unparseable", []byte{0xff, 0xff, 0xff, 0xff}},
		{"from-proto", unknownType},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, control := deserializeTemplate(cachedTemplateRow(name, 1, c.md))
			var controlErr *api.Error
			if !errors.As(control, &controlErr) {
				t.Fatalf("fixture: deserializeTemplate error = %v, want an *api.Error", control)
			}
			for i := range 2 {
				tmpl, err := loadTemplate(cachedTemplateRow(name, 1, c.md))
				var apiErr *api.Error
				if tmpl != nil || !errors.As(err, &apiErr) || apiErr.Code != controlErr.Code {
					t.Fatalf("load %d = (%v, %v), want deserializeTemplate's %s error", i, tmpl, err, controlErr.Code)
				}
				if cached, ok := templateCached(name, 1, c.md); ok {
					t.Fatalf("load %d cached %v for a row that failed", i, cached)
				}
			}
		})
	}
	valid := demoMetaData(t, 3, nil)
	tmpl := mustLoadTemplate(t, cachedTemplateRow(name, 1, valid))
	if metaDataVersion(t, tmpl) != 3 {
		t.Fatalf("valid row loaded metadata version %d, want 3", metaDataVersion(t, tmpl))
	}
	if cached, ok := templateCached(name, 1, valid); !ok || cached != tmpl {
		t.Fatal("the valid row was not cached")
	}
}

// The cache holds templateCacheEntries rows and evicts the oldest first. It
// resets the process-wide cache, so it is deliberately not parallel: Go runs
// every sequential top-level test before a parallel one resumes.
func TestLoadTemplate_EvictsOldestBeyondCapacity(t *testing.T) {
	reset := func() {
		templateCache.mu.Lock()
		templateCache.entries, templateCache.order = nil, nil
		templateCache.mu.Unlock()
	}
	reset()
	t.Cleanup(reset)
	md := demoMetaData(t, 1, nil)
	name := func(i int) string { return fmt.Sprintf("%s/%d", t.Name(), i) }

	loaded := make([]api.SchemaTemplate, templateCacheEntries+1)
	for i := range loaded {
		loaded[i] = mustLoadTemplate(t, cachedTemplateRow(name(i), 1, md))
	}
	templateCache.mu.Lock()
	size, order := len(templateCache.entries), len(templateCache.order)
	templateCache.mu.Unlock()
	if size != templateCacheEntries || order != templateCacheEntries {
		t.Fatalf("cache holds %d entries (%d ordered), want the bound %d", size, order, templateCacheEntries)
	}
	if _, ok := templateCached(name(0), 1, md); ok {
		t.Fatal("the first row inserted survived templateCacheEntries newer ones")
	}
	for i := 1; i < len(loaded); i++ {
		if cached, ok := templateCached(name(i), 1, md); !ok || cached != loaded[i] {
			t.Fatalf("row %d of the newest %d was evicted", i, templateCacheEntries)
		}
	}

	// The evicted row is a miss: a new object, inserted as the newest, which
	// evicts the next oldest; a resident row is still a hit.
	again := mustLoadTemplate(t, cachedTemplateRow(name(0), 1, md))
	if again == loaded[0] {
		t.Fatal("an evicted row returned its old template")
	}
	if cached, ok := templateCached(name(0), 1, md); !ok || cached != again {
		t.Fatal("the reloaded row was not cached")
	}
	if _, ok := templateCached(name(1), 1, md); ok {
		t.Fatal("reinserting the evicted row did not evict the next oldest")
	}
	if hit := mustLoadTemplate(t, cachedTemplateRow(name(2), 1, md)); hit != loaded[2] {
		t.Fatal("a resident row was rebuilt")
	}
}

// LoadSchemaTemplate and LoadSchemaTemplateAtVersion read the row in each
// transaction and share the template for unchanged bytes, and a row rewritten
// in place (a restore or repair) is read as written.
func TestFDB_LoadSchemaTemplateSharesUnchangedRowsAndReadsRewrites(t *testing.T) {
	t.Parallel()
	e := newRestoreEnv(t)
	e.name = "template_cache_rewrite"
	load := func() (latest, exact api.SchemaTemplate) {
		t.Helper()
		mustRun(t, e.run, func(tx api.Transaction) error {
			tc := e.cat.SchemaTemplateCatalog()
			var err error
			if latest, err = tc.LoadSchemaTemplate(tx, e.name); err != nil {
				return err
			}
			exact, err = tc.LoadSchemaTemplateAtVersion(tx, e.name, 1)
			return err
		})
		return latest, exact
	}

	e.writeRow(1, demoMetaData(t, 3, nil))
	latest, exact := load()
	if latest != exact {
		t.Error("the latest and exact-version reads of one row built two templates")
	}
	if again, _ := load(); again != latest {
		t.Error("a later transaction reading the unchanged row built a new template")
	}
	if metaDataVersion(t, latest) != 3 || templateHasIndex(t, latest, "Order$price") {
		t.Fatalf("first row read as metadata version %d", metaDataVersion(t, latest))
	}

	e.writeRow(1, demoMetaData(t, 4, withPriceIndex(4, 4, recordlayer.Field("price"), nil)))
	rewrittenLatest, rewrittenExact := load()
	for name, tmpl := range map[string]api.SchemaTemplate{"latest": rewrittenLatest, "exact": rewrittenExact} {
		if tmpl == latest {
			t.Errorf("%s read of the rewritten row returned the template of the old bytes", name)
		}
		if metaDataVersion(t, tmpl) != 4 || !templateHasIndex(t, tmpl, "Order$price") {
			t.Errorf("%s read of the rewritten row has metadata version %d", name, metaDataVersion(t, tmpl))
		}
	}
}
