package catalog

import (
	"sort"
	"sync"

	"fdb.dev/gen"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/metadata"
)

// InMemorySchemaTemplateCatalog stores schema templates keyed by
// (name, version). Companion to InMemoryStoreCatalog.
type InMemorySchemaTemplateCatalog struct {
	mu sync.Mutex
	// templates[name][version] → template
	templates map[string]map[int]api.SchemaTemplate
	// bindings is the store catalog whose schemas bind these templates, nil
	// for a template catalog of its own. Its mutex is taken BEFORE this one,
	// by the version guard here and by the store catalog's SaveSchema,
	// LoadSchema and RepairSchema, so a schema cannot bind a version the
	// guard has just found unbound.
	bindings *InMemoryStoreCatalog
}

// lockBindings takes the store catalog's mutex, when there is one, and
// returns its unlock. It is taken before c.mu.
func (c *InMemorySchemaTemplateCatalog) lockBindings() func() {
	if c.bindings == nil {
		return func() {}
	}
	c.bindings.mu.Lock()
	return c.bindings.mu.Unlock
}

// firstBinding is the version guard's read (template_bindings.go) over the
// store catalog: the first binding of templateName, in the order of Java's
// TEMPLATES_VALUE_INDEX, at a version from `from` through `through` (a
// negative bound is none). The store catalog's mutex must be held.
func (c *InMemorySchemaTemplateCatalog) firstBinding(templateName string, from, through int) *boundSchema {
	if c.bindings == nil {
		return nil
	}
	return c.bindings.firstBindingLocked(templateName, from, through)
}

// NewInMemorySchemaTemplateCatalog returns an empty template catalog.
func NewInMemorySchemaTemplateCatalog() *InMemorySchemaTemplateCatalog {
	return &InMemorySchemaTemplateCatalog{
		templates: map[string]map[int]api.SchemaTemplate{},
	}
}

