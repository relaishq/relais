"""Appended to clock.py only for historical reads; existing commands stay intact."""


class HistoricalOS:
    def __init__(self, original, end):
        self.original, self.end = original, end

    def __getattr__(self, name):
        return getattr(self.original, name)

    def fstat(self, descriptor):
        fields = list(self.original.fstat(descriptor))
        fields[6] = min(fields[6], self.end)  # st_size, including parser lookahead.
        return self.original.stat_result(fields)


def historical_tracking(cursor, boundary, first, last):
    global READ_LIMIT, ENCODED_LIMIT, os
    def check(ok, cause):
        if not ok:
            raise ValueError('clock-history-' + cause)
    try:
        check(2 <= first <= last and (last + 1) * 1000 <= boundary, 'invalid')
        key = [boundary, first, last]
        with TRACKING_LOG.open('rb') as log:
            info = os.fstat(log.fileno())
            size = cursor['size'] if cursor else info.st_size
            check(0 < size <= 8 * 1024 * 1024, 'limit')
            data = log.read(size)
        check(len(data) == size, 'changed')
        identity = [info.st_dev, info.st_ino]
        digest = hashlib.sha256(data).hexdigest()
        if cursor:
            check(cursor['key'] == key and cursor['file'] == identity and info.st_size >= size
                  and cursor['sha256'] == digest, 'changed')
            tracking = cursor['tracking']
            end = cursor['end']
        else:
            position, offset, end = None, 0, None
            for raw in data.splitlines(keepends=True):
                try:
                    at = int(datetime.datetime.strptime(raw[:19].decode(), '%Y-%m-%d %H:%M:%S').replace(tzinfo=datetime.timezone.utc).timestamp())
                except (ValueError, UnicodeError):
                    offset += len(raw)
                    continue
                if position is None and at >= first:
                    position = offset
                if at > last:
                    end = offset
                    break
                offset += len(raw)
            check(position is not None and end is not None, 'incomplete')
            before = time.monotonic_ns()
            wall = time.time_ns()
            tracking = dict(file=str(info.st_dev)+':'+str(info.st_ino), position=position,
                            anchor=hashlib.sha256(data[max(0, position-256):position]).hexdigest(),
                            daemon=daemon_identity(), wallNs=wall, monotonicNs=before,
                            readWidthNs=time.monotonic_ns()-before, lastRow=None, skipLine=False)
        # The existing parser sees only the requested bytes, including during
        # lookahead. Linux CLOCK_MONOTONIC is shared across helper processes.
        READ_LIMIT, ENCODED_LIMIT = 4096, 20000
        original_os = os
        os = HistoricalOS(original_os, end)
        try:
            page = tracking_evidence(tracking, boundary, precision=True)
        finally:
            os = original_os
        next_line = data[page['cursor']['position']:].split(b'\n', 1)[0]
        try:
            next_at = int(datetime.datetime.strptime(next_line[:19].decode(), '%Y-%m-%d %H:%M:%S').replace(tzinfo=datetime.timezone.utc).timestamp())
        except (ValueError, UnicodeError):
            next_at = first
        page['readComplete'] = next_at > last
        page['rows'] = [r for r in page['rows'] if first <= r['unixSeconds'] <= last]
        with TRACKING_LOG.open('rb') as log:
            after = os.fstat(log.fileno())
            check(identity == [after.st_dev, after.st_ino] and hashlib.sha256(log.read(size)).hexdigest() == digest, 'changed')
        check(page['cursor'] is not None and (page['readComplete'] or page['cursor']['position'] > tracking['position']), 'incomplete')
        page['cursor'] = dict(key=key, size=size, file=identity, sha256=digest, end=end, tracking=page['cursor'])
        check(len(json.dumps(page).encode()) <= 256000
              and len(base64.b64encode(gzip.compress(json.dumps(page).encode()))) <= WIRE_LIMIT, 'limit')
        return page
    except (OSError, ValueError, KeyError, TypeError, IndexError, UnicodeError, subprocess.SubprocessError) as error:
        cause = str(error)
        return dict(cause=cause if cause in {'clock-history-' + s for s in ['invalid', 'limit', 'changed', 'gap', 'precision', 'incomplete']} else 'clock-history-gap')
