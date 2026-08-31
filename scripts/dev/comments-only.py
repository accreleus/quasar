#!/usr/bin/env python3
"""Assert a diff touches only comments and blank lines.

Usage: comments-only.py <base-ref> [-- <pathspec>...]

Exit 0 when no hunk in `git diff <base-ref>` changes code, 1 otherwise (naming each
offending file:line). Used to gate comment-compression commits: a code change
slipping into one is invisible in a 2000-line diff.

A hunk passes when both its sides carry the same code once comments are stripped and
whitespace is normalized. Per-LINE classification is not enough: editing a trailing
comment rewrites its code line, and gofmt then realigns neighbouring ones.

Each side is classified by scanning the WHOLE file from line 1, never by replaying
the hunk. A -U0 hunk inside a `/** ... */` run carries no local evidence that its
lines are comments.
"""

import re
import subprocess
import sys

LINE_MARKERS = {
    ".go": ("//",),
    ".rs": ("//", "///", "//!"),
    ".ts": ("//",),
    ".tsx": ("//",),
    ".js": ("//",),
    ".jsx": ("//",),
    ".mjs": ("//",),
    ".css": (),
    ".py": ("#",),
    ".sh": ("#",),
    ".yaml": ("#",),
    ".yml": ("#",),
    ".toml": ("#",),
    ".sql": ("--",),
    ".md": (),
    ".html": (),
}

BLOCK_DELIMS = {
    ".go": [("/*", "*/")],
    ".rs": [("/*", "*/")],
    ".ts": [("/*", "*/")],
    ".tsx": [("/*", "*/")],
    ".js": [("/*", "*/")],
    ".jsx": [("/*", "*/")],
    ".mjs": [("/*", "*/")],
    ".css": [("/*", "*/")],
    ".html": [("<!--", "-->")],
    ".md": [("<!--", "-->")],
    ".sql": [("/*", "*/")],
}

# `{/* ... */}` — a JSX comment expression on its own line.
JSX_COMMENT = re.compile(r"^\s*\{\s*/\*.*\*/\s*\}\s*$", re.DOTALL)
# Opening line of a multi-line JSX comment: `{/*` with no code before or after.
JSX_COMMENT_OPEN = re.compile(r"^\s*\{\s*/\*")
JSX_EXTS = {".tsx", ".jsx"}


def ext_of(path):
    dot = path.rfind(".")
    return path[dot:] if dot > path.rfind("/") else ""


class Scanner:
    """Replays one side of a file, tracking whether we are inside a block comment."""

    def __init__(self, ext):
        self.markers = LINE_MARKERS.get(ext, ())
        self.blocks = BLOCK_DELIMS.get(ext, [])
        if ext in JSX_EXTS:
            # Multi-line `{/* ... */}` JSX comment blocks close with `*/}`.
            # Listed before plain `/* ... */` so the longer opener wins.
            self.blocks = [("{/*", "*/}")] + self.blocks
        self.open_delim = None

    def is_comment(self, raw):
        """Classify `raw`, then advance block state past it."""
        line = raw.strip()
        if self.open_delim is not None:
            close = self.open_delim[1]
            if close in line:
                self.open_delim = None
                # Trailing code after the closing delimiter is code.
                return line.split(close, 1)[1].strip() == ""
            return True
        if line == "":
            return True
        if JSX_COMMENT.match(line):
            return True
        if any(line.startswith(m) for m in self.markers):
            return True
        for start, close in self.blocks:
            if line.startswith(start):
                rest = line[len(start):]
                if close in rest:
                    return rest.split(close, 1)[1].strip() == ""
                self.open_delim = (start, close)
                return True
        return False


def blob(ref, path):
    r = subprocess.run(["git", "show", f"{ref}:{path}"], capture_output=True, text=True)
    return r.stdout if r.returncode == 0 else ""


QUOTES = {'"': '"', "'": "'", "`": "`"}


