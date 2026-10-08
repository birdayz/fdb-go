package factory_test

import "fdb.dev/pkg/relational/core/keyspace"

// A database path is /DOMAIN/DATABASE under a registered domain. Java's tests
// register theirs before connecting: FRL (the yaml runner, the JDBC test) and
// TEST (EmbeddedRelationalExtension). These tests use both.
func init() {
	keyspace.RegisterDomainIfNotExists("FRL")
	keyspace.RegisterDomainIfNotExists("TEST")
}
