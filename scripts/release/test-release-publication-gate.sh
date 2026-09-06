#!/usr/bin/env bash
# Offline dependency-graph regression: a release must not advertise an updater
# image before that image has passed validation and its version tag exists.
set -euo pipefail
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
python3 - "$repo/.github/workflows/images.yml" <<'PY'
import copy
import re
import sys
from pathlib import Path

text = Path(sys.argv[1]).read_text()
# Parse only job IDs and their literal needs lists, supporting both YAML forms
# used by this workflow. Unknown/dynamic needs fail rather than silently pass.
blocks = dict(re.findall(r'^  ([a-z][a-z0-9-]*):\n(.*?)(?=^  [a-z][a-z0-9-]*:\n|\Z)', text, re.M | re.S))
graph = {}
for job, body in blocks.items():
    match = re.search(r'^    needs:\s*(\[[^\n]*\]|[a-z][a-z0-9-]*|\n(?:      - [a-z][a-z0-9-]*\n)+)', body, re.M)
    if not match:
        assert not re.search(r'^    needs:', body, re.M), f'unrecognized needs syntax: {job}'
        graph[job] = set()
        continue
    value = match.group(1)
    graph[job] = set(re.findall(r'[a-z][a-z0-9-]*', value))

def validate(dependencies):
    seen = set()
    def visit(job):
        if job in seen:
            return
        assert job in dependencies, f'unknown prerequisite {job}'
        seen.add(job)
        for prerequisite in dependencies[job]:
            visit(prerequisite)
    visit('release')
    required = {'release-gate', 'preflight', 'promote', 'promote-updater',
                'validate-control-plane', 'validate-node-agent', 'validate-updater'}
    assert required <= seen, f'publication bypasses required gates: {sorted(required - seen)}'
    for job in required | {'release'}:
        assert not re.search(r'^    (?:if:.*(?:always\(|failure\(|cancelled\()|continue-on-error:\s*true)', blocks[job], re.M), f'{job} bypasses successful prerequisites'

validate(graph)
# Prove the guard catches the actual pre-fix publication race and its validation
# equivalent, rather than merely reading a keyword somewhere in the workflow.
for job, edge in [('release', 'promote-updater'), ('promote-updater', 'validate-updater')]:
    changed = copy.deepcopy(graph)
    changed[job].discard(edge)
    try:
        validate(changed)
    except AssertionError:
        continue
    raise AssertionError(f'guard failed to reject removed dependency {job} -> {edge}')
print('PASS release publication waits for all three validated/promoted runtime images')
PY
