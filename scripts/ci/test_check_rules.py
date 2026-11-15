"""Known-bad inputs must fail each mechanical repository rule."""
import unittest

from check_rules import (bad_name, doc_reader_errors, go_embed_patterns, go_pattern_regex, lint_version_errors,
                         source_errors, release_binary_errors)


class BehaviourNames(unittest.TestCase):
    def test_process_names_fail(self):
        for name in ('TestFooRound2', 'foo_fix3_test.go', 'ReviewState',
                     'test_review', 'Reviews', 'Reviewer', 'Reviewers', 'Reviewed', 'Reviewing', 'rereview', 'TestReview2', 'TestRound_2', 'fix_3.go', 'docs/review/notes.md', 'round2.go', 'fooReviewHTTP'):
            with self.subTest(name=name):
                self.assertTrue(bad_name(name))

    def test_behaviour_names_pass(self):
        for name in ('TestPreview', 'preview.go', 'Previewer', 'RoundTrip', 'Prefix32', 'Fixation'):
            self.assertFalse(bad_name(name), name)

    def test_go_declarations_ignore_comments_and_strings(self):
        self.assertFalse(source_errors('pkg/state_test.go', '''package state
// func TestFooRound2() {}
/* type ReviewState struct{} */
var text = `func Review() {}`
func TestPreview() {}
'''))
        for declaration in ('func TestFooRound2() {}',
                            'func (s *State[T]) Review() {}',
                            'type ReviewState[T any] struct{}',
                            'type (ReviewState int; Safe int)',
                            'type (\n Safe int\n Fix3 struct{}\n)'):
            self.assertTrue(source_errors('pkg/state_test.go', 'package state\n' + declaration), declaration)


    def test_all_identifier_kinds(self):
        for declaration in ('var reviewState int', 'const reviewed = 1',
                            'type State struct { Reviews int }',
                            'type State interface { Reviewer() }',
                            'func safe(reviewers int) { reviewing := reviewers; _ = reviewing }',
                            'func (reviewState *State) Safe() {}'):
            self.assertTrue(source_errors('pkg/state.go', 'package state\n' + declaration), declaration)


class RedisEndpoints(unittest.TestCase):
    def test_direct_reads_fail(self):
        for call in ('os.Getenv("RELAIS_TEST_REDIS_ADDR")',
                     'os.LookupEnv(\n"RELAIS_TEST_REDIS_CLUSTER_ADDRS"\n)',
                     'operating.Getenv(`RELAIS_TEST_REDIS_ADDR`)'):
            source = 'package state\nimport operating "os"\nfunc TestState() { _ = ' + call + ' }'
            self.assertTrue(source_errors('pkg/state_test.go', source), call)
        self.assertTrue(source_errors('pkg/state_test.go', 'package state\nimport . "os"\nfunc TestState() { Getenv("RELAIS_TEST_REDIS_ADDR") }'))

    def test_literal_indirection_fails(self):
        for literal in ('"RELAIS_TEST_REDIS_ADDR"', '`RELAIS_TEST_REDIS_CLUSTER_ADDRS`',
                        r'"\x52ELAIS_TEST_REDIS_ADDR"'):
            self.assertTrue(source_errors('pkg/state.go', 'package state\nconst key = ' + literal))
        self.assertFalse(source_errors('internal/redistestenv/env_test.go', 'package redistestenv\nconst key = "RELAIS_TEST_REDIS_ADDR"'))
        self.assertFalse(source_errors('internal/redistestcluster/standalone_gate_test.go', 'package state\nconst key = "RELAIS_TEST_REDIS_ADDR"'))

    def test_literal_exception_does_not_allow_direct_reads(self):
        self.assertTrue(source_errors('internal/redistestcluster/standalone_gate_test.go',
                                      'package state\nimport "os"\nvar value = os.Getenv("RELAIS_TEST_REDIS_ADDR")'))

    def test_other_env_and_helper_pass(self):
        self.assertFalse(source_errors('pkg/state_test.go', 'package state\nfunc TestState() { os.Getenv("OTHER") }'))
        self.assertFalse(source_errors('internal/redistestenv/env.go', 'package redistestenv\nfunc Address() { os.Getenv("RELAIS_TEST_REDIS_ADDR") }'))
        self.assertFalse(source_errors('pkg/state_test.go', '// os.Getenv("RELAIS_TEST_REDIS_ADDR")'))

    def test_workflow_port_policy(self):
        path = '.github/workflows/ci.yml'
        for bad in ('ports:\n - 6379:6379', 'ports: ["6379:6379"]', 'ports:\n - target: 6379\n   published: 6379',
                    'ports:\n - "127.0.0.1:6379:6379"',
                    'RELAIS_TEST_REDIS_ADDR: "localhost:6379"',
                    'RELAIS_TEST_REDIS_CLUSTER_ADDRS: "cache:16379,cache:6379"',
                    'RELAIS_TEST_REDIS_ADDR: >-\n  localhost:6379',
                    'RELAIS_TEST_REDIS_ADDR: "cache#tag:6379"',
                    "RELAIS_TEST_REDIS_ADDR: 'cache#tag:6379'",
                    'run: export RELAIS_TEST_REDIS_ADDR=127.0.0.1:06379'):
            self.assertTrue(source_errors(path, bad), bad)
        for good in ('ports:\n - 16379:6379', 'ports: ["16379:6379"]',
                     'RELAIS_TEST_REDIS_ADDR: "127.0.0.1:16379"',
                     '# RELAIS_TEST_REDIS_ADDR: cache:6379',
                     'RELAIS_TEST_REDIS_ADDR: cache:16379 # bad example cache:6379'):
            self.assertFalse(source_errors(path, good), good)