def strip_trailing_comment(line, ext):
    """Drop a trailing comment, respecting string literals.

    `url := "http://x" // note` keeps the URL's `//` and drops only the note.
    """
    markers = [m for m in LINE_MARKERS.get(ext, ())] + [
        s for s, _ in BLOCK_DELIMS.get(ext, [])
    ]
    i = 0
    quote = None
    while i < len(line):
        ch = line[i]
        if quote:
            if ch == "\\":
                i += 2
                continue
            if ch == quote:
                quote = None
            i += 1
            continue
        if ch in QUOTES:
            quote = ch
            i += 1
            continue
        for m in markers:
            if line.startswith(m, i):
                return line[:i]
        i += 1
    return line


def code_lines(text, ext):
    """1-indexed map: line number -> its code content, normalized ('' if none)."""
    scanner = Scanner(ext)
    out = {}
    for i, line in enumerate(text.splitlines(), 1):
        if scanner.is_comment(line):
            out[i] = ""
        else:
            out[i] = " ".join(strip_trailing_comment(line, ext).split())
    return out


# Kept as the classifier the counting/reporting path uses.
def classify(text, ext):
    """1-indexed map: line number -> is-comment-or-blank."""
    scanner = Scanner(ext)
    return {i: scanner.is_comment(line) for i, line in enumerate(text.splitlines(), 1)}


def check_hunk(path, old_code, new_code, dels, adds, violations):
    """A hunk passes when its two sides carry identical code, comments aside."""
    left = [c for c in (old_code.get(n, "") for n, _ in dels) if c]
    right = [c for c in (new_code.get(n, "") for n, _ in adds) if c]
    if left == right:
        return
    # Report the specific lines that differ, not the whole hunk.
    for n, text in dels:
        if old_code.get(n, "") and old_code.get(n, "") not in right:
            violations.append(f"{path}:{n}: - {text.strip()[:100]}")
    for n, text in adds:
        if new_code.get(n, "") and new_code.get(n, "") not in left:
            violations.append(f"{path}:{n}: + {text.strip()[:100]}")


def main():
    if len(sys.argv) < 2:
        print("usage: comments-only.py <base-ref> [-- <pathspec>...]", file=sys.stderr)
        return 2
    base = sys.argv[1]
    pathspec = [p for p in sys.argv[2:] if p != "--"]
    cmd = ["git", "diff", "-U0", base, "--"] + pathspec
    diff = subprocess.run(cmd, capture_output=True, text=True, check=True).stdout

    violations = []
    path = None
    new_code = old_code = {}
    lineno_add = lineno_del = 0
    dels, adds = [], []

    def flush():
        if path and (dels or adds):
            check_hunk(path, old_code, new_code, dels, adds, violations)
        dels.clear()
        adds.clear()

    for raw in diff.splitlines():
        if raw.startswith("--- a/"):
            flush()
            old_path = raw[6:]
            old_code = code_lines(blob(base, old_path), ext_of(old_path))
            continue
        if raw.startswith("+++ b/"):
            path = raw[6:]
            try:
                with open(path, encoding="utf-8") as fh:
                    new_code = code_lines(fh.read(), ext_of(path))
            except OSError:
                new_code = {}
            continue
        if raw.startswith("@@"):
            flush()
            m = re.match(r"@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@", raw)
            if m:
                lineno_del, lineno_add = int(m.group(1)), int(m.group(2))
            continue
        if path is None or raw.startswith("---") or raw.startswith("+++"):
            continue
        if raw.startswith("+"):
            adds.append((lineno_add, raw[1:]))
            lineno_add += 1
        elif raw.startswith("-"):
            dels.append((lineno_del, raw[1:]))
            lineno_del += 1
    flush()

    if violations:
        print(f"comments-only: {len(violations)} code-touching line(s) vs {base}:")
        for v in violations:
            print("  " + v)
        return 1
    print(f"comments-only: clean vs {base} (comments and blank lines only)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
