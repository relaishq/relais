#!/usr/bin/env python3
"""Repository rules that need no third-party Python dependencies."""
import ast
import fnmatch
import posixpath
import re
import shlex
from pathlib import Path
import subprocess
import sys

# Isolated mode (-I) leaves this directory off sys.path; the fallback list
# matcher lives beside this file.
sys.path.insert(0, str(Path(__file__).resolve().parent))
from docs_only import docs_candidate, in_fallback, load_fallback  # noqa: E402

ROOT = Path(__file__).resolve().parents[2]
HELPER_PACKAGE = 'internal/redistestenv/'
# This test filters child-process environment keys, rather than reading an endpoint.
REDIS_LITERAL_ALLOWLIST = {'internal/redistestcluster/standalone_gate_test.go'}
PROCESS_WORDS = {'review', 'reviews', 'reviewer', 'reviewers', 'reviewed', 'reviewing', 'rereview'}
REDIS_VARS = {'RELAIS_TEST_REDIS_ADDR', 'RELAIS_TEST_REDIS_CLUSTER_ADDRS'}
GO_TOKEN = re.compile(r'//[^\n]*|/\*[\s\S]*?\*/|"(?:\\.|[^"\\])*"|`[^`]*`|\'(?:\\.|[^\'\\])*\'|[^\W\d]\w*|[^\s]')
HOME_PATH = re.compile(r'/(?:Users|home)/(?!\$)[^/\s`\"\'<>]+(?:/|(?=$|[\s`\"\'<>]))|/private/tmp/|/var/folders/')
FLOATING_RUNNER = re.compile(r'(?<![\w.-])(?:ubuntu|macos|windows)-latest(?:-[\w-]+)?(?![\w.-])', re.I)
URL = re.compile(r'\b[a-zA-Z][a-zA-Z0-9+.-]*://[^\s<>`\"\']+')
# Code, build files and test fixtures that could read docs/ or root Markdown.
# A docs-only PR skips the heavy jobs that run most of them.
READER_FILE = re.compile(r'(?:^|/)(?:[^/]+\.(?:go|py|sh|js|cjs|mjs|ts)|Makefile|testdata/.*)$|^\.github/workflows/[^/]+$')
# Readers that may name docs freely, with the reason each is safe.
READER_ALLOWLIST = {
    'scripts/ci/docs_only.py': 'the docs-only classifier itself; runs in changes, which always runs',
    'scripts/ci/check_rules.py': 'reads every Markdown file for repository rules; runs in quality, which always runs',
    'scripts/ci/test_check_rules.py': 'synthetic doc paths as rule inputs; runs in quality, which always runs',
    'scripts/ci/test_docs_only.py': 'synthetic doc paths as classifier inputs; runs in quality, which always runs',
}
# A ./ or ../-relative docs/ path or root-level Markdown name, including globs
# such as docs/d?mo.md, docs/[d]emo.md and [^R]*.md.
GLOB_CHAR = r'(?:[\w.*?-]|\[[^\]/\s]+\])'
DOC_PATH = re.compile(rf'(?<![\w./-])(?:\./)?(?:\.\./)*(docs/(?:/|{GLOB_CHAR})*|{GLOB_CHAR}+?\.md(?![\w.-]))')
# A quoted docs path component, as in filepath.Join("..", "docs") or Path(root) / 'docs'.
DOC_COMPONENT = re.compile(r'''(["'`])(?:\./)?(?:\.\./)*docs/?\1''')
GO_EMBED = re.compile(r'^\s*//go:embed\s+(.+)$')


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


def go_pattern_regex(pattern):
    """Compile Go path.Match syntax: * and ? skip /, [lo-hi] and [^...] classes, \ escapes.

    ! is a literal inside a class. A malformed pattern raises ValueError."""
    out, i, n = [], 0, len(pattern)

    def escaped():
        nonlocal i
        if i >= n or pattern[i] in '-]':
            raise ValueError(f'bad pattern {pattern!r}')
        if pattern[i] == '\\':
            i += 1
            if i >= n:
                raise ValueError(f'bad pattern {pattern!r}')
        i += 1
        return pattern[i - 1]

    while i < n:
        char = pattern[i]
        i += 1
        if char == '*':
            out.append('[^/]*')
        elif char == '?':
            out.append('[^/]')
        elif char == '\\':
            if i >= n:
                raise ValueError(f'bad pattern {pattern!r}')
            out.append(re.escape(pattern[i]))
            i += 1
        elif char == '[':
            negate = i < n and pattern[i] == '^'
            i += negate
            ranges = []
            while not (i < n and pattern[i] == ']' and ranges):
                low = high = escaped()
                if i < n and pattern[i] == '-':
                    i += 1
                    high = escaped()
                ranges.append((low, high))
            i += 1
            body = ''.join(re.escape(low) + '-' + re.escape(high) for low, high in ranges if low <= high)
            out.append(('[^' + body + ']' if body else '(?s:.)') if negate else ('[' + body + ']' if body else '(?!)'))
        else:
            out.append(re.escape(char))
    return re.compile(''.join(out), re.S)


