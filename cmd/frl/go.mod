// Deprecated: frl is a package of the root module fdb.dev since v0.2.0. Install it with: go install fdb.dev/cmd/frl@latest
module fdb.dev/cmd/frl

go 1.26

// Tag cmd/frl/v0.1.1 at this commit, never on master: a nested go.mod there
// would split frl out of the root module again.
retract (
	v0.1.1 // Publishes the retraction only; contains no buildable frl.
	v0.1.0 // Superseded: frl moved into the root module. Use go install fdb.dev/cmd/frl@latest.
)
