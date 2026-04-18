package catalog

import (
	"sort"
	"sync"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
)

// InMemorySchemaTemplateCatalog stores schema templates keyed by
// (name, version). Companion to InMemoryStoreCatalog.
type InMemorySchemaTemplateCatalog struct {
	mu sync.Mutex
	// templates[name][version] → template
	templates map[string]map[int]api.SchemaTemplate
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
		return nil, api.NewErrorf(api.ErrCodeUnknownSchemaTemplate, "schema template %q not found", templateName)
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
		return nil, api.NewErrorf(api.ErrCodeUnknownSchemaTemplate, "schema template %q version %d not found", templateName, version)
	}
	return tmpl, nil
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

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.templates[name][version]; ok {
		return api.NewErrorf(api.ErrCodeDuplicateSchemaTemplate, "schema template %q version %d already exists", name, version)
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
func (c *InMemorySchemaTemplateCatalog) DeleteTemplateVersion(txn api.Transaction, templateName string, version int, throwIfDoesNotExist bool) error {
	if err := checkOpenTxn(txn); err != nil {
		return err
	}
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
	delete(byVersion, version)
	if len(byVersion) == 0 {
		delete(c.templates, templateName)
	}
	return nil
}

// Compile-time interface check.
var _ api.SchemaTemplateCatalog = (*InMemorySchemaTemplateCatalog)(nil)
