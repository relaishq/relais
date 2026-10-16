#!/usr/bin/env python3
"""Own Redis for units; run the named make gate and retain wall time on failure."""
import argparse,json,os,pathlib,socket,subprocess,time
p=argparse.ArgumentParser();p.add_argument('--out',required=True);p.add_argument('--gate',choices=['test-unit','test-harness-short'],required=True);a=p.parse_args();out=pathlib.Path(a.out).resolve();out.mkdir(parents=True,exist_ok=True);env=os.environ.copy();env.update(GOPROXY='off',GOFLAGS='-mod=readonly',GOMAXPROCS='4',RELAIS_TEST_REDIS_ADDR='',RELAIS_TEST_REDIS_REQUIRE='0',RELAIS_HARNESS_REQUIRE_FFMPEG='1');env['PATH']='/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin:'+env.get('PATH','');redis=None;log=None
try:
 if a.gate=='test-unit':
  with socket.socket() as s:s.bind(('127.0.0.1',0));port=s.getsockname()[1]
  assert port not in [6379,9101,9201];log=open(out/'redis.log','w');redis=subprocess.Popen(['redis-server','--bind','127.0.0.1','--port',str(port),'--save','','--appendonly','no','--dir',str(out)],stdout=log,stderr=log,env=env)
  for _ in range(100):
   if redis.poll() is not None:raise RuntimeError('owned Redis exited')
   try:
    with socket.create_connection(('127.0.0.1',port),timeout=.1):break
   except OSError:time.sleep(.03)
  else:raise RuntimeError('owned Redis readiness timeout')
  env.update(RELAIS_TEST_REDIS_ADDR=f'127.0.0.1:{port}',RELAIS_TEST_REDIS_REQUIRE='1')
 start=time.monotonic()
 with open(out/(a.gate+'.log'),'w') as f:run=subprocess.run(['make',a.gate,'GOTEST=go test -p 2'],stdout=f,stderr=subprocess.STDOUT,env=env,timeout=1100)
 result={'gate':a.gate,'exit_code':run.returncode,'wall_seconds':time.monotonic()-start,'redis_addr':env['RELAIS_TEST_REDIS_ADDR'],'redis_pid':redis.pid if redis else None,'gOMAXPROCS':4};(out/'result.json').write_text(json.dumps(result,indent=2));print(json.dumps(result),flush=True)
finally:
 if redis and redis.poll() is None:
  redis.terminate()
  try:redis.wait(timeout=3)
  except subprocess.TimeoutExpired:redis.kill();redis.wait()
 if log:log.close()
# Preserve the gate verdict as the workflow exit status as well as JSON.
raise SystemExit(0 if run.returncode == 0 else 1)
