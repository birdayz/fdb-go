package plugin

import (
	"bytes"
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/resource/httpadapter"

	"fdb.dev/layers/otel"
)

// Autocomplete caps and cache lifetime (RFC §8).
const (
	maxAttributeKeys   = 1000
	maxAttributeValues = 100
	attributeCacheTTL  = 5 * time.Minute
)

// newResourceHandler serves tenants, attribute-keys and attribute-values,
// sqlds' schemas route, and hands tables and columns to the tenant's sqlds
// datasource.
func (d *Datasource) newResourceHandler() backend.CallResourceHandler {
	mux := http.NewServeMux()
	mux.HandleFunc("/tenants", d.handleTenants)
	mux.HandleFunc("/schemas", d.handleSchemas)
	mux.HandleFunc("/attribute-keys", d.handleAttributeKeys)
	mux.HandleFunc("/attribute-values", d.handleAttributeValues)
	adapter := httpadapter.New(mux)
	return backend.CallResourceHandlerFunc(func(ctx context.Context, req *backend.CallResourceRequest, sender backend.CallResourceResponseSender) error {
		if d.cfgErr != nil {
			return sendError(sender, http.StatusBadRequest, d.cfgErr)
		}
		switch strings.Trim(req.Path, "/") {
		case "tables", "columns":
			return d.completion(ctx, req, sender)
		}
		return adapter.CallResource(ctx, req, sender)
	})
}

func sendError(sender backend.CallResourceResponseSender, status int, err error) error {
	return sender.Send(&backend.CallResourceResponse{Status: status, Body: []byte(describeError(err))})
}

// completion resolves the tenant from sqlds' options body ({"schema": ...})
// and lets that tenant's sqlds datasource answer.
func (d *Datasource) completion(ctx context.Context, req *backend.CallResourceRequest, sender backend.CallResourceResponseSender) error {
	var opts map[string]string
	if len(bytes.TrimSpace(req.Body)) > 0 {
		if err := json.Unmarshal(req.Body, &opts); err != nil {
			return sendError(sender, http.StatusBadRequest, err)
		}
	}
	tenant, err := d.tenant(opts["schema"])
	if err != nil {
		return sendError(sender, http.StatusBadRequest, err)
	}
	t, release, err := d.sqlPools.acquire(ctx, tenant)
	if err != nil {
		return sendError(sender, http.StatusBadGateway, err)
	}
	defer release()
	return t.ds.CallResource(ctx, req, sender)
}

func respondJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	var qe *QueryError
	var te *TenantError
	var de *OTelDisabledError
	if errors.As(err, &qe) || errors.As(err, &te) || errors.As(err, &de) {
		status = http.StatusBadRequest
	}
	http.Error(w, describeError(err), status)
}

// allowedTenants lists the database's schemas that allowedSchemas admits.
func (d *Datasource) allowedTenants(ctx context.Context) ([]string, error) {
	t, release, err := d.sqlPools.acquire(ctx, catalogPoolKey)
	if err != nil {
		return nil, err
	}
	defer release()
	return listSchemas(ctx, t.db, d.cfg.AllowedSchemas)
}

// GET tenants → {"tenants": [...]}
func (d *Datasource) handleTenants(w http.ResponseWriter, r *http.Request) {
	tenants, err := d.allowedTenants(r.Context())
	if err != nil {
		httpError(w, err)
		return
	}
	respondJSON(w, map[string][]string{"tenants": tenants})
}

// POST schemas → [...] (sqlds' completion shape)
func (d *Datasource) handleSchemas(w http.ResponseWriter, r *http.Request) {
	tenants, err := d.allowedTenants(r.Context())
	if err != nil {
		httpError(w, err)
		return
	}
	respondJSON(w, tenants)
}

type attrRequest struct {
	tenant, signal, scope, typ, key, prefix string
	limit                                   int
}

func (d *Datasource) parseAttrRequest(r *http.Request, maxLimit int, needKey bool) (attrRequest, error) {
	q := r.URL.Query()
	a := attrRequest{signal: q.Get("signal"), scope: q.Get("scope"), typ: q.Get("type"), key: q.Get("key"), prefix: q.Get("prefix"), limit: maxLimit}
	var err error
	if a.tenant, err = d.tenant(strings.TrimSpace(q.Get("tenant"))); err != nil {
		return a, err
	}
	if a.signal == "" {
		a.signal = signalSpan
	}
	scopes := spanScopes
	switch a.signal {
	case signalSpan:
	case signalLog:
		scopes = logScopes
	default:
		return a, queryErrorf("signal", "%q is not span or log", a.signal)
	}
	if !slices.Contains(scopes, a.scope) {
		return a, queryErrorf("scope", "%q is not %s", a.scope, strings.Join(scopes, ", "))
	}
	if a.typ != "" && !slices.Contains(attrTypes, a.typ) || needKey && a.typ == "" {
		return a, queryErrorf("type", "%q is not string, int, double or bool", a.typ)
	}
	if needKey && a.key == "" {
		return a, queryErrorf("key", "missing")
	}
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > maxLimit {
			return a, queryErrorf("limit", "%q is outside 1..%d", s, maxLimit)
		}
		a.limit = n
	}
	return a, nil
}

