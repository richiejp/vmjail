package devcontainer

// devcontainer.json is JSON-with-comments (jsonc): it permits // line
// comments, /* block */ comments, and trailing commas before a closing
// brace or bracket — none of which encoding/json accepts. The two
// passes below normalise a jsonc byte slice into strict JSON so the
// standard decoder can take over.
//
// Both passes are string-aware: a // or /* inside a JSON string (e.g.
// the "https://…" in a comment-pointing URL, or a ",}" inside a value)
// must survive untouched. Tracking the in-string state with escape
// handling is what makes that correct where a naive regex would not be.

// stripJSONC returns src with comments and trailing commas removed,
// yielding strict JSON. It never reorders or rewrites string contents.
func stripJSONC(src []byte) []byte {
	return stripTrailingCommas(stripComments(src))
}

// stripComments removes // line comments and /* block */ comments,
// preserving newlines so byte offsets in decoder errors stay roughly
// aligned with the source lines.
func stripComments(src []byte) []byte {
	out := make([]byte, 0, len(src))
	inStr := false
	for i := 0; i < len(src); i++ {
		c := src[i]
		if inStr {
			out = append(out, c)
			// A backslash escapes the next byte (incl. an escaped quote),
			// so copy it verbatim and skip the escaped char.
			if c == '\\' && i+1 < len(src) {
				out = append(out, src[i+1])
				i++
				continue
			}
			if c == '"' {
				inStr = false
			}
			continue
		}
		switch {
		case c == '"':
			inStr = true
			out = append(out, c)
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
			if i < len(src) {
				out = append(out, '\n') // keep the line break
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			i += 2
			for i+1 < len(src) && (src[i] != '*' || src[i+1] != '/') {
				i++
			}
			i++ // land on the '/' of "*/"; the loop's i++ steps past it
		default:
			out = append(out, c)
		}
	}
	return out
}

// stripTrailingCommas drops a comma whose next significant byte is a
// closing '}' or ']'. Runs after stripComments so the only thing
// between a trailing comma and the brace is whitespace.
func stripTrailingCommas(src []byte) []byte {
	out := make([]byte, 0, len(src))
	inStr := false
	for i := 0; i < len(src); i++ {
		c := src[i]
		if inStr {
			out = append(out, c)
			if c == '\\' && i+1 < len(src) {
				out = append(out, src[i+1])
				i++
				continue
			}
			if c == '"' {
				inStr = false
			}
			continue
		}
		if c == '"' {
			inStr = true
			out = append(out, c)
			continue
		}
		if c == ',' && nextSignificantIsClose(src, i+1) {
			continue // drop the trailing comma
		}
		out = append(out, c)
	}
	return out
}

// nextSignificantIsClose reports whether the first non-whitespace byte
// at or after i is a closing brace or bracket.
func nextSignificantIsClose(src []byte, i int) bool {
	for i < len(src) {
		switch src[i] {
		case ' ', '\t', '\n', '\r':
			i++
		case '}', ']':
			return true
		default:
			return false
		}
	}
	return false
}
