package antlrgen

import "github.com/antlr4-go/antlr/v4"

// Hand-written; generate_parser.sh preserves this file across regeneration.

// NewRelationalLexerATN deserializes a private copy of the lexer's ATN. The
// ANTLR runtime takes the ATN's locks for every DFA edge it adds, so lexers
// sharing one ATN serialize on them; a copy per prediction-state lease does not.
func NewRelationalLexerATN() *antlr.ATN {
	RelationalLexerInit()
	return antlr.NewATNDeserializer(nil).Deserialize(RelationalLexerLexerStaticData.serializedATN)
}

// NewRelationalParserATN is NewRelationalLexerATN for the parser.
func NewRelationalParserATN() *antlr.ATN {
	RelationalParserInit()
	return antlr.NewATNDeserializer(nil).Deserialize(RelationalParserParserStaticData.serializedATN)
}
