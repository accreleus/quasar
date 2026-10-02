#!/usr/bin/env python3
"""scripts/dx/bench_config.py — find the bench server and key the way qbench does.

Every repo script that talks to quasar-bench resolves its server and key here,
before it constructs `Bench()`, so a machine set up for `qbench` (the server's
install.sh writes `~/.config/qbench/url`, the operator puts the key in
`~/.config/qbench/key`) needs no extra exports:

    from bench_config import bench_env, bench_url
    bench_env()                              # fill BENCH_URL / BENCH_KEY if unset
    b = Bench(bench_url(args.url), args.key)

Resolution, identical to qbench's own:
  server  --url flag, else $BENCH_URL, else $XDG_CONFIG_HOME/qbench/url
          (XDG_CONFIG_HOME defaults to ~/.config)
  key     --key flag, else $BENCH_KEY, else $XDG_CONFIG_HOME/qbench/key — which is
          refused (a warning, and the key stays unset) unless its mode is 600 or
          stricter

There is deliberately NO default server. The vendored client's own fallback
(`bench.DEFAULT_URL`, localhost) is upstream's; a script that reaches it has
silently posted nowhere useful, so `bench_url()` stops with a message instead.
Nothing here ever prints the URL or the key.

    python3 scripts/dx/bench_config.py            # exit 0 if server + key resolve
                                                  # (prints only WHERE each came from)
"""

from __future__ import annotations

import os
import stat
import sys

NO_SERVER = ("no bench server configured: set BENCH_URL, or run `qbench doctor` "
             "(the bench server's install.sh records it in %s)")


def config_dir() -> str:
    base = os.environ.get("XDG_CONFIG_HOME") or os.path.join(os.path.expanduser("~"), ".config")
    return os.path.join(base, "qbench")


def _read(path: str) -> str:
    try:
        with open(path) as fh:
            return fh.read().strip()
    except OSError:
        return ""


def _key_from_file(path: str) -> str:
    try:
        mode = stat.S_IMODE(os.stat(path).st_mode)
    except OSError:
        return ""
    if mode & 0o077:
        print("WARN  bench key file %s has mode %03o; it must not be readable by others "
              "(chmod 600 %s) — not using it" % (path, mode, path), file=sys.stderr)
        return ""
    return _read(path)


def bench_env(environ=None) -> dict:
    """Fill BENCH_URL / BENCH_KEY from qbench's config files, only where unset.

    Returns {"url": source, "key": source} naming where each value came from
    ("BENCH_URL", a file path, or "none") — never the values themselves.
    """
    env = os.environ if environ is None else environ
    where = {"url": "none", "key": "none"}
    if env.get("BENCH_URL"):
        where["url"] = "BENCH_URL"
    else:
        path = os.path.join(config_dir(), "url")
        url = _read(path)
        if url:
            env["BENCH_URL"] = url.rstrip("/")
            where["url"] = path
    if env.get("BENCH_KEY"):
        where["key"] = "BENCH_KEY"
    else:
        path = os.path.join(config_dir(), "key")
        key = _key_from_file(path)
        if key:
            env["BENCH_KEY"] = key
            where["key"] = path
    return where


def bench_url(flag: str | None = None) -> str:
    """The server to talk to: --url, else $BENCH_URL (after bench_env()).

    Exits 2 with a next-step message when neither is set, rather than letting
    the vendored client fall back to its localhost default.
    """
    url = flag or os.environ.get("BENCH_URL") or ""
    if not url:
        print("error: " + NO_SERVER % os.path.join(config_dir(), "url"), file=sys.stderr)
        sys.exit(2)
    return url.rstrip("/")


def main() -> int:
    where = bench_env()
    print("url   %s" % where["url"])
    print("key   %s" % where["key"])
    return 0 if where["url"] != "none" and where["key"] != "none" else 1


if __name__ == "__main__":
    sys.exit(main())
