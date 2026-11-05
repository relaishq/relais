# gate-mutation

`gate-mutation` proves that a test catches a broken guard. It copies the
source tree to a temporary directory, changes the code, and requires the named
test to fail with the expected text.

With no flags, it runs the embedded operator cases in `cases.json`. CI runs
them in the "Kill operator gate mutants" step, and `make test-operator` runs
them locally.

## Run your own cases

Write a JSON case file for any package, then run it from the repository root:

```bash
go run ./scripts/ci/gate-mutation -cases path/to/cases.json
go run ./scripts/ci/gate-mutation -cases path/to/cases.json -only case-a,case-b -jobs 1
```

`testdata/example-cases.json` is a working example for `internal/iceufrag`.
`TestExampleCasesKillOutsideOperator` runs it.

## Case fields

Each case is one object in a JSON array. Unknown fields are refused.

| Field | Required | Meaning |
| --- | --- | --- |
| `ID` | yes | Unique name for the mutant. |
| `Package` | yes | Package that holds the test, such as `./pkg/sessionstore`. |
| `Test` | yes | One test name. A subtest is `TestName/subtest`. |
| `Marker` | yes | Text that the failing test must print, such as an assertion message. |
| `File`, `Old`, `New` | one change | Replace `Old` with `New` in `File`. `Old` must match exactly once. |
| `Function` | no | Limit the `File` edit to one declaration: a function name, `type:Name` or `var:Name`. Empty means the whole file. |
| `Receiver` | no | Method receiver type that selects one `Function`. |
| `Edits` | one change | A list of `File`/`Function`/`Old`/`New` edits, applied in order. Use it for a change across files. |
| `Patch` | one change | A unified diff, applied with `git apply`. A relative path is relative to the case file. The patch may only modify existing files. |

Give a `Patch`, or give `File` or `Edits`; never both. Use one case for each
test that must catch the change. Make a patch with `git diff` from the
repository root.

## Kill rules

- First, every named test must pass on the unchanged copy.
- A mutant is killed only when its named test runs, fails, and prints `Marker`.
- A build error, missing test, panic, timeout or different failure is
  `INVALID`. A mutant whose test passes is `SURVIVED`. Both fail the run.
- Tests run with `-short`, a fresh build cache, offline read-only modules, and
  a 60 s test timeout.

## Restore check

Workers reuse their copy for the next mutant. After each mutant, the tool writes
back the original bytes, reads every touched file again, and compares it byte
for byte. If one file differs, the run fails with `restored file differs`.
Your checkout is never changed.
