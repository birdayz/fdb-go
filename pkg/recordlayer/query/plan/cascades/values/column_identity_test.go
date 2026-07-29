package values

import (
	"reflect"
	"strings"
	"testing"
)

// TestColumnIdentity_CarriesNoName is the structural half of RFC-197 item 2.
//
// The `.Field` gate in pkg/docscheck fires on composite-literal KEYS and on a
// returned `.Field` selector. A display name smuggled through a returned
// STRUCT FIELD is invisible to it — so an "escape" site could be converted to
// return a key type, look migrated, and still decide by name. The defense is
// that the key type has nowhere to put a name, and this test is what holds
// that true after the migration lands.
//
// Two string-kinded leaves are reachable and both are allowed BY TYPE, with a
// reason each:
//
//   - OrdinalDomain.sig is a signature of a whole ordered LAYOUT, unexported,
//     obtainable only from OrdinalDomainOfType / OrdinalDomainOfColumnNames —
//     both of which take an entire layout. A single column's display name
//     cannot be forged into one.
//   - CorrelationIdentifier.name is a QUANTIFIER alias, the correlation element
//     of the identity triple, minted by NamedCorrelationIdentifier /
//     UniqueCorrelationIdentifier from quantifier aliases — never from a
//     column's `.Field`.
//
// Anything else of string kind, at any depth, fails: that is a column name
// wearing a struct.
func TestColumnIdentity_CarriesNoName(t *testing.T) {
	t.Parallel()

	allowed := map[reflect.Type]string{
		reflect.TypeOf(OrdinalDomain{}):         "layout signature, unforgeable from a single column name",
		reflect.TypeOf(CorrelationIdentifier{}): "quantifier alias, the correlation element of the triple",
	}

	var walk func(t reflect.Type, path string)
	var offenders []string
	seen := map[reflect.Type]bool{}
	walk = func(rt reflect.Type, path string) {
		if _, ok := allowed[rt]; ok {
			return
		}
		if seen[rt] {
			return
		}
		seen[rt] = true
		switch rt.Kind() {
		case reflect.String:
			offenders = append(offenders, path)
		case reflect.Struct:
			for i := 0; i < rt.NumField(); i++ {
				f := rt.Field(i)
				walk(f.Type, path+"."+f.Name)
			}
		case reflect.Ptr, reflect.Slice, reflect.Array, reflect.Map:
			if rt.Kind() == reflect.Map {
				walk(rt.Key(), path+"[key]")
			}
			walk(rt.Elem(), path+"[elem]")
		}
	}
	walk(reflect.TypeOf(ColumnIdentity{}), "ColumnIdentity")

	if len(offenders) != 0 {
		t.Fatalf("ColumnIdentity reaches string-kinded field(s) %s.\n"+
			"A column's DISPLAY name carried inside a key struct is invisible to pkg/docscheck's\n"+
			".Field gate (it fires on composite-literal keys and returned selectors, not on a\n"+
			"returned struct field), so an escape site could look migrated and still decide by\n"+
			"name. Identity is (correlation, domain, ordinal) — render diagnostics from the\n"+
			"FieldValue the caller already holds instead.",
			strings.Join(offenders, ", "))
	}
}

