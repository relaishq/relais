#!/usr/bin/env python3
"""Show the existing Chrome gate's gap rows without changing its verdict."""
from pathlib import Path
import re
import sys


ROW = re.compile(
    r'CHROME_GATE row=([\w-]+) track=(audio|video) n=(\d+) '
    r'p50_ms=(\d+(?:\.\d+)?) p95_ms=(\d+(?:\.\d+)?) max_ms=(\d+(?:\.\d+)?)')


def summary(log, version):
    lines = ['### Chrome relay change', '', version.strip(), '',
             '| Phase | Track | Events | p50 ms | p95 ms | Max ms |',
             '| --- | --- | --- | --- | --- | --- |']
    rows = [match.groups() for match in ROW.finditer(log)]
    lines += ['| ' + ' | '.join(row) + ' |' for row in rows]
    expected = {(phase, track) for phase in ('1-planned', '2-SIGKILL-local')
                for track in ('audio', 'video')}
    if {(row[0], row[1]) for row in rows} != expected:
        lines += ['', 'PARTIAL: gap rows are missing; inspect the test log.']
    lines += ['', 'The test step decides pass/fail. These rows alone do not prove a pass.']
    if 'Chrome inbound-rtp decrypt counter not exposed' in log:
        lines += ['', 'NOT-RUN: Chrome does not expose caller decryption counters.']
    return '\n'.join(lines) + '\n'


def read(path):
    return Path(path).read_text() if Path(path).is_file() else 'NOT-RUN: log missing'


if __name__ == '__main__':
    print(summary(read(sys.argv[1]), read(sys.argv[2])), end='')
