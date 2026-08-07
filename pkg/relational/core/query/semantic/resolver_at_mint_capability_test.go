package semantic

import (
	"errors"
	"testing"
)

// The RESOLVER-AT-MINT capability probe.
//
// query.legWindowSlot decides which leg a qualifier selects by comparing that
// qualifier's TEXT against RecordTypeLeg.Name. RFC-197's position is that a name
// may not decide a column's identity, and the reader's documented blocker is
// that it holds no CorrelationIdentifier to key an identity lookup with — the
// qualifier reaches it as a string, either sliced out of a rendered column name
// or carried as a parse-tree segment.
//
// "Resolver-at-mint" is the proposed answer: have the producer that MINTS the
// reference record the identity the resolver already selected, so the baker
// receives a correlation instead of a qualifier string. Whether that is a
// WIRING job or a MISSING CAPABILITY is the question these two tests answer, and
// they answer it in opposite directions on purpose — the capability exists for
// one of the reader's two key kinds and does not exist for the other.
//
// Both are pinned rather than reasoned because the reasoning is what has been
// wrong on this path: a reader's blocker has repeatedly been described from the
// reader's side, where the qualifier is text, without checking whether the
// identity was available one hop earlier and discarded.

// The POSITIVE half: for a qualifier that names a FROM source, the scope already
// resolves it to a ScopeSource carrying a CorrelationName — the runtime
// correlation key. That is exactly the identity legWindowSlot lacks, available
// at the point the reference is resolved, one hop before any leg table is
// walked.
//
// WHAT A FAILURE MEANS: resolver-at-mint stops being a wiring job and becomes a
// capability that has to be BUILT (the analyzer would have to start tracking
// which quantifier a qualifier selects). Every plan that reads "the identity is
// already there, it is only discarded" has to be re-derived from scratch.
func TestResolverAtMint_QualifiedReferenceCarriesACorrelationName(t *testing.T) {
	t.Parallel()
	s, _, _ := buildScope(t)

	col, src, err := s.ResolveQualifiedColumn(NewUnquoted("o"), NewUnquoted("order_id"))
	if err != nil {
		t.Fatalf("resolving a qualified reference the corpus writes every day failed: %v", err)
	}
	if col.Id.Name() != "ORDER_ID" {
		t.Fatalf("resolved the wrong column: %q", col.Id.Name())
	}
	if src.CorrelationName == "" {
		t.Fatalf("the qualifier %q resolved to a source stating NO CorrelationName.\n"+
			"  WHAT THIS RE-ARMS: resolver-at-mint is only a wiring job while the\n"+
			"  resolver already knows which quantifier a qualifier selects. With an\n"+
			"  empty correlation the identity is not merely discarded downstream — it\n"+
			"  was never derived, and query.legWindowSlot's conversion becomes a\n"+
			"  capability to BUILD in the analyzer rather than a field to carry on\n"+
			"  logical.ColumnRef.", "o")
	}
	if src.CorrelationName != "O" {
		t.Fatalf("CorrelationName = %q, want the UPPER-folded registration form %q — "+
			"the canonicalisation Scope.AddSource performs is what makes this key "+
			"comparable to a leg's Alias at all.", src.CorrelationName, "O")
	}
}

// The NEGATIVE half, and it is the one that decides the crux: the scope matches
// a qualifier against a source's ALIAS only. A reference qualified by the scan
// TABLE name of an aliased source does NOT resolve.
//
// That matters because the leg-layout map query.legWindowSlot walks registers a
// second addressing route — `FROM PA AS "s"` answers `PA."ID"` as well as
// `S."ID"` — and the dotted-leg-qualifier census measures that route LIVE
// (matchViaTableName). So the two key kinds are not both reachable through the
// resolver: resolver-at-mint supplies an identity for the alias route and
// supplies NOTHING for the table-name route, because the resolver rejects that
// spelling outright.
//
// WHAT A FAILURE MEANS: if this ever resolves, the scope has grown the
// table-name addressing route, and the "one of the map's two key kinds names a
// TABLE, so it cannot be re-keyed by identity even in principle" argument —
// recorded at values.RecordTypeLeg.Name and at the census — is no longer true.
// Re-read the census's matchViaTableName class before planning anything on top
// of it.
func TestResolverAtMint_TableNameQualifierDoesNotResolve(t *testing.T) {
	t.Parallel()
	c := buildTestCatalog()
	users, ok := c.LookupTable(ParseQualifiedName("users", false))
	if !ok {
		t.Fatal("test catalog lost its users table")
	}
	s := NewScope(nil)
	if err := s.AddSource(ScopeSource{
		Table:           users,
		Alias:           NewUnquoted("u"),
		CorrelationName: "u",
	}); err != nil {
		t.Fatalf("AddSource: %v", err)
	}

	// The alias route answers.
	if _, _, err := s.ResolveQualifiedColumn(NewUnquoted("u"), NewUnquoted("name")); err != nil {
		t.Fatalf("the ALIAS route must answer: %v", err)
	}

	// The TABLE-NAME route does not.
	_, _, err := s.ResolveQualifiedColumn(NewUnquoted("users"), NewUnquoted("name"))
	if err == nil {
		t.Fatalf("`USERS.NAME` resolved against `FROM users AS u`.\n" +
			"  The scope has grown the TABLE-NAME addressing route that the leg-layout\n" +
			"  map serves and the resolver did not.\n" +
			"  WHAT THIS RE-ARMS: the standing argument that query.legWindowSlot's map\n" +
			"  cannot be re-keyed by identity even in principle rests on one of its two\n" +
			"  key kinds naming a TABLE rather than a quantifier. If the resolver now\n" +
			"  names the quantifier for that spelling too, resolver-at-mint covers BOTH\n" +
			"  key kinds and legWindowSlot's retirement condition must be rewritten.")
	}
	var snf *SourceNotFoundError
	if !errors.As(err, &snf) {
		t.Fatalf("the table-name route failed with %T, want *SourceNotFoundError — the\n"+
			"  error CLASS is the finding: the qualifier named no source at all, as\n"+
			"  opposed to naming one that lacks the column. A different class means the\n"+
			"  qualifier matched something, and what it matched has to be identified\n"+
			"  before the crux argument above can be quoted again.\n  got: %v", err, err)
	}
}
