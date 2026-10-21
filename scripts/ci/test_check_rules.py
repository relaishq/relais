"""Known-bad inputs must fail each mechanical repository rule."""
import unittest

from check_rules import bad_name, lint_version_errors, source_errors


class BehaviourNames(unittest.TestCase):
    def test_process_names_fail(self):
        for name in ('TestFooRound2', 'foo_fix3_test.go', 'ReviewState',
                     'test_review', 'TestReview2', 'TestRound_2', 'fix_3.go', 'docs/review/notes.md', 'round2.go', 'fooReviewHTTP'):
            with self.subTest(name=name):
                self.assertTrue(bad_name(name))

    def test_behaviour_names_pass(self):
        for name in ('TestPreview', 'preview.go', 'Reviewer', 'RoundTrip', 'Prefix32', 'Fixation'):
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


class RedisEndpoints(unittest.TestCase):
    def test_direct_reads_fail(self):
        for call in ('os.Getenv("RELAIS_TEST_REDIS_ADDR")',
                     'os.LookupEnv(\n"RELAIS_TEST_REDIS_CLUSTER_ADDRS"\n)',
                     'operating.Getenv(`RELAIS_TEST_REDIS_ADDR`)'):
            source = 'package state\nimport operating "os"\nfunc TestState() { _ = ' + call + ' }'
            self.assertTrue(source_errors('pkg/state_test.go', source), call)
        self.assertTrue(source_errors('pkg/state_test.go', 'package state\nimport . "os"\nfunc TestState() { Getenv("RELAIS_TEST_REDIS_ADDR") }'))

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
                    'run: export RELAIS_TEST_REDIS_ADDR=127.0.0.1:06379'):
            self.assertTrue(source_errors(path, bad), bad)
        for good in ('ports:\n - 16379:6379', 'ports: ["16379:6379"]',
                     'RELAIS_TEST_REDIS_ADDR: "127.0.0.1:16379"'):
            self.assertFalse(source_errors(path, good), good)


class PortableDocs(unittest.TestCase):
    def test_local_paths_fail(self):
        for path in ('/Users/someone/work/', '/home/someone/work/',
                     '/private/tmp/build', '/var/folders/ab/build'):
            self.assertTrue(source_errors('docs/testing.md', 'Use ' + path))
        self.assertFalse(source_errors('docs/testing.md', 'Use $HOME/project/ or ./project/'))


class LintVersion(unittest.TestCase):
    def test_pin_matches_action(self):
        makefile = 'GOLANGCI_LINT_VERSION := v2.14.0\n'
        workflow = 'uses: golangci/golangci-lint-action@v9\nwith:\n  version: v2.14.0\n'
        self.assertFalse(lint_version_errors(makefile, workflow))
        self.assertTrue(lint_version_errors(makefile, workflow.replace('v2.14.0', 'v2.13.0')))
        self.assertTrue(lint_version_errors(makefile, ''))
        self.assertTrue(lint_version_errors('', workflow))


if __name__ == '__main__':
    unittest.main()
