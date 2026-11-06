"""Independent chrony evidence; never adjusts the host clock."""
import base64
import datetime
import gzip
import hashlib
import json
import pathlib
import os
import math
import io
import stat
import re
import subprocess
import time


def require(ok, message):
    if not ok:
        raise ValueError(message)


# chrony.conf(5), "log tracking": column 14 bounds error since the
# preceding update, including remaining correction and frequency uncertainty.
TRACKING_LOG = pathlib.Path('/var/log/chrony/tracking.log')
READ_LIMIT = 64 * 1024
ENCODED_LIMIT = 220000  # Leave room for the cursor and existing chronyc evidence.


def tracking_row(line):
    c = line.split()
    require(len(c) == 14, 'Invalid tracking log row')
    at = datetime.datetime.strptime(' '.join(c[:2]), '%Y-%m-%d %H:%M:%S').replace(tzinfo=datetime.timezone.utc)
    values = [float(c[i]) for i in (4, 5, 6, 9, 10, 11, 12, 13)]
    require(all(math.isfinite(v) for v in values), 'Invalid tracking log number')
    stratum, combined = int(c[3]), int(c[8])
    require(0 <= stratum <= 16 and 0 <= combined <= 2147483647, 'Invalid tracking log count')
    freq, skew, offset, sd, correction, delay, dispersion, maximum = values
    require(min(skew, sd, delay, dispersion, maximum) >= 0, 'Invalid tracking log bound')
    require(re.fullmatch(r'[A-Za-z0-9_.:%+-]{1,255}', c[2]) is not None, 'Invalid reference')
    return dict(unixSeconds=int(at.timestamp()), reference=c[2], stratum=stratum,
                frequencyPpm=freq, skewPpm=skew, offsetSeconds=offset, leapStatus=c[7],
                combinedSources=combined, offsetSdSeconds=sd, remainingCorrectionSeconds=correction,
                rootDelaySeconds=delay, rootDispersionSeconds=dispersion, maxErrorSeconds=maximum)


def daemon_identity():
    pid = subprocess.run(['systemctl', 'show', '-p', 'MainPID', '--value', 'chronyd'],
                         capture_output=True, text=True, timeout=5, check=True).stdout.strip()
    require(pid.isdigit() and int(pid) > 0, 'Missing chronyd')
    # Include boot and process start time, so PID reuse is a restart too.
    start = pathlib.Path('/proc/' + pid + '/stat').read_text().rsplit(')', 1)[1].split()[19]
    boot = pathlib.Path('/proc/sys/kernel/random/boot_id').read_text()
    return hashlib.sha256((boot + pid + ':' + start).encode()).hexdigest()


def discard_line(log, end):
    while log.tell() < end:
        if log.readline(min(READ_LIMIT, end - log.tell())).endswith(b'\n'):
            return False
    return True


