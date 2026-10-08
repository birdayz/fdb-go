package expressions

func preparedSameChildReferences(a, b RelationalExpression) bool {
	aQs, bQs := a.GetQuantifiers(), b.GetQuantifiers()
	if len(aQs) != len(bQs) {
		return false
	}
	for i := range aQs {
		if aQs[i].GetAlias() != bQs[i].GetAlias() || !quantifierAttributesEqual(aQs[i], bQs[i]) ||
			preparedQuantifierReference(aQs[i]) != preparedQuantifierReference(bQs[i]) {
			return false
		}
	}
	return true
}

func preparedQuantifierReference(quantifier Quantifier) *Reference {
	return canonicalReferenceReadOnly(quantifier.rangesOver)
}
