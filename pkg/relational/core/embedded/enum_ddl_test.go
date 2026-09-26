package embedded

import (
	"errors"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// enumNamesOf is the stored records file's enum types, in file order, each
// with its values as name=number.
func enumNamesOf(t *testing.T, ddl string) []string {
	t.Helper()
	tmpl, err := BuildSchemaTemplateFromDDLNamed(ddl, "ENUMS")
	if err != nil {
		t.Fatalf("%s: %v", ddl, err)
	}
	p, err := tmpl.Underlying().ToProto()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range p.GetRecords().GetEnumType() {
		s := e.GetName() + "{"
		for i, v := range e.GetValue() {
			if i > 0 {
				s += ","
			}
			s += v.GetName() + "=" + itoa(int(v.GetNumber()))
		}
		out = append(out, s+"}")
	}
	return out
}

func itoa(i int) string {
	if i < 10 {
		return string(rune('0' + i))
	}
	return itoa(i/10) + string(rune('0'+i%10))
}

// TestEnumDDL_StoredAsJavaStoresIt pins Java's enum emission
// (DdlVisitor.visitEnumDefinition, FileDescriptorSerializer.
// registerTypeDescriptors): values numbered 0..n-1 as declared, names as the
// literals read, escaped by ProtoUtils.toProtoBufCompliantName ("." is __2,
// "$" is __1); a value no escape makes valid (a quote) is refused, as Java
// refuses it; per table in table order, the
// enums its closure reaches, in name order, each stored once under the first
// table that reaches it; an enum no table reaches never stored; two enums
// sharing a value name both stored with their own values. The whole-template
// bytes of these shapes are pinned against the JVM by the WS-J oracle
// (enum_value_index, enum_on_moved_table, enums_sharing_value_name,
// enum_unreferenced).
func TestEnumDDL_StoredAsJavaStoresIt(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, ddl string
		want      []string
	}{
		{
			"values numbered as declared, stored under Java's escapes",
			`create type as enum mood('JOYFUL', 'a.b$c', 'SAD') create table t(id bigint, m mood, primary key(id))`,
			[]string{"MOOD{JOYFUL=0,a__2b__1c=1,SAD=2}"},
		},
		{
			"an enum no table reaches is not stored",
			`create type as enum unused('Q') create table t(id bigint, x bigint, primary key(id))`,
			nil,
		},
		{
			"per table, in name order; a later table's first enum after the earlier table's",
			`create type as enum zz('Z') create type as enum aa('A') create type as enum mm('M') ` +
				`create table t1(id bigint, z zz, a aa, primary key(id)) create table t2(id bigint, m mm, a aa, primary key(id))`,
			[]string{"AA{A=0}", "ZZ{Z=0}", "MM{M=0}"},
		},
		{
			"two enums sharing a value name keep their own values",
			`create type as enum e1('X', 'Y') create type as enum e2('Y', 'Z') create table t(id bigint, a e1, b e2, primary key(id))`,
			[]string{"E1{X=0,Y=1}", "E2{Y=0,Z=1}"},
		},
		{
			"an enum reached through a struct and an array",
			`create type as enum c('R', 'G') create type as struct s(k c) create table t(id bigint, s s, cs c array, primary key(id))`,
			[]string{"C{R=0,G=1}"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := enumNamesOf(t, c.ddl)
			if len(got) != len(c.want) {
				t.Fatalf("stored enums %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("stored enums %v, want %v", got, c.want)
				}
			}
		})
	}
}

// TestEnumDDL_AnInvalidValueNameIsRefused: a doubled quote reads as a quote,
// which no protobuf identifier holds; Java's InvalidNameException text.
func TestEnumDDL_AnInvalidValueNameIsRefused(t *testing.T) {
	t.Parallel()
	_, err := BuildSchemaTemplateFromDDL(`create type as enum mood('O''NEIL') create table t(id bigint, m mood, primary key(id))`)
	if err == nil || !strings.Contains(err.Error(), "O'NEIL it not a valid protobuf identifier") {
		t.Fatalf("an enum value holding a quote = %v, want Java's InvalidNameException text", err)
	}
}

// TestEnumDDL_ANameCollisionIsRefused: the enum is registered before any struct
// or table (Java's clause loop), so the later declaration of the same name is
// the one refused.
func TestEnumDDL_ANameCollisionIsRefused(t *testing.T) {
	t.Parallel()
	for _, ddl := range []string{
		`create type as enum t('A') create table t(id bigint, primary key(id))`,
		`create type as enum s('A') create type as struct s(x bigint) create table u(id bigint, primary key(id))`,
	} {
		if _, err := BuildSchemaTemplateFromDDL(ddl); err == nil {
			t.Errorf("%s: accepted two types of one name", ddl)
		}
	}
}

// TestEnumDDL_AnIndexPredicateOverAnEnumIsRefused is F10: the target refuses an
// enum comparison in an index predicate with Java's RecordCoreException
// (XXXXX, unmapped) and its message; an IS NULL predicate on the same column is
// a NullComparison and builds.
func TestEnumDDL_AnIndexPredicateOverAnEnumIsRefused(t *testing.T) {
	t.Parallel()
	const table = `create type as enum mood('JOYFUL', 'HAPPY', 'SAD') create table t(id bigint, m mood, primary key(id)) `
	_, err := BuildSchemaTemplateFromDDL(table + `create index ix as select id from t where m = 'HAPPY' order by id`)
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Code != api.ErrCodeUnknown ||
		ae.Message != "attempt to create PoJo index comparison from unsupported comparison" {
		t.Fatalf("an enum comparison in an index predicate = %v, want XXXXX with Java's message", err)
	}
	if _, err := BuildSchemaTemplateFromDDL(table + `create index ix as select id from t where m is null order by id`); err != nil {
		t.Fatalf("an IS NULL predicate on an enum column: %v, want it built", err)
	}
}