def matched_files(pattern, files, go_only):
    """Tracked files a path pattern reaches; a matched directory includes its files.

    go:embed uses Go syntax only. Other literals may be shell, Python or JS
    globs, so they also match with fnmatch, where [!...] negates."""
    pattern = pattern.strip('/')
    tests = [go_pattern_regex(pattern).fullmatch]
    if not go_only:
        tests.append(re.compile(fnmatch.translate(re.sub(r'\[\^', '[!', pattern))).match)
    matches = []
    for file in files:
        segments = file.split('/')
        prefixes = ('/'.join(segments[:i]) for i in range(1, len(segments) + 1))
        if any(test(prefix) for prefix in prefixes for test in tests):
            matches.append(file)
    return matches


def go_embed_patterns(text):
    """Split go:embed arguments like cmd/go: spaces, "quoted" Go strings and `raw` strings.

    Go also separates arguments with Unicode spaces. Rather than model them,
    any non-ASCII character fails closed as an unresolvable pattern.
    """
    if not text.isascii():
        raise ValueError(f'non-ASCII go:embed argument {text!r}')
    patterns, i = [], 0
    while True:
        while i < len(text) and text[i] in ' \t':
            i += 1
        if i == len(text):
            return patterns
        if text[i] == '`':
            end = text.find('`', i + 1)
            if end < 0:
                raise ValueError(f'bad go:embed argument {text[i:]!r}')
            patterns.append(text[i + 1:end])
            i = end + 1
        elif text[i] == '"':
            match = re.match(r'"(?:\\.|[^"\\])*"', text[i:])
            value = go_string(match[0]) if match else None
            if value is None:
                raise ValueError(f'bad go:embed argument {text[i:]!r}')
            patterns.append(value)
            i += match.end()
        else:
            end = i
            while end < len(text) and text[end] not in ' \t':
                end += 1
            patterns.append(text[i:end])
            i = end


def doc_references(path, source):
    """Yield (line, pattern, kind) for each path pattern the source may read.

    kind is 'embed' (Go syntax), 'literal' (any glob dialect) or 'bad' (unparsed go:embed)."""
    if path.endswith('.go'):
        # Only string literals read files; comments that cite docs do not.
        texts = [(line, token) for token, line in go_tokens(source) if token[:1] in '"`']
        base = posixpath.dirname(path)
        for line, text in enumerate(source.splitlines(), 1):
            embed = GO_EMBED.match(text)
            if embed:
                try:
                    patterns = go_embed_patterns(embed[1].strip())
                except ValueError:
                    yield line, embed[1].strip(), 'bad'
                    continue
                for pattern in patterns:
                    yield line, posixpath.join(base, pattern.removeprefix('all:')), 'embed'
    else:
        texts = [(line, text) for line, text in enumerate(source.splitlines(), 1)
                 if not text.lstrip().startswith(('#', '//', '/*', '* '))]
    for line, text in texts:
        for match in DOC_PATH.finditer(text):
            reference = match[1].rstrip('.')
            yield line, reference, 'literal'
            if '?' in reference:  # Also read ? as the start of a URL query.
                yield line, reference.split('?', 1)[0], 'literal'
        if DOC_COMPONENT.search(text):
            yield line, 'docs', 'literal'


def doc_reader_errors(path, source, fallback, files):
    """Code that reads a docs-only candidate must keep every file it reaches on the full CI path."""
    if not READER_FILE.search(path) or path in READER_ALLOWLIST:
        return []
    errors = []
    for line, pattern, kind in sorted(set(doc_references(path, source))):
        try:
            if kind == 'bad':
                raise ValueError(pattern)
            reached = matched_files(pattern, files, go_only=kind == 'embed')
        except ValueError:
            errors.append(f'{path}:{line}: cannot resolve path pattern {pattern!r}; write a plain path or a Go path.Match pattern')
            continue
        unlisted = [file for file in reached if docs_candidate(file) and not in_fallback(file, fallback)]
        if unlisted:
            errors.append(f'{path}:{line}: {pattern} reaches {", ".join(unlisted[:3])}; a docs-only PR '
                          'would skip this reader, so add the path to scripts/ci/docs-only-fallback.txt')
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
    # A runner label can reach runs-on through matrix values, dispatch inputs
    # and expression fallbacks, so reject the alias anywhere outside comments.
    for line, text in enumerate(source.splitlines(), 1):
        for match in FLOATING_RUNNER.finditer(strip_yaml_comment(text)):
            errors.append(f'{path}:{line}: floating runner label {match[0]}; '
                          'select a versioned image label, such as ubuntu-24.04 or macos-26')
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
    fallback = load_fallback()
    files = sorted(set(paths) - {''})
    for path in files:
        file = root / path
        if not file.is_file():
            continue
        reader = bool(READER_FILE.search(path))
        source = file.read_text(errors='replace') if reader or path.endswith(('.go', '.md', '.yml', '.yaml')) else ''
        errors.extend(source_errors(path, source))
        if reader:
            errors.extend(doc_reader_errors(path, source, fallback, files))
    errors.extend(release_binary_errors((root / '.golangci.yml').read_text(), (root / 'scripts/release/build-archives.sh').read_text()))
    errors.extend(lint_version_errors((root / 'Makefile').read_text(), (root / '.github/workflows/ci.yml').read_text()))
    return errors


def main():
    errors = repository_errors()
    if errors:
        print('\n'.join(errors), file=sys.stderr)
        return 1
    print('Repository rules passed (behaviour names, Redis endpoints, workflow ports, runner labels, doc paths, docs-only readers, lint version, release binaries).')
    return 0


if __name__ == '__main__':
    sys.exit(main())
