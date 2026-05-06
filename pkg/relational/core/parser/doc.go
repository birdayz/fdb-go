// Package parser houses the generated ANTLR4 lexer and parser for the
// Relational SQL dialect, plus the thin Go wrapper that wires them into a
// ParseTree suitable for consumption by the semantic analyzer.
//
// The generated code lives under gen/. Regenerate with:
//
//	just generate-parser
//
// That recipe is a thin wrapper around the Bazel target
// //pkg/relational/core/parser/grammar:generate_parser. Bazel fetches + caches
// the ANTLR 4.13.1 complete jar as an external repo (@antlr4_tool_jar, pinned
// by sha256 in MODULE.bazel; version must match the antlr4-go/antlr/v4 runtime
// in go.mod). The sh_binary runs the lexer first, then the parser with -lib
// pointing at the lexer's .tokens file, and drops the output into gen/.
//
// CI gates generator drift via `just generate && git diff --exit-code`, so any
// change to the .g4 grammars must be accompanied by a fresh regeneration.
// `just generate-parser` is itself part of `just generate`.
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
