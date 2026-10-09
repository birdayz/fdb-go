//go:build bazelrunfiles

package conformance_test

import "fdb.dev/pkg/relational/core/keyspace"

// The Go side registers the domains of the paths these tests use: TEST, which
// the Java conformance server registers (sql_plan_steps.java), and FRL.
func init() {
	keyspace.RegisterDomainIfNotExists("FRL")
	keyspace.RegisterDomainIfNotExists("TEST")
}
