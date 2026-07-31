package fdbgo

import (
	_ "embed"
	"strings"
	"testing"
)

// The behavioural pins (client/unbounded_default_pin_test.go, fdb/unbounded_default_pin_test.go)
// prove what the CODE does. This gate proves the DOCS still say it. Both halves are needed: the
// prose is the deliverable a migrator actually reads, and prose has no compiler.
//
// The specific failure being guarded is not "the doc went missing" but "the doc came back WRONG".
// Shipped godoc in this package tree previously asserted that unbounded retry was a difference
// from libfdb_c's internal timeouts. It is not — libfdb_c's per-transaction defaults are
// timeoutInSeconds=0.0 and maxRetries=-1 (ReadYourWrites.actor.cpp:2078-2082), so a default C
// transaction is unbounded too. That false framing is easy to reintroduce because it is the
// intuitive story, so it gets a negative assertion of its own.

//go:embed doc.go
var packageDoc string

//go:embed README.md
var readme string

func TestDocsStateTheUnboundedContract(t *testing.T) {
	t.Parallel()

	docs := map[string]string{
		"pkg/fdbgo/doc.go":    packageDoc,
		"pkg/fdbgo/README.md": readme,
	}

	// Each doc must carry the C++ citation that makes the claim checkable, name the
	// unbounded default, and name all three bounds a caller may choose.
	required := []struct{ needle, why string }{
		{"maxRetries = -1", "the libfdb_c default that proves the unbounded default is MATCHED, not divergent"},
		{"ReadYourWrites.actor.cpp:2078-2082", "the citation a reader needs to verify the claim against the C++ spec"},
		{"SetTimeout", "one of the three bounds a caller may choose"},
		{"SetRetryLimit", "one of the three bounds a caller may choose"},
		{"TransactCtx", "one of the three bounds a caller may choose"},
	}
	for name, body := range docs {
		for _, r := range required {
			if !strings.Contains(body, r.needle) {
				t.Errorf("%s no longer mentions %q — %s.\n"+
					"A migrator reading this doc must be able to learn that the default is unbounded, "+
					"that this matches libfdb_c, and how to bound it.", name, r.needle, r.why)
			}
		}
	}

	// The false framing this documentation exists to correct. If it reappears, the docs are
	// telling migrators that Go diverges from libfdb_c on a point where it does not.
	for name, body := range docs {
		lower := strings.ToLower(body)
		for _, bad := range []string{
			"unlike libfdb_c",
			"unlike `libfdb_c`",
			"libfdb_c's internal timeout",
			"a real difference from libfdb_c",
		} {
			if strings.Contains(lower, bad) {
				t.Errorf("%s contains the refuted claim %q.\n"+
					"libfdb_c's per-transaction defaults are timeoutInSeconds=0.0 / maxRetries=-1 "+
					"(ReadYourWrites.actor.cpp:2078-2082) and resetTimeout() arms the timebomb only when "+
					"the timeout is non-zero (:1576-1578), so a default libfdb_c transaction is unbounded "+
					"too. The unbounded default is MATCHED behaviour, not a divergence.", name, bad)
			}
		}
	}

	// Bootstrap is the one axis where Go really is stricter; losing it would leave readers
	// believing OpenDatabase can hang, which it cannot.
	for name, body := range docs {
		if !strings.Contains(body, "ootstrap") {
			t.Errorf("%s no longer documents the bootstrap bound — Go caps the initial coordinator "+
				"connection at 60s (fdb/database.go:139-153) where libfdb_c waits forever. "+
				"That is the one place this client is STRICTER, and readers need it to know "+
				"OpenDatabase does not hang.", name)
		}
	}
}
