// Package grammar is a go-sentinel (empty) package whose only job is
// to anchor the *.g4 grammar files in the Go module tree so they
// travel with builds and gazelle doesn't delete the directory.
//
// The .g4 files are NOT Go source. They are copied verbatim from
// fdb-record-layer/fdb-relational-core/src/main/antlr/ and consumed
// by the ANTLR code generator to produce pkg/relational/core/parser/gen/.
//
// Grammar sync: whenever the Java-side grammar changes, copy the new
// files here and regenerate (see ../doc.go).
package grammar
