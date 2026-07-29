package values

// RFC-197 step 0: column identity is (correlation, DOMAIN, ordinal path), and
// the domain is the element revision 1 of the RFC omitted. These pin the two
// halves of that claim: OrdinalIn answers ONLY inside a stated layout, and the
// token that makes the answer possible is invisible to identity.
//
// The dimension being probed here is the one no name comparison can express:
// the SAME ordinal under the SAME name meaning a different column because it
// indexes a different layout. A name-keyed site conflates two same-named
// columns; a naive ordinal-keyed site conflates two same-numbered slots, which
// reads as authoritative and is therefore worse.

import "testing"

func recordLayout(name string, cols ...string) *RecordType {
	fields := make([]Field, len(cols))
	for i, c := range cols {
		fields[i] = Field{Name: c, FieldType: UnknownType, Ordinal: i}
	}
	return NewRecordType(name, false, fields)
}

func TestOrdinalIn_AnswersOnlyInsideItsOwnDomain(t *testing.T) {
	t.Parallel()

	orders := recordLayout("ORDERS", "ID", "STATUS", "TOTAL")
	items := recordLayout("ITEMS", "ID", "SKU", "QTY")
	ordersDomain := OrdinalDomainOfType(orders)
	itemsDomain := OrdinalDomainOfType(items)

	if ordersDomain == itemsDomain {
		t.Fatal("two different layouts must not share a domain token")
	}

	// SOURCE-RELATIVE (unpinned) bake: answers in its own source's layout.
	statusOfOrders := NewFieldValueWithResolvedOrdinalInDomain("STATUS", 1, UnknownType, ordersDomain)
	if got, ok := statusOfOrders.OrdinalIn(ordersDomain); !ok || got != 1 {
		t.Fatalf("source-relative reference in its OWN domain = (%d,%v), want (1,true)", got, ok)
	}
	// The SAME ordinal, asked about a different layout. ITEMS slot 1 is SKU,
	// not STATUS — a name comparison ("STATUS" is not a column of ITEMS)
	// happens to decline here, but ORDERS.STATUS and ITEMS.SKU are both
	// ordinal 1 and nothing about the integer says which layout it addresses.
	if got, ok := statusOfOrders.OrdinalIn(itemsDomain); ok {
		t.Fatalf("ordinal 1 of ORDERS answered in the ITEMS layout as %d — a cross-domain conflation", got)
	}

	// FRONTIER-PINNED bake: the machinery-owned form, stamped by the
	// constructor that resolved it, answers in the frontier it is final for
	// and nowhere else.
	pinned, err := NewFieldValueOfOrdinal(NewQuantifiedObjectValueOfType(NamedCorrelationIdentifier("Q"), orders), 2)
	if err != nil {
		t.Fatalf("pinned bake: %v", err)
	}
	if !pinned.Resolved.FrontierPinned {
		t.Fatal("NewFieldValueOfOrdinal must produce a FrontierPinned path")
	}
	if got, ok := pinned.OrdinalIn(ordersDomain); !ok || got != 2 {
		t.Fatalf("pinned reference in its OWN frontier = (%d,%v), want (2,true)", got, ok)
	}
	if _, ok := pinned.OrdinalIn(itemsDomain); ok {
		t.Fatal("pinned reference answered in a frontier it was not pinned to")
	}
}

func TestOrdinalIn_SameLeafNameDifferentQuantifiers(t *testing.T) {
	t.Parallel()

	// The original bug class: two columns that share a leaf name. Here they
	// also share nothing else — different layouts, different slots — and only
	// the domain distinguishes them.
	left := recordLayout("EMP", "ID", "NAME", "DEPT_ID")
	right := recordLayout("DEPT", "NAME", "ID")
	leftDomain, rightDomain := OrdinalDomainOfType(left), OrdinalDomainOfType(right)

	empName := NewCorrelatedFieldValueWithResolvedOrdinalInDomain(
		NewQuantifiedObjectValue(NamedCorrelationIdentifier("E")), "NAME", 1, UnknownType, leftDomain)
	deptName := NewCorrelatedFieldValueWithResolvedOrdinalInDomain(
		NewQuantifiedObjectValue(NamedCorrelationIdentifier("D")), "NAME", 0, UnknownType, rightDomain)

	if got, ok := empName.OrdinalIn(leftDomain); !ok || got != 1 {
		t.Fatalf("E.NAME in EMP = (%d,%v), want (1,true)", got, ok)
	}
	if got, ok := deptName.OrdinalIn(rightDomain); !ok || got != 0 {
		t.Fatalf("D.NAME in DEPT = (%d,%v), want (0,true)", got, ok)
	}
	if _, ok := empName.OrdinalIn(rightDomain); ok {
		t.Fatal("E.NAME answered in DEPT's layout — the same leaf name is not the same column")
	}
	if _, ok := deptName.OrdinalIn(leftDomain); ok {
		t.Fatal("D.NAME answered in EMP's layout — the same leaf name is not the same column")
	}
}

