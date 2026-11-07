package catalog

import (
	"crypto/sha256"
	"sync"

	"fdb.dev/gen"
	"fdb.dev/pkg/relational/api"
)

// templateCacheEntries bounds the process-wide deserialized-template cache.
const templateCacheEntries = 256

// templateCacheKey is a template row by name, version and the exact bytes of
// its META_DATA: a row rewritten in place (a restore, a repair) is a miss,
// never stale metadata.
type templateCacheKey struct {
	name     string
	version  int32
	metaData [sha256.Size]byte
}

// templateCache memoises the read path's deserializeTemplate across
// connections and transactions. Every catalog read still loads the row in
// its own transaction (so DDL is observed at the reader's read version); only
// the CPU of rebuilding RecordMetaData from identical bytes is shared. That
// rebuild dominated an auto-commit statement on a pooled *sql.DB, whose
// ResetSession drops the connection's schema cache at every checkout. The
// cached api.SchemaTemplate and its RecordMetaData are immutable.
var templateCache struct {
	mu      sync.Mutex
	entries map[templateCacheKey]api.SchemaTemplate
	order   []templateCacheKey // insertion order, for FIFO eviction
}

// loadTemplate is deserializeTemplate through templateCache. Errors are not
// cached.
func loadTemplate(msg *gen.Templates) (api.SchemaTemplate, error) {
	key := templateCacheKey{name: msg.GetTEMPLATE_NAME(), version: msg.GetTEMPLATE_VERSION(), metaData: sha256.Sum256(msg.GetMETA_DATA())}
	templateCache.mu.Lock()
	t, ok := templateCache.entries[key]
	templateCache.mu.Unlock()
	if ok {
		return t, nil
	}
	t, err := deserializeTemplate(msg)
	if err != nil {
		return nil, err
	}
	templateCache.mu.Lock()
	defer templateCache.mu.Unlock()
	if prior, ok := templateCache.entries[key]; ok {
		return prior, nil
	}
	if templateCache.entries == nil {
		templateCache.entries = make(map[templateCacheKey]api.SchemaTemplate)
	}
	if len(templateCache.order) >= templateCacheEntries {
		delete(templateCache.entries, templateCache.order[0])
		templateCache.order = templateCache.order[1:]
	}
	templateCache.entries[key] = t
	templateCache.order = append(templateCache.order, key)
	return t, nil
}