def tracking_evidence(cursor=None, run_start_unix_ms=0):
    rows, breaks = [], []
    before = time.monotonic_ns()
    wall = time.time_ns()
    after = time.monotonic_ns()
    result = dict(rows=rows, breaks=breaks, cursor=None, runStartUnixMs=run_start_unix_ms, readComplete=False)

    def before_run(at):
        # A log timestamp has one-second resolution. Count a straddling second in-run.
        return (at + 1) * 1000 <= run_start_unix_ms

    def record(kind, at=None, since=None):
        end = wall // 10**9 if at is None else at
        if since is None:
            prior = cursor.get('lastRow') if isinstance(cursor, dict) else None
            prior_wall = cursor.get('wallNs') if isinstance(cursor, dict) else None
            since = prior_wall // 10**9 if isinstance(prior_wall, int) else end
            if isinstance(prior, dict) and isinstance(prior.get('unixSeconds'), int):
                since = prior['unixSeconds']
        breaks.append(dict(kind=kind, fromUnixSeconds=min(since, end), unixSeconds=max(since, end), beforeRun=at is not None and before_run(max(since, end))))

    try:
        daemon = daemon_identity()
        with TRACKING_LOG.open('rb') as log:
            info = os.fstat(log.fileno())
            identity = str(info.st_dev) + ':' + str(info.st_ino)
            position, previous, skip_line = 0, None, False
            if cursor:
                previous = cursor['lastRow']
                if daemon != cursor['daemon']:
                    record('daemon-restart')
                # Both clocks slew together; allow the measured read brackets plus
                # 1 us for clock read resolution. This detects wall steps only.
                delta = wall - cursor['wallNs'] - (before - cursor['monotonicNs'])
                if abs(delta) > (after - before) + cursor['readWidthNs'] + 1000:
                    record('clock-step')
                if identity != cursor['file']:
                    record('log-rotation')
                    previous = None
                elif info.st_size < cursor['position']:
                    record('log-loss', since=previous['unixSeconds'] if previous else None)
                    previous = None
                else:
                    position = cursor['position']
                    skip_line = cursor['skipLine']
                    log.seek(max(0, position - 256))
                    if hashlib.sha256(log.read(min(position, 256))).hexdigest() != cursor['anchor']:
                        record('cursor-gap')
                        position, previous, skip_line = 0, None, False
            if not cursor and info.st_size > READ_LIMIT:
                log.seek(info.st_size - READ_LIMIT)
                fragment = log.readline(READ_LIMIT)
                position = log.tell()
                if not fragment.endswith(b'\n'):
                    record('log-loss')
                    skip_line = True
            log.seek(position)
            # Discard the rest of an oversized line in bounded chunks, at most
            # through this read's EOF snapshot. Never re-record its one loss break.
            if skip_line:
                skip_line = discard_line(log, info.st_size)
                position = log.tell()
            data = log.read(min(READ_LIMIT, max(0, info.st_size - position)))
            end = data.rfind(b'\n') + 1
            start, encoded_size, capped = position, 0, False
            for raw in data[:end].splitlines(keepends=True):
                count, prior = len(breaks), previous
                row = None
                try:
                    line = raw.decode().strip()
                    if line and not line.startswith(('=', 'Date', '#')):
                        row = tracking_row(line)
                        at = row['unixSeconds']
                        row['beforeRun'] = before_run(at)
                        if row['leapStatus'] != 'N':
                            record('leap-status', at, at)
                        if abs(row['offsetSeconds']) > row['maxErrorSeconds']:
                            record('self-consistency', at, previous['unixSeconds'] if previous else at)
                        if previous:
                            dt = at - previous['unixSeconds']
                            if dt > 60:
                                record('cursor-gap', at, previous['unixSeconds'])
                            if dt < 0:
                                record('clock-step', at, previous['unixSeconds'])
                            if row['reference'] != previous['reference']:
                                record('source-change', at, previous['unixSeconds'])
                except ValueError:
                    record('log-loss', since=previous['unixSeconds'] if previous else None)
                    previous = None
                added = sum(len(json.dumps(b)) + 1 for b in breaks[count:])
                if row is not None:
                    added += len(json.dumps(row)) + 1
                if encoded_size + added > ENCODED_LIMIT:
                    del breaks[count:]
                    previous, capped = prior, True
                    break
                encoded_size += added
                if row is not None:
                    rows.append(row)
                    previous = row
                position += len(raw)
            if position < info.st_size and (len(data) == READ_LIMIT or capped):
                log.seek(position)
                fragment = log.readline(min(READ_LIMIT + 1, info.st_size - position))
                since = rows[-1]['unixSeconds'] if rows else None
                if not capped and position == start + end and len(fragment) >= READ_LIMIT and not fragment[:READ_LIMIT].endswith(b'\n'):
                    record('log-loss', since=since)
                    skip_line = not fragment.endswith(b'\n') and discard_line(log, info.st_size)
                    position = log.tell()
                # Either cap loses nothing: the next read resumes at the cursor.
            if not cursor and run_start_unix_ms > 0:
                if rows and not rows[0]['beforeRun']:
                    record('cursor-gap', rows[0]['unixSeconds'], run_start_unix_ms // 1000)
                elif not rows and wall > run_start_unix_ms * 10**6:
                    record('cursor-gap', since=run_start_unix_ms // 1000)
                breaks.sort(key=lambda b: b['fromUnixSeconds'])
            log.seek(max(0, position - 256))
            anchor = hashlib.sha256(log.read(min(position, 256))).hexdigest()
        result['readComplete'] = position == info.st_size and not skip_line
        result['cursor'] = dict(file=identity, position=position, anchor=anchor, daemon=daemon,
                                wallNs=wall, monotonicNs=before, readWidthNs=after-before, lastRow=previous, skipLine=skip_line)
    except (OSError, ValueError, KeyError, TypeError, IndexError, subprocess.SubprocessError):
        record('log-loss')
    return result


def read_clock(cursor=None, run_start_unix_ms=0, probe_file='', probe_cursor=None):
    result = clock_evidence()
    result['trackingLog'] = tracking_evidence(cursor, run_start_unix_ms)
    # Match operator.MaxHostOutputBytes, reserving the print newline. A hostile
    # high-entropy log must not turn recorded diagnostics into a failed read.
    if len(base64.b64encode(gzip.compress(json.dumps(result).encode()))) > 23998:
        timeline = result['trackingLog']
        prior = cursor.get('lastRow') if isinstance(cursor, dict) else None
        now = int(result['measuredUnixNs']) // 10**9
        since = prior['unixSeconds'] if isinstance(prior, dict) else timeline['rows'][0]['unixSeconds'] if timeline['rows'] else now
        since = min([since] + [b['fromUnixSeconds'] for b in timeline['breaks'] if not b['beforeRun']])
        timeline['rows'] = []
        timeline['breaks'] = [dict(kind='cursor-gap', fromUnixSeconds=min(since, now), unixSeconds=max(since, now), beforeRun=False)]
    if probe_file:
        return add_probe_evidence(result, probe_file, probe_cursor, run_start_unix_ms)
    return result


# Mirror internal/clockprobe.ParseLine. A marker is file-wide, not a timestamp.
def probe_line(raw):
    if len(raw) < 84 and b'\n' not in raw:
        return None
    require(len(raw) == 84 and raw[83:] == b'\n', 'Invalid probe line')
    require(all(raw[i:i+1] == b' ' for i in (1, 21, 41, 61, 81)), 'Invalid probe spacing')
    require(raw[:1] in (b'0', b'1', b'2', b'3') and raw[82:83] in (b'0', b'1', b'2', b'3'), 'Invalid probe code')
    values = []
    for i in (2, 22, 42, 62):
        field = raw[i:i+19]
        require(all(48 <= n <= 57 for n in field), 'Invalid probe number')
        value = int(field)
        require(value <= 2**63 - 1, 'Invalid probe range')
        values.append(value)
    target, result = raw[0] - 48, raw[82] - 48
    require((target <= 2 and result <= 2) or (target == result == 3 and values == [0]*4), 'Invalid probe marker')
    return target, values, result


def probe_fits(baseline, evidence):
    candidate = dict(baseline, probeLog=evidence)
    raw = json.dumps(candidate).encode()
    return len(raw) <= 256000 and len(base64.b64encode(gzip.compress(raw))) <= 23998


def add_probe_evidence(baseline, path, cursor, run_start_unix_ms):
    losses = dict(readFailures=0, malformedLines=0, cursorGaps=0, capacityEvents=0, omittedRecords=0)
    evidence = dict(lines='', cursor=cursor, lossCounts=dict(losses))
    try:
        # Probe output must be a direct regular file. Never wait for a FIFO or
        # follow a substituted symlink on this optional read.
        with os.fdopen(os.open(path, os.O_RDONLY | os.O_NONBLOCK | os.O_NOFOLLOW), 'rb') as log:
            info = os.fstat(log.fileno())
            require(info.st_size <= 4 * 1024 * 1024 and stat.S_ISREG(info.st_mode), 'Invalid probe file')
            identity = str(info.st_dev) + ':' + str(info.st_ino)
            position, skip = 0, False
            if cursor:
                try:
                    position, skip = cursor['position'], cursor['skipLine']
                    require(isinstance(position, int) and 0 <= position <= info.st_size and cursor['file'] == identity, 'Probe cursor gap')
                    log.seek(max(0, position - 256))
                    require(hashlib.sha256(log.read(min(position, 256))).hexdigest() == cursor['anchor'], 'Probe anchor gap')
                except (ValueError, KeyError, TypeError):
                    # Unknown loss: do not replay old attempts from a replaced file.
                    losses['cursorGaps'] += 1
                    position = info.st_size
                    log.seek(max(0, position - 1))
                    skip = bool(position) and log.read(1) != b'\n'
            log.seek(position)
            if skip:
                skip = discard_line(log, info.st_size)
                position = log.tell()
            # Snapshot at most the existing read window. Each candidate owns its
            # cursor and counters, so wire deferral cannot acknowledge lost data.
            log.seek(position)
            data = log.read(min(READ_LIMIT, info.st_size - position))
            steps, chunks, length = [(position, skip, 0, dict(losses))], [], 0
            consumed = position
            for raw in io.BytesIO(data).readlines():
                if not raw.endswith(b'\n'):
                    if len(raw) <= 83:
                        break
                    losses['malformedLines'] += 1
                    consumed += len(raw)
                    skip = True
                else:
                    consumed += len(raw)
                    try:
                        parsed = probe_line(raw)
                        target, stamps, result = parsed
                        if target == 3:
                            losses['capacityEvents'] += 1
                        elif not (stamps[3] > 0 and stamps[3] + 10**9 < run_start_unix_ms * 10**6):
                            chunks.append(raw.decode('ascii'))
                            length += len(raw)
                    except (ValueError, TypeError):
                        losses['malformedLines'] += 1
                steps.append((consumed, skip, length, dict(losses)))
            # Compression is not strictly monotone. A fitting chronological
            # prefix is sufficient; never spend unbounded time finding the max.
            text = ''.join(chunks)
            best = None
            low, high = 0, len(steps) - 1
            for _ in range(13):
                if low > high:
                    break
                middle = (low + high) // 2
                end, skip_line, text_end, counts = steps[middle]
                log.seek(max(0, end - 256))
                next_cursor = dict(file=identity, position=end, anchor=hashlib.sha256(log.read(min(end, 256))).hexdigest(), skipLine=skip_line)
                candidate = dict(lines=text[:text_end], cursor=next_cursor, lossCounts=counts)
                if probe_fits(baseline, candidate):
                    best = candidate
                    low = middle + 1
                else:
                    high = middle - 1
            if best is not None:
                return dict(baseline, probeLog=best)
    except Exception:
        # The baseline is complete. Optional failures must not fail this read,
        # including unexpected decoder or packing exceptions.
        evidence['lossCounts']['readFailures'] += 1
        try:
            if probe_fits(baseline, evidence):
                return dict(baseline, probeLog=evidence)
        except Exception:
            pass

    return baseline


def clock_evidence():
    output = subprocess.run(['/usr/bin/chronyc', 'tracking'], capture_output=True, text=True, timeout=5, check=True).stdout
    require('Leap status' in output and re.search(r'Leap status\s*:\s*Normal', output), 'Unsynchronised host clock')
    def number(label):
        m = re.search(re.escape(label) + r'\s*:\s*([-+0-9.eE]+)', output)
        require(m is not None, 'Missing chrony tracking field')
        value = float(m.group(1))
        require(math.isfinite(value), 'Invalid chrony tracking value')
        return value
    error = abs(number('System time')) + abs(number('Root dispersion')) + abs(number('Root delay')) / 2
    return {'tracking': output, 'errorBoundNs': int(math.ceil(error * 1e9)),
            'rateErrorPpm': abs(number('Residual freq')) + abs(number('Skew')), 'measuredUnixNs': str(time.time_ns())}

