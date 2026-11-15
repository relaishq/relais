"""The docs-only classifier picks the short path only for safe documentation edits."""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

from docs_only import classify, docs_candidate, git_changes, in_fallback, load_fallback

SCRIPTS = Path(__file__).resolve().parent
WORKFLOW = SCRIPTS.parents[1] / '.github/workflows/ci.yml'
BASE = 'a' * 40
FALLBACK = load_fallback()


def edit(path, status='M', old='100644', new='100644'):
    return (status, old, new, path)


def run(changes, event='pull_request', base=BASE):
    return classify(event, base, 'HEAD', changes=lambda _base, _head: changes, fallback=lambda: FALLBACK)


class Classify(unittest.TestCase):
    def test_docs_edits_and_additions_are_docs_only(self):
        self.assertIsNone(run([edit('docs/guide-example.md'), edit('CONTRIBUTING.md'), edit('AGENTS.md'),
                               edit('docs/adr/0099-example.md', 'A', '000000'),
                               edit('NOTES-example.md', 'A', '000000')]))

    def test_each_fallback_path_takes_full_path(self):
        for path in ('docs/api_reference.md', 'docs/build-an-agent.md', 'docs/adr/0010-pluggable-agents.md',
                     'README.md', '.github/NOTES.md', '.github/workflows/ci.yml', 'Makefile', 'go.mod', 'go.sum',
                     'scripts/dev/README.md', 'testdata/a.md', 'pkg/mediaworker/testdata/v10-components.md',
                     'examples/voice-agent/voice/testdata/README.md', '.golangci.yml', 'python/README.md'):
            with self.subTest(path=path):
                self.assertIsNotNone(run([edit('docs/guide-example.md'), edit(path)]))

    def test_non_documentation_takes_full_path(self):
        for path in ('pkg/relay/relay.go', 'cmd/relais-demo/page.js', 'docs.go', 'LICENSE',
                     'deploy/aws/boot/README.md', 'cmd/relais-demo/web/x.md', 'pkg/foo/README.md',
                     'internal/operatorrehearsal/assets/notes.md', 'mydocs/x.md',
                     'docs/example.go', 'docs/example_test.go', 'docs/example.py', 'docs/img.png',
                     'docs/observability/grafana/example.json', 'NOTES.txt'):
            with self.subTest(path=path):
                self.assertIsNotNone(run([edit(path)]))
                self.assertIsNotNone(run([edit('docs/guide-example.md'), edit(path, 'A', '000000')]))

    def test_rename_delete_type_and_mode_changes_take_full_path(self):
        for change in (edit('docs/old-example.md', 'D', '100644', '000000'),
                       edit('docs/new-example.md', 'R100'), edit('docs/copy-example.md', 'C100'),
                       edit('docs/link-example.md', 'T', '100644', '120000'),
                       edit('docs/run-example.md', 'M', '100644', '100755'),
                       edit('docs/link-example.md', 'A', '000000', '120000'),
                       edit('docs/module-example', 'A', '000000', '160000')):
            with self.subTest(change=change):
                self.assertIsNotNone(run([edit('docs/guide-example.md'), change]))

    def test_empty_diff_takes_full_path(self):
        self.assertEqual(run([]), 'empty diff')

    def test_non_pr_event_or_missing_base_takes_full_path(self):
        for event in ('push', 'schedule', 'workflow_dispatch', ''):
            with self.subTest(event=event):
                self.assertIsNotNone(run([edit('docs/guide-example.md')], event=event))
        for base in ('', '0' * 40):
            with self.subTest(base=base):
                self.assertIsNotNone(run([edit('docs/guide-example.md')], base=base))

    def test_classifier_errors_take_full_path(self):
        def fail(_base, _head):
            raise subprocess.CalledProcessError(128, ['git', 'diff'])
        self.assertIn('classifier error', classify('pull_request', BASE, 'HEAD', changes=fail, fallback=lambda: FALLBACK))
        def no_list():
            raise OSError('missing fallback list')
        self.assertIn('classifier error', classify('pull_request', BASE, 'HEAD',
                                                   changes=lambda _b, _h: [edit('docs/guide-example.md')], fallback=no_list))

    def test_fallback_matching(self):
        self.assertTrue(in_fallback('a/b/testdata/c.md', ['**/testdata/']))
        self.assertTrue(in_fallback('a/b/testdata/c.md', ['**/testdata']))
        self.assertTrue(in_fallback('docs/demo.md', ['docs']))
        self.assertTrue(in_fallback('docs/adr/x.md', ['docs/adr']))
        self.assertFalse(in_fallback('docs/adr-x.md', ['docs/adr']))
        self.assertFalse(in_fallback('a/mytestdata/c.md', ['**/testdata/']))
        self.assertFalse(in_fallback('pkg/README.md', ['README.md']))
        self.assertFalse(in_fallback('pkg/scripts/x.md', ['scripts/']))

    def test_docs_candidates_are_markdown_under_docs_or_root(self):
        self.assertTrue(docs_candidate('docs/a/b.md') and docs_candidate('GUIDE.md'))
        self.assertFalse(docs_candidate('docs/a.go') or docs_candidate('pkg/a.md') or docs_candidate('docs/a.png'))

    def test_fallback_list_rejects_globs_and_odd_paths(self):
        with tempfile.TemporaryDirectory() as folder:
            listing = Path(folder) / 'fallback.txt'
            for entry in ('docs/*.md', 'docs/d?mo.md', 'docs/[d]emo.md', '**/*.md', '/docs', './docs', 'docs/../x',
                          '**/', '/'):
                with self.subTest(entry=entry):
                    listing.write_text('README.md\n' + entry + '\n')
                    with self.assertRaises(ValueError):
                        load_fallback(listing)
            listing.write_text('# reason\ndocs\ndocs/adr/  # inline reason\n**/testdata/\nREADME.md\n')
            self.assertEqual(load_fallback(listing), ['docs', 'docs/adr/', '**/testdata/', 'README.md'])

    def test_invalid_fallback_list_takes_full_path(self):
        with tempfile.TemporaryDirectory() as folder:
            listing = Path(folder) / 'fallback.txt'
            listing.write_text('docs/*.md\n')
            self.assertIn('classifier error', classify('pull_request', BASE, 'HEAD',
                                                       changes=lambda _b, _h: [edit('docs/guide-example.md')],
                                                       fallback=lambda: load_fallback(listing)))


