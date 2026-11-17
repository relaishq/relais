#!/usr/bin/env python3
"""Decide whether a pull request changes only documentation that no heavy CI job reads.

Prints docs_only=true or docs_only=false. True needs a pull_request event, a
base SHA, a non-empty local git diff, and only content edits or additions of
regular Markdown files under docs/ or at the root, outside the fallback list.
Other files (code, images) and Markdown in other directories may be compiled,
embedded or read by code, so they take the full path. Anything else,
including any error, prints false.
"""
import argparse
from pathlib import Path
import subprocess
import sys

FALLBACK_FILE = Path(__file__).resolve().with_name('docs-only-fallback.txt')
NO_COMMIT = '0' * 40
REGULAR = '100644'


def load_fallback(path=FALLBACK_FILE):
    """Read plain repository paths; only a leading **/ may use a wildcard."""
    lines = (line.split('#', 1)[0].strip() for line in path.read_text().splitlines())
    entries = [line for line in lines if line]
    for entry in entries:
        name = entry[3:] if entry.startswith('**/') else entry
        if not name.strip('/') or set('*?[]\\') & set(name) or name.startswith(('/', './')) or '/../' in '/' + name:
            raise ValueError(f'fallback entry {entry!r}: use a plain path, a directory or **/<path>')
    return entries


def in_fallback(path, fallback):
    """An entry covers its exact path and everything below it; **/ matches at any depth."""
    for entry in fallback:
        candidates = [path]
        if entry.startswith('**/'):
            entry = entry[3:]
            candidates += [path[i + 1:] for i, char in enumerate(path) if char == '/']
        entry = entry.rstrip('/')
        if any(c == entry or c.startswith(entry + '/') for c in candidates):
            return True
    return False


def docs_candidate(path):
    """Paths the short path may accept: Markdown under docs/ or at the root."""
    return path.endswith('.md') and (path.startswith('docs/') or '/' not in path)


def git_changes(base, head, cwd=None):
    """Return (status, old mode, new mode, path) for each change in the local diff."""
    out = subprocess.run(['git', 'diff', '--raw', '-z', '--no-renames', base, head],
                         check=True, capture_output=True, cwd=cwd).stdout.decode()
    fields = out.split('\0')[:-1]
    if len(fields) % 2:
        raise ValueError('unexpected git diff --raw output')
    changes = []
    for meta, path in zip(fields[::2], fields[1::2]):
        old_mode, new_mode, _, _, status = meta.lstrip(':').split(' ')
        changes.append((status, old_mode, new_mode, path))
    return changes


def full_reason(changes, fallback):
    """Return why these changes need the full path, or None when docs-only."""
    if not changes:
        return 'empty diff'
    for status, old_mode, new_mode, path in changes:
        plain_edit = status == 'M' and old_mode == new_mode == REGULAR
        plain_add = status == 'A' and new_mode == REGULAR
        if not (plain_edit or plain_add):
            return f'{path}: {status} {old_mode}->{new_mode} is not a plain content edit or addition'
        if not docs_candidate(path):
            return f'{path}: not Markdown under docs/ or at the root'
        if in_fallback(path, fallback):
            return f'{path}: in the fallback list'
    return None


def classify(event, base, head, changes=git_changes, fallback=load_fallback):
    if event != 'pull_request':
        return f'{event or "unknown"} event'
    if not base or base == NO_COMMIT:
        return 'no base SHA'
    try:
        return full_reason(changes(base, head), fallback())
    except Exception as error:  # Any classifier failure must select the full path.
        return f'classifier error: {error}'


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--event', default='')
    parser.add_argument('--base', default='')
    parser.add_argument('--head', default='HEAD')
    parser.add_argument('--fallback', type=Path, default=FALLBACK_FILE)
    args = parser.parse_args(argv)
    reason = classify(args.event, args.base, args.head, fallback=lambda: load_fallback(args.fallback))
    print(f'full path: {reason}' if reason else 'docs-only path', file=sys.stderr)
    print(f'docs_only={"false" if reason else "true"}')
    return 0


if __name__ == '__main__':
    sys.exit(main())
