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
			"an enum with the values of one the table already reached is that type, not stored",
			`create type as enum e1('X', 'Y') create type as enum e2('X', 'Y') create table t(id bigint, a e1, b e2, primary key(id))`,
			[]string{"E1{X=0,Y=1}"},
		},
		{
			"the same shape is one type per table: a later table reaching the other name stores it",
			`create type as enum e2('X', 'Y') create type as enum e1('X', 'Y') ` +
				`create table t2(id bigint, b e2, primary key(id)) create table t1(id bigint, a e1, c e2, primary key(id))`,
			[]string{"E2{X=0,Y=1}", "E1{X=0,Y=1}"},
		},
		{
			"a value spelled like its enum",
			`create type as enum status('STATUS', 'DONE') create table t(id bigint, s status, primary key(id))`,
			[]string{"STATUS{STATUS=0,DONE=1}"},
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

// TestDDL_OneShapeIsOneTypePerTable pins which name each field references when
// a table reaches two enums with the same values, or two structs with the same
// fields: Java's TypeRepository.Builder keys a table's types by Type.equals,
// which is blind to the type's name (Type.Enum compares values, Type.Record its
// fields by name, index and type), so the first name the table reaches (in
// FIELD order, Type.Record.defineProtoType's walk of the columns as declared)
// is the one every field of that shape references. Pinned against
// the JVM by the WS-J oracle shapes enums_identical_values and
// structs_identical_fields (whole-template bytes equal) and
// enums_identical_values_two_tables (canonical-equal: its record_types order
// differs, DIVERGENCES.md "Go does not reproduce Java's record_types order or
// anonymous type names").
func TestDDL_OneShapeIsOneTypePerTable(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, ddl string
		want      map[string]map[string]string // table -> field -> type_name
		messages  []string
	}{
		{
			"enums",
			`create type as enum e1('X', 'Y') create type as enum e2('X', 'Y') create table t(id bigint, a e1, b e2, primary key(id))`,
			map[string]map[string]string{"T": {"A": "E1", "B": "E1"}},
			[]string{"T"},
		},
		{
			"enums over two tables",
			`create type as enum e2('X', 'Y') create type as enum e1('X', 'Y') ` +
				`create table t2(id bigint, b e2, primary key(id)) create table t1(id bigint, a e1, c e2, primary key(id))`,
			map[string]map[string]string{"T2": {"B": "E2"}, "T1": {"A": "E1", "C": "E1"}},
			[]string{"T2", "T1"},
		},
		{
			// Field order, not name order: the first column reaches E2.
			"enums in field order",
			`create type as enum e1('X', 'Y') create type as enum e2('X', 'Y') create table t(id bigint, a e2, b e1, primary key(id))`,
			map[string]map[string]string{"T": {"A": "E2", "B": "E2"}},
			[]string{"T"},
		},
		{
			"enums differing in a value's number are two types",
			`create type as enum e1('X', 'Y') create type as enum e2('Y', 'X') create table t(id bigint, a e1, b e2, primary key(id))`,
			map[string]map[string]string{"T": {"A": "E1", "B": "E2"}},
			[]string{"T"},
		},
		{
			"structs",
			`create type as struct s1(x bigint) create type as struct s2(x bigint) create table t(id bigint, a s1, b s2, primary key(id))`,
			map[string]map[string]string{"T": {"A": "S1", "B": "S1"}},
			[]string{"S1", "T"},
		},
		{
			"structs differing in a field's name are two types",
			`create type as struct s1(x bigint) create type as struct s2(y bigint) create table t(id bigint, a s1, b s2, primary key(id))`,
			map[string]map[string]string{"T": {"A": "S1", "B": "S2"}},
			[]string{"S1", "S2", "T"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			tmpl, err := BuildSchemaTemplateFromDDLNamed(c.ddl, "SHAPES")
			if err != nil {
				t.Fatal(err)
			}
			p, err := tmpl.Underlying().ToProto()
			if err != nil {
				t.Fatal(err)
			}
			var messages []string
			for _, m := range p.GetRecords().GetMessageType() {
				if m.GetName() == "RecordTypeUnion" {
					continue
				}
				messages = append(messages, m.GetName())
				for _, f := range m.GetField() {
					if want, ok := c.want[m.GetName()][f.GetName()]; ok && f.GetTypeName() != want {
						t.Errorf("%s.%s references %q, want %q", m.GetName(), f.GetName(), f.GetTypeName(), want)
					}
				}
			}
			if strings.Join(messages, ",") != strings.Join(c.messages, ",") {
				t.Errorf("stored messages %v, want %v", messages, c.messages)
			}
		})
	}
}

// TestEnumDDL_AnInvalidValueNameIsRefused: a doubled quote reads as a quote,
// which no protobuf identifier holds; Java's PlanGenerator refuses the
// InvalidNameException as 42602 with its text. Pinned against the JVM by the
// WS-J enum DDL refusal spec.
func TestEnumDDL_AnInvalidValueNameIsRefused(t *testing.T) {
	t.Parallel()
	_, err := BuildSchemaTemplateFromDDL(`create type as enum mood('O''NEIL') create table t(id bigint, m mood, primary key(id))`)
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Code != api.ErrCodeInvalidName || ae.Message != "O'NEIL it not a valid protobuf identifier" {
		t.Fatalf("an enum value holding a quote = %v, want 42602 with Java's InvalidNameException text", err)
	}
}

// TestEnumDDL_ANameCollisionIsRefused: the enum is registered before any struct
// or table (Java's clause loop), so the later declaration of the same name is
// the one refused, 42F59 with Java's text. Pinned against the JVM by the WS-J
// enum DDL refusal spec.
func TestEnumDDL_ANameCollisionIsRefused(t *testing.T) {
	t.Parallel()
	for ddl, name := range map[string]string{
		`create type as enum t('A') create table t(id bigint, primary key(id))`:                                   "T",
		`create type as enum s('A') create type as struct s(x bigint) create table u(id bigint, primary key(id))`: "S",
	} {
		_, err := BuildSchemaTemplateFromDDL(ddl)
		var ae *api.Error
		if !errors.As(err, &ae) || ae.Code != api.ErrCodeInvalidSchemaTemplate ||
			ae.Message != "type with name '"+name+"' already exists" {
			t.Errorf("%s = %v, want 42F59 \"type with name '%s' already exists\"", ddl, err, name)
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

// TestDDL_AnIndexPredicateOverAUUIDIsRefused is F10's UUID sibling: the
// comparand is a string promoted to a java.util.UUID, which no Value field
// holds, so LiteralKeyExpression.toProtoValue refuses it (RecordCoreException,
// XXXXX, "Unsupported value type class java.util.UUID"); IS NULL builds. Both
// pinned against the JVM by the WS-J oracle shapes uuid_predicate_index and
// uuid_is_null_predicate_index.
func TestDDL_AnIndexPredicateOverAUUIDIsRefused(t *testing.T) {
	t.Parallel()
	const table = `create table t(id bigint, u uuid, primary key(id)) `
	_, err := BuildSchemaTemplateFromDDL(table + `create index ix as select id from t where u = '5ea7a4c2-54a4-4f7a-9d0e-2d1b0b3f8e10' order by id`)
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Code != api.ErrCodeUnknown || ae.Message != "Unsupported value type class java.util.UUID" {
		t.Fatalf("a UUID comparison in an index predicate = %v, want XXXXX with Java's message", err)
	}
	if _, err := BuildSchemaTemplateFromDDL(table + `create index ix as select id from t where u is null order by id`); err != nil {
		t.Fatalf("an IS NULL predicate on a UUID column: %v, want it built", err)
	}
}
