#!/usr/bin/env python3
"""Repository rules that need no third-party Python dependencies."""
import ast
import re
import shlex
from pathlib import Path
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[2]
HELPER_PACKAGE = 'internal/redistestenv/'
# This test filters child-process environment keys, rather than reading an endpoint.
REDIS_LITERAL_ALLOWLIST = {'internal/redistestcluster/standalone_gate_test.go'}
PROCESS_WORDS = {'review', 'reviews', 'reviewer', 'reviewers', 'reviewed', 'reviewing', 'rereview'}
REDIS_VARS = {'RELAIS_TEST_REDIS_ADDR', 'RELAIS_TEST_REDIS_CLUSTER_ADDRS'}
GO_TOKEN = re.compile(r'//[^\n]*|/\*[\s\S]*?\*/|"(?:\\.|[^"\\])*"|`[^`]*`|\'(?:\\.|[^\'\\])*\'|[^\W\d]\w*|[^\s]')
HOME_PATH = re.compile(r'/(?:Users|home)/(?!\$)[^/\s`\"\'<>]+(?:/|(?=$|[\s`\"\'<>]))|/private/tmp/|/var/folders/')
URL = re.compile(r'\b[a-zA-Z][a-zA-Z0-9+.-]*://[^\s<>`\"\']+')


def words(name):
    name = re.sub(r'([A-Z]+)([A-Z][a-z])', r'\1 \2', name)
    name = re.sub(r'([a-z0-9])([A-Z])', r'\1 \2', name)
    return re.findall(r'[a-z]+|[0-9]+', name.lower())


def bad_name(name):
    parts = words(name)
    return bool(PROCESS_WORDS.intersection(parts)) or any(
        part in ('round', 'fix') and parts[i + 1].isdigit()
        for i, part in enumerate(parts[:-1]))


def go_tokens(source):
    line, previous = 1, 0
    tokens = []
    for match in GO_TOKEN.finditer(source):
        line += source.count('\n', previous, match.start())
        previous = match.start()
        if not match.group().startswith(('//', '/*')):
            tokens.append((match.group(), line))
    return tokens


def go_string(token):
    if token.startswith('`'):
        return token[1:-1].replace('\r', '')
    if token.startswith('"'):
        try:
            return ast.literal_eval(token)
        except (SyntaxError, ValueError):
            pass
    return None


def source_errors(path, source):
    errors = []
    if bad_name(path):
        errors.append(f'{path}: name describes a process counter; rename it after behaviour')
    if path.endswith('.md'):
        for line, text in enumerate(source.splitlines(), 1):
            if HOME_PATH.search(URL.sub('', text)):
                errors.append(f'{path}:{line}: local machine path; use a portable placeholder or relative path')
    if path.endswith('.go'):
        tokens = go_tokens(source)
        aliases = {'os'}
        for i, (token, _) in enumerate(tokens):
            if token == '"os"' and i and re.fullmatch(r'[^\W\d]\w*', tokens[i - 1][0]):
                aliases.add(tokens[i - 1][0])
        if not path.startswith(HELPER_PACKAGE):
            for i in range(len(tokens) - 4):
                values = [token for token, _ in tokens[i:i + 5]]
                if (values[0] in aliases and values[1] == '.' and values[2] in ('Getenv', 'LookupEnv')
                        and values[3] == '(' and go_string(values[4]) in REDIS_VARS):
                    errors.append(f'{path}:{tokens[i][1]}: direct Redis test endpoint read; use internal/redistestenv')
            if any(tokens[i][0] == '.' and tokens[i + 1][0] == '"os"' for i in range(len(tokens) - 1)):
                for i in range(len(tokens) - 2):
                    if (tokens[i][0] in ('Getenv', 'LookupEnv') and tokens[i + 1][0] == '('
                            and go_string(tokens[i + 2][0]) in REDIS_VARS):
                        errors.append(f'{path}:{tokens[i][1]}: direct Redis test endpoint read; use internal/redistestenv')
        # Every lexical identifier counts, including selectors and declarations
        # inside interfaces, parameter lists and function bodies.
        for token, line in tokens:
            if re.fullmatch(r'[^\W\d]\w*', token) and bad_name(token):
                errors.append(f'{path}:{line}: {token}: rename the identifier after behaviour')
            if (not path.startswith(HELPER_PACKAGE) and path not in REDIS_LITERAL_ALLOWLIST
                    and go_string(token) in REDIS_VARS):
                errors.append(f'{path}:{line}: Redis test endpoint literal; use internal/redistestenv')
    if path.startswith('.github/workflows/') and path.endswith(('.yml', '.yaml')):
        errors.extend(workflow_errors(path, source))
    return errors


