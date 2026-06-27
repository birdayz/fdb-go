package fdb

// StartNetwork and StopNetwork exist for source compatibility with Apple's Go
// binding, where they start and stop a single process-global network thread that
// must be running before any database is opened. The pure-Go client has no such
// global thread: it manages connections per database, started lazily by
// OpenDatabase. So both are no-ops here.
//
// Code ported from the Apple binding can keep its `fdb.StartNetwork()` init call
// unchanged. Note the one semantic difference from libfdb_c: Apple's StopNetwork
// is process-final (the network thread cannot be restarted), whereas here it does
// nothing and databases keep working afterward.

// StartNetwork is a no-op (see the package note above). Returns nil.
func StartNetwork() error { return nil }

// StopNetwork is a no-op (see the package note above). Returns nil.
func StopNetwork() error { return nil }