class RunnerLabels(unittest.TestCase):
    path = '.github/workflows/ci.yml'

    def assertFloating(self, workflow):
        errors = source_errors(self.path, workflow)
        self.assertTrue(errors, workflow)
        self.assertIn('ubuntu-24.04', errors[0])

    def test_direct_label_fails(self):
        self.assertFloating('jobs:\n  test:\n    runs-on: ubuntu-latest')
        self.assertFloating('runs-on: macos-latest-xlarge')

    def test_quoted_label_fails(self):
        self.assertFloating('runs-on: "macos-latest"')
        self.assertFloating("runs-on: 'Windows-Latest'")

    def test_list_label_fails(self):
        self.assertFloating('runs-on: [self-hosted, ubuntu-latest]')
        self.assertFloating('runs-on:\n  - ubuntu-latest')

    def test_matrix_value_fails(self):
        self.assertFloating('strategy:\n  matrix:\n    include:\n      - runner: macos-latest\nruns-on: ${{ matrix.runner }}')
        self.assertFloating('matrix:\n  os: [ubuntu-24.04, windows-latest]')

    def test_dispatch_input_default_fails(self):
        self.assertFloating('on:\n  workflow_dispatch:\n    inputs:\n      runner_label:\n        default: ubuntu-latest\n'
                            'jobs:\n  soak:\n    runs-on: ${{ inputs.runner_label }}')

    def test_expression_fallback_fails(self):
        self.assertFloating("runs-on: ${{ inputs.runner_label || 'ubuntu-latest' }}")

    def test_comment_is_ignored(self):
        self.assertFalse(source_errors(self.path, 'runs-on: ubuntu-24.04 # not ubuntu-latest\n# runs-on: macos-latest'))

    def test_versioned_labels_pass(self):
        for label in ('ubuntu-24.04', 'ubuntu-24.04-arm', 'macos-15', 'macos-26', 'windows-2025',
                      "${{ inputs.runner_label || 'ubuntu-24.04' }}", '[self-hosted, linux]'):
            self.assertFalse(source_errors(self.path, 'runs-on: ' + label), label)


class PortableDocs(unittest.TestCase):
    def test_local_paths_fail(self):
        for path in ('/Users/someone/work/', '/home/someone/work/',
                     '/Users/alice', '/home/alice', '/Users/you/', '/private/tmp/build', '/var/folders/ab/build'):
            self.assertTrue(source_errors('docs/testing.md', 'Use ' + path))
        self.assertFalse(source_errors('docs/testing.md', 'Use $HOME/project/ or ./project/ or /tmp/build'))

    def test_placeholders_and_urls_are_portable(self):
        for path in ('/home/$USER/x', '/home/${USER}/x', '/Users/$USER',
                     '/Users/<name>/', '/home/<name>',
                     'https://example.com/home/index',
                     'https://example.com/Users/alice',
                     '<https://example.com/private/tmp/build>'):
            with self.subTest(path=path):
                self.assertFalse(source_errors('docs/testing.md', 'Use ' + path))
        self.assertTrue(source_errors('docs/testing.md',
                                     'https://example.com/home/index /Users/you/'))


