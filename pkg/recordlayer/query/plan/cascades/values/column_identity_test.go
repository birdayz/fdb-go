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
// requires of every conversion: two columns sharing a leaf name, reached
// through different quantifiers, must not be one column.
//
// The old escape shape returned the bare name, so both of these answered "ID"
// and any caller keying a map by that answer conflated them. The identity
// answers differently on BOTH the correlation and the domain.
func TestIdentityIn_SameLeafNameDifferentQuantifiers(t *testing.T) {
	t.Parallel()

	orders := recordLayout("ORDERS", "ID", "CUSTOMER_ID")
	items := recordLayout("ITEMS", "ID", "ORDER_ID", "QTY")
	ordersDomain, itemsDomain := OrdinalDomainOfType(orders), OrdinalDomainOfType(items)

	o := NamedCorrelationIdentifier("o")
	i := NamedCorrelationIdentifier("i")

	ordersID := NewCorrelatedFieldValueWithResolvedOrdinalInDomain(
		NewQuantifiedObjectValue(o), "ID", 0, UnknownType, ordersDomain)
	itemsID := NewCorrelatedFieldValueWithResolvedOrdinalInDomain(
		NewQuantifiedObjectValue(i), "ID", 0, UnknownType, itemsDomain)

	if ordersID.Field != itemsID.Field {
		t.Fatalf("test setup: both columns must share the leaf name, got %q and %q", ordersID.Field, itemsID.Field)
	}

	oKey, ok := ordersID.CorrelatedIdentityIn(ordersDomain)
	if !ok {
		t.Fatal("orders.ID must have an identity in the ORDERS layout")
	}
	iKey, ok := itemsID.CorrelatedIdentityIn(itemsDomain)
	if !ok {
		t.Fatal("items.ID must have an identity in the ITEMS layout")
	}
	if oKey == iKey {
		t.Fatalf("orders.ID and items.ID share one identity %v — the name conflation survived the conversion", oKey)
	}
	if oKey.Ordinal != iKey.Ordinal {
		t.Fatalf("test setup: both must sit at the same ordinal so the DOMAIN and CORRELATION are what separate them, got %d and %d", oKey.Ordinal, iKey.Ordinal)
	}

	// Cross-domain is not a mismatch to be resolved by falling back to the
	// name — it is unanswerable.
	if _, ok := ordersID.CorrelatedIdentityIn(itemsDomain); ok {
		t.Fatal("orders.ID must not answer in the ITEMS layout")
	}
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
