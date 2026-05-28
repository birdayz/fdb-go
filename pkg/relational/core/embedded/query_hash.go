package embedded

import (
	"strings"
	"unicode"
)

// normalizeSQL strips comments, collapses whitespace, uppercases
// characters outside single-quoted string literals, and trims.
func normalizeSQL(sql string) string {
	sql = stripComments(sql)
	sql = collapseWhitespace(sql)
	sql = upperOutsideStrings(sql)
	sql = strings.TrimSpace(sql)
	return sql
}

// upperOutsideStrings uppercases only characters outside single-quoted
// string literals, preserving the exact case of literal values.
func upperOutsideStrings(sql string) string {
	var b strings.Builder
	b.Grow(len(sql))
	inString := false
	for i := 0; i < len(sql); i++ {
		ch := sql[i]
		if inString {
			b.WriteByte(ch)
			if ch == '\'' {
				// Escaped quote ('') stays inside the string.
				if i+1 < len(sql) && sql[i+1] == '\'' {
					b.WriteByte(sql[i+1])
					i++
					continue
				}
				inString = false
			}
		} else {
			if ch == '\'' {
				inString = true
				b.WriteByte(ch)
			} else {
				b.WriteRune(unicode.ToUpper(rune(ch)))
			}
		}
	}
	return b.String()
}

// stripComments removes single-line (--) and block (/* */) comments.
func stripComments(sql string) string {
	var b strings.Builder
	b.Grow(len(sql))

	i := 0
	for i < len(sql) {
		// Block comment: /* ... */
		if i+1 < len(sql) && sql[i] == '/' && sql[i+1] == '*' {
			i += 2
			for i+1 < len(sql) {
				if sql[i] == '*' && sql[i+1] == '/' {
					i += 2
					break
				}
				i++
			}
			// If we ran off the end without finding */, just stop.
			if i >= len(sql) {
				break
			}
			// Replace the comment with a space so tokens don't merge.
			b.WriteByte(' ')
			continue
		}

		// Single-line comment: -- to end of line
		if i+1 < len(sql) && sql[i] == '-' && sql[i+1] == '-' {
			i += 2
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
			// Replace the comment with a space so tokens don't merge.
			b.WriteByte(' ')
			continue
		}

		// String literal: don't strip comments inside quotes.
		if sql[i] == '\'' {
			b.WriteByte(sql[i])
			i++
			for i < len(sql) {
				if sql[i] == '\'' {
					b.WriteByte(sql[i])
					i++
					// Escaped single quote ('')
					if i < len(sql) && sql[i] == '\'' {
						b.WriteByte(sql[i])
						i++
						continue
					}
					break
				}
				b.WriteByte(sql[i])
				i++
			}
			continue
		}

		b.WriteByte(sql[i])
		i++
	}

	return b.String()
}

// collapseWhitespace replaces runs of whitespace with a single space.
func collapseWhitespace(sql string) string {
	var b strings.Builder
	b.Grow(len(sql))

	inSpace := false
	for _, r := range sql {
		if unicode.IsSpace(r) {
			if !inSpace {
				b.WriteByte(' ')
				inSpace = true
			}
			continue
		}
		inSpace = false
		b.WriteRune(r)
	}

	return b.String()
}