def strip_yaml_comment(text):
    quote, i = None, 0
    while i < len(text):
        char = text[i]
        if quote:
            if char == "\\" and quote == '"':
                i += 2
                continue
            if char == quote:
                if quote == "'" and i + 1 < len(text) and text[i + 1] == "'":
                    i += 2
                    continue
                quote = None
        elif char in ("'", '"') and (i == 0 or not text[i - 1].isalnum()):
            quote = char
        elif char == '#' and (i == 0 or text[i - 1].isspace()):
            return text[:i]
        i += 1
    return text


def workflow_errors(path, source):
    errors = []
    # Inspect YAML port entries (including inline arrays and quoted bindings).
    # A bare container port asks Docker for a random host port and is safe.
    for line, text in enumerate(source.splitlines(), 1):
        text = strip_yaml_comment(text)
        if re.search(r'(?<![\w])0*6379\s*:\s*[0-9]+|\bpublished:\s*[\"\']?0*6379\b', text):
            errors.append(f'{path}:{line}: host Redis port 6379; map a dedicated host port, such as 16379:6379')
    # Include block scalars and comma-separated seed lists; any Redis address
    # on 6379 is forbidden here, even if it is a remote hostname.
    active = False
    indent = 0
    for line, text in enumerate(source.splitlines(), 1):
        clean = strip_yaml_comment(text)
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
    lines = [strip_yaml_comment(line) for line in workflow.splitlines()]
    versions = []
    for i, line in enumerate(lines):
        if not re.search(r"\buses:\s*['\"]?golangci/golangci-lint-action@", line):
            continue
        indent = len(line) - len(line.lstrip())
        step_indent = indent if line.lstrip().startswith('- ') else max(0, indent - 2)
        version, with_indent, key_indent = None, None, None
        for child in lines[i + 1:]:
            if not child.strip():
                continue
            depth = len(child) - len(child.lstrip())
            if (depth < step_indent or
                    (depth <= step_indent and child.lstrip().startswith('- ')) or
                    (depth <= indent and re.search(r'\buses:', child))):
                break
            if re.match(r'\s*with:\s*$', child):
                with_indent, key_indent = depth, None
                continue
            if with_indent is not None:
                if depth <= with_indent:
                    with_indent = None
                else:
                    if key_indent is None:
                        key_indent = depth
                    match = re.match(r'\s*version:\s*(.*?)\s*$', child)
                    if match and depth == key_indent:
                        version = match[1].strip('\'"')
        versions.append(version)
    if pin is None or not versions or any(version != pin[1] for version in versions):
        return ['lint version mismatch: set every golangci-lint-action version to GOLANGCI_LINT_VERSION in Makefile']
    return []


def release_binary_errors(config, script):
    config = '\n'.join(strip_yaml_comment(line) for line in config.splitlines())
    guard = re.findall(r'\$\{base-path\}/cmd/relais-\{([^}]+)\}/\*\*\.go', config)
    archive = re.search(r'^binaries=\(([^)]*)\)', script, re.M)
    expected = shlex.split(archive[1], comments=True) if archive else []
    actual = ['relais-' + suffix for suffix in guard[0].split(',')] if len(guard) == 1 else []
    if not expected or not actual or set(actual) != set(expected) or len(actual) != len(set(actual)):
        return ['release binary list mismatch: align the anchored cmd depguard glob in .golangci.yml with binaries in scripts/release/build-archives.sh']
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
    errors.extend(release_binary_errors((root / '.golangci.yml').read_text(), (root / 'scripts/release/build-archives.sh').read_text()))
    errors.extend(lint_version_errors((root / 'Makefile').read_text(), (root / '.github/workflows/ci.yml').read_text()))
    return errors


def main():
    errors = repository_errors()
    if errors:
        print('\n'.join(errors), file=sys.stderr)
        return 1
    print('Repository rules passed (behaviour names, Redis endpoints, workflow ports, doc paths, lint version, release binaries).')
    return 0


if __name__ == '__main__':
    sys.exit(main())