class GitRepo(unittest.TestCase):
    """A temporary repository whose base commit holds docs/a.md and docs/b.md."""

    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        self.root = Path(self.dir.name)
        self.git('init', '-q')
        (self.root / 'docs').mkdir()
        (self.root / 'docs/a.md').write_text('a\n')
        (self.root / 'docs/b.md').write_text('b\n')
        self.base = self.commit()

    def git(self, *args):
        return subprocess.run(['git', '-c', 'user.name=CI', '-c', 'user.email=ci@example.invalid',
                               '-c', 'commit.gpgsign=false', *args],
                              cwd=self.root, check=True, capture_output=True, text=True).stdout.strip()

    def commit(self):
        self.git('add', '-A')
        self.git('commit', '-q', '-m', 'change')
        return self.git('rev-parse', 'HEAD')


class LocalGitDiff(GitRepo):
    """Parse real git output, so status and mode checks see what CI sees."""

    def changes(self):
        head = self.commit()
        return classify('pull_request', self.base, head,
                        changes=lambda base, head: git_changes(base, head, cwd=self.root), fallback=lambda: FALLBACK)

    def test_edit_and_add_are_docs_only(self):
        (self.root / 'docs/a.md').write_text('a2\n')
        (self.root / 'docs/space name.md').write_text('c\n')
        self.assertIsNone(self.changes())

    def test_rename_takes_full_path(self):
        self.git('mv', 'docs/a.md', 'docs/c.md')
        self.assertIsNotNone(self.changes())

    def test_delete_takes_full_path(self):
        (self.root / 'docs/a.md').unlink()
        self.assertIsNotNone(self.changes())

    def test_mode_change_takes_full_path(self):
        os.chmod(self.root / 'docs/a.md', 0o755)
        self.git('config', 'core.fileMode', 'true')
        self.assertIsNotNone(self.changes())

    def test_symlink_takes_full_path(self):
        (self.root / 'docs/link.md').symlink_to('a.md')
        self.assertIsNotNone(self.changes())

    def test_missing_base_takes_full_path(self):
        (self.root / 'docs/a.md').write_text('a2\n')
        self.base = 'b' * 40
        self.assertIn('classifier error', self.changes())


