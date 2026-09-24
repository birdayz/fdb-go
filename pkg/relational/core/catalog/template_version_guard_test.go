package catalog

import (
	"strconv"
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/metadata"
)

// The version guard and the gone-version refusal (template_bindings.go) on the
// in-memory catalog, whose template catalog reads the bindings of the store
// catalog that owns it.

func TestInMemory_VersionGuard_FreshTemplateRefusedWhileDroppedVersionBound(t *testing.T) {
	t.Parallel()
	c := NewInMemoryStoreCatalog()
	tx := NewInMemoryTransaction()
	tc := c.SchemaTemplateCatalog()

	if err := tc.CreateTemplate(tx, buildTemplateAtVersion(t, "g", 3)); err != nil {
		t.Fatal(err)
	}
	for _, db := range []string{"/db2", "/db1"} {
		if err := c.SaveSchema(tx, buildTemplateAtVersion(t, "g", 3).GenerateSchema(db, "s"), true); err != nil {
			t.Fatal(err)
		}
	}
	if err := tc.DeleteTemplate(tx, "g", true); err != nil {
		t.Fatal(err)
	}
	for _, v := range []int{0, 1, 3, 7} {
		wantAPIError(t, tc.CreateTemplate(tx, buildTemplateAtVersion(t, "g", v)), api.ErrCodeInvalidSchemaTemplate,
			"schema template g version "+strconv.Itoa(v)+" cannot be created: schemas are still bound to its dropped version 3 (/db1/s)")
	}
	for _, db := range []string{"/db1", "/db2"} {
		if err := c.DeleteSchema(tx, db, "s"); err != nil {
			t.Fatal(err)
		}
	}
	if err := tc.CreateTemplate(tx, buildTemplateAtVersion(t, "g", 1)); err != nil {
		t.Fatal(err)
	}
}

func TestInMemory_VersionGuard_VersionZeroBindingBlocksFreshTemplate(t *testing.T) {
	t.Parallel()
	c := NewInMemoryStoreCatalog()
	tx := NewInMemoryTransaction()
	tc := c.SchemaTemplateCatalog()
	if err := tc.CreateTemplate(tx, buildTemplateAtVersion(t, "z", 0)); err != nil {
		t.Fatal(err)
	}
	if err := c.SaveSchema(tx, buildTemplateAtVersion(t, "z", 0).GenerateSchema("/db", "s"), true); err != nil {
		t.Fatal(err)
	}
	if err := tc.DeleteTemplate(tx, "z", true); err != nil {
		t.Fatal(err)
	}
	wantAPIError(t, tc.CreateTemplate(tx, buildTemplateAtVersion(t, "z", 1)), api.ErrCodeInvalidSchemaTemplate,
		"schema template z version 1 cannot be created: schemas are still bound to its dropped version 0 (/db/s)")
}