// TestIdentityIn_SameLeafNameDifferentQuantifiers is the DIMENSION test the RFC
// requires of every conversion: two columns sharing a leaf name must not be one
// column.
//
// The old escape shape returned the bare name, so every case below answered
// "ID" and any caller keying a map by that answer conflated them. Each subtest
// varies exactly ONE element of the triple and holds the other two equal — the
// separation is the whole point, because a case that moves the correlation AND
// the domain together is satisfied by an identity that carries only one of
// them, and a conversion missing the other would still pass.
func TestIdentityIn_SameLeafNameDifferentQuantifiers(t *testing.T) {
	t.Parallel()

	orders := recordLayout("ORDERS", "ID", "CUSTOMER_ID")
	items := recordLayout("ITEMS", "ID", "ORDER_ID", "QTY")
	ordersDomain, itemsDomain := OrdinalDomainOfType(orders), OrdinalDomainOfType(items)

	o := NamedCorrelationIdentifier("o")
	i := NamedCorrelationIdentifier("i")

	t.Run("the CORRELATION alone separates them", func(t *testing.T) {
		t.Parallel()
		// A SELF-JOIN: one layout, so both references carry the same domain and
		// the same ordinal, and the quantifier is the only element left. This is
		// the case an identity built from (name, ordinal) — or from
		// (domain, ordinal) — cannot tell apart at all.
		left := NewCorrelatedFieldValueWithResolvedOrdinalInDomain(
			NewQuantifiedObjectValue(o), "ID", 0, UnknownType, ordersDomain)
		right := NewCorrelatedFieldValueWithResolvedOrdinalInDomain(
			NewQuantifiedObjectValue(i), "ID", 0, UnknownType, ordersDomain)

		leftKey, ok := left.CorrelatedIdentityIn(ordersDomain)
		if !ok {
			t.Fatal("o.ID must have an identity in the ORDERS layout")
		}
		rightKey, ok := right.CorrelatedIdentityIn(ordersDomain)
		if !ok {
			t.Fatal("i.ID must have an identity in the ORDERS layout")
		}
		if leftKey.Domain != rightKey.Domain || leftKey.Ordinal != rightKey.Ordinal {
			t.Fatalf("test setup: the domain and the ordinal must be EQUAL so only the correlation "+
				"can separate the two, got %v/%d and %v/%d",
				leftKey.Domain, leftKey.Ordinal, rightKey.Domain, rightKey.Ordinal)
		}
		if left.Field != right.Field {
			t.Fatalf("test setup: both must share the leaf name, got %q and %q", left.Field, right.Field)
		}
		if leftKey == rightKey {
			t.Fatalf("o.ID and i.ID share one identity %v — ordinal 0 of two quantifiers are "+
				"different columns, and this is the element a (name, ordinal) pair can never carry", leftKey)
		}
	})

	t.Run("the DOMAIN alone separates them", func(t *testing.T) {
		t.Parallel()
		// One quantifier, two layouts that both declare ID at ordinal 0. The
		// correlation and the ordinal are equal, so only the layout the ordinal
		// indexes can refuse — an ordinal compared across layouts is the same
		// conflation as a name, wearing a type that reads as authoritative.
		left := NewCorrelatedFieldValueWithResolvedOrdinalInDomain(
			NewQuantifiedObjectValue(o), "ID", 0, UnknownType, ordersDomain)
		right := NewCorrelatedFieldValueWithResolvedOrdinalInDomain(
			NewQuantifiedObjectValue(o), "ID", 0, UnknownType, itemsDomain)

		leftKey, ok := left.CorrelatedIdentityIn(ordersDomain)
		if !ok {
			t.Fatal("o.ID must have an identity in the ORDERS layout")
		}
		rightKey, ok := right.CorrelatedIdentityIn(itemsDomain)
		if !ok {
			t.Fatal("o.ID baked in ITEMS must have an identity in the ITEMS layout")
		}
		if leftKey.Correlation != rightKey.Correlation || leftKey.Ordinal != rightKey.Ordinal {
			t.Fatalf("test setup: the correlation and the ordinal must be EQUAL so only the domain "+
				"can separate the two, got %v/%d and %v/%d",
				leftKey.Correlation, leftKey.Ordinal, rightKey.Correlation, rightKey.Ordinal)
		}
		if leftKey == rightKey {
			t.Fatalf("two layouts' ordinal-0 columns share one identity %v — the name conflation "+
				"survived the conversion as an ordinal conflation", leftKey)
		}

		// Cross-domain is not a mismatch to be resolved by falling back to the
		// name — it is unanswerable.
		if _, ok := left.CorrelatedIdentityIn(itemsDomain); ok {
			t.Fatal("an ORDERS-baked o.ID must not answer in the ITEMS layout")
		}
	})
}

