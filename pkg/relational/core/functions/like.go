package functions

// LikeMatch implements SQL LIKE pattern matching: % = any sequence,
// _ = any single char. If escape >= 0, that rune makes the following
// char literal (only valid before %, _, or escape itself per SQL
// standard; we accept any literal char after escape, matching Java's
// lenient interpretation).
func LikeMatch(pattern, s string, escape rune) bool {
	if pattern == "" {
		return s == ""
	}
	p, str := []rune(pattern), []rune(s)
	return likeMatchRunes(p, str, escape)
}

func likeMatchRunes(p, s []rune, escape rune) bool {
	for len(p) > 0 {
		// Escape handling: consume the escape char and treat the next char
		// as a literal. SQL: escape must precede %, _, or itself; otherwise
		// undefined. Match Java's lenient interpretation (just literal char).
		if escape >= 0 && p[0] == escape && len(p) >= 2 {
			if len(s) == 0 || p[1] != s[0] {
				return false
			}
			p, s = p[2:], s[1:]
			continue
		}
		switch p[0] {
		case '%':
			// skip consecutive %
			for len(p) > 0 && p[0] == '%' {
				p = p[1:]
			}
			if len(p) == 0 {
				return true
			}
			for i := 0; i <= len(s); i++ {
				if likeMatchRunes(p, s[i:], escape) {
					return true
				}
			}
			return false
		case '_':
			if len(s) == 0 {
				return false
			}
			p, s = p[1:], s[1:]
		default:
			if len(s) == 0 || p[0] != s[0] {
				return false
			}
			p, s = p[1:], s[1:]
		}
	}
	return len(s) == 0
}