class DocsOnlyReaders(unittest.TestCase):
    fallback = ['docs/listed-example.md', 'README.md', '**/testdata/']
    files = ['docs/listed-example.md', 'docs/unlisted-example.md', 'docs/demo.md', 'docs/adr/a.md',
             'docs/img.png', 'docs/dev/ci.md', 'README.md', 'GUIDE.md', 'cmd/demo/web/x.md', 'cmd/demo/main.go',
             'site.go']

    def errors(self, path, source, fallback=None):
        return doc_reader_errors(path, source, self.fallback if fallback is None else fallback, self.files)

    def test_code_and_fixtures_reading_unlisted_docs_fail(self):
        for path, source in (('pkg/a/a_test.go', 'os.ReadFile("../../docs/unlisted-example.md")'),
                             ('pkg/a/a.go', 'os.ReadFile("../../docs/unlisted-example.md")'),
                             ('cmd/demo/page.test.cjs', "fs.readFileSync('docs/unlisted-example.md')"),
                             ('cmd/demo/web/page.mjs', "fetch('../../GUIDE.md')"),
                             ('cmd/demo/web/page.mjs', "fetch('docs/demo.md?raw')"),
                             ('scripts/x/test_page.py', "Path('./docs/adr/')"),
                             ('scripts/x/run.sh', 'cat docs/adr/*.md'),
                             ('Makefile', '\tcp GUIDE.md bin/'),
                             ('.github/workflows/ci.yml', '        run: grep x docs/unlisted-example.md'),
                             ('pkg/a/testdata/case.json', '{"doc": "docs/unlisted-example.md"}')):
            with self.subTest(path=path, source=source):
                self.assertTrue(self.errors(path, source))
        self.assertTrue(self.errors('pkg/a/a_test.go', 'name := "README.md"', fallback=[]))

    def test_glob_literals_match_files(self):
        for source in ('"../../docs/d?mo.md"', '"../../docs/[d]emo.md"', '"../../docs/[^x]emo.md"', '"../../G*.md"'):
            with self.subTest(source=source):
                self.assertTrue(self.errors('pkg/a/a_test.go', source))
        self.assertFalse(self.errors('pkg/a/a_test.go', '"../../docs/[x]emo.md" "../../docs/*.png"'))

    def test_joined_path_readers_fail(self):
        for path, source in (('pkg/a/a_test.go', 'os.ReadFile(filepath.Join("..", "..", "docs", name))'),
                             ('pkg/a/a.go', 'root := filepath.Join(repo, `docs`)'),
                             ('scripts/x/report.py', "text = (Path(root) / 'docs' / name).read_text()"),
                             ('scripts/x/report.py', 'os.path.join(root, "../docs")'),
                             ('cmd/demo/page.test.cjs', "path.join(__dirname, '../../docs/', name)")):
            with self.subTest(source=source):
                self.assertTrue(self.errors(path, source))

    def test_go_embed_resolves_against_tracked_files(self):
        embed = 'package site\n//go:embed {}\nvar pages embed.FS'
        for pattern in ('doc*', 'all:docs', 'docs/d?mo.md', 'docs/[d]emo.md', '"G*.md" web', '*.md', 'd*/adr'):
            with self.subTest(pattern=pattern):
                self.assertTrue(self.errors('site.go', embed.format(pattern)))
        for pattern in ('web', 'docs/img.png', 'docs/listed-example.md', 'd[x]cs'):
            with self.subTest(pattern=pattern):
                self.assertFalse(self.errors('site.go', embed.format(pattern)))
        self.assertFalse(self.errors('cmd/demo/main.go', embed.format('web *.md')))

    def test_go_embed_quoting_and_go_class_syntax(self):
        embed = 'package site\n//go:embed {}\nvar pages embed.FS'
        # Backquoted and double-quoted arguments are Go strings, not shell words.
        for pattern in ('`docs/dev/ci.md`', '"docs/dev/ci.md"', '"docs/dev/\\x63i.md"', 'web `docs/dev/c i.md` "docs/demo.md"',
                        # In Go ! is a literal class member; [!c] still matches ci.md.
                        'docs/dev/[!c]i.md', 'docs/dev/[^x]i.md', 'docs/dev/[a-c]i.md', 'docs/dev/\\ci.md'):
            with self.subTest(pattern=pattern):
                self.assertTrue(self.errors('site.go', embed.format(pattern)))
        for pattern in ('docs/dev/[^c]i.md', 'docs/dev/[!x]i.md', '`docs/img.png`'):
            with self.subTest(pattern=pattern):
                self.assertFalse(self.errors('site.go', embed.format(pattern)))

    def test_unresolvable_patterns_fail_closed(self):
        embed = 'package site\n//go:embed {}\nvar pages embed.FS'
        # Go also splits on Unicode spaces; the rule does not model them and fails closed.
        for pattern in ('`docs/dev/ci.md', '"docs/dev/ci.md', 'docs/[ci.md', 'docs/[]ci.md', 'docs/[-]ci.md', 'docs\\',
                        'README.md\u00a0docs/dev/ci.md'):
            with self.subTest(pattern=pattern):
                errors = self.errors('site.go', embed.format(pattern))
                self.assertTrue(errors and 'cannot resolve' in errors[0], errors)
        self.assertTrue(self.errors('scripts/x/run.sh', 'cat docs/[ci.md'))

    def test_go_pattern_matching(self):
        cases = (('*', 'abc', True), ('*', 'a/b', False), ('a?c', 'abc', True), ('a?c', 'a/c', False),
                 ('[!c]i', '!i', True), ('[!c]i', 'xi', False), ('[^c]i', 'xi', True), ('[^c]i', 'ci', False),
                 ('[a-c]', 'b', True), ('[c-a]', 'b', False), ('\\*', '*', True), ('\\*', 'a', False),
                 ('[\\]]', ']', True))
        for pattern, name, expected in cases:
            with self.subTest(pattern=pattern, name=name):
                self.assertEqual(bool(go_pattern_regex(pattern).fullmatch(name)), expected)

    def test_go_embed_argument_split(self):
        self.assertEqual(go_embed_patterns('a  `b c` "d\\x65" all:f'), ['a', 'b c', 'de', 'all:f'])

    def test_rule_uses_the_classifier_fallback_matcher(self):
        # A directory entry covers every file below it for both the rule and the classifier.
        self.assertFalse(self.errors('site.go', 'package site\n//go:embed doc*\nvar d embed.FS', fallback=['docs', 'README.md']))
        self.assertTrue(self.errors('site.go', 'package site\n//go:embed doc*\nvar d embed.FS', fallback=['docs/adr/']))

    def test_listed_missing_allowlisted_and_comment_references_pass(self):
        self.assertFalse(self.errors('pkg/a/a_test.go', 'os.ReadFile("../../docs/listed-example.md")'))
        self.assertFalse(self.errors('pkg/a/a_test.go', 'names := []string{"README.md", "../../README.md"}'))
        self.assertFalse(self.errors('pkg/a/a_test.go', '"../../docs/missing-example.md"'))
        self.assertFalse(self.errors('pkg/a/a_test.go', '"pkg/docs/x.md" "examples/README.md" "mydocs/x.md"'))
        self.assertFalse(self.errors('pkg/a/a.go', 'package a\n// See docs/unlisted-example.md.\nvar docsOnly = 1'))
        self.assertFalse(self.errors('scripts/x/run.sh', '# See docs/unlisted-example.md'))
        self.assertFalse(self.errors('scripts/ci/check_rules.py', "Path('docs') / 'unlisted-example.md'"))
        self.assertFalse(self.errors('pkg/a/notes.txt', 'docs/unlisted-example.md'))


