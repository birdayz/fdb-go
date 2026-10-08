package embedded

import (
	"errors"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// TestDDLNormalizer_RefusesWhatJavasAstNormalizerRefuses drives every arm of
// rejectNormalizerFaults through the one DDL front end, each against the
// target's code and message (the same shapes are measured against the JVM by
// the WS-J oracle, prefix normalizer_). The order is the target's: first fault
// in tree order wins, OFFSET before LIMIT within one clause, and an IN
// predicate's checks at entry, before its items are visited.
func TestDDLNormalizer_RefusesWhatJavasAstNormalizerRefuses(t *testing.T) {
	t.Parallel()
	const table = `create table t(id bigint, a bigint, b bigint, primary key(id)) `
	const (
		offsetMsg = "OFFSET clause is not supported."
		limitMsg  = "LIMIT clause is not supported."
		nestedMsg = "IN predicate does not support nested SELECT"
		nullMsg   = "NULL values are not allowed in the IN list"
	)
	for _, tc := range []struct {
		name string
		ddl  string
		code api.ErrorCode
		msg  string // "" means the walk admits it
	}{
		{
			"limit and offset in an index", table + `create index ix as select a from t order by a limit 5 offset 1`,
			api.ErrCodeUnsupportedQuery, offsetMsg,
		},
		{
			"limit in an index", table + `create index ix as select a from t order by a limit 5`,
			api.ErrCodeUnsupportedQuery, limitMsg,
		},
		{
			"limit in a derived table of an index", table + `create index ix as select d.a from (select a from t limit 5) as d order by d.a`,
			api.ErrCodeUnsupportedQuery, limitMsg,
		},
		{
			"limit in a view body", table + `create view v as select a from t limit 5`,
			api.ErrCodeUnsupportedQuery, limitMsg,
		},
		{
			"limit in a function body", table + `create function f() as select a from t limit 5`,
			api.ErrCodeUnsupportedQuery, limitMsg,
		},
		{
			"an earlier limit wins over a later offset", table +
				`create index i1 as select a from t order by a limit 5 create index i2 as select b from t order by b limit 5 offset 1`,
			api.ErrCodeUnsupportedQuery, limitMsg,
		},
		{
			"a limit wins over an earlier faulty table", `create table u(id bigint, primary key(nope)) ` + table +
				`create index ix as select a from t order by a limit 5`,
			api.ErrCodeUnsupportedQuery, limitMsg,
		},
		{
			"in over a nested select", table + `create index ix as select a from t where a in (select b from t) order by a`,
			api.ErrCodeUnsupportedQuery, nestedMsg,
		},
		{
			"a bare null in an in list", table + `create index ix as select a from t where a in (1, null) order by a`,
			api.ErrCodeWrongObjectType, nullMsg,
		},
		{
			"an earlier null item wins over a later limit", table +
				`create index i1 as select a from t where a in (1, null) order by a create index i2 as select b from t order by b limit 5`,
			api.ErrCodeWrongObjectType, nullMsg,
		},
		{
			"an earlier limit wins over a later nested select", table +
				`create index i1 as select a from t order by a limit 5 create index i2 as select a from t where a in (select b from t) order by a`,
			api.ErrCodeUnsupportedQuery, limitMsg,
		},
		{
			"the nested select is refused before its limit is visited", table +
				`create index ix as select a from t where a in (select b from t limit 1) order by a`,
			api.ErrCodeUnsupportedQuery, nestedMsg,
		},
		{
			"a null item is refused before an earlier item's limit is visited", table +
				`create index ix as select a from t where a in (exists (select b from t limit 1), null) order by a`,
			api.ErrCodeWrongObjectType, nullMsg,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := BuildSchemaTemplateFromDDL(tc.ddl)
			var ae *api.Error
			if !errors.As(err, &ae) || ae.Code != tc.code || ae.Message != tc.msg {
				t.Fatalf("got %v, want %s %q", err, tc.code, tc.msg)
			}
		})
	}
}

// TestDDLNormalizer_AdmitsWhatItDoesNotCheck: a NULL that is not a bare item
// (`NULL + 1`), a parameter list and a column list are not the walk's to refuse;
// the template is built or refused by what comes after it, never with one of the
// walk's messages.
func TestDDLNormalizer_AdmitsWhatItDoesNotCheck(t *testing.T) {
	t.Parallel()
	const table = `create table t(id bigint, a bigint, b bigint, primary key(id)) `
	// The control: a template the walk passes must build, or every arm below
	// could read as admitted while the walk refused everything.
	if _, err := BuildSchemaTemplateFromDDL(table + `create index ix as select a from t order by a`); err != nil {
		t.Fatalf("a plain index: %v, want it built", err)
	}
	for _, ddl := range []string{
		table + `create index ix as select a from t where a in (null + 1) order by a`,
		table + `create index ix as select a from t where a in (1, 2) order by a`,
	} {
		_, err := BuildSchemaTemplateFromDDL(ddl)
		if err == nil {
			continue
		}
		for _, msg := range []string{"OFFSET clause", "LIMIT clause", "nested SELECT", "NULL values are not allowed"} {
			if strings.Contains(err.Error(), msg) {
				t.Errorf("%s: refused by the normalizer walk: %v", ddl, err)
			}
		}
	}
}
