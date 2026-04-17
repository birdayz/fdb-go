// Package parser will house the generated ANTLR4 lexer and parser
// for the Relational SQL dialect, plus the thin Go wrapper that
// wires them into a ParseTree suitable for consumption by the
// semantic analyzer.
//
// Status: grammar files vendored in grammar/; Go parser not yet
// generated. Phase 1 of the relational layer port (see TODO.md)
// wires up generation.
//
// Regenerating the parser:
//
//  1. Install ANTLR 4.13+ (any runtime matching the antlr4-go/antlr/v4
//     module version once added to go.mod).
//
//  2. Run:
//
//     antlr4 -Dlanguage=Go \
//     -package parser \
//     -o gen \
//     pkg/relational/core/parser/grammar/RelationalLexer.g4 \
//     pkg/relational/core/parser/grammar/RelationalParser.g4
//
//  3. Commit the contents of gen/ alongside any grammar change.
//
// The grammar files in grammar/ are copied verbatim from Java
// source at fdb-record-layer/fdb-relational-core/src/main/antlr/.
// DO NOT edit them independently — they MUST stay byte-identical to
// the Java-side grammar so the SQL dialect does not drift.
package parser