def classify_step():
    """The bash body of the changes job's classify step in ci.yml."""
    step = WORKFLOW.read_text().split('        id: classify\n', 1)[1].split('        run: |\n', 1)[1]
    lines = []
    for line in step.splitlines():
        if line.strip() and not line.startswith(' ' * 10):
            break
        lines.append(line[10:])
    return '\n'.join(lines) + '\n'


class WorkflowStep(GitRepo):
    """Run the workflow step: the classifier and fallback list come from the PR base."""

    def setUp(self):
        super().setUp()
        self.temp = self.root / '.runner-temp'
        self.temp.mkdir()
        (self.root / '.git/info/exclude').write_text('.runner-temp/\n')

    def add_classifier(self, fallback_text=None):
        (self.root / 'scripts/ci').mkdir(parents=True, exist_ok=True)
        shutil.copy(SCRIPTS / 'docs_only.py', self.root / 'scripts/ci/docs_only.py')
        fallback = fallback_text if fallback_text is not None else (SCRIPTS / 'docs-only-fallback.txt').read_text()
        (self.root / 'scripts/ci/docs-only-fallback.txt').write_text(fallback)
        self.base = self.commit()

    def step(self, event='pull_request'):
        self.commit()
        output = self.temp / 'output'
        output.write_text('')
        env = dict(os.environ, EVENT_NAME=event, BASE_SHA=self.base, RUNNER_TEMP=str(self.temp),
                   GITHUB_OUTPUT=str(output))
        subprocess.run(['bash', '-e', '-o', 'pipefail', '-c', classify_step()], cwd=self.root, env=env,
                       check=True, capture_output=True)
        return output.read_text()

    def test_base_without_classifier_takes_full_path_and_stays_green(self):
        (self.root / 'docs/a.md').write_text('a2\n')
        self.assertEqual(self.step(), 'docs_only=false\n')

    def test_base_classifier_allows_docs_only(self):
        self.add_classifier()
        (self.root / 'docs/a.md').write_text('a2\n')
        self.assertEqual(self.step(), 'docs_only=true\n')

    def test_explicit_base_fallback_list_is_used(self):
        self.add_classifier('docs/a.md\n')
        (self.root / 'docs/a.md').write_text('a2\n')
        self.assertEqual(self.step(), 'docs_only=false\n')
        (self.root / 'docs/a.md').write_text('a3\n')
        (self.root / 'docs/b.md').write_text('b2\n')
        self.git('checkout', '-q', self.base, '--', 'docs/a.md')
        self.assertEqual(self.step(), 'docs_only=true\n')

    def test_pr_edit_to_classifier_is_ignored(self):
        self.add_classifier()
        (self.root / 'scripts/ci/docs_only.py').write_text('print("docs_only=true")\n')
        (self.root / 'scripts/ci/docs-only-fallback.txt').write_text('')
        self.assertEqual(self.step(), 'docs_only=false\n')

    def test_non_pr_event_takes_full_path(self):
        self.add_classifier()
        (self.root / 'docs/a.md').write_text('a2\n')
        self.assertEqual(self.step('push'), 'docs_only=false\n')


if __name__ == '__main__':
    unittest.main()
