package plugin

import (
	"fmt"
	"regexp"
	"strings"
)

// schemaName is a relational schema (tenant) name the plugin accepts. Template
// variables Grafana did not interpolate ($tenant) fail here, before any read.
var schemaName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// TenantError reports a tenant the datasource refuses before reading.
type TenantError struct {
	Tenant string
	Reason string
}

func (e *TenantError) Error() string {
	return fmt.Sprintf("tenant %q refused: %s", e.Tenant, e.Reason)
}

// schemaPolicy is allowedSchemas: exact names plus at most one prefix
// ("T*"). The zero value admits every schema name.
type schemaPolicy struct {
	names     map[string]bool
	prefix    string
	hasPrefix bool
}

func parseSchemaPolicy(entries []string) (schemaPolicy, error) {
	var p schemaPolicy
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if prefix, ok := strings.CutSuffix(e, "*"); ok {
			if p.hasPrefix {
				return schemaPolicy{}, settingsErrorf("allowedSchemas", "only one entry may end in *, got %q and %q*", p.prefix, prefix)
			}
			if prefix != "" && !schemaName.MatchString(prefix) {
				return schemaPolicy{}, settingsErrorf("allowedSchemas", "%q is not a schema name prefix", e)
			}
			p.prefix, p.hasPrefix = prefix, true
			continue
		}
		if !schemaName.MatchString(e) {
			return schemaPolicy{}, settingsErrorf("allowedSchemas", "%q is not a schema name", e)
		}
		if p.names == nil {
			p.names = map[string]bool{}
		}
		p.names[e] = true
	}
	return p, nil
}

// restricted reports whether the policy limits schemas at all.
func (p schemaPolicy) restricted() bool { return p.hasPrefix || len(p.names) > 0 }

// check refuses a malformed or disallowed schema name.
func (p schemaPolicy) check(schema string) error {
	if !schemaName.MatchString(schema) {
		reason := "not a schema name"
		if strings.Contains(schema, "$") {
			reason = "not a schema name (an uninterpolated template variable?)"
		}
		return &TenantError{Tenant: schema, Reason: reason}
	}
	if !p.restricted() || p.names[schema] || (p.hasPrefix && strings.HasPrefix(schema, p.prefix)) {
		return nil
	}
	return &TenantError{Tenant: schema, Reason: "not in the datasource's allowedSchemas"}
}

// allows reports whether check would admit schema.
func (p schemaPolicy) allows(schema string) bool { return p.check(schema) == nil }