func TestOrdinalIn_FailsClosed(t *testing.T) {
	t.Parallel()

	layout := recordLayout("T", "A", "B", "C")
	domain := OrdinalDomainOfType(layout)
	other := OrdinalDomainOfType(recordLayout("U", "A", "B"))

	t.Run("lazy value has no ordinal", func(t *testing.T) {
		t.Parallel()
		lazy := NewFlatFieldValue("A", UnknownType)
		if _, ok := lazy.OrdinalIn(domain); ok {
			t.Fatal("a LAZY value answered — its display name is not an ordinal")
		}
	})

	t.Run("nil path", func(t *testing.T) {
		t.Parallel()
		var p *FieldPath
		if _, ok := p.OrdinalIn(domain); ok {
			t.Fatal("a nil path answered")
		}
	})

	t.Run("multi-accessor path", func(t *testing.T) {
		t.Parallel()
		// `t.addr.city` fused into one node: the root ordinal is ADDR's, so
		// answering with it drops the nested descent.
		fused := NewFieldPathOfSingleInDomain("ADDR", 1, false, domain).
			WithSuffix(NewFieldPathOfSingle("CITY", 0, false))
		if got, ok := fused.OrdinalIn(domain); ok {
			t.Fatalf("a FUSED path answered %d — that is ADDR's slot, not the leaf's", got)
		}
	})

	t.Run("negative name-only ordinal", func(t *testing.T) {
		t.Parallel()
		// Java asserts ordinal >= 0 at ResolvedAccessor construction
		// (FieldValue.java:651); Go mints -1 name-only accessors at the
		// unnest/gather/index-expansion seeds, where two accessors are
		// ordinal-equal by construction. Answering -1 would hand the caller
		// an ordinal that matches every other name-only accessor.
		nameOnly := NewFieldPathOfSingleInDomain("ARR", -1, false, domain)
		if got, ok := nameOnly.OrdinalIn(domain); ok {
			t.Fatalf("a NAME-ONLY (-1) accessor answered %d", got)
		}
	})

	t.Run("value states no domain", func(t *testing.T) {
		t.Parallel()
		untaught := NewFieldValueWithResolvedOrdinal("A", 0, UnknownType)
		if _, ok := untaught.OrdinalIn(domain); ok {
			t.Fatal("a value with no domain token answered — an untaught producer must fail closed")
		}
	})

	t.Run("caller states no frontier", func(t *testing.T) {
		t.Parallel()
		v := NewFieldValueWithResolvedOrdinalInDomain("A", 0, UnknownType, domain)
		if _, ok := v.OrdinalIn(OrdinalDomain{}); ok {
			t.Fatal("an UNKNOWN frontier answered — a caller that cannot name its layout has nothing to check")
		}
	})

	t.Run("domain mismatch", func(t *testing.T) {
		t.Parallel()
		v := NewFieldValueWithResolvedOrdinalInDomain("A", 0, UnknownType, domain)
		if _, ok := v.OrdinalIn(other); ok {
			t.Fatal("a MISMATCHED domain answered")
		}
	})

	t.Run("nil receiver", func(t *testing.T) {
		t.Parallel()
		var fv *FieldValue
		if _, ok := fv.OrdinalIn(domain); ok {
			t.Fatal("a nil FieldValue answered")
		}
	})
}

func TestOrdinalDomain_UnknownForUntypedLayouts(t *testing.T) {
	t.Parallel()

	// A multi-record-type index's row type degrades to Unknown: no single
	// column order exists, so no token does either, and every OrdinalIn
	// against it declines.
	if OrdinalDomainOfType(UnknownType).IsKnown() {
		t.Fatal("UnknownType must not yield a domain")
	}
	if OrdinalDomainOfType(nil).IsKnown() {
		t.Fatal("a nil type must not yield a domain")
	}
	if OrdinalDomainOfType(recordLayout("EMPTY")).IsKnown() {
		t.Fatal("a zero-column layout must not yield a domain")
	}
	if OrdinalDomainOfColumnNames(nil).IsKnown() {
		t.Fatal("an empty column list must not yield a domain")
	}
}

func TestOrdinalDomain_SignatureIsInjectiveAndCaseFolded(t *testing.T) {
	t.Parallel()

	// A separator-joined signature would make ["A","B"] and ["A|B"] one
	// layout, and two layouts answering to one token is the check being
	// theatre. Length prefixes are what make the encoding injective.
	ab := OrdinalDomainOfColumnNames([]string{"A", "B"})
	joined := OrdinalDomainOfColumnNames([]string{"A|B"})
	concat := OrdinalDomainOfColumnNames([]string{"AB"})
	if ab == joined || ab == concat || joined == concat {
		t.Fatalf("colliding signatures: %v / %v / %v", ab, joined, concat)
	}
	// Order matters: the same names in a different order is a different
	// layout, and an ordinal means a different column in it.
	if OrdinalDomainOfColumnNames([]string{"A", "B"}) == OrdinalDomainOfColumnNames([]string{"B", "A"}) {
		t.Fatal("column ORDER must be part of the layout signature")
	}
	// Case folding matches how every resolution path in the engine compares
	// names, so a lower-cased declaration and an upper-cased descriptor name
	// the same layout.
	if OrdinalDomainOfColumnNames([]string{"a", "b"}) != ab {
		t.Fatal("the signature must be case-folded")
	}
	// The structural derivations agree: a layout given as a record type and
	// the same layout given as a column list are ONE domain. This is what lets
	// the translator (which resolves against a declared column list) and a
	// match candidate (which holds a descriptor-shaped row type) check each
	// other's ordinals at all.
	if OrdinalDomainOfType(recordLayout("T", "A", "B")) != ab {
		t.Fatal("OrdinalDomainOfType and OrdinalDomainOfColumnNames must agree on the same layout")
	}
}

