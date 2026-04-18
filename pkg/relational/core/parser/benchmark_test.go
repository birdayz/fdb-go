package parser

import (
	"strings"
	"testing"
)

var (
	// Short: typical OLTP path — single SELECT with a WHERE clause.
	benchShortSQL = `SELECT id, name, price FROM orders WHERE id = 42 AND status = 'OPEN'`

	// Medium: schema template with several CREATE clauses.
	benchMediumSQL = `CREATE SCHEMA TEMPLATE bench_template
		CREATE TYPE AS STRUCT Address (street string, city string, zip integer)
		CREATE TABLE customers (id bigint, name string, address Address, PRIMARY KEY(id))
		CREATE TABLE orders (id bigint, customer_id bigint, price double, PRIMARY KEY(id))
		CREATE INDEX order_by_customer AS SELECT customer_id, id FROM orders`

	// Long: moderately large INSERT with 50 tuples. Exercises the parser's
	// repeat-many path, which is what high-fan-out INSERTs hit in practice.
	benchLongSQL = buildLongInsert()
)

func buildLongInsert() string {
	var b strings.Builder
	b.WriteString("INSERT INTO orders VALUES ")
	for i := 0; i < 50; i++ {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("(")
		b.WriteString("42, 'customer_name', 99.95, 'PAID'")
		b.WriteString(")")
	}
	return b.String()
}

func benchmarkParse(b *testing.B, sql string) {
	b.ReportAllocs()
	b.SetBytes(int64(len(sql)))
	for i := 0; i < b.N; i++ {
		if _, err := Parse(sql); err != nil {
			b.Fatalf("Parse failed: %v", err)
		}
	}
}

func BenchmarkParseShort(b *testing.B)  { benchmarkParse(b, benchShortSQL) }
func BenchmarkParseMedium(b *testing.B) { benchmarkParse(b, benchMediumSQL) }
func BenchmarkParseLong(b *testing.B)   { benchmarkParse(b, benchLongSQL) }

func BenchmarkCaseInsensitiveCharStream_LA(b *testing.B) {
	// Cost of the LA() case-fold hot path on its own.
	s := newCaseInsensitiveCharStream("SELECT Foo FROM Bar WHERE Baz = 1")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.LA(1)
		s.Consume()
		if s.LA(1) <= 0 {
			s.Seek(0)
		}
	}
}