type attrResult struct {
	Values    []string
	Truncated bool
}

// attributeLookup answers from the cache or the tenant's reader.
func (d *Datasource) attributeLookup(ctx context.Context, kind string, a attrRequest, read func(otelReader) ([]string, bool, error)) (attrResult, error) {
	cacheKey := strings.Join([]string{kind, a.tenant, a.signal, a.scope, a.typ, a.key, a.prefix, strconv.Itoa(a.limit)}, "\x00")
	if v, ok := d.attrs.get(cacheKey); ok {
		return v, nil
	}
	var vals []string
	var truncated bool
	err := d.withReader(ctx, a.tenant, func(r otelReader) error {
		var err error
		vals, truncated, err = read(r)
		return err
	})
	if err != nil {
		return attrResult{}, err
	}
	if vals == nil {
		vals = []string{}
	}
	res := attrResult{Values: vals, Truncated: truncated}
	d.attrs.put(cacheKey, res)
	return res, nil
}

// GET attribute-keys?tenant=&signal=&scope=&type=&prefix=&limit= → {"keys", "truncated"}
func (d *Datasource) handleAttributeKeys(w http.ResponseWriter, r *http.Request) {
	a, err := d.parseAttrRequest(r, maxAttributeKeys, false)
	if err != nil {
		httpError(w, err)
		return
	}
	res, err := d.attributeLookup(r.Context(), "keys", a, func(rd otelReader) ([]string, bool, error) {
		return rd.AttributeKeys(r.Context(), a.signal, a.scope, a.typ, a.prefix, a.limit)
	})
	if err != nil {
		httpError(w, err)
		return
	}
	respondJSON(w, struct {
		Keys      []string `json:"keys"`
		Truncated bool     `json:"truncated"`
	}{res.Values, res.Truncated})
}

// GET attribute-values?tenant=&signal=&scope=&key=&type=&prefix=&limit= → {"values", "truncated"}
func (d *Datasource) handleAttributeValues(w http.ResponseWriter, r *http.Request) {
	a, err := d.parseAttrRequest(r, maxAttributeValues, true)
	if err != nil {
		httpError(w, err)
		return
	}
	res, err := d.attributeLookup(r.Context(), "values", a, func(rd otelReader) ([]string, bool, error) {
		return rd.AttributeValues(r.Context(), a.signal, otel.Attribute{Scope: a.scope, Key: a.key, Type: a.typ}, a.prefix, a.limit)
	})
	if err != nil {
		httpError(w, err)
		return
	}
	respondJSON(w, struct {
		Values    []string `json:"values"`
		Truncated bool     `json:"truncated"`
	}{res.Values, res.Truncated})
}

// ttlCache is a size-bounded LRU whose entries expire.
type ttlCache struct {
	mu    sync.Mutex
	max   int
	ttl   time.Duration
	now   func() time.Time
	order *list.List
	items map[string]*list.Element
}

type ttlEntry struct {
	key     string
	val     attrResult
	expires time.Time
}

func newTTLCache(maxEntries int, ttl time.Duration) *ttlCache {
	return &ttlCache{max: maxEntries, ttl: ttl, now: time.Now, order: list.New(), items: map[string]*list.Element{}}
}

func (c *ttlCache) get(key string) (attrResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return attrResult{}, false
	}
	e := el.Value.(*ttlEntry)
	if !c.now().Before(e.expires) {
		c.order.Remove(el)
		delete(c.items, key)
		return attrResult{}, false
	}
	c.order.MoveToFront(el)
	return e.val, true
}

func (c *ttlCache) put(key string, v attrResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.order.Remove(el)
	}
	c.items[key] = c.order.PushFront(&ttlEntry{key: key, val: v, expires: c.now().Add(c.ttl)})
	for c.order.Len() > c.max {
		el := c.order.Back()
		c.order.Remove(el)
		delete(c.items, el.Value.(*ttlEntry).key)
	}
}