func TestOrdinalDomain_ExcludedFromIdentityAndHash(t *testing.T) {
	t.Parallel()

	// The token is an evaluation-contract marker, exactly like FrontierPinned:
	// two references to the same column that arrived through different
	// producers must still intern as ONE memo member. If the domain leaked
	// into identity, every rule that dedups values would start splitting them
	// by provenance.
	withDomain := NewFieldValueWithResolvedOrdinalInDomain("A", 0, UnknownType, OrdinalDomainOfType(recordLayout("T", "A")))
	withOther := NewFieldValueWithResolvedOrdinalInDomain("A", 0, UnknownType, OrdinalDomainOfType(recordLayout("U", "A", "B")))
	withNone := NewFieldValueWithResolvedOrdinal("A", 0, UnknownType)

	if !withDomain.Resolved.Equals(withOther.Resolved) || !withDomain.Resolved.Equals(withNone.Resolved) {
		t.Fatal("FieldPath.Equals must ignore the domain token (ordinal-list equality, Java FieldValue.java:411-420)")
	}
	for _, other := range []*FieldValue{withOther, withNone} {
		if !SemanticEqualsUnderAliasMap(withDomain, other, nil) {
			t.Fatalf("semantic equality must ignore the domain token: %v vs %v", withDomain.Resolved.Domain, other.Resolved.Domain)
		}
		if SemanticHashCode(withDomain) != SemanticHashCode(other) {
			t.Fatal("semantic hash must ignore the domain token — unequal hashes for equal values break memo interning")
		}
	}
	// Nor may it render: Explain output is a plan-cache key.
	if ExplainValue(withDomain) != ExplainValue(withNone) {
		t.Fatalf("the domain token must not render: %q vs %q", ExplainValue(withDomain), ExplainValue(withNone))
	}
}

func TestOrdinalDomain_PreservedAcrossCopyAndRebuild(t *testing.T) {
	t.Parallel()

	// The preserve-on-copy contract Resolved already imposes now covers the
	// token: a rewrite that drops it silently degrades a domain-checkable
	// reference into one that fails closed — a lost optimization nobody sees.
	layout := recordLayout("T", "ID", "ADDR")
	domain := OrdinalDomainOfType(layout)
	src := NamedCorrelationIdentifier("SRC")
	tgt := NamedCorrelationIdentifier("TGT")
	base := NewCorrelatedFieldValueWithResolvedOrdinalInDomain(
		NewQuantifiedObjectValue(src), "ADDR", 1, UnknownType, domain)

	t.Run("WithChildren rebuild", func(t *testing.T) {
		t.Parallel()
		rebuilt := WithChildren(base, []Value{NewQuantifiedObjectValue(tgt)})
		if _, ok := rebuilt.(*FieldValue).OrdinalIn(domain); !ok {
			t.Fatal("WithChildren dropped the domain token")
		}
	})

	t.Run("rebase", func(t *testing.T) {
		t.Parallel()
		rebased := RebaseValue(base, AliasMap{src: tgt})
		if _, ok := rebased.(*FieldValue).OrdinalIn(domain); !ok {
			t.Fatal("Rebase dropped the domain token")
		}
	})

	t.Run("WithSuffix keeps the RECEIVER's root context", func(t *testing.T) {
		t.Parallel()
		// The fused path's root is still the receiver's, so the token rides
		// along — and the fused path still declines OrdinalIn for being
		// multi-accessor, which is a different arm.
		fused := base.Resolved.WithSuffix(NewFieldPathOfSingle("CITY", 0, false))
		if fused.Domain != domain {
			t.Fatalf("WithSuffix lost the receiver's domain: %v", fused.Domain)
		}
	})

	t.Run("pull-up through a record constructor", func(t *testing.T) {
		t.Parallel()
		// The pulled-up reference is re-framed onto the RC's OUTPUT layout, so
		// it must state THAT layout — not the input's, and not nothing.
		rc := NewRecordConstructorValue(RecordConstructorField{Name: "ADDR", Value: base})
		up := PullUpValue(base, rc, tgt)
		if up == nil {
			t.Fatal("pull-up produced nothing")
		}
		upFV, ok := up.(*FieldValue)
		if !ok {
			t.Fatalf("pull-up produced %T", up)
		}
		if _, answered := upFV.OrdinalIn(OrdinalDomainOfType(rc.Type().(*RecordType))); !answered {
			t.Fatalf("pulled-up reference states no usable domain: %#v", upFV.Resolved)
		}
	})
}
