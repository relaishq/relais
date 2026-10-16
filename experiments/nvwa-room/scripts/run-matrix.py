#!/usr/bin/env python3
"""Twelve bounded sequential runs. Owns only driver children; each cleans up."""
import argparse, json, pathlib, subprocess, time, sys
p=argparse.ArgumentParser();p.add_argument('--scratch',required=True);p.add_argument('--count',type=int,default=1000);a=p.parse_args();s=pathlib.Path(a.scratch).resolve();out=s/'ws-matrix';out.mkdir(exist_ok=True)
start=time.monotonic();failed=[]
for mode in ['normal','disconnect','worker-kill','lost-receipt']:
 for trial in range(1,4):
  name=f'{mode}-{trial}';path=out/name
  command=[str(s/'bin/driver'),'-bin',str(s/'bin'),'-out',str(path),'-tone-a',str(s/'tone-440.ogg'),'-tone-b',str(s/'tone-880.ogg'),'-fault',mode,'-count',str(a.count),'-rate','30']
  run=subprocess.run(command,text=True,capture_output=True,timeout=100)
  (out/f'{name}.stdout').write_text(run.stdout);(out/f'{name}.stderr').write_text(run.stderr)
  print(json.dumps({'run':name,'exit_code':run.returncode,'elapsed_s':round(time.monotonic()-start,3),'summary':json.loads(run.stdout) if run.stdout.strip().startswith('{') else run.stdout,'stderr':run.stderr}),flush=True)
  if run.returncode:failed.append(name)
print(json.dumps({'matrix_duration_s':time.monotonic()-start,'failed':failed}),flush=True)
sys.exit(bool(failed))
