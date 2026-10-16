#!/usr/bin/env python3
"""Wait for owned regression gates, then run paired fixed binaries sequentially."""
import argparse,json,pathlib,subprocess,time
p=argparse.ArgumentParser();p.add_argument('--scratch',required=True);a=p.parse_args();s=pathlib.Path(a.scratch).resolve()
start=time.monotonic();deadline=start+1200
for name in ['unit','harness']:
 path=s/name/'result.json'
 while not path.exists():
  if time.monotonic()>deadline:raise RuntimeError('owned gate did not finish: '+name)
  print(json.dumps({'waiting_for_owned_gate':name}),flush=True);time.sleep(20)
 print(json.dumps({'owned_gate_finished':json.loads(path.read_text())}),flush=True)
results=[]
cmd=['python3','experiments/nvwa-room/scripts/run-matrix.py','--scratch',str(s),'--transport','paired']
t=time.monotonic();run=subprocess.run(cmd,timeout=1100);results.append({'name':'paired-matrix','command':cmd,'exit_code':run.returncode,'wall_seconds':time.monotonic()-t});print(json.dumps(results[-1]),flush=True)
for kind,transports in [('burst',['dc','ws']),('backpressure',['dc','ws']),('policies',['dc'])]:
 for transport in transports:
  name=kind+'-'+transport;out=s/name
  if out.exists():raise RuntimeError('refusing to overwrite '+name)
  cmd=[str(s/'bin'/kind),'-bin',str(s/'bin'),'-out',str(out),'-tone-a',str(s/'tone-440.ogg'),'-tone-b',str(s/'tone-880.ogg')]
  if transport=='dc' and kind!='policies':cmd.append('-datachannel')
  t=time.monotonic();run=subprocess.run(cmd,capture_output=True,text=True,timeout=100)
  (s/(name+'.stdout')).write_text(run.stdout);(s/(name+'.stderr')).write_text(run.stderr)
  results.append({'name':name,'command':cmd,'exit_code':run.returncode,'wall_seconds':time.monotonic()-t,'stdout':run.stdout,'stderr':run.stderr});print(json.dumps(results[-1]),flush=True)
(s/'stage3-run.json').write_text(json.dumps({'stages':results,'wall_seconds':time.monotonic()-start},indent=2))
raise SystemExit(any(r['exit_code'] for r in results))
