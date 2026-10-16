#!/usr/bin/env python3
"""Five paired, sequential targeted trials on an archived main and this tree.
No added CPU stress: both use GOMAXPROCS=4, -race/-short, no Redis or parallel
other gates. This controls current conditions; it cannot recreate historic load.
"""
import argparse,json,os,pathlib,subprocess,time
p=argparse.ArgumentParser();p.add_argument('--main',required=True);p.add_argument('--out',required=True);a=p.parse_args()
out=pathlib.Path(a.out).resolve();out.mkdir(parents=True,exist_ok=True)
env=os.environ.copy();env.update(GOPROXY='off',GOFLAGS='-mod=readonly',GOMAXPROCS='4',RELAIS_TEST_REDIS_ADDR='',RELAIS_TEST_REDIS_REQUIRE='0',RELAIS_HARNESS_REQUIRE_FFMPEG='1')
env['PATH']='/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin:'+env.get('PATH','')
targets=[('stall','./internal/loadgen','^TestStallIntervalsRecover$'),('relay-restart','./pkg/callharness','^TestRelayBufferFailurePaths$/^memory$/^relay-restart$')]
records=[]
for name,package,pattern in targets:
 # Compile both first, then measure tests without cross-tree compilation load.
 for label,root in [('main',a.main),('branch',os.getcwd())]:
  cmd=['go','test','-mod=readonly','-race','-c','-o',str(out/f'{label}-{name}.test'),package]
  subprocess.run(cmd,cwd=root,env=env,check=True)
 for trial in range(1,6):
  for label in (['main','branch'] if trial%2 else ['branch','main']):
   command=[str(out/f'{label}-{name}.test'),'-test.v','-test.short','-test.count=1','-test.timeout=30s','-test.run='+pattern]
   start=time.monotonic()
   run=subprocess.run(command,env=env,capture_output=True,text=True,timeout=40)
   (out/f'{name}-{trial}-{label}.log').write_text(run.stdout+run.stderr)
   record={'test':name,'trial':trial,'tree':label,'exit_code':run.returncode,'wall_seconds':time.monotonic()-start,'command':command}
   records.append(record);print(json.dumps(record),flush=True)
(out/'summary.json').write_text(json.dumps({'main_commit':'7dfde0b7fbb52debf0651b5f23766e1ab09dd9fd','conditions':{'GOMAXPROCS':4,'race':True,'short':True,'redis':False,'parallel_other_gates':False,'historic_full_suite_load_recreated':False},'runs':records},indent=2))