class LintVersion(unittest.TestCase):
    def test_pin_matches_action(self):
        makefile = 'GOLANGCI_LINT_VERSION := v2.14.0\n'
        workflow = 'uses: golangci/golangci-lint-action@v9\nwith:\n  version: v2.14.0\n'
        self.assertFalse(lint_version_errors(makefile, workflow))
        self.assertTrue(lint_version_errors(makefile, workflow.replace('v2.14.0', 'v2.13.0')))
        self.assertTrue(lint_version_errors(makefile, ''))
        self.assertTrue(lint_version_errors('', workflow))
        reordered = workflow.replace('  version:', '  args: --verbose\n  version:')
        self.assertFalse(lint_version_errors(makefile, reordered))
        self.assertTrue(lint_version_errors(makefile, reordered.replace('v2.14.0', 'v2.13.0')))
        self.assertTrue(lint_version_errors(makefile, workflow + '\n' + reordered.replace('v2.14.0', 'v2.13.0')))
        self.assertTrue(lint_version_errors(makefile, workflow.replace('  version: v2.14.0', '  args: |\n    version: v2.14.0')))
        self.assertTrue(lint_version_errors(makefile, workflow + '\n- uses: golangci/golangci-lint-action@v9\n  with:\n    args: --verbose'))


class ReleaseBinaries(unittest.TestCase):
    def test_archive_and_depguard_sets_match(self):
        config = 'files:\n - "${base-path}/cmd/relais-{relay,worker}/**.go"'
        script = 'binaries=(relais-worker relais-relay)'
        self.assertFalse(release_binary_errors(config, script))
        self.assertTrue(release_binary_errors(config, script.replace('relais-worker', 'relais-demo')))
        self.assertTrue(release_binary_errors('', script))
        self.assertTrue(release_binary_errors(config, ''))


if __name__ == '__main__':
    unittest.main()