func TestInMemory_VersionGuard_DanglingBindingAboveLatest(t *testing.T) {
	t.Parallel()
	c := NewInMemoryStoreCatalog()
	tx := NewInMemoryTransaction()
	tc := c.SchemaTemplateCatalog()
	for _, v := range []int{1, 3} {
		if err := tc.CreateTemplate(tx, buildTemplateAtVersion(t, "d", v)); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.SaveSchema(tx, buildTemplateAtVersion(t, "d", 1).GenerateSchema("/db", "low"), true); err != nil {
		t.Fatal(err)
	}
	if err := c.SaveSchema(tx, buildTemplateAtVersion(t, "d", 3).GenerateSchema("/db", "high"), true); err != nil {
		t.Fatal(err)
	}
	// The state a pre-guard DeleteTemplateVersion left: (d, 3) gone, bound.
	delete(c.templates.templates["d"], 3)

	for _, v := range []int{2, 4} {
		wantAPIError(t, tc.CreateTemplate(tx, buildTemplateAtVersion(t, "d", v)), api.ErrCodeInvalidSchemaTemplate,
			"schema template d version "+strconv.Itoa(v)+" cannot be created: schemas are still bound to its dropped version 3 (/db/high)")
	}
	gone := "SchemaTemplate=d, version=3 is not in catalog"
	_, err := c.LoadSchema(tx, "/db", "high")
	wantAPIError(t, err, api.ErrCodeUnknownSchemaTemplate, gone)
	wantAPIError(t, c.RepairSchema(tx, "/db", "high"), api.ErrCodeUnknownSchemaTemplate, gone)
	wantAPIError(t, c.SaveSchema(tx, buildTemplateAtVersion(t, "d", 1).GenerateSchema("/db", "high"), false),
		api.ErrCodeUnknownSchemaTemplate, gone)
	if s, err := c.LoadSchema(tx, "/db", "low"); err != nil || s.SchemaTemplate().Version() != 1 {
		t.Fatalf("schema low: %v, %v", s, err)
	}
	if err := c.DeleteSchema(tx, "/db", "high"); err != nil {
		t.Fatal(err)
	}
	if err := tc.CreateTemplate(tx, buildTemplateAtVersion(t, "d", 2)); err != nil {
		t.Fatal(err)
	}
}

func TestInMemory_VersionGuard_DeleteBoundVersionRefused(t *testing.T) {
	t.Parallel()
	c := NewInMemoryStoreCatalog()
	tx := NewInMemoryTransaction()
	tc := c.SchemaTemplateCatalog()
	for _, v := range []int{1, 2} {
		if err := tc.CreateTemplate(tx, buildTemplateAtVersion(t, "del", v)); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.SaveSchema(tx, buildTemplateAtVersion(t, "del", 2).GenerateSchema("/db", "s"), true); err != nil {
		t.Fatal(err)
	}
	wantAPIError(t, tc.DeleteTemplateVersion(tx, "del", 2, true), api.ErrCodeInvalidSchemaTemplate,
		"schema template del version 2 cannot be deleted: schemas are still bound to it (/db/s)")
	if ok, _ := tc.DoesSchemaTemplateExistAtVersion(tx, "del", 2); !ok {
		t.Fatal("refused delete removed version 2")
	}
	if err := tc.DeleteTemplateVersion(tx, "del", 1, true); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteSchema(tx, "/db", "s"); err != nil {
		t.Fatal(err)
	}
	if err := tc.DeleteTemplateVersion(tx, "del", 2, true); err != nil {
		t.Fatal(err)
	}
}

// A template catalog of its own has no schemas, so its guard reads none.
func TestInMemory_VersionGuard_StandaloneTemplateCatalog(t *testing.T) {
	t.Parallel()
	c := NewInMemorySchemaTemplateCatalog()
	tx := NewInMemoryTransaction()
	if err := c.CreateTemplate(tx, buildTemplateAtVersion(t, "s", 1)); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteTemplateVersion(tx, "s", 1, true); err != nil {
		t.Fatal(err)
	}
	if err := c.CreateTemplate(tx, buildTemplateAtVersion(t, "s", 1)); err != nil {
		t.Fatal(err)
	}
}

// Binds, deletes, repairs and re-creations of one template race each other;
// under the store mutex the guard's read and the bind are one step, so no
// schema is ever bound to a version the catalog does not hold. Every worker
// checks that after each of its operations, under both mutexes in the guard's
// order (store before template), so the check samples the states between
// operations rather than only the last one. Run under -race, it is also the
// lock-order check.
func TestInMemory_VersionGuard_ConcurrentBindAndDelete(t *testing.T) {
	t.Parallel()
	c := NewInMemoryStoreCatalog()
	tx := NewInMemoryTransaction()
	tc := c.SchemaTemplateCatalog()
	tmpls := map[int]api.SchemaTemplate{}
	for _, v := range []int{1, 2, 3} {
		tmpls[v] = buildTemplateAtVersion(t, "r", v)
		if err := tc.CreateTemplate(tx, tmpls[v]); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.CreateDatabase(tx, "/db"); err != nil {
		t.Fatal(err)
	}

	// dangling is the first schema bound to a version the catalog lacks.
	dangling := func() string {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.templates.mu.Lock()
		defer c.templates.mu.Unlock()
		for name, s := range c.schemas["/db"] {
			v := s.SchemaTemplate().Version()
			if _, ok := c.templates.templates["r"][v]; !ok {
				return name + " is bound to version " + strconv.Itoa(v) + ", which the catalog does not hold"
			}
		}
		return ""
	}

	const rounds = 300
	var (
		wg       sync.WaitGroup
		failOnce sync.Once
	)
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			schema := "s" + strconv.Itoa(w)
			for i := 0; i < rounds; i++ {
				v := 1 + (i+w)%3
				switch i % 4 {
				case 0:
					_ = c.SaveSchema(tx, tmpls[v].GenerateSchema("/db", schema), false)
				case 1:
					_ = tc.DeleteTemplateVersion(tx, "r", v, false)
				case 2:
					_ = c.RepairSchema(tx, "/db", schema)
				case 3:
					_ = tc.CreateTemplate(tx, tmpls[v])
					if i%8 == 3 {
						_ = c.DeleteSchema(tx, "/db", schema)
					}
				}
				if msg := dangling(); msg != "" {
					failOnce.Do(func() { t.Errorf("after round %d of worker %d: %s", i, w, msg) })
					return
				}
			}
		}(w)
	}
	wg.Wait()
}

// The in-memory LoadTemplateProto is the stored template's ToProto.
func TestInMemory_LoadTemplateProto(t *testing.T) {
	t.Parallel()
	c := NewInMemorySchemaTemplateCatalog()
	tx := NewInMemoryTransaction()
	tmpl := buildTemplateAtVersion(t, "p", 1)
	if err := c.CreateTemplate(tx, tmpl); err != nil {
		t.Fatal(err)
	}
	got, err := c.LoadTemplateProto(tx, "p", 1)
	if err != nil {
		t.Fatal(err)
	}
	want, err := tmpl.(*metadata.RecordLayerSchemaTemplate).Underlying().ToProto()
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(got, want) {
		t.Fatal("LoadTemplateProto differs from the template's ToProto")
	}
	_, err = c.LoadTemplateProto(tx, "p", 2)
	wantAPIError(t, err, api.ErrCodeUnknownSchemaTemplate, "SchemaTemplate=p, version=2 is not in catalog")
}