// DoesSchemaTemplateExist: any version of templateName.
func (c *InMemorySchemaTemplateCatalog) DoesSchemaTemplateExist(txn api.Transaction, templateName string) (bool, error) {
	if err := checkOpenTxn(txn); err != nil {
		return false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.templates[templateName]
	return ok, nil
}

// DoesSchemaTemplateExistAtVersion: specific (name, version).
func (c *InMemorySchemaTemplateCatalog) DoesSchemaTemplateExistAtVersion(txn api.Transaction, templateName string, version int) (bool, error) {
	if err := checkOpenTxn(txn); err != nil {
		return false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	byVersion, ok := c.templates[templateName]
	if !ok {
		return false, nil
	}
	_, ok = byVersion[version]
	return ok, nil
}

// LoadSchemaTemplate returns the highest-versioned template with the
// given name. ErrCodeUnknownSchemaTemplate when not found.
func (c *InMemorySchemaTemplateCatalog) LoadSchemaTemplate(txn api.Transaction, templateName string) (api.SchemaTemplate, error) {
	if err := checkOpenTxn(txn); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	byVersion, ok := c.templates[templateName]
	if !ok || len(byVersion) == 0 {
		return nil, errTemplateNotInCatalog(templateName)
	}
	maxVer := -1
	for v := range byVersion {
		if v > maxVer {
			maxVer = v
		}
	}
	return byVersion[maxVer], nil
}

// LoadSchemaTemplateAtVersion returns one specific (name, version).
func (c *InMemorySchemaTemplateCatalog) LoadSchemaTemplateAtVersion(txn api.Transaction, templateName string, version int) (api.SchemaTemplate, error) {
	if err := checkOpenTxn(txn); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	tmpl, ok := c.templates[templateName][version]
	if !ok {
		return nil, errTemplateVersionNotInCatalog(templateName, version)
	}
	return tmpl, nil
}

// LoadTemplateProto returns the stored template's MetaData. The in-memory
// catalog keeps template objects and no bytes, so this is the template's
// ToProto: it holds only templates Go built, through CreateTemplate, so there
// is no unknown field or extension another engine wrote for it to lose.
func (c *InMemorySchemaTemplateCatalog) LoadTemplateProto(txn api.Transaction, templateName string, version int) (*gen.MetaData, error) {
	tmpl, err := c.LoadSchemaTemplateAtVersion(txn, templateName, version)
	if err != nil {
		return nil, err
	}
	rl, ok := tmpl.(*metadata.RecordLayerSchemaTemplate)
	if !ok {
		return nil, api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
			"schema template %s version %d is a %T, which has no stored metadata", templateName, version, tmpl)
	}
	p, err := rl.Underlying().ToProto()
	if err != nil {
		return nil, api.WrapErrorf(err, api.ErrCodeInternalError, "template to-proto")
	}
	return p, nil
}

// CreateTemplate: persist a new (name, version). Error on duplicate.
func (c *InMemorySchemaTemplateCatalog) CreateTemplate(txn api.Transaction, newTemplate api.SchemaTemplate) error {
	if err := checkOpenTxn(txn); err != nil {
		return err
	}
	if newTemplate == nil {
		return api.NewError(api.ErrCodeInvalidSchemaTemplate, "template is nil")
	}
	name := newTemplate.MetadataName()
	version := newTemplate.Version()

	defer c.lockBindings()()
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.templates[name][version]; ok {
		return api.NewErrorf(api.ErrCodeDuplicateSchemaTemplate, "schema template %q version %d already exists", name, version)
	}
	// The version guard: no schema may bind a dropped version above the
	// latest stored one, every version when none is stored.
	from := -1
	for v := range c.templates[name] {
		if v+1 > from {
			from = v + 1
		}
	}
	if bound := c.firstBinding(name, from, -1); bound != nil {
		return errBoundOnCreate(name, version, *bound)
	}
	if c.templates[name] == nil {
		c.templates[name] = map[int]api.SchemaTemplate{}
	}
	c.templates[name][version] = newTemplate
	return nil
}

// ListTemplates returns every (name, version) pair, sorted by
// (name, version).
func (c *InMemorySchemaTemplateCatalog) ListTemplates(txn api.Transaction) (api.ResultSet, error) {
	if err := checkOpenTxn(txn); err != nil {
		return nil, err
	}
	c.mu.Lock()
	type entry struct {
		name    string
		version int
	}
	var entries []entry
	for name, byVersion := range c.templates {
		for v := range byVersion {
			entries = append(entries, entry{name, v})
		}
	}
	c.mu.Unlock()
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].name != entries[j].name {
			return entries[i].name < entries[j].name
		}
		return entries[i].version < entries[j].version
	})
	rows := make([][]any, len(entries))
	for i, e := range entries {
		rows[i] = []any{e.name, e.version}
	}
	return newStringResultSet([]string{"TEMPLATE_NAME", "VERSION"}, rows), nil
}

// DeleteTemplate removes every version of templateName.
func (c *InMemorySchemaTemplateCatalog) DeleteTemplate(txn api.Transaction, templateName string, throwIfDoesNotExist bool) error {
	if err := checkOpenTxn(txn); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.templates[templateName]; !ok {
		if throwIfDoesNotExist {
			return api.NewErrorf(api.ErrCodeUnknownSchemaTemplate, "schema template %q not found", templateName)
		}
		return nil
	}
	delete(c.templates, templateName)
	return nil
}

// DeleteTemplateVersion removes one specific (name, version).
// A version a schema binds is not deleted (the version guard,
// template_bindings.go).
func (c *InMemorySchemaTemplateCatalog) DeleteTemplateVersion(txn api.Transaction, templateName string, version int, throwIfDoesNotExist bool) error {
	if err := checkOpenTxn(txn); err != nil {
		return err
	}
	defer c.lockBindings()()
	c.mu.Lock()
	defer c.mu.Unlock()
	byVersion, ok := c.templates[templateName]
	if !ok {
		if throwIfDoesNotExist {
			return api.NewErrorf(api.ErrCodeUnknownSchemaTemplate, "schema template %q not found", templateName)
		}
		return nil
	}
	if _, ok := byVersion[version]; !ok {
		if throwIfDoesNotExist {
			return api.NewErrorf(api.ErrCodeUnknownSchemaTemplate, "schema template %q version %d not found", templateName, version)
		}
		return nil
	}
	if bound := c.firstBinding(templateName, version, version); bound != nil {
		return errBoundOnDelete(templateName, version, *bound)
	}
	delete(byVersion, version)
	if len(byVersion) == 0 {
		delete(c.templates, templateName)
	}
	return nil
}

// Compile-time interface check.
var _ api.SchemaTemplateCatalog = (*InMemorySchemaTemplateCatalog)(nil)
