package semantic

import (
	"reflect"
	"testing"
)

func TestParseQualifiedName_Unqualified(t *testing.T) {
	t.Parallel()
	q := ParseQualifiedName("age", false)
	if q.IsQualified() {
		t.Fatal("bare name should be unqualified")
	}
	if got, want := q.Name(), "AGE"; got != want {
		t.Fatalf("Name: got %q, want %q", got, want)
	}
	if got := q.Qualifier(); len(got) != 0 {
		t.Fatalf("unqualified Qualifier: got %v, want []", got)
	}
}

func TestParseQualifiedName_Qualified(t *testing.T) {
	t.Parallel()
	q := ParseQualifiedName("t.col", false)
	if !q.IsQualified() {
		t.Fatal("t.col should be qualified")
	}
	if got, want := q.Name(), "COL"; got != want {
		t.Fatalf("Name: got %q, want %q", got, want)
	}
	if got, want := q.Qualifier(), []string{"T"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Qualifier: got %v, want %v", got, want)
	}
	if got, want := q.String(), "T.COL"; got != want {
		t.Fatalf("String: got %q, want %q", got, want)
	}
}

func TestParseQualifiedName_DeepQualified(t *testing.T) {
	t.Parallel()
	q := ParseQualifiedName("db.schema.table.col", false)
	if got, want := q.Name(), "COL"; got != want {
		t.Fatalf("Name: got %q, want %q", got, want)
	}
	if got, want := q.Qualifier(), []string{"DB", "SCHEMA", "TABLE"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Qualifier: got %v, want %v", got, want)
	}
	if got, want := q.Segments(), []string{"DB", "SCHEMA", "TABLE", "COL"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Segments: got %v, want %v", got, want)
	}
}

func TestParseQualifiedName_MixedQuotingPerSegment(t *testing.T) {
	t.Parallel()
	q := ParseQualifiedName(`t."Col"`, false)
	if got, want := q.Name(), "Col"; got != want {
		t.Fatalf("leaf Name case preserved: got %q, want %q", got, want)
	}
	if got, want := q.Qualifier(), []string{"T"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("qualifier case-folded: got %v, want %v", got, want)
	}
	// LeafIdentifier surfaces the quoting flag.
	leaf := q.LeafIdentifier()
	if !leaf.WasQuoted() {
		t.Fatal("leaf identifier should report wasQuoted=true")
	}
	if got, want := leaf.Name(), "Col"; got != want {
		t.Fatalf("leaf Identifier.Name: got %q, want %q", got, want)
	}
}

func TestParseQualifiedName_CaseSensitive(t *testing.T) {
	t.Parallel()
	q := ParseQualifiedName("T.Col", true)
	if got, want := q.String(), "T.Col"; got != want {
		t.Fatalf("case-sensitive: got %q, want %q", got, want)
	}
}

func TestParseQualifiedName_Empty(t *testing.T) {
	t.Parallel()
	q := ParseQualifiedName("", false)
	if !q.IsZero() {
		t.Fatal("empty input should produce zero QualifiedName")
	}
}

func TestQualifiedName_EqualsIgnoreQuoting(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		a, b QualifiedName
		want bool
	}{
		{"same bare", ParseQualifiedName("t.x", false), ParseQualifiedName("T.X", false), true},
		{"same via quoting", ParseQualifiedName(`"T"."X"`, false), ParseQualifiedName("t.x", false), true},
		{"different length", ParseQualifiedName("t.x", false), ParseQualifiedName("s.t.x", false), false},
		{"different leaf", ParseQualifiedName("t.x", false), ParseQualifiedName("t.y", false), false},
		{"different qualifier", ParseQualifiedName("t.x", false), ParseQualifiedName("u.x", false), false},
		{"both zero", QualifiedName{}, QualifiedName{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.a.EqualsIgnoreQuoting(tc.b); got != tc.want {
				t.Fatalf("%s ≈ %s: got %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestQualifiedName_PrefixedWith(t *testing.T) {
	t.Parallel()
	long := ParseQualifiedName("schema.table.col", false)
	cases := []struct {
		name   string
		prefix QualifiedName
		want   bool
	}{
		{"exact", long, true},
		{"leading qualifier", ParseQualifiedName("schema.table", false), true},
		{"first segment only", ParseQualifiedName("schema", false), true},
		{"different segment", ParseQualifiedName("other.table", false), false},
		{"longer than self", ParseQualifiedName("schema.table.col.extra", false), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := long.PrefixedWith(tc.prefix); got != tc.want {
				t.Fatalf("%s.PrefixedWith(%s): got %v, want %v",
					long, tc.prefix, got, tc.want)
			}
		})
	}
}

func TestQualifiedName_String_CanonicalDotted(t *testing.T) {
	t.Parallel()
	q := ParseQualifiedName("t.x", false)
	if got, want := q.String(), "T.X"; got != want {
		t.Fatalf("String: got %q, want %q", got, want)
	}
}

// Segments + Qualifier must be defensive copies — mutation must not
// leak back into the QualifiedName.
func TestQualifiedName_DefensiveCopies(t *testing.T) {
	t.Parallel()
	q := ParseQualifiedName("schema.table.col", false)
	segs := q.Segments()
	segs[0] = "HACKED"
	if got, want := q.String(), "SCHEMA.TABLE.COL"; got != want {
		t.Fatalf("mutation via Segments() leaked: got %q", got)
	}
	qual := q.Qualifier()
	qual[0] = "HACKED"
	if got, want := q.Qualifier()[0], "SCHEMA"; got != want {
		t.Fatalf("mutation via Qualifier() leaked: got %q", got)
	}
}

func TestFromSegments_Roundtrip(t *testing.T) {
	t.Parallel()
	// Pre-tokenised path: each segment fed in as-is.
	q := FromSegments([]string{"schema", "Table", `"Weird"`}, false)
	if got, want := q.Name(), "Weird"; got != want {
		t.Fatalf("quoted leaf Name: got %q, want %q", got, want)
	}
	// Unquoted segments case-fold.
	if got, want := q.Qualifier(), []string{"SCHEMA", "TABLE"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Qualifier: got %v, want %v", got, want)
	}
}
