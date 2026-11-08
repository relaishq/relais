"""Appended to clock.py only for historical reads; existing commands stay intact."""


def historical_tracking(cursor, boundary, first, last):
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
                  and len(data) == size and cursor['sha256'] == digest, 'changed')
            tracking = cursor['tracking']
        else:
            position, offset, closed = None, 0, False
            for raw in data.splitlines(keepends=True):
                line = raw.decode().strip()
                if line and not line.startswith(('=', 'Date', '#')):
                    row = tracking_row(line)
                    if position is None and row['unixSeconds'] >= first:
                        position = offset
                    if row['unixSeconds'] > last:
                        closed = True
                        break
                offset += len(raw)
            check(position is not None and closed, 'incomplete')
            before = time.monotonic_ns()
            wall = time.time_ns()
            tracking = dict(file=str(info.st_dev)+':'+str(info.st_ino), position=position,
                            anchor=hashlib.sha256(data[max(0, position-256):position]).hexdigest(),
                            daemon=daemon_identity(), wallNs=wall, monotonicNs=before,
                            readWidthNs=time.monotonic_ns()-before, lastRow=None, skipLine=False)
        # Only this separately rendered command uses these smaller page limits.
        global READ_LIMIT, ENCODED_LIMIT
        READ_LIMIT, ENCODED_LIMIT = 4096, 20000
        page = tracking_evidence(tracking, boundary, precision=True)
        check(not any(b['fromUnixSeconds'] <= last and b['unixSeconds'] >= first for b in page['breaks']), 'gap')
        page['readComplete'] = any(r['unixSeconds'] > last for r in page['rows'])
        page['rows'] = [r for r in page['rows'] if first <= r['unixSeconds'] <= last]
        check(all(re.fullmatch(r'[0-9]\.[0-9]{3}e[+-][0-9]{2,3}', r['maxErrorDecimal']) for r in page['rows']), 'precision')
        with TRACKING_LOG.open('rb') as log:
            after = os.fstat(log.fileno())
            check(identity == [after.st_dev, after.st_ino] and hashlib.sha256(log.read(size)).hexdigest() == digest, 'changed')
        check(page['cursor'] is not None and (page['readComplete'] or page['cursor']['position'] > tracking['position']), 'incomplete')
        page['cursor'] = dict(key=key, size=size, file=identity, sha256=digest, tracking=page['cursor'])
        check(len(json.dumps(page).encode()) <= 256000
              and len(base64.b64encode(gzip.compress(json.dumps(page).encode()))) <= WIRE_LIMIT, 'limit')
        return page
    except (OSError, ValueError, KeyError, TypeError, IndexError, UnicodeError, subprocess.SubprocessError) as error:
        cause = str(error)
        return dict(cause=cause if cause in {'clock-history-' + s for s in ['invalid', 'limit', 'changed', 'gap', 'precision', 'incomplete']} else 'clock-history-gap')
