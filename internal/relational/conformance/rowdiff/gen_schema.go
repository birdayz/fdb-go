// Portions derived from FoundationDB Record Layer (Type.java,
// SemanticAnalyzer.java),
// Copyright 2015-2026 Apple Inc. and the FoundationDB project authors
// Copyright 2021-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package rowdiff

import (
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
)

// ColType is the P1 column-type universe. DOUBLE and FLOAT close RFC-182
// OQ-3: no generator in this harness could previously emit a floating-point
// column at all, which is exactly the blind spot that let an indexed FLOAT
// column matching NOTHING for any predicate (and a cross-type numeric
// comparison against an index building the wrong tuple-type range) ship
// undetected. LIKE/IN/BETWEEN arrive with P2.
type ColType int

const (
	ColBigint ColType = iota
	ColString
	ColBoolean
	ColDouble // 64-bit IEEE double
	ColFloat  // 32-bit IEEE single, carried as float64 everywhere above the
	// tuple-packing boundary (see coerceTupleElementForKey) -- genRows rounds
	// through float32 once at generation time so the oracle's row already
	// holds the value the engine will actually store and read back.
)

// NaN and +/-Infinity are deliberately NOT part of the DOUBLE/FLOAT data
// domain: the grammar's REAL_LITERAL (RelationalLexer.g4) requires at least
// one digit plus a '.' or exponent -- there is no NaN/Infinity token, so no
// SQL literal can spell either value, and genRows/InsertSQL seed data purely
// through literal INSERT text. Both values ARE reachable at runtime through
// computed expressions (e.g. 1.0/0.0 = Infinity), which is a materially
// different, non-literal-based harness and out of scope here.

// isNumericColType reports whether t is one of the numeric column types this
// generator supports (as opposed to STRING/BOOLEAN). Numeric types all
// promote against one another (Java's Type.maximumType / the value layer's
// values.MaximumType), so cross-type numeric comparisons and self-joins are
// meaningful, sargable shapes -- not merely the error path a cross-CATEGORY
// comparison (numeric vs string/bool) would be.
func isNumericColType(t ColType) bool {
	switch t {
	case ColBigint, ColDouble, ColFloat:
		return true
	}
	return false
}

// ColumnDef describes one generated column.
type ColumnDef struct {
	Name    string
	Type    ColType
	NotNull bool
}

// IndexDef is a single- or multi-column value index.
type IndexDef struct {
	Name string
	Cols []string
}

// TableDef is the generated table: fixed pk column "ID" plus value columns.
type TableDef struct {
	Name    string
	Cols    []ColumnDef // excludes ID
	Indexes []IndexDef
	// Struct, when non-nil, is the struct-typed column whose leaves the dotted
	// entries of Cols name. See nested.go; nil for a flat table, which is
	// every table the flat generator builds.
	Struct *StructCol
}

// Row maps UPPER-CASE column name (incl. "ID") to a driver-typed value:
// int64, string, bool, or nil.
type Row map[string]any

func genTable(rng *rand.Rand) TableDef {
	// Fixed column pool: A,B,C BIGINT (B NOT NULL so it is sortable in P1),
	// S STRING, F BOOLEAN, D DOUBLE, E FLOAT (both nullable). Small and
	// stable so index/predicate interplay — not schema exotica — is what
	// varies.
	cols := []ColumnDef{
		{Name: "A", Type: ColBigint},
		{Name: "B", Type: ColBigint, NotNull: true},
		{Name: "C", Type: ColBigint},
		{Name: "S", Type: ColString},
		{Name: "F", Type: ColBoolean},
		{Name: "D", Type: ColDouble},
		{Name: "E", Type: ColFloat},
	}

	pool := []IndexDef{
		{Name: "IDX_A", Cols: []string{"A"}},
		{Name: "IDX_B", Cols: []string{"B"}},
		{Name: "IDX_C", Cols: []string{"C"}},
		{Name: "IDX_S", Cols: []string{"S"}},
		{Name: "IDX_AB", Cols: []string{"A", "B"}},
		{Name: "IDX_D", Cols: []string{"D"}},
		{Name: "IDX_E", Cols: []string{"E"}},
	}
	// SAFETY INVARIANT (read TODO.md CQ-28/CQ-83 before lifting): D and E
	// (DOUBLE/FLOAT) are ONLY ever single-column indexes here — the one
	// composite index, IDX_AB, is BIGINT-only. genRows seeds -0.0 into the
	// D/E domains. The Go-side non-terminal signed-zero gaps this once
	// guarded are CLOSED by RFC-208's typed physical range-set binder, so the
	// restriction no longer hides a Go bug. It stays because this is a
	// CROSS-ENGINE corpus: Java and Go do not share one signed-zero equality
	// contract on a leading-float composite shape. Lifting the restriction
	// therefore means classifying those divergences deliberately, not
	// inheriting them as mystery nightly reds.
	rng.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
	// ≥2 indexes with high probability: that is where intersections and
	// cost ties live. 0-1 indexes still occur so full scans stay covered.
	nIdx := 2 + rng.IntN(3)
	if rng.IntN(10) == 0 {
		nIdx = rng.IntN(2)
	}
	idxs := make([]IndexDef, nIdx)
	copy(idxs, pool[:nIdx])

	return TableDef{Name: "T_RD", Cols: cols, Indexes: idxs}
}

