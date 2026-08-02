package embedded

import (
	"errors"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// TestAsSelectIndex_StarOverArrayRejectsAtTheSemanticBoundary pins that a star
// index definition and the equivalent EXPLICIT projection fail the SAME way.
//
// The generator's array rejection keys on the value's Typ. Star expansion built
// its FieldValues from the descriptor WITHOUT a type, so a repeated column
// walked straight past the check and only surfaced far downstream as an XX000
// metadata-validation failure:
//
//	SELECT * FROM t2 ORDER BY p1   -> XX000: build RecordMetaData: ... field "ARR" in "T2" is repeated
//	SELECT p1, arr FROM t2 ORDER BY p1 -> 0A000: cannot create index on array field 'ARR' without unnesting
//
// A star must not be a cheaper way past an admission check than writing the
// columns out. XX000 is an internal error — it tells the user the engine broke,
// not that their DDL is unsupported.
func TestAsSelectIndex_StarOverArrayRejectsAtTheSemanticBoundary(t *testing.T) {
	t.Parallel()

	const want = "cannot create index on array field"
	for _, ddl := range []string{
		"CREATE INDEX gidx AS SELECT * FROM t2 ORDER BY p1",
		"CREATE INDEX gidx AS SELECT p1, arr FROM t2 ORDER BY p1",
	} {
		_, err := buildSchemaTemplateFromDDL(asSelectGoldenDDL + "\n" + ddl)
		if err == nil {
			t.Errorf("%s was ACCEPTED — an index over a repeated field without "+
				"unnesting is unsupported", ddl)
			continue
		}
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeUnsupportedOperation {
			t.Errorf("%s\n got %v\nwant %s (unsupported operation) at the semantic "+
				"boundary — XX000 reports an engine failure, not an unsupported DDL",
				ddl, err, api.ErrCodeUnsupportedOperation)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s: message must name the array column; got %v", ddl, err)
		}
	}

	// The star form over a table with NO array column must still build — the
	// typing added for the check must not reject ordinary star indexes.
	if _, err := buildSchemaTemplateFromDDL(asSelectGoldenDDL +
		"\nCREATE INDEX gidx AS SELECT * FROM t1 ORDER BY p1"); err != nil {
		t.Errorf("a star index over a table with no repeated column must still build: %v", err)
	}
}