// TestIdentityIn_FailsClosed pins every arm that must decline rather than hand
// back an ordinal the caller would trust.
func TestIdentityIn_FailsClosed(t *testing.T) {
	t.Parallel()

	layout := recordLayout("T", "ID", "ADDR", "NAME")
	domain := OrdinalDomainOfType(layout)
	corr := NamedCorrelationIdentifier("q")

	t.Run("lazy value", func(t *testing.T) {
		t.Parallel()
		lazy := NewFieldValue(NewQuantifiedObjectValue(corr), "ID", UnknownType)
		if _, ok := lazy.CorrelatedIdentityIn(domain); ok {
			t.Fatal("a lazy value has no ordinal; it must not produce an identity")
		}
	})

	t.Run("chained accessor", func(t *testing.T) {
		t.Parallel()
		// inner.addr.city — the quantifier's row is NOT the layout the
		// ordinal indexes, so reporting (corr, domain, ordinal) would name a
		// slot of the wrong layout.
		inner := NewCorrelatedFieldValueWithResolvedOrdinalInDomain(
			NewQuantifiedObjectValue(corr), "ADDR", 1, UnknownType, domain)
		outer := NewFieldValueWithResolvedOrdinalInDomain("CITY", 0, UnknownType, domain)
		outer.Child = inner
		if _, ok := outer.IdentityIn(domain); ok {
			t.Fatal("a chained accessor must decline: its child is not the quantifier's row")
		}
	})

	t.Run("name-only negative ordinal", func(t *testing.T) {
		t.Parallel()
		nameOnly := &FieldValue{
			Field:    "ARR",
			Typ:      UnknownType,
			Child:    NewQuantifiedObjectValue(corr),
			Resolved: NewFieldPathOfSingleInDomain("ARR", -1, false, domain),
		}
		if _, ok := nameOnly.CorrelatedIdentityIn(domain); ok {
			t.Fatal("a -1 name-only accessor is ordinal-equal to every other one; it must decline")
		}
	})

	t.Run("unknown domain", func(t *testing.T) {
		t.Parallel()
		v := NewCorrelatedFieldValueWithResolvedOrdinal(
			NewQuantifiedObjectValue(corr), "ID", 0, UnknownType)
		if _, ok := v.CorrelatedIdentityIn(domain); ok {
			t.Fatal("a producer that could not state its layout must not answer")
		}
		taught := NewCorrelatedFieldValueWithResolvedOrdinalInDomain(
			NewQuantifiedObjectValue(corr), "ID", 0, UnknownType, domain)
		if _, ok := taught.CorrelatedIdentityIn(OrdinalDomain{}); ok {
			t.Fatal("a caller that cannot state its frontier must not get an answer")
		}
	})

	t.Run("childless declines the correlated form", func(t *testing.T) {
		t.Parallel()
		v := NewFieldValueWithResolvedOrdinalInDomain("ID", 0, UnknownType, domain)
		if _, ok := v.CorrelatedIdentityIn(domain); ok {
			t.Fatal("a childless value reads no quantifier's row; the correlated form must decline")
		}
		if got, ok := v.IdentityIn(domain); !ok || got.Correlation != (CorrelationIdentifier{}) || got.Ordinal != 0 {
			t.Fatalf("the uncorrelated form must answer with the zero correlation, got %v ok=%v", got, ok)
		}
	})
}

// TestOrdinalOfNameIn_ResolvesMetadataNamesOnce pins the one sanctioned
// direction for a name: metadata (index/PK/descriptor column lists) resolved
// against a stated layout, at a boundary, exactly once.
func TestOrdinalOfNameIn_ResolvesMetadataNamesOnce(t *testing.T) {
	t.Parallel()

	layout := recordLayout("T", "ID", "DEPT_ID", "NAME")
	domain := OrdinalDomainOfType(layout)

	for _, tc := range []struct {
		name string
		want int
	}{{"ID", 0}, {"DEPT_ID", 1}, {"NAME", 2}} {
		got, ok := OrdinalOfNameIn(layout, tc.name)
		if !ok || got.Ordinal != tc.want || got.Domain != domain {
			t.Fatalf("OrdinalOfNameIn(%q) = %v ok=%v, want ordinal %d in %v", tc.name, got, ok, tc.want, domain)
		}
	}

	// Case folds: metadata lists do not agree with a record type's spelling.
	if got, ok := OrdinalOfNameIn(layout, "dept_id"); !ok || got.Ordinal != 1 {
		t.Fatalf("case-folded lookup = %v ok=%v, want ordinal 1", got, ok)
	}

	if _, ok := OrdinalOfNameIn(layout, "NO_SUCH"); ok {
		t.Fatal("a name the layout does not declare must decline")
	}
	if _, ok := OrdinalOfNameIn(UnknownType, "ID"); ok {
		t.Fatal("a layout with no declared column order must decline")
	}
	if _, ok := OrdinalOfNameIn(layout, ""); ok {
		t.Fatal("an empty name must decline")
	}
}

