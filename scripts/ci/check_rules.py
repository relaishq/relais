#!/usr/bin/env python3
"""Repository rules that need no third-party Python dependencies."""
import re
from pathlib import Path
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[2]
HELPER = 'internal/redistestenv/env.go'
REDIS_VARS = {'RELAIS_TEST_REDIS_ADDR', 'RELAIS_TEST_REDIS_CLUSTER_ADDRS'}
GO_TOKEN = re.compile(r'//[^\n]*|/\*[\s\S]*?\*/|"(?:\\.|[^"\\])*"|`[^`]*`|\'(?:\\.|[^\'\\])*\'|[A-Za-z_][A-Za-z_0-9]*|[^\s]')
HOME_PATH = re.compile(r'/(?:Users|home)/[^/\s]+/|/private/tmp/|/var/folders/')


def words(name):
    name = re.sub(r'([A-Z]+)([A-Z][a-z])', r'\1 \2', name)
    name = re.sub(r'([a-z0-9])([A-Z])', r'\1 \2', name)
    return re.findall(r'[a-z]+|[0-9]+', name.lower())


def bad_name(name):
    parts = words(name)
    return 'review' in parts or any(
        part in ('round', 'fix') and parts[i + 1].isdigit()
        for i, part in enumerate(parts[:-1]))


def go_tokens(source):
    return [(m.group(), source.count('\n', 0, m.start()) + 1)
            for m in GO_TOKEN.finditer(source) if not m.group().startswith(('//', '/*'))]


def go_names(tokens):
    """Extract declarations; strings and comments cannot create declarations."""
    for i, (token, _) in enumerate(tokens):
        if token == 'func':
            j = i + 1
            if j < len(tokens) and tokens[j][0] == '(':
                depth = 1
                j += 1
                while j < len(tokens) and depth:
                    depth += (tokens[j][0] == '(') - (tokens[j][0] == ')')
                    j += 1
            if j < len(tokens) and re.fullmatch(r'[A-Za-z_]\w*', tokens[j][0]):
                yield tokens[j]
        elif token == 'type' and i + 1 < len(tokens):
            if tokens[i + 1][0] != '(':
                yield tokens[i + 1]
            else:
                # Grouped type declarations start on a new line or after ';',
                # outside the preceding type's (), [] or {} body.
                depth = 0
                previous_line = tokens[i + 1][1]
                previous_token = ';'
                for value, line in tokens[i + 2:]:
                    if value == ')' and depth == 0:
                        break
                    if depth == 0 and (line > previous_line or previous_token == ';') and re.fullmatch(r'[A-Za-z_]\w*', value):
                        yield value, line
                    depth += (value in ('(', '[', '{')) - (value in (')', ']', '}'))
                    previous_line, previous_token = line, value


def source_errors(path, source):
    errors = []
    if bad_name(path):
        errors.append(f'{path}: name describes a process counter; rename it after behaviour')
    if path.endswith('.md'):
        for line, text in enumerate(source.splitlines(), 1):
            if HOME_PATH.search(text):
                errors.append(f'{path}:{line}: local machine path; use a portable placeholder or relative path')
    if path.endswith('.go'):
        tokens = go_tokens(source)
        for name, line in go_names(tokens):
            if bad_name(name):
                errors.append(f'{path}:{line}: {name}: rename the declaration after behaviour')
        # Recognize renamed os imports as well as ordinary os.Getenv calls.
        aliases = {'os'}
        for i, (value, _) in enumerate(tokens):
            if value == '"os"' and i and re.fullmatch(r'[A-Za-z_]\w*', tokens[i - 1][0]):
                aliases.add(tokens[i - 1][0])
        if path != HELPER:
            for i in range(len(tokens) - 4):
                values = [value for value, _ in tokens[i:i + 5]]
                if values[0] in aliases and values[1] == '.' and values[2] in ('Getenv', 'LookupEnv') and values[3] == '(':
                    if values[4].strip('"`') in REDIS_VARS:
                        errors.append(f'{path}:{tokens[i][1]}: direct Redis test endpoint read; use internal/redistestenv')
        # Dot imports are rare but must not bypass the endpoint rule.
        if re.search(r'\.\s*"os"', source) and path != HELPER:
            for i in range(len(tokens) - 2):
                if tokens[i][0] in ('Getenv', 'LookupEnv') and tokens[i + 1][0] == '(' and tokens[i + 2][0].strip('"`') in REDIS_VARS:
                    errors.append(f'{path}:{tokens[i][1]}: direct Redis test endpoint read; use internal/redistestenv')
    if path.startswith('.github/workflows/') and path.endswith(('.yml', '.yaml')):
        errors.extend(workflow_errors(path, source))
    return errors


def workflow_errors(path, source):
    errors = []
    # Inspect YAML port entries (including inline arrays and quoted bindings).
    # A bare container port asks Docker for a random host port and is safe.
    for line, text in enumerate(source.splitlines(), 1):
        text = text.split('#', 1)[0]
        if re.search(r'(?<![\w])0*6379\s*:\s*[0-9]+|\bpublished:\s*[\"\']?0*6379\b', text):
            errors.append(f'{path}:{line}: host Redis port 6379; map a dedicated host port, such as 16379:6379')
    # Include block scalars and comma-separated seed lists; any Redis address
    # on 6379 is forbidden here, even if it is a remote hostname.
    active = False
    indent = 0
    for line, text in enumerate(source.splitlines(), 1):
        clean = text.split('#', 1)[0]
        match = re.search(r'\b(?:RELAIS_[A-Z_]*REDIS[A-Z_]*|REDIS_[A-Z_]*(?:ADDR|URL)[A-Z_]*)\s*[:=]', clean)
        if match:
            active = True
            indent = len(text) - len(text.lstrip())
        elif clean.strip() and len(text) - len(text.lstrip()) <= indent:
            active = False
        if active and re.search(r':0*6379(?:\b|/)', clean):
            errors.append(f'{path}:{line}: Redis address uses 6379; set a dedicated test endpoint')
    return errors


def lint_version_errors(makefile, workflow):
    pin = re.search(r'^GOLANGCI_LINT_VERSION\s*:?=\s*(v[0-9.]+)\s*$', makefile, re.M)
    versions = re.findall(r'uses:\s*golangci/golangci-lint-action@[^\n]+\n\s*with:\s*\n\s*version:\s*[\'\"]?(v[0-9.]+)', workflow)
    if pin is None or not versions or any(version != pin[1] for version in versions):
        return ['lint version mismatch: set golangci-lint-action version to GOLANGCI_LINT_VERSION in Makefile']
    return []


def repository_errors(root=ROOT):
    # Include untracked candidate files so the same command works before staging.
    paths = subprocess.check_output(['git', 'ls-files', '--cached', '--others', '--exclude-standard', '-z'], cwd=root).decode().split('\0')
    errors = []
    for path in sorted(set(paths) - {''}):
        file = root / path
        if not file.is_file():
            continue
        source = file.read_text(errors='replace') if path.endswith(('.go', '.md', '.yml', '.yaml')) else ''
        errors.extend(source_errors(path, source))
    errors.extend(lint_version_errors((root / 'Makefile').read_text(), (root / '.github/workflows/ci.yml').read_text()))
    return errors


def main():
    errors = repository_errors()
    if errors:
        print('\n'.join(errors), file=sys.stderr)
        return 1
    print('Repository rules passed (behaviour names, Redis endpoints, workflow ports, doc paths, lint version).')
    return 0


if __name__ == '__main__':
    sys.exit(main())
