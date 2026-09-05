package updater

import "strings"

// The stack's `.env` is operator-owned: it holds the database password, the
// enrollment token and the operator's comments, and the updater rewrites two of
// its lines. The rule is byte preservation, never round-tripping — nothing is
// parsed into a map and re-serialised, so comments, ordering, CRLF and a
// missing trailing newline all survive.
//
// The dialect matched is compose's own and no wider: `NAME=value`, optional
// leading whitespace, no `export`. Anything else is not a definition of NAME.

// envSplitLines splits content into lines while remembering their terminators,
// so joining them back is byte-identical to the input.
func envSplitLines(content string) (lines []string, terms []string) {
	for len(content) > 0 {
		i := strings.IndexByte(content, '\n')
		if i < 0 {
			lines = append(lines, content)
			terms = append(terms, "")
			return
		}
		line := content[:i]
		term := "\n"
		if strings.HasSuffix(line, "\r") {
			line = line[:len(line)-1]
			term = "\r\n"
		}
		lines = append(lines, line)
		terms = append(terms, term)
		content = content[i+1:]
	}
	return
}

func envJoin(lines, terms []string) string {
	var b strings.Builder
	for i := range lines {
		b.WriteString(lines[i])
		b.WriteString(terms[i])
	}
	return b.String()
}

// envMatch reports whether a line is a definition of name, and the value if so.
func envMatch(line, name string) (string, bool) {
	rest := strings.TrimLeft(line, " \t")
	if !strings.HasPrefix(rest, name) {
		return "", false
	}
	rest = rest[len(name):]
	if !strings.HasPrefix(rest, "=") {
		return "", false
	}
	return rest[1:], true
}

// envLookup returns the EFFECTIVE value of name — the last definition wins,
// which is what compose does — and whether name is defined at all.
func envLookup(content, name string) (string, bool) {
	lines, _ := envSplitLines(content)
	value, found := "", false
	for _, l := range lines {
		if v, ok := envMatch(l, name); ok {
			value, found = v, true
		}
	}
	return value, found
}

// envSet replaces EVERY definition of name with `name=value`, or appends one.
// Every, not just the last: a stale duplicate would make the file disagree with
// itself about which digest is installed. Appending adds a newline first when
// the file lacks one, so an unterminated last line cannot absorb the new var.
func envSet(content, name, value string) string {
	lines, terms := envSplitLines(content)
	replaced := false
	for i, l := range lines {
		if _, ok := envMatch(l, name); ok {
			lines[i] = name + "=" + value
			if terms[i] == "" {
				terms[i] = "\n"
			}
			replaced = true
		}
	}
	if replaced {
		return envJoin(lines, terms)
	}
	out := content
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return out + name + "=" + value + "\n"
}
