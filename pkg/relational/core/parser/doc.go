// Package parser houses the generated ANTLR4 lexer and parser for the
// Relational SQL dialect, plus the thin Go wrapper that wires them into a
// ParseTree suitable for consumption by the semantic analyzer.
//
// The generated code lives under gen/. Regenerate with:
//
//	just generate-parser
//
// That recipe downloads ANTLR 4.13.2 (matching the antlr4-go/antlr/v4 runtime
// pinned in go.mod), runs the lexer first, then the parser with -lib pointing
// at the lexer's .tokens file, and drops the output into gen/.
//
// The grammar files in grammar/ are copied near-verbatim from Java source at
// fdb-record-layer/fdb-relational-core/src/main/antlr/. There is one
// documented deviation: the Java-side ERROR_RECOGNITION lexer rule contained
// an inline Java action block which ANTLR's Go target copies literally and
// cannot compile. The action is removed in our copy with a NOTE comment
// explaining the change; default error listeners still surface unknown
// characters as "token recognition errors", so behaviour is preserved.
//
// When re-syncing grammar from Java source: copy the files over, then delete
// the action block from the ERROR_RECOGNITION rule again.
package parser
