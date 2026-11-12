"""Fake chronyc/clock, real tracking reader and its wire limits; Go drives time."""
import contextlib
import io
import base64
import gzip
import datetime
import json
import pathlib
import random
import sys

if __name__ == '__main__':
    directory, slot, now, old, *timing = sys.argv[1:]
    now, start = int(now), 1792886400
    # Optional: tracking rows every `interval` seconds at `phase`, probes from `probes`.
    interval, phase, probes = (int(v) for v in timing) if timing else (1, 0, start)
    last = start+115 if old == 'true' else now-1
    path = pathlib.Path(directory)/slot
    rng, rows = random.Random(7), []
    for at in range(start-(16 if old == 'true' else 600), last+1):
        if (at-phase) % interval:
            continue
        stamp = datetime.datetime.fromtimestamp(at, datetime.timezone.utc).strftime('%Y-%m-%d %H:%M:%S')
        v = ['%.16e' % rng.uniform(0, 1e-6) for _ in range(7)]
        rows.append(f'{stamp} 192.0.2.9 2 {v[0]} {v[1]} {v[2]} N 1 {v[3]} {v[4]} {v[5]} {v[6]} 1.000e-05\n')
    path.write_text(''.join(rows))
    probe = path.with_suffix('.probes')
    probe.write_text(''.join(f'{t} {at*10**9:019d} {at*10**9+1000:019d} {at*10**9+1000:019d} {at*10**9+2000:019d} 0\n' for at in range(max(probes, start-(14 if old == 'true' else 0)), last-1) for t in range(3)))
    lease = dict(available=True, errorBoundNs=10002, measuredUnixNs=str(last*10**9+10**8), referenceUnixNs=str(last*10**9), referenceAgeNs=10**8, pollSeconds='1', residualPpm='0.1', skewPpm='0.1', dispersionStartSeconds='0.000001000', dispersionEndSeconds='0.000001200', intervalNs=200000000, hostElapsedNs=250000000)
    scalar = dict(errorBoundNs=10002, rateErrorPpm=0.2, measuredUnixNs=lease['measuredUnixNs'])
    inject = f"TRACKING_LOG=pathlib.Path({str(path)!r}); daemon_identity=lambda:'test'; time.time_ns=lambda:{now*10**9}; time.monotonic_ns=lambda:{(now-start+1)*10**9}; clock_evidence=lambda:{scalar!r}; lease_evidence=lambda:{lease!r}"
    source = sys.stdin.read().split("python3 - <<'PY'\n",1)[1].rsplit('\nPY',1)[0]
    source = source.replace('\nimport json,gzip,base64\ntry:', '\n'+inject+'\nimport json,gzip,base64\ntry:')
    source = source.replace('\n print(base64', f"\n args['path']={str(probe)!r} if args['path'] else ''\n print(base64",1)
    with contextlib.redirect_stdout(io.StringIO()) as output:
        exec(source)
    if now == start and old != 'true' and interval == 1:
        tracking = json.loads(gzip.decompress(base64.b64decode(output.getvalue())))['trackingLog']
        assert not tracking['rows'] and tracking['breaks'][0]['kind'] == 'cursor-gap', 'oversized log was not exercised'
    print(output.getvalue(), end='')
