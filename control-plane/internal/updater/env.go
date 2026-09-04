package updater

import "strings"

// The stack's `.env` is an OPERATOR-OWNED file. It carries the database
// password, the enrollment token, every tuning knob and the operator's own
// comments, and the updater rewrites exactly two of its lines. So the rule here
// is byte preservation, not round-tripping: nothing is parsed into a map and
// re-serialised. Lines are found, the matched one is replaced in place, and
// every other byte — comments, blank lines, ordering, CRLF, a missing trailing
// newline — comes out unchanged.
//
// The dialect matched is compose's own, which is deliberately narrow:
// `NAME=value`, optional leading whitespace, no `export`, value to end of line.
// Anything else is not a definition of NAME and is left alone.

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

// envSet replaces every definition of name with `name=value`, or appends one
// when there is none.
//
// EVERY definition, not just the last: a duplicate is already pathological, and
// leaving a stale one behind would make the file disagree with itself about
// which digest is installed — the exact confusion `.env.prev` exists to avoid.
// Replacement is in place, so a duplicate's position is preserved too.
//
// Appending adds a newline first when the file does not end with one, so a
// `.env` whose last line has no terminator does not silently absorb the new
// variable into it.
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