// TestSameColumnPath varies ONE identity element per case and holds the rest
// equal — the same discipline TestIdentityIn_SameLeafNameDifferentQuantifiers
// applies, and for the same reason: a case that moves two elements at once is
// satisfied by a comparison carrying only one of them.
func TestSameColumnPath(t *testing.T) {
	t.Parallel()

	orders := OrdinalDomainOfColumnNames([]string{"ID", "CUSTOMER_ID"})
	items := OrdinalDomainOfColumnNames([]string{"ID", "ORDER_ID", "QTY"})

	path := func(name string, ordinal int, domain OrdinalDomain) *FieldPath {
		return NewFieldPathOfSingleInDomain(name, ordinal, false, domain)
	}

	t.Run("the DISPLAY NAME alone is not a difference", func(t *testing.T) {
		t.Parallel()
		// Same layout, same ordinal, two renderings. Java's
		// ResolvedAccessor.equals excludes the name for exactly this case.
		if !SameColumnPath(path("ID", 0, orders), path("ALIASED", 0, orders)) {
			t.Fatal("two renderings of ordinal 0 of one layout were called different columns: " +
				"the display name is deciding identity")
		}
	})

	t.Run("the ORDINAL alone separates them", func(t *testing.T) {
		t.Parallel()
		if SameColumnPath(path("ID", 0, orders), path("ID", 1, orders)) {
			t.Fatal("two ordinals of one layout matched")
		}
	})

	t.Run("the DOMAIN alone separates them", func(t *testing.T) {
		t.Parallel()
		// Identical name AND ordinal, so nothing but the layout can refuse.
		if SameColumnPath(path("ID", 0, orders), path("ID", 0, items)) {
			t.Fatal("ordinal 0 of two different layouts matched: an ordinal is comparable " +
				"only against a stated layout")
		}
	})

	t.Run("fails closed", func(t *testing.T) {
		t.Parallel()
		unknown := OrdinalDomain{}
		if SameColumnPath(path("ID", 0, unknown), path("ID", 0, unknown)) {
			t.Fatal("two paths with an UNSTATED layout matched: unknown is not a domain")
		}
		if SameColumnPath(path("ID", 0, unknown), path("ID", 0, orders)) {
			t.Fatal("an unstated layout matched a stated one")
		}
		// Go's -1 name-only accessors are ordinal-equal by construction, so an
		// ordinal comparison over them is vacuous (Java forbids the shape,
		// FieldValue.java:651).
		if SameColumnPath(path("X", -1, orders), path("Y", -1, orders)) {
			t.Fatal("two name-only (-1) accessors matched: ordinal equality is vacuous there")
		}
		if SameColumnPath(nil, path("ID", 0, orders)) || SameColumnPath(path("ID", 0, orders), nil) {
			t.Fatal("a nil path matched")
		}
		if SameColumnPath(&FieldPath{Domain: orders}, &FieldPath{Domain: orders}) {
			t.Fatal("two empty paths matched: an empty path reads nothing")
		}
	})

	t.Run("every accessor of a nested path is compared", func(t *testing.T) {
		t.Parallel()
		nested := func(rootOrd, leafOrd int) *FieldPath {
			return &FieldPath{
				Accessors: []ResolvedAccessor{
					{Field: "ADDR", Ordinal: rootOrd}, {Field: "CITY", Ordinal: leafOrd},
				},
				Domain: orders,
			}
		}
		if !SameColumnPath(nested(0, 3), nested(0, 3)) {
			t.Fatal("an identical two-accessor path did not match")
		}
		if SameColumnPath(nested(0, 3), nested(1, 3)) {
			t.Fatal("two nested paths differing at the ROOT accessor matched")
		}
		if SameColumnPath(nested(0, 3), path("CITY", 0, orders)) {
			t.Fatal("a two-accessor path matched a one-accessor path")
		}
	})
}
