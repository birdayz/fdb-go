// Package otelsql defines the Java-readable OTel SQL schema template.
// See docs/compatibility.md "OTel SQL schema foundation" for writer contracts.
package otelsql

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"fdb.dev/layers/otel/otelsql/otelddl"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/keyspace"
)

// TemplateName names the schema template; a schema of every tenant database
// is created with it.
const TemplateName = otelddl.Name

// TemplateVersion is the initial catalog version; evolution must use the
// catalog's version/evolution validator rather than overwrite its DDL.
const TemplateVersion = 1

// MaxIndexedStringBytes keeps indexed strings within FDB's key limit;
// longer values belong in long_str_attrs and are looked up by digest.
const MaxIndexedStringBytes = 1024

// TemplateBody and TemplateOverlay are the template's DDL (otelddl.Body,
// otelddl.Overlay): the body Java builds too and the Go-only FULLTEXT index.
const (
	TemplateBody    = otelddl.Body
	TemplateOverlay = otelddl.Overlay
)

// TemplateDDL is the statement that creates the template: TemplateBody and
// TemplateOverlay.
func TemplateDDL() string { return templateDDL(TemplateName) }

func templateDDL(name string) string { return otelddl.DDL(name) }

// RevisionTemplateName is TemplateName suffixed with a digest of
// TemplateDDL: the template under a name no other revision of OTEL_V1 uses,
// for benchmark runs on a shared development cluster whose catalog holds
// OTEL_V1 as another revision defined it. Its stores are OTEL_V1 stores:
// the name is only the records descriptor's file name.
func RevisionTemplateName() string {
	sum := sha256.Sum256([]byte(TemplateDDL()))
	return fmt.Sprintf("%s_%X", TemplateName, sum[:4])
}

// CreateTemplate stores the template in the catalog of the connection's
// cluster.
func CreateTemplate(ctx context.Context, db *sql.DB) error {
	return CreateTemplateNamed(ctx, db, TemplateName)
}

// CreateTemplateNamed is CreateTemplate under another name, such as
// RevisionTemplateName.
func CreateTemplateNamed(ctx context.Context, db *sql.DB, name string) error {
	if _, err := db.ExecContext(ctx, templateDDL(name)); err != nil {
		return fmt.Errorf("otelsql: create schema template %s: %w", name, err)
	}
	return nil
}

// CreateTenant creates the schema, resuming if the database already exists.
// Paths use /DOMAIN/DATABASE and one ASCII schema identifier; names fold uppercase.
func CreateTenant(ctx context.Context, db *sql.DB, databasePath, schema string) error {
	return CreateTenantWithTemplate(ctx, db, databasePath, schema, TemplateName)
}

// CreateTenantWithTemplate is CreateTenant with the schema bound to template,
// a name CreateTemplateNamed stored.
func CreateTenantWithTemplate(ctx context.Context, db *sql.DB, databasePath, schema, template string) error {
	databasePath, schemaPath, err := tenantPaths(databasePath, schema)
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `CREATE DATABASE "`+databasePath+`"`); err != nil {
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeDatabaseAlreadyExists {
			return fmt.Errorf("otelsql: create database %s: %w", databasePath, err)
		}
	}
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA "`+schemaPath+`" WITH TEMPLATE `+template); err != nil {
		return fmt.Errorf("otelsql: create schema %s: %w", schemaPath, err)
	}
	return nil
}

func tenantPaths(databasePath, schema string) (string, string, error) {
	invalid := func() (string, string, error) {
		return "", "", api.NewErrorf(api.ErrCodeInvalidPath,
			"otelsql: expected /DOMAIN/DATABASE and one schema identifier: %q, %q", databasePath, schema)
	}
	parts, err := keyspace.ParseDBPath(databasePath)
	if err != nil || len(parts) != 2 {
		return invalid()
	}
	for _, name := range []string{parts[0], parts[1], schema} {
		if name == "" {
			return invalid()
		}
		for i, c := range name {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' || (i > 0 && c >= '0' && c <= '9')) {
				return invalid()
			}
		}
	}
	databasePath = strings.ToUpper(databasePath)
	resolved, err := keyspace.ToDatabasePath(databasePath)
	if err != nil || resolved.System {
		return invalid()
	}
	return databasePath, databasePath + "/" + strings.ToUpper(schema), nil
}