func genRows(rng *rand.Rand, table TableDef) []Row {
	n := 20 + rng.IntN(101)
	rows := make([]Row, 0, n)
	// Whitespace-padded entries make TRIM meaningful and exercise every string
	// function over leading/trailing spaces (which round-trip and compare
	// byte-lexicographically, verified by probe).
	stringDomain := []string{"", "alpha", "beta", "gamma", "delta", "epsilon", " a", "b ", " c "}
	for i := 0; i < n; i++ {
		r := Row{"ID": int64(i + 1)}
		for _, col := range table.Cols {
			if !col.NotNull && rng.IntN(6) == 0 {
				r[col.Name] = nil
				continue
			}
			switch col.Type {
			case ColBigint:
				// Domain 0..9 → heavy duplicates → intersections do real work,
				// plus boundary values at low probability.
				switch rng.IntN(20) {
				case 0:
					r[col.Name] = int64(-1)
				case 1:
					r[col.Name] = int64(1) << 62
				default:
					r[col.Name] = int64(rng.IntN(10))
				}
			case ColString:
				r[col.Name] = stringDomain[rng.IntN(len(stringDomain))]
			case ColBoolean:
				r[col.Name] = rng.IntN(2) == 0
			case ColDouble:
				// Domain 0..9 (matching the BIGINT columns' heavy-duplicate
				// pattern) plus the boundary values that make DOUBLE hard:
				// 2^53 and its negation (the exactness boundary beyond which
				// a double can no longer represent every integer exactly,
				// probed on both sides of zero), 0.1 (classic non-terminating
				// binary fraction), and signed zero (TODO.md CQ-27, FIXED: an
				// indexed column's SARG range/probe construction used to
				// disagree with the residual-filter path — cmpAny's IEEE
				// equality (-0.0 == +0.0) — on a query landing exactly on
				// zero; bindScanComparisonsToRangeSet now widens the zero
				// endpoint of a range to span both adjacent keys, so the two
				// physical plans agree and this domain can include -0.0 like
				// any other boundary value. The dedicated deterministic pin
				// (negative_zero_index_sarg_probe_test.go) still exists as
				// the fast, targeted regression; this is the
				// random/nightly-scale corroboration).
				switch rng.IntN(20) {
				case 0:
					r[col.Name] = float64(-9007199254740992) // -(2^53)
				case 1:
					r[col.Name] = float64(9007199254740992) // 2^53
				case 2:
					r[col.Name] = 0.1 // classic non-terminating binary fraction
				case 3:
					r[col.Name] = math.Copysign(0, -1) // -0.0
				default:
					r[col.Name] = float64(rng.IntN(10))
				}
			case ColFloat:
				var f64 float64
				switch rng.IntN(20) {
				case 0:
					f64 = -16777216 // -(2^24): FLOAT's negative-side mantissa-exactness boundary
				case 1:
					// NOT exactly representable in float32 — the width-
					// narrowing boundary that hid the indexed-FLOAT SARG bug
					// (narrowConstAgainstFloatColumn's ceil/floor rewrite).
					f64 = 0.1
				case 2:
					f64 = float64(16777216) // 2^24: float32's mantissa-exactness boundary
				case 3:
					// -0.0 (TODO.md CQ-27, FIXED — see the ColDouble domain's
					// comment above for the SARG-vs-residual signed-zero gap
					// this now exercises rather than avoids).
					f64 = math.Copysign(0, -1)
				default:
					f64 = float64(rng.IntN(10))
				}
				// FLOAT columns store as a genuine float32; the value layer
				// and the driver widen it back to float64 on read (the
				// row-domain convention documented at coerceTupleElementForKey).
				// The oracle's authoritative row value must reflect that
				// ROUNDING or it would compare against a value the engine
				// can never actually produce — this line is what keeps
				// genRows and the real stored/read-back value in agreement.
				r[col.Name] = float64(float32(f64))
			}
		}
		rows = append(rows, r)
	}
	return rows
}

// DDL renders the schema-template body (CREATE TABLE + CREATE INDEX lines).
func (c *Case) DDL() string {
	var b strings.Builder
	b.WriteString(c.Table.structDDL())
	b.WriteString("CREATE TABLE ")
	b.WriteString(c.Table.Name)
	b.WriteString(" (id BIGINT")
	byName := map[string]ColumnDef{}
	for _, col := range c.Table.Cols {
		byName[col.Name] = col
	}
	for _, name := range c.Table.physicalCols() {
		b.WriteString(", ")
		b.WriteString(strings.ToLower(name))
		if c.Table.Struct != nil && name == c.Table.Struct.Name {
			b.WriteString(" " + strings.ToLower(c.Table.Struct.TypeName))
			continue
		}
		b.WriteString(" " + sqlTypeName(byName[name].Type))
	}
	b.WriteString(", PRIMARY KEY (id))")
	for _, idx := range c.Table.Indexes {
		fmt.Fprintf(&b, " CREATE INDEX %s ON %s (%s)",
			strings.ToLower(idx.Name), c.Table.Name, strings.ToLower(strings.Join(idx.Cols, ", ")))
	}
	return b.String()
}

// InsertSQL renders one multi-row INSERT for the dataset.
func (c *Case) InsertSQL() string {
	colType := map[string]ColType{}
	for _, col := range c.Table.Cols {
		colType[col.Name] = col.Type
	}
	var b strings.Builder
	b.WriteString("INSERT INTO ")
	b.WriteString(c.Table.Name)
	b.WriteString(" VALUES ")
	for i, r := range c.Rows {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("(")
		b.WriteString(renderLiteral(r["ID"]))
		for _, name := range c.Table.physicalCols() {
			b.WriteString(", ")
			if c.Table.Struct != nil && name == c.Table.Struct.Name {
				b.WriteString(c.Table.structLiteral(r))
				continue
			}
			b.WriteString(renderColumnLiteral(colType[name], r[name]))
		}
		b.WriteString(")")
	}
	return b.String()
}
