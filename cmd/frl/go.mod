// Deprecated: frl is a package of the root module fdb.dev since v0.2.0. Drop this module with go get fdb.dev/cmd/frl@none, then go install fdb.dev/cmd/frl@latest (or go get -tool fdb.dev/cmd/frl@latest).
module fdb.dev/cmd/frl

go 1.26

// Tag cmd/frl/v0.1.1 at this commit, never on master: a nested go.mod there
// would split frl out of the root module again.
retract (
	v0.1.1 // Publishes the retraction only; contains no buildable frl.
	[v0.0.0-0, v0.1.0] // Superseded by the root module fdb.dev: run go get fdb.dev/cmd/frl@none.
)
