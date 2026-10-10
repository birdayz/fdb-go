// Portions derived from FoundationDB Record Layer (ParseHelpers.java),
// Copyright 2021-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package expr

import (
	"errors"
	"strconv"
	"strings"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
)

// ParseDecimal is Java's ParseHelpers.parseDecimal over a decimal token's text:
// int32 (Integer) or int64 (Long) without a '.', where an L suffix keeps Long
// and an I suffix checks int range; float32 or float64 with one, overflowing to
// an infinity as Float/Double.parseFloat do. A REAL token without '.' (`1e5`)
// reaches Long.parseLong too. A refused text is Java's NumberFormatException,
// which reaches the client unclassified (XXXXX).
func ParseDecimal(text string) (any, error) {
	n := len(text)
	if n == 0 {
		return nil, numberFormatError(text)
	}
	if strings.Contains(text, ".") {
		switch text[n-1] {
		case 'f', 'F':
			return parseJavaFloat(text[:n-1], 32)
		case 'd', 'D':
			return parseJavaFloat(text[:n-1], 64)
		}
		return parseJavaFloat(text, 64)
	}
	switch text[n-1] {
	case 'l', 'L':
		v, err := strconv.ParseInt(text[:n-1], 10, 64)
		if err != nil {
			return nil, numberFormatError(text[:n-1])
		}
		return v, nil
	case 'i', 'I':
		v, err := strconv.ParseInt(text[:n-1], 10, 32)
		if err != nil {
			return nil, numberFormatError(text[:n-1])
		}
		return int32(v), nil
	}
	v, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return nil, numberFormatError(text)
	}
	if v == int64(int32(v)) {
		return int32(v), nil
	}
	return v, nil
}

// parseJavaFloat parses a REAL token's text; the lexer admits only texts both
// parsers accept, and out-of-range magnitudes round to an infinity or zero.
func parseJavaFloat(text string, bits int) (any, error) {
	f, err := strconv.ParseFloat(text, bits)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return nil, numberFormatError(text)
	}
	if bits == 32 {
		return float32(f), nil
	}
	return f, nil
}

func numberFormatError(input string) error {
	nfe := &recordlayer.NumberFormatError{Input: input}
	return api.WrapError(api.ErrCodeUnknown, nfe.Error(), nfe)
}
