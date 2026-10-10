// Portions derived from FoundationDB Record Layer (
// OfflineStoredQueriesProcessor.java, RecordStoreState.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2021-2026 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package embedded

import (
	"context"
	"log/slog"
	"time"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/catalog"
	"fdb.dev/pkg/relational/core/metadata"
	"fdb.dev/pkg/relational/core/parser"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
	"fdb.dev/pkg/relational/core/session"
)

// StoredQueryWarmUpCounts are Java's OFFLINE_STORED_QUERIES_* counts and the
// OFFLINE_STORED_QUERIES_WARM_UP duration of one warm-up.
type StoredQueryWarmUpCounts struct {
	TemplatesProcessed     int
	QueriesProcessed       int
	QueriesFailed          int
	TempFunctionsProcessed int
	TempFunctionsFailed    int
	Duration               time.Duration
}

// StoredQueryTemplates reads the latest version of every schema template in
// the catalog and returns those declaring stored queries, Java's
// OfflineStoredQueriesProcessor.getSchemaTemplates. A catalog-read failure is
// logged and yields no templates: a warm-up never fails the engine's start.
func StoredQueryTemplates(ctx context.Context, db *recordlayer.FDBDatabase, cat *catalog.RecordLayerStoreCatalog) []*metadata.RecordLayerSchemaTemplate {
	if db == nil || cat == nil {
		return nil
	}
	result, err := db.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		txn := catalog.NewFDBTransaction(rctx)
		templates := cat.SchemaTemplateCatalog()
		rs, err := templates.ListTemplates(txn)
		if err != nil {
			return nil, err
		}
		defer rs.Close()
		seen := map[string]bool{}
		var out []*metadata.RecordLayerSchemaTemplate
		for rs.Next() {
			name, err := rs.StringByName(catalog.ColTemplateName)
			if err != nil {
				return nil, err
			}
			if seen[name] {
				continue
			}
			seen[name] = true
			loaded, err := templates.LoadSchemaTemplate(txn, name)
			if err != nil {
				return nil, err
			}
			tmpl, ok := loaded.(*metadata.RecordLayerSchemaTemplate)
			if !ok {
				continue
			}
			if queries, err := tmpl.StoredQueries(); err == nil && len(queries) > 0 {
				out = append(out, tmpl)
			}
		}
		return out, rs.Err()
	})
	if err != nil {
		slog.Default().Error("OfflineStoredQueriesProcessor failed to read catalog", "error", err)
		return nil
	}
	templates, _ := result.([]*metadata.RecordLayerSchemaTemplate)
	return templates
}

// WarmStoredQueries plans every stored query of every template into cache,
// Java's OfflineStoredQueriesProcessor.planStoredQueriesForSchemaTemplates. It
// runs offline, with no store: the planner's default options and every index
// readable (Java's empty RecordStoreState), so a plan is found by a later
// statement on any store of the template whose indexes are all readable. A
// stored query's temporary functions are declared first, in a transaction of
// their own as a client must declare them; one that fails skips its query. No
// failure aborts the warm-up; each is logged and counted.
func WarmStoredQueries(ctx context.Context, cache *RelationalPlanCache, templates []*metadata.RecordLayerSchemaTemplate) StoredQueryWarmUpCounts {
	return warmStoredQueries(ctx, cache, templates, slog.Default())
}

func warmStoredQueries(ctx context.Context, cache *RelationalPlanCache, templates []*metadata.RecordLayerSchemaTemplate, logger *slog.Logger) StoredQueryWarmUpCounts {
	var counts StoredQueryWarmUpCounts
	start := time.Now()
	for _, tmpl := range templates {
		queries, err := tmpl.StoredQueries()
		if err != nil {
			continue
		}
		for name, q := range queries {
			warmStoredQuery(ctx, cache, tmpl, name, q, &counts)
		}
		counts.TemplatesProcessed++
	}
	counts.Duration = time.Since(start)
	logger.Debug("OfflineStoredQueriesProcessor finished",
		"templatesProcessed", counts.TemplatesProcessed,
		"storedQueriesProcessed", counts.QueriesProcessed,
		"storedQueriesFailed", counts.QueriesFailed,
		"tempFunctionsProcessed", counts.TempFunctionsProcessed,
		"tempFunctionsFailed", counts.TempFunctionsFailed,
		"durationMicros", counts.Duration.Microseconds())
	return counts
}

// offlineSchemaName names the schema a warm-up plans in. It reaches no key:
// the plan cache shares a plan across a template's schemas.
const offlineSchemaName = "OFFLINE_STORED_QUERIES"

func warmStoredQuery(ctx context.Context, cache *RelationalPlanCache, tmpl *metadata.RecordLayerSchemaTemplate,
	name string, q api.StoredQuery, counts *StoredQueryWarmUpCounts,
) {
	logFailure := func(what, sql string, err error) {
		slog.Default().Error("OfflineStoredQueriesProcessor failed to plan "+what,
			"schemaTemplate", tmpl.MetadataName(), "storedQueryName", name, "sql", sql, "error", err)
	}
	key := session.SchemaCacheKey("", offlineSchemaName)
	schema := tmpl.GenerateSchema("", offlineSchemaName)
	sess := &session.Session{Schema: offlineSchemaName, SchemaCache: map[string]api.Schema{key: schema}}
	c := &EmbeddedConnection{
		sess:                      sess,
		planCache:                 cache,
		slowQueryThresholdMicros:  defaultSlowQueryThresholdMicros(),
		offlineAllIndexesReadable: true,
	}
	if len(q.TempFunctions) > 0 {
		c.activeTx = &embeddedTx{conn: c, schemaCache: map[string]api.Schema{key: schema}}
	}
	for _, sql := range q.TempFunctions {
		if err := declareOfflineTempFunction(ctx, c, sql); err != nil {
			logFailure("temporary function", sql, err)
			counts.TempFunctionsFailed++
			counts.QueriesFailed++
			return
		}
		counts.TempFunctionsProcessed++
	}
	if _, err := newCascadesGenerator(c).Plan(ctx, q.Query); err != nil {
		logFailure("stored query", q.Query, err)
		counts.QueriesFailed++
		return
	}
	counts.QueriesProcessed++
}

func declareOfflineTempFunction(ctx context.Context, c *EmbeddedConnection, sql string) error {
	root, err := parser.Parse(sql)
	if err != nil {
		return err
	}
	stmts := root.Statements()
	if stmts == nil || len(stmts.AllStatement()) != 1 || stmts.AllStatement()[0].DdlStatement() == nil {
		return api.NewError(api.ErrCodeInvalidParameter, "not a temporary function declaration")
	}
	ct, ok := stmts.AllStatement()[0].DdlStatement().CreateTempFunction().(*antlrgen.CreateTempFunctionContext)
	if !ok || ct == nil {
		return api.NewError(api.ErrCodeInvalidParameter, "not a temporary function declaration")
	}
	_, err = c.execCreateTempFunction(ctx, ct)
	return err
}
